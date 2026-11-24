package ai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// Ultracave frozen compaction — the load-bearing tests.
//
// The contract under test: between two durable folds, the WIRE BYTES of the
// history sent to the provider must be a byte-for-byte PREFIX of what is sent
// on the next turn. Nothing in the request path may alter a row that was
// already sent, and nothing may drop a row from the HEAD, because KV prompt
// caching (llama.cpp/avalon/strata, cloud prefix caches) reuses the longest
// common prefix — one changed byte near the head and the whole conversation
// is re-prefilled.
//
// Helpers are prefixed uc_ : mkMsg (eco_filter_test.go), checkpointMsg /
// toolMsg (eco_test.go) and mcMsg / mcTool (megacave_render_test.go) already
// exist in this package with different signatures.

// ucSystemMsg is a fixed system prompt: a constant, so it is part of the
// stable prefix the tests compare.
func ucSystemMsg() utils.Message {
	return utils.Message{Role: "system", Content: utils.NewTextContent("ULTRACAVE TEST SYSTEM PROMPT")}
}

// ucModelName is deliberately unregistered: activeCavemanStones returns
// all-false for an unknown model, so the render under test carries NO
// caveman projection — the bytes are a pure function of the stored rows.
const ucModelName = "ultracave-test-model-not-registered"

// ucRenderWire renders a stored slice EXACTLY the way SendChatCompletion
// does — filterCheckpointsForRequest (eco on), prepend the system prompt,
// convertMessagesToOpenAI — and returns the WIRE BYTES of the message list
// plus the rendered rows.
//
// The bytes are the per-row marshals joined by ',': that is the exact byte
// sequence of the `messages` array's ELEMENTS, which is what a chat template
// renders and what the KV prefix matches on. (Marshalling the whole slice
// appends the array's closing bracket, which moves every time a row is
// appended and would make the prefix assertion trivially false.)
func ucRenderWire(t *testing.T, msgs []utils.Message) ([]byte, []StandardMessageReq) {
	t.Helper()
	visible := filterCheckpointsForRequest(msgs, true)
	all := append([]utils.Message{ucSystemMsg()}, visible...)
	rendered, _ := convertMessagesToOpenAI(all, utils.Model{Name: ucModelName})
	return ucMarshalRows(t, rendered), rendered
}

// ucMarshalRows concatenates the marshalled wire bytes of each rendered row.
func ucMarshalRows(t *testing.T, rows []StandardMessageReq) []byte {
	t.Helper()
	var out []byte
	for i := range rows {
		b, err := json.Marshal(rows[i])
		if err != nil {
			t.Fatalf("marshal rendered message: %v", err)
		}
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, b...)
	}
	return out
}

// ucPairBytes renders JUST the checkpoint pair (the first content after the
// system block) — the "prompt head" whose stability every fold contract
// protects.
func ucPairBytes(t *testing.T, msgs []utils.Message) []byte {
	t.Helper()
	var pair []utils.Message
	for _, m := range msgs {
		if db.IsCheckpointMessage(m) {
			pair = append(pair, m)
		}
	}
	if len(pair) == 0 {
		return nil
	}
	rendered, _ := convertMessagesToOpenAI(pair, utils.Model{Name: ucModelName})
	return ucMarshalRows(t, rendered)
}

// ucTurn appends one assistant(tool_call) + tool-result turn.
func ucTurn(id int, toolName string, toolBody string) []utils.Message {
	callID := "call_" + string(rune('a'+id%26)) + "_" + itoa(id)
	return []utils.Message{
		{
			Role:    "assistant",
			Content: utils.NewTextContent("calling " + toolName + " step " + itoa(id)),
			ToolCalls: []utils.ToolCall{{
				ID:       callID,
				Type:     "function",
				Function: utils.FunctionCall{Name: toolName, Arguments: "{}"},
			}},
		},
		{
			Role:       "tool",
			Name:       toolName,
			ToolCallID: callID,
			Content:    utils.NewTextContent(toolBody),
		},
	}
}

// itoa keeps the fixtures dependency-free (no strconv noise in the tests).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// ucHistory builds a conversation with `turns` tool round-trips spread over
// several user turns. Multiple user rows matter: the newest user turn is the
// FLOOR durable compaction never folds past, so a fixture with a single
// leading user row can never be folded at all (the storage layer returns 0).
//
// Row content is prose + tool output (the realistic shape; the megacave
// ratio notes in context_compaction.go explain why JSON-only fixtures lie).
func ucHistory(userText string, turns int, toolChars int) []utils.Message {
	msgs := []utils.Message{{Role: "user", Content: utils.NewTextContent(userText)}}
	for i := 0; i < turns; i++ {
		if i > 0 && i%4 == 0 {
			msgs = append(msgs, utils.Message{Role: "user",
				Content: utils.NewTextContent("continue step " + itoa(i))})
		}
		body := "file=/srv/app/module_" + itoa(i) + ".go\n" +
			strings.Repeat("handle request and write the result to disk ", 8) +
			itoa(i)
		if len(body) < toolChars {
			body += strings.Repeat("x", toolChars-len(body))
		}
		msgs = append(msgs, ucTurn(i, "system_tools__shell_exec", body)...)
	}
	return msgs
}

