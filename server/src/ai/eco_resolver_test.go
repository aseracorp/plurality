package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// mkText builds a user/assistant message with the given text.
func mkMsg(role, text string) utils.Message {
	return utils.Message{Role: role, Content: utils.NewTextContent(text)}
}

// mkTool builds a tool message (e.g. a large result).
func mkTool(name, text string) utils.Message {
	return utils.Message{Role: "tool", Name: name, Content: utils.NewTextContent(text)}
}

// mkConv builds a synthetic conversation: system + N user/assistant turns,
// each turn containing a big tool dump (to make it large).
func mkConv(turns, bigToolChars int) []utils.Message {
	var msgs []utils.Message
	for i := 0; i < turns; i++ {
		msgs = append(msgs, mkMsg("user", "user request "+itoa(i)))
		msgs = append(msgs, mkMsg("assistant", "assistant reply "+itoa(i)))
		msgs = append(msgs, mkTool("shell", strings.Repeat("x", bigToolChars)))
	}
	return msgs
}

func itoa(i int) string {
	return string(rune('0' + i%10))
}

// TestResolver_BelowTriggerReturnsUnchanged: when the whole history fits the
// content budget, the staged resolver MUST return it unchanged (byte-stable
// prefix => KV caching). This is the regime that was broken before the
// trigger gate (#154) — the resolver must never megacave content that fits.
func TestResolver_BelowTriggerReturnsUnchanged(t *testing.T) {
	msgs := mkConv(3, 100) // small
	window := 65536
	budget := 60000
	out := resolveEcoContextCompaction(msgs, window, budget)
	if len(out) != len(msgs) {
		t.Fatalf("below-trigger: expected %d messages unchanged, got %d", len(msgs), len(out))
	}
	for i := range msgs {
		if out[i].TextContent() != msgs[i].TextContent() {
			t.Fatalf("message %d changed below trigger", i)
		}
	}
}

// TestResolver_OverflowStageA: when content exceeds the budget, stage A should
// keep the latest full turn verbatim and megacave everything older (so context
// is preserved via the excerpt, not dropped).
func TestResolver_OverflowStageA(t *testing.T) {
	msgs := mkConv(200, 500) // large: ~200 turns x ~600 chars
	window := 65536
	budget := 4000 // small -> forces collapse
	out := resolveEcoContextCompaction(msgs, window, budget)
	if len(out) == 0 {
		t.Fatal("resolver returned empty on overflow")
	}
	if approxTokens(out) > 6000 {
		t.Fatalf("stage A output too large: ≈%d tokens", approxTokens(out))
	}
	// The megacave excerpt should be present (assistant+tool artifact), and the
	// latest user turn should remain verbatim.
	combined := ""
	for i := range out {
		combined += out[i].TextContent()
	}
	if !strings.Contains(combined, "user request 9") {
		t.Errorf("latest user turn lost in stage A: %s", combined)
	}
	if !strings.Contains(combined, "megacave_") {
		t.Errorf("no megacave artifact in stage A output")
	}
}

// TestResolver_OverflowStageB: when even stage A is too big, stage B keeps only
// the most recent USER message + megacave of the rest (no tail).
func TestResolver_OverflowStageB(t *testing.T) {
	msgs := mkConv(500, 900) // really large
	window := 65536
	budget := 2000 // tiny -> A and B both too big, becomes B
	out := resolveEcoContextCompaction(msgs, window, budget)
	if len(out) == 0 {
		t.Fatal("resolver returned empty")
	}
	// Must contain a megacave excerpt.
	combined := ""
	for i := range out {
		combined += out[i].TextContent()
	}
	if !strings.Contains(combined, "megacave_") {
		t.Errorf("no megacave artifact in output")
	}
	if approxTokens(out) > 4000 {
		t.Errorf("output still too large: ≈%d tokens", approxTokens(out))
	}
}

// TestResolver_IdempotentOnAlreadySmall: resolving a small conversation twice
// must return the same unchanged result (stability across turns).
func TestResolver_IdempotentOnAlreadySmall(t *testing.T) {
	msgs := mkConv(2, 50)
	window := 65536
	budget := 60000
	first := resolveEcoContextCompaction(msgs, window, budget)
	second := resolveEcoContextCompaction(first, window, budget)
	if len(first) != len(second) {
		t.Fatalf("not idempotent: %d vs %d messages", len(first), len(second))
	}
	for i := range first {
		if first[i].TextContent() != second[i].TextContent() {
			t.Fatalf("message %d changed between calls", i)
		}
	}
}

// TestMegacavePreservesCodeBlock: MachineExcerptMegacave must keep code blocks
// verbatim (the /caveman-compress contract the user asked for).
func TestMegacavePreservesCodeBlock(t *testing.T) {
	code := "```go\nfunc main() { println(\"hello\") }\n```"
	msgs := []utils.Message{mkMsg("user", "here is my code:\n"+code)}
	out := MachineExcerptMegacave(msgs)
	if !strings.Contains(out, "func main() { println(\"hello\") }") {
		t.Errorf("megacave did not preserve code block verbatim:\n%s", out)
	}
}
