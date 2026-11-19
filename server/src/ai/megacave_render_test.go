package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// Unit tests for the ported megacave renderer in context_compaction.go
// (MegacaveRowBudget / MachineExcerptMegacaveBudget / writeMegacave* +
// looksJSON / compactJSON / isFillerLine).
//
// These are the renderer's OWN tests: they call the functions directly. At
// 54712a1 the only coverage of this code lived inside the staged eco resolver
// tests (eco_resolver_test.go, which reaches the renderer through
// megacaveArtifactWithDropMarker / resolveEcoContextCompaction). None of that
// exists on master, so the tests here exercise the ported surface directly and
// assert the contract the doc comments claim: deterministic, no-LLM, and — for
// the BUDGETED variant — never over the budget it was handed.
//
// Helpers are named mcMsg/mcTool on purpose: mkMsg already exists in
// eco_filter_test.go with a DIFFERENT signature (3 args), and mkTool in
// 54712a1's eco_resolver_test.go, so reusing those names in this package would
// not compile.

// mcMsg builds a user/assistant message with the given text.
func mcMsg(role, text string) utils.Message {
	return utils.Message{Role: role, Content: utils.NewTextContent(text)}
}

// mcTool builds a tool-role message (e.g. a large tool result).
func mcTool(name, text string) utils.Message {
	return utils.Message{Role: "tool", Name: name, Content: utils.NewTextContent(text)}
}

// TestMegacaveRowBudget: the derivation is an exact uniform split of maxChars
// over rows, minus the per-row label overhead, clamped into
// [minMegacaveRowShowChars, ...] with 0 meaning "uncapped".
func TestMegacaveRowBudget(t *testing.T) {
	cases := []struct {
		name     string
		maxChars int
		rows     int
		want     int
	}{
		// rows <= 0: nothing to bound -> uncapped.
		{"no rows", 0, 0, 0},
		{"negative rows", 10_000, -1, 0},
		// maxChars <= 0 must NOT mean unlimited: it renders at the floor so the
		// fit test fails and the caller escalates.
		{"zero budget floors", 0, 10, minMegacaveRowShowChars},
		{"negative budget floors", -5, 10, minMegacaveRowShowChars},
		// A budget so generous that the fixed default is already tighter: 0.
		{"generous budget is uncapped", megacaveLabelOverheadChars*10 + maxMegacaveToolShowChars*10, 10, 0},
		// Exact uniform split lands above the default constant -> 0.
		{"split equal to default", megacaveLabelOverheadChars*10 + maxMegacaveToolShowChars*10, 10, 0},
		// Mid budget: (maxChars - overhead*rows) / rows.
		{"mid budget exact split", megacaveLabelOverheadChars*100 + 1000*100, 100, 1000},
		// The 1830-row shape from the doc comment: the overhead alone exceeds
		// the budget, so the per-row split goes negative and the floor applies.
		{"row-heavy fold set floors", 93_344, 1830, minMegacaveRowShowChars},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MegacaveRowBudget(tc.maxChars, tc.rows)
			if got != tc.want {
				t.Errorf("MegacaveRowBudget(%d, %d) = %d, want %d", tc.maxChars, tc.rows, got, tc.want)
			}
		})
	}
}

// TestMegacaveRowBudgetBoundsRender: the reason the derived budget exists at
// all. With showChars = MegacaveRowBudget(budget, rows) the rendered excerpt
// must not exceed `budget`, on any content shape — the property the fixed
// maxMegacaveToolShowChars constant could not deliver (measured ratio ~1.045,
// i.e. the transform ADDS characters).
func TestMegacaveRowBudgetBoundsRender(t *testing.T) {
	const rows = 50
	const budget = 90_000
	const dumpChars = 9_000

	msgs := make([]utils.Message, 0, rows)
	for i := 0; i < rows; i++ {
		msgs = append(msgs, mcTool("shell", strings.Repeat("x", dumpChars)))
	}

	show := MegacaveRowBudget(budget, rows)
	if show <= 0 {
		t.Fatalf("expected a derived budget for %d chars over %d rows, got %d (uncapped)", budget, rows, show)
	}
	if show >= maxMegacaveToolShowChars {
		t.Fatalf("budget %d over %d rows should be tighter than the default constant, got %d", budget, rows, show)
	}

	out := MachineExcerptMegacaveBudget(msgs, show)
	if len(out) > budget {
		t.Errorf("budgeted render is %d chars, over the %d char budget (showChars=%d)", len(out), budget, show)
	}

	// And the budgeted render must actually be the smaller one: every row is
	// above the cap, so each row gets the head+tail elision banner.
	uncapped := MachineExcerptMegacaveBudget(msgs, 0)
	if len(uncapped) <= len(out) {
		t.Errorf("budgeted render %d chars is not smaller than uncapped %d chars", len(out), len(uncapped))
	}
	if got := strings.Count(out, "[... megacave elided "); got != rows {
		t.Errorf("expected %d elision banners (one per oversized row), got %d:\n%s", rows, got, out)
	}
}

