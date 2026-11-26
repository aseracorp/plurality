package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

func mkMsg(role string, content string, toolName string) utils.Message {
	m := utils.Message{Role: role}
	if role == "assistant" {
		m.Content = utils.NewTextContent(content)
	}
	if role == "tool" {
		m.Content = utils.NewTextContent(content)
		m.Name = toolName
	}
	return m
}

func TestFilterCheckpointsNoOOB(t *testing.T) {
	var msgs []utils.Message
	msgs = append(msgs, utils.Message{
		Role:      "assistant",
		ToolCalls: []utils.ToolCall{{ID: "eco_x", Type: "function", Function: utils.FunctionCall{Name: db.EcoCheckpointToolName, Arguments: "{}"}}},
	})
	msgs = append(msgs, mkMsg("tool", "summary", db.EcoCheckpointToolName))
	seq := 0
	for i := 0; i < 2100; i++ {
		role := "tool"
		if seq%4 == 0 {
			role = "user"
		} else if seq%4 == 1 {
			role = "assistant"
		}
		content := "small payload"
		if i%500 == 42 {
			content = strings.Repeat("X", 200*1024)
		}
		msgs = append(msgs, mkMsg(role, content, "system_tools__shell_exec"))
		seq++
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC: %v", r)
		}
	}()
	for i := 0; i < 100; i++ {
		out := filterCheckpointsForRequest(msgs, true)
		if len(out) == 0 {
			t.Fatalf("empty tail returned")
		}
		total := 0
		for _, m := range out {
			total += len(m.TextContent())
		}
		// NO CHAR BUDGET: the deleted capTail bounded the view by slicing it,
		// which was silent history loss, and the deleted per-message truncation
		// rewrote stored rows, which killed the KV prefix. The sent view is the
		// stored rows; bounding it is the preemptive guard's job.
		if len(out) != len(msgs) {
			t.Fatalf("sent view dropped %d of %d rows", len(msgs)-len(out), len(msgs))
		}
		for i := range out {
			if out[i].Content.TotalContentChars() != msgs[i].Content.TotalContentChars() {
				t.Fatalf("pass %d rewrote row %d: the view builder must be a no-op on content", i, i)
			}
		}
	}
	out := filterCheckpointsForRequest(msgs, false)
	if len(out) == 0 {
		t.Fatalf("eco off returned empty")
	}
}

func TestFilterCheckpointsTiny(t *testing.T) {
	msgs := []utils.Message{
		mkMsg("user", "hi", ""),
		mkMsg("assistant", "hello", ""),
	}
	out := filterCheckpointsForRequest(msgs, true)
	if len(out) != 2 {
		t.Fatalf("expected 2, got %d", len(out))
	}
}

// TestFilterNoCheckpointHugeHistory_WordPressRegression pins the exact
// failure mode of the "WordPress Security Review" conversation: eco on, NO
// checkpoint ever written (all eco summaries 500'd on the 131k fast model),
// and a ~770k-token raw history.
//
// What it pins now: NO row is dropped from the sent view (that was capTail's
// silent history loss), and every row is bounded PER MESSAGE so no single
// blob can blow the window on its own. The whole-view bound is the preemptive
// guard's job, which turns "this view no longer fits" into a durable fold.
func TestFilterNoCheckpointHugeHistory_WordPressRegression(t *testing.T) {
	var msgs []utils.Message
	// 1900 tool results ~50k chars each ≈ 95M chars ≈ 24M tokens — far
	// beyond any window, and no checkpoint anywhere.
	for i := 0; i < 1900; i++ {
		role := "tool"
		if i%4 == 0 {
			role = "user"
		} else if i%4 == 1 {
			role = "assistant"
		}
		content := strings.Repeat("y", 50_000)
		if i == 0 {
			content = strings.Repeat("z", 200_000) // one truly giant blob
		}
		msgs = append(msgs, mkMsg(role, content, "system_tools__shell_exec"))
	}
	out := filterCheckpointsForRequest(msgs, true)
	if len(out) == 0 {
		t.Fatal("empty tail")
	}
	// NO HISTORY LOSS: every live row is in the sent view.
	if len(out) != len(msgs) {
		t.Fatalf("sent view dropped %d of %d rows - the model cannot see them and "+
			"nothing records their removal; that is the history loss eco removes",
			len(msgs)-len(out), len(msgs))
	}
	max := 0
	for _, m := range out {
		if l := m.Content.TotalContentChars(); l > max {
			max = l
		}
	}
	t.Logf("sent view %d msgs, max msg %d chars", len(out), max)
	// Every row ships at its stored size: nothing on this path rewrites a row.
	for i := range out {
		if out[i].Content.TotalContentChars() != msgs[i].Content.TotalContentChars() {
			t.Fatalf("row %d rewritten by the request path", i)
		}
	}
	// The whole view is unbounded by design; the guard is what bounds it, by
	// raising the context-length error the loop folds durably in response.
	// This fixture is ~95M chars, so the guard MUST fire on a normal window.
	if err := preemptiveContextGuard("gpt-4o", SentViewTokens(out), nil, 0, 128_000, len(out)); err == nil {
		t.Fatal("preemptive guard accepted a ~95M-char view on a 128k window - " +
			"the unbounded no-pair path would ship it and die at the provider")
	}
	// And the overflow fold has a boundary for it, so the recovery is durable.
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			msgs[i].PromptTokens = 700_000
		}
	}
	if b := UltracaveOverflowFoldFrom(msgs, 24); b <= 0 {
		t.Fatalf("no overflow boundary for a never-compacted huge history: %d", b)
	}
}
