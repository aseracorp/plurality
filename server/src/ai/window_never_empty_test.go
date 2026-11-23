package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The eco tail cap must NEVER return an empty window.
//
// Every drop rule in truncateOversizedTail can fire on the same conversation at
// once: a window made only of oversized tool results whose parent assistant
// calls were dropped. An empty window is not a degraded request — it is a
// request with NO history. The model then receives the system prompt, the tool
// schemas and nothing else, and appendCurrentTimeTail takes its len(msgs)==0
// branch and invents a clock-only row, so the last thing in the prompt is
// "The current time is ...".
//
// Observed live on the deployed build: a turn arrived as ONLY that clock line,
// and one stored tool row carried 18 copies of it, because the model narrated
// the timestamp back into its own tool output.
//
// wneSys / wneModel are local: an unregistered model gets all-false caveman
// stones, so the render is a pure function of the stored rows.

func wneSys() utils.Message {
	return utils.Message{Role: "system", Content: utils.NewTextContent("WNE SYSTEM PROMPT")}
}

const wneModel = "wne-model-not-registered"

func wneToolRow(text string) utils.Message {
	return utils.Message{
		Role:       "tool",
		Name:       "system_tools__shell_exec",
		ToolCallID: "call_x",
		Content:    utils.NewTextContent(text),
	}
}

func TestTailCapNeverEmptiesTheWindow(t *testing.T) {
	// A window of nothing but oversized tool results, with NO surviving
	// assistant tool_calls to keep them anchored: every row hits the drop rule.
	var tail []utils.Message
	huge := strings.Repeat("Z", maxKeepTailChars+10)
	for i := 0; i < 5; i++ {
		tail = append(tail, wneToolRow(huge))
	}

	out := truncateOversizedTail(tail)
	if len(out) == 0 {
		t.Fatalf("tail cap emptied the window — the request would ship with NO history")
	}

	// And the request must not collapse to a clock-only row.
	all := append([]utils.Message{wneSys()}, out...)
	rows, _ := convertMessagesToOpenAI(all, utils.Model{Name: wneModel})
	history := 0
	for _, r := range rows {
		if r.Role != "system" {
			history++
		}
	}
	if history == 0 {
		t.Errorf("rendered request has no history row: appendCurrentTimeTail would invent a clock-only prompt")
		return
	}

	withClock := appendCurrentTimeTail(append([]StandardMessageReq(nil), rows...))
	if len(withClock) == 1 && withClock[0].Role == "system" {
		if s, ok := withClock[0].Content.(string); ok && strings.HasPrefix(s, "The current time is") {
			t.Errorf("EMPTY REQUEST: the model receives only %q", s)
		}
	}
}

// The kept row must stay byte-stable: the guard uses the same idempotent
// truncation as the loop above, so a second pass is a no-op.
func TestTailCapGuardRowIsByteStable(t *testing.T) {
	var tail []utils.Message
	huge := strings.Repeat("Q", maxKeepTailChars+10)
	for i := 0; i < 3; i++ {
		tail = append(tail, wneToolRow(huge))
	}

	p1 := truncateOversizedTail(tail)
	if len(p1) == 0 {
		t.Fatalf("tail cap emptied the window")
	}
	p2 := truncateOversizedTail(p1)
	if len(p2) != len(p1) {
		t.Fatalf("row count changed across passes: %d -> %d", len(p1), len(p2))
	}
	a, b := p1[0].Content.TextContent(), p2[0].Content.TextContent()
	if a != b {
		t.Errorf("guard row is not byte-stable: len %d -> %d", len(a), len(b))
	}
	if len(b) > maxKeepTailChars {
		t.Errorf("guard row exceeds the bound: %d > %d", len(b), maxKeepTailChars)
	}
}