// TestMegacavePreservesCodeBlock: MachineExcerptMegacave must keep code blocks
// verbatim (the /caveman-compress contract).
func TestMegacavePreservesCodeBlock(t *testing.T) {
	code := "```go\nfunc main() { println(\"hello\") }\n```"
	msgs := []utils.Message{mcMsg("user", "here is my code:\n"+code)}
	out := MachineExcerptMegacave(msgs)
	if !strings.Contains(out, "func main() { println(\"hello\") }") {
		t.Errorf("megacave did not preserve code block verbatim:\n%s", out)
	}
	if !strings.Contains(out, "```go") {
		t.Errorf("megacave dropped the fence:\n%s", out)
	}
}

// TestMegacaveCompactsJSON: structured tool output is re-serialized with
// whitespace stripped, keys AND values preserved (no factual loss).
func TestMegacaveCompactsJSON(t *testing.T) {
	pretty := "{\n  \"path\": \"/usr/simplify/pl-ultracave\",\n  \"exit\": 0,\n  \"lines\": [\"a\", \"b\"]\n}"
	if !looksJSON(pretty) {
		t.Fatalf("looksJSON must accept an indented JSON object")
	}
	if looksJSON("not json at all") {
		t.Errorf("looksJSON accepted prose")
	}
	if looksJSON("{broken") {
		t.Errorf("looksJSON accepted invalid JSON")
	}

	compacted := compactJSON(pretty)
	if strings.ContainsAny(compacted, "\n") {
		t.Errorf("compactJSON left newlines in the output: %q", compacted)
	}
	if compactJSON(pretty) == pretty {
		t.Errorf("compactJSON did not strip whitespace: %q", compacted)
	}
	for _, needle := range []string{"/usr/simplify/pl-ultracave", "\"exit\"", "\"lines\""} {
		if !strings.Contains(compacted, needle) {
			t.Errorf("compactJSON lost content %q: %q", needle, compacted)
		}
	}

	// The renderer must route a JSON tool result through the compact path.
	out := MachineExcerptMegacaveBudget([]utils.Message{mcTool("shell", pretty)}, 0)
	if strings.Count(out, "\n") != 1 { // "TOOL_RESULT shell:" + one JSON line
		t.Errorf("expected the JSON tool result on a single line, got:\n%s", out)
	}
	if !strings.Contains(out, "\"path\":\"/usr/simplify/pl-ultracave\"") {
		t.Errorf("renderer did not compact the JSON body:\n%s", out)
	}
}

// TestMegacaveElidesOversizedRow: a prose row above the per-row budget is cut
// to head+tail with an elision banner naming the number of dropped chars.
func TestMegacaveElidesOversizedRow(t *testing.T) {
	text := strings.Repeat("alpha beta gamma ", 2000) // ~38KB of prose
	out := MachineExcerptMegacaveBudget([]utils.Message{mcTool("shell", text)}, 500)
	if !strings.Contains(out, "[... megacave elided ") {
		t.Fatalf("oversized row was not elided:\n%s", out)
	}
	if !strings.HasPrefix(out, "TOOL_RESULT shell:") {
		t.Errorf("missing the row label:\n%s", out)
	}
	if len(out) > 1_200 {
		t.Errorf("elided row rendered %d chars, far above the 500 char budget", len(out))
	}
	// Head and tail must both survive.
	if !strings.Contains(out, "alpha beta gamma") {
		t.Errorf("elision lost the head content:\n%s", out)
	}
}

