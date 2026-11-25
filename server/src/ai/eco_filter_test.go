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
		// No sliding window: the whole history from the checkpoint forward is
		// kept, so the total is NOT bounded. What must be bounded is each
		// individual row (the per-message cap is a pure function of a stored
		// row, so it renders identically every turn and cannot move the
		// prompt prefix).
		for _, m := range out {
			if len(m.TextContent()) > 100_000+200 {
				t.Fatalf("row not bounded by the per-message cap: %d chars", len(m.TextContent()))
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
// and a ~770k-token raw history containing one ~4.2MB inline blob.
//
// The per-message bound is what saves this conversation, NOT a sliding tail
// cap: the blob is truncated to the per-row bound so it can never blow the
// window on its own, and the whole history is kept so the prompt prefix stays
// byte-identical turn to turn. Oversized rows are bounded; nothing is slid
// out of the view. Context-window pressure is answered by the preemptive
// guard + durable compaction (rare), not by hiding history every turn.
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
	total := 0
	max := 0
	for _, m := range out {
		l := len(m.TextContent())
		total += l
		if l > max {
			max = l
		}
	}
	t.Logf("filtered %d msgs -> %d msgs, %d chars, max msg %d", len(msgs), len(out), total, max)
	// Append-only: every row survives, so the total is not bounded.
	if len(out) != len(msgs) {
		t.Fatalf("expected every row kept, got %d of %d", len(out), len(msgs))
	}
	// The per-row bound is the context-safety mechanism.
	if max > 100_000 {
		t.Fatalf("oversized message not truncated: %d", max)
	}
}