// TestUltracaveSentHistoryPrefixIsByteIdenticalAcrossTurns is THE
// byte-identity test: snapshot the marshalled wire bytes, simulate a turn
// (append an assistant + tool row), re-render, and assert the previous
// render is a byte prefix of the new one.
//
// Pre-fold (no checkpoint pair): rows are only ever appended. Post-fold: the
// pair is the head and stays byte-identical, so the same property must hold
// across the next N turns. Both shapes are checked here.
func TestUltracaveSentHistoryPrefixIsByteIdenticalAcrossTurns(t *testing.T) {
	msgs := ucHistory("start the task", 6, 900)

	prev, _ := ucRenderWire(t, msgs)
	if len(prev) == 0 {
		t.Fatal("empty render")
	}
	for turn := 0; turn < 12; turn++ {
		// A turn the provider would answer: user (only on the first), then
		// an assistant tool round-trip.
		if turn == 5 {
			msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent("keep going " + itoa(turn))})
		} else {
			msgs = append(msgs, ucTurn(100+turn, "system_tools__fs_read", "line 42: ok "+strings.Repeat("y", 300))...)
		}
		next, rendered := ucRenderWire(t, msgs)
		if !bytes.HasPrefix(next, prev) {
			t.Fatalf("turn %d: sent history is NOT a byte prefix of the next request (old %d bytes, new %d bytes)\nfirst divergence at %d",
				turn, len(prev), len(next), firstDivergence(prev, next))
		}
		if len(next) <= len(prev) {
			t.Fatalf("turn %d: prefix did not grow (%d -> %d) — cache reuse cannot grow", turn, len(prev), len(next))
		}
		// The clock is the ONE tail mutation the request path is allowed:
		// it appends into the newest content-bearing row. Everything before
		// the attachment point must stay byte-identical.
		withClock := appendCurrentTimeTail(rendered)
		if len(withClock) != len(rendered) {
			t.Fatalf("turn %d: clock tail changed the message count (%d -> %d)", turn, len(rendered), len(withClock))
		}
		prev = next
	}
}

// TestUltracaveFrozenPrefixSurvivesPastTheTailCapBudget is the test the other
// byte-identity fixtures do NOT cover: a conversation whose live tail is
// BIGGER than the old tail-cap budget. Every earlier fixture stays under that budget, so
// the sliding tail-cap was a no-op there and the prefix assertion passed
// against the old sliding-cap code too (verified by mutation: restoring
// `return the sliding tail cap(tail)` left those tests green).
//
// Above the budget, the sliding tail cap walks backwards from the newest row, accumulates
// ~100k chars and backs up to a user row — so the START index of the sent
// history moves on (almost) every turn. The first bytes of the prompt then
// change every turn and the provider's prefix cache is invalidated from the
// head: reuse collapses to the system block and each agent turn re-prefills
// tens of thousands of tokens.
//
// The frozen-prefix contract: the sent view opens on the checkpoint pair and
// carries EVERY live row after it, so the previous render is a byte prefix of
// the next one no matter how large the conversation grows. Bounding the view
// is durable compaction's job, not a per-request window's.
func TestUltracaveFrozenPrefixSurvivesPastTheTailCapBudget(t *testing.T) {
	// Past the budget on purpose: ~75 turns x ~3k chars ≈ 225k chars of live
	// tail, i.e. > the old tail-cap budget (which is 200k).
	hist := append([]utils.Message{checkpointMsg(), toolMsg()},
		ucHistory("refactor the storage layer", 75, 3_000)...)

	liveChars := 0
	for _, m := range hist[2:] {
		liveChars += m.Content.TotalContentChars()
	}
	if liveChars <= 200_000 {
		t.Fatalf("fixture is %d chars, must exceed 200k to exercise a large live tail",
			liveChars)
	}

	prev, _ := ucRenderWire(t, hist)
	if len(prev) == 0 {
		t.Fatal("empty render")
	}
	for turn := 0; turn < 8; turn++ {
		hist = append(hist, ucTurn(200+turn, "system_tools__shell_exec",
			"output "+strings.Repeat("q", 2_000))...)
		next, _ := ucRenderWire(t, hist)

		if !bytes.HasPrefix(next, prev) {
			t.Fatalf("turn %d: sent history is NOT a byte prefix of the next request "+
				"(old %d bytes, new %d bytes; first divergence at %d) — the sent view is "+
				"re-anchoring its head, which is the sliding tail-cap this change removes",
				turn, len(prev), len(next), firstDivergence(prev, next))
		}
		if len(next) <= len(prev) {
			t.Fatalf("turn %d: prefix did not grow (%d -> %d)", turn, len(prev), len(next))
		}
		prev = next
	}
}
