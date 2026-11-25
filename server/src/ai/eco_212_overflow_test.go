package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_212_overflow_test.go — the self-lock #212 left in place.
//
// #212 removed capTail (right: the sliding window was silent history loss) and
// removed the second gate arm with it, reasoning:
//
//	"The lock is gone with the cap, so the arm is gone with it."
//
// The lock is not gone. It moved from capTail to the preemptive guard, and it
// is the same lock. The gate measures lastPT = the provider's prompt count of
// the last SUCCESSFUL request. The guard in index.go refuses an oversized
// request BEFORE it is sent, so the provider never counts it, so lastPT stays
// pinned at the last small request while the view that will not fit keeps
// growing. The trigger can never fire.
//
// Live trace, conv 9f0ab2352bf2bdc40e77384b, /app/data/server.log, 2026-11-25,
// on the #212 build:
//
//	06:13:52 [Eco] preemptive context guard: estimated 707728 tokens
//	         (sent view 679333 + schemas 28395) vs window 262144, 1201 live rows
//	06:13:52 [LLMLoop] context length exceeded — compacting history durably
//	06:13:52 [LLMLoop] durable fold: 1196 rows folded, history 2439 -> 5
//	         messages (view ~34042 tokens, fold #1)
//	06:55:10 [Eco] lastPT=94602  below trigger=216000 (sent view 98148)  - skip
//	08:00:21 [Eco] lastPT=111325 below trigger=216000 (sent view 131383) - skip
//
// 679333 tokens became 34042 in one turn: 95 % of the conversation gone, 1196
// rows folded, the model left with 5 messages.
//
// The annihilation is the CONSEQUENCE, not the defect. Overflow recovery must
// fit the window AND leave room for the roll-up it writes (durableFold budgets
// the roll-up at (target - liveTail) * 3), so it cuts toward half the target.
// From 679333 tokens that arithmetic IS a 95 % shed — correct for a view that
// size, and the only fold available, because the tick skipped every turn. Had
// the tick fired at the trigger, the fold to the target would have run at the
// policy's own cadence and told the user. The gate is the bug.

// e212Window is this deployment's model window (strata-swift).
const e212Window = 262_144