// TestMegacaveDeterministic: no LLM call and no wall-clock input, so the same
// messages must render byte-identically on every call. Durable compaction
// re-renders the same fold set, and a byte-stable render is what keeps the
// prompt prefix cacheable.
func TestMegacaveDeterministic(t *testing.T) {
	msgs := []utils.Message{
		mcMsg("system", "system prompt"),
		mcMsg("user", "so in summary: can you fix the file at /tmp/x.go?"),
		mcTool("read_file", "line one\n\n\nline two\n   spaced   out\n"),
		mcMsg("assistant", "done"),
	}
	first := MachineExcerptMegacave(msgs)
	for i := 0; i < 5; i++ {
		if got := MachineExcerptMegacave(msgs); got != first {
			t.Fatalf("render %d differs from render 0:\n--- got ---\n%s\n--- want ---\n%s", i, got, first)
		}
	}
	// Budgeted, showChars <= 0 must be byte-identical to the uncapped variant.
	if got := MachineExcerptMegacaveBudget(msgs, -1); got != first {
		t.Errorf("showChars <= 0 is not identical to MachineExcerptMegacave:\n--- got ---\n%s\n--- want ---\n%s", got, first)
	}
	// Unknown roles are skipped entirely; the leading system row is not labelled.
	if strings.Contains(first, "system prompt") {
		t.Errorf("system row must not be rendered:\n%s", first)
	}
	if !strings.Contains(first, "USER:") || !strings.Contains(first, "ASSISTANT:") || !strings.Contains(first, "TOOL_RESULT read_file:") {
		t.Errorf("missing row labels:\n%s", first)
	}
}

// TestMegacaveRendersToolCalls: assistant tool_calls become compacted
// "[tool_call name] args=..." lines.
func TestMegacaveRendersToolCalls(t *testing.T) {
	msg := utils.Message{
		Role:    "assistant",
		Content: utils.NewTextContent("I will read it"),
		ToolCalls: []utils.ToolCall{{
			ID:   "call_1",
			Type: "function",
			Function: utils.FunctionCall{
				Name:      "filesystem_server__fs_read",
				Arguments: "{\n  \"op\": \"read\",\n  \"path\": \"/tmp/x\"\n}",
			},
		}},
	}
	out := MachineExcerptMegacave([]utils.Message{msg})
	if !strings.Contains(out, "[tool_call filesystem_server__fs_read] args=") {
		t.Fatalf("tool_call line missing:\n%s", out)
	}
	if !strings.Contains(out, "\"path\":\"/tmp/x\"") {
		t.Errorf("tool_call arguments were not compacted:\n%s", out)
	}
	if strings.Contains(out, "\n  ") {
		t.Errorf("tool_call arguments kept their indentation:\n%s", out)
	}
}

// TestMegacaveContentPartFiltering: writeMegacaveContent renders text/snippet
// parts only, labels a part that carries a Filename, and skips empty parts.
func TestMegacaveContentPartFiltering(t *testing.T) {
	msg := utils.Message{
		Role: "user",
		Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "see the attachment", Filename: "notes.txt"},
			{Type: "text", Text: ""},
			{Type: "image_url", Text: "must not be rendered", ImageURL: &utils.ContentImageURL{URL: "http://example.invalid/x.png"}},
			{Type: "snippet", Text: "snippet payload verbatim"},
		}),
	}
	out := MachineExcerptMegacave([]utils.Message{msg})
	if !strings.Contains(out, "[attachment: notes.txt]") {
		t.Errorf("attachment banner missing:\n%s", out)
	}
	if !strings.Contains(out, "snippet payload verbatim") {
		t.Errorf("snippet part was not rendered:\n%s", out)
	}
	if strings.Contains(out, "must not be rendered") {
		t.Errorf("image part text leaked into the excerpt:\n%s", out)
	}
}

// TestIsFillerLine: trivial prose lines are filler — empty lines and lines with
// no letter and no digit in them.
func TestIsFillerLine(t *testing.T) {
	filler := []string{"", "   ", "...", "---", "?!?\n\t"}
	for _, l := range filler {
		if !isFillerLine(l) {
			t.Errorf("isFillerLine(%q) = false, want true", l)
		}
	}
	notFiller := []string{"fix the bug", "42", "x=1"}
	for _, l := range notFiller {
		if isFillerLine(l) {
			t.Errorf("isFillerLine(%q) = true, want false", l)
		}
	}
}