// e212Turn: user -> (assistant(tool_call) -> tool) x rows, each tool row
// toolChars. The live shape: agent turns, whose payload is the tool output.
func e212Turn(n, rows, toolChars int) []utils.Message {
	msgs := []utils.Message{{Role: "user", Content: utils.NewTextContent("turn " + itoa(n))}}
	for r := 0; r < rows; r++ {
		callID := "e212_" + itoa(n) + "_" + itoa(r)
		asst := utils.Message{Role: "assistant", Content: utils.NewTextContent("run")}
		asst.ToolCalls = []utils.ToolCall{{ID: callID, Type: "function",
			Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}
		tool := mkMsg("tool", strings.Repeat("x", toolChars), "system_tools__shell_exec")
		tool.ToolCallID = callID
		msgs = append(msgs, asst, tool)
	}
	return msgs
}

// TestTickGateFiresOnTheSentView: the gate must fire on EITHER measure.
//
// RED on #212: the gate is `lastPT > trigger` alone. The live 08:00:21 pair —
// lastPT 111325, sent view 131383, trigger 216000 — is a correct skip; the same
// conversation with the view past the trigger is the one that must fold, and
// #212 skips it forever.
func TestTickGateFiresOnTheSentView(t *testing.T) {
	const trigger = 216_000
	if tickNeedsFold(111_325, 220_000, trigger) {
		return
	}
	t.Fatalf("gate skipped a conversation whose sent view (220000) is over the trigger (%d) while "+
		"lastPT=111325 sits below it. The preemptive guard refuses the oversized request, so the "+
		"provider never counts it and lastPT can never reach the trigger — the self-lock #212 removed "+
		"the arm for, moved from capTail to the guard. Overflow recovery is then the only compaction, "+
		"and it cuts toward half the target (live: 679333 tokens -> 34042, 1196 rows folded).", trigger)
}

// TestTickGateTable pins the gate in both directions, so the fix cannot become
// "always fold".
func TestTickGateTable(t *testing.T) {
	const trigger = 216_000
	for _, c := range []struct {
		name     string
		lastPT   int
		view     int
		wantFold bool
	}{
		{"both below: stay quiet", 90_000, 95_000, false},
		{"live 06:55:10: both below, quiet", 94_602, 98_148, false},
		{"live 08:00:21: both below, quiet", 111_325, 131_383, false},
		{"view exactly at the trigger: quiet", 90_000, 216_000, false},
		{"lastPT crossed: fold", 240_000, 130_000, true},
		{"view crossed, lastPT stale: fold", 111_325, 220_000, true},
		{"view at the guard bound: fold", 94_602, 258_048, true},
	} {
		if got := tickNeedsFold(c.lastPT, c.view, trigger); got != c.wantFold {
			t.Fatalf("%s: lastPT=%d view=%d trigger=%d -> fold=%v, want %v",
				c.name, c.lastPT, c.view, trigger, got, c.wantFold)
		}
	}
}

// TestTickFoldsWhenTheViewCrossesTheTrigger: end to end, on the live shape.
//
// RED on #212: the tick logs "below trigger - skip" and folds nothing, so the
// conversation is left to the overflow fold.
func TestTickFoldsWhenTheViewCrossesTheTrigger(t *testing.T) {
	ctx, d, convID := ucDB(t)
	if _, err := d.Exec(`UPDATE conversations SET model_selected = ? WHERE id = ?`,
		`{"eco_mode":true}`, convID); err != nil {
		t.Fatalf("set eco mode: %v", err)
	}
	// No model in model_selected -> ecoConversationWindow is 0 -> the tick uses
	// the CONFIGURED policy verbatim.
	trigger, target := ecoConfiguredTrigger(), ecoTargetTokens()

	// 13 turns x 8 rows x 6750 chars ~ 235k tokens: over the configured trigger.
	var msgs []utils.Message
	for i := 0; i < 13; i++ {
		msgs = append(msgs, e212Turn(i, 8, 6_750)...)
	}
	view := SentViewTokens(msgs)
	if view <= trigger {
		t.Fatalf("fixture is %d tokens, must exceed the trigger %d", view, trigger)
	}
	// lastPT: the last request the provider actually counted. The guard blocks
	// the big ones, so this stays pinned well below the trigger forever.
	lastPT := trigger / 2
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			msgs[i].PromptTokens = lastPT
		}
	}
	ucPush(t, ctx, convID, msgs...)

	runEcoSummary(ctx, convID)

	after := ucLLMView(t, ctx, convID)
	viewAfter := SentViewTokens(after)
	if viewAfter == view {
		t.Fatalf("tick folded nothing: the sent view (%d tokens) is over the trigger (%d) while "+
			"lastPT=%d sits below it. The gate reads lastPT alone, and the preemptive guard keeps "+
			"lastPT stale — so the tick skips every turn and the overflow fold becomes the only "+
			"compaction (live: 679333 tokens -> 34042, 1196 rows folded, 2439 -> 5 messages).",
			view, trigger, lastPT)
	}
	// The fold lands NEAR the target, not at the floor. Slightly over target is
	// the documented tail-bound regime (a fold whose live tail alone exceeds the
	// target converges tail-bound); a fold at the newest-user-row floor is the
	// annihilation this test exists to prevent.
	if viewAfter > target*6/5 {
		t.Fatalf("fold left %d tokens, far above the target %d — the fold did not converge", viewAfter, target)
	}
	if viewAfter < target/2 {
		t.Fatalf("fold left %d tokens, under half the target %d: it shed far more than the window "+
			"required (view was %d) — the overflow-style collapse, live 679333 -> 34042",
			viewAfter, target, view)
	}
	pair := 0
	for _, m := range after {
		if m.Role == "assistant" && db.IsCheckpointMessage(m) {
			pair++
		}
	}
	if pair == 0 {
		t.Fatal("the fold minted no checkpoint pair — the conversation stays on the no-pair path")
	}
	t.Logf("tick fold: view %d -> %d tokens (trigger %d, target %d), pair minted",
		view, viewAfter, trigger, target)
}

// TestTickStaysQuietUnderTheTrigger: the new arm must not fold a view that fits
// comfortably. That is the early-fold defect #203 fixed, and the live
// 06:55:10 / 08:00:21 skips are CORRECT — pinned so this fix cannot regress
// into folding at a fifth of the trigger.
func TestTickStaysQuietUnderTheTrigger(t *testing.T) {
	ctx, d, convID := ucDB(t)
	if _, err := d.Exec(`UPDATE conversations SET model_selected = ? WHERE id = ?`,
		`{"eco_mode":true}`, convID); err != nil {
		t.Fatalf("set eco mode: %v", err)
	}
	trigger := ecoConfiguredTrigger()
	var msgs []utils.Message
	for i := 0; i < 4; i++ {
		msgs = append(msgs, e212Turn(i, 8, 6_750)...)
	}
	view := SentViewTokens(msgs)
	if view > trigger {
		t.Fatalf("fixture is %d tokens, must stay under the trigger %d", view, trigger)
	}
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			msgs[i].PromptTokens = trigger / 2
		}
	}
	ucPush(t, ctx, convID, msgs...)

	runEcoSummary(ctx, convID)

	if after := SentViewTokens(ucLLMView(t, ctx, convID)); after != view {
		t.Fatalf("tick folded a %d-token view that fits the window (now %d): folding early is not "+
			"compacting safely, it is losing context the model had room for", view, after)
	}
}
