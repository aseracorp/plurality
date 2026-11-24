package ai

import (
	"regexp"
	"strconv"
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

// ---------------------------------------------------------------------------
// CHANGE 1: fenced code and tool_call arguments are BUDGETED content.
//
// Measured on a real 1467-row agentic conversation (fixture:
// /tmp/ultracave-fixture.json, 2,759,284 wire chars): the two paths that ADD
// bytes and bypass the per-row budget are (a) fenced code blocks, written
// VERBATIM (46 rows: in=383,866 out=424,843, ratio 1.107), and (b) tool_call
// arguments, rendered with compactJSON and NO budget at all (754,350 raw ->
// 764,533 out, ratio 1.013, and 52.4% of the whole excerpt). The excerpt is
// meant to compress PLAIN PROSE (that population measures ratio 0.117); these
// two tests pin the fence/args paths to the same `show` budget.
// ---------------------------------------------------------------------------

// mcHead shortens a long render for failure messages.
func mcHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated for the test log]"
}

// mcFenced builds a markdown fenced block of `lines` body lines (~50 chars
// each), so a 200-line block is ~10KB — far above any per-row show.
func mcFenced(lang string, lines int) string {
	var b strings.Builder
	b.WriteString("```" + lang + "\n")
	for i := 0; i < lines; i++ {
		b.WriteString("    fmt.Println(\"fenced body line " + strconv.Itoa(i) + "\")\n")
	}
	b.WriteString("```\n")
	return b.String()
}

// TestMegacaveFencedBlockIsBudgeted: a fenced block larger than the per-row
// show must render at most `show` chars of content plus ONE marker line that
// names the block's line and char counts — the fact that survives ("a 200-line
// go block was here") without the bulk.
func TestMegacaveFencedBlockIsBudgeted(t *testing.T) {
	const show = 200
	text := "look:\n" + mcFenced("go", 200)
	// An AGENT row: megacave never touches user rows (they render verbatim), so
	// the fence budget is a property of the rows it DOES compress.
	out := MachineExcerptMegacaveBudget([]utils.Message{mcMsg("assistant", text)}, show)

	re := regexp.MustCompile(`\[... fenced (\S+) block elided: (\d+) lines, (\d+) chars \.\.\.\]`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no fenced elision marker — the block was rendered verbatim (%d chars):\n%s", len(out), mcHead(out, 400))
	}
	if m[1] != "go" {
		t.Errorf("marker lost the fence language: %q", m[0])
	}
	if lines, _ := strconv.Atoi(m[2]); lines != 200 {
		t.Errorf("marker reports %d lines, want the 200 body lines of the block", lines)
	}
	chars, _ := strconv.Atoi(m[3])
	// The marker must report the block's REAL body size (200 lines x ~39 chars
	// + newlines), not the number of chars it kept.
	if chars < 7_000 || chars > 9_000 {
		t.Errorf("marker reports %d chars, want the ~7.9K body chars of the block", chars)
	}
	// The head of the block survives (bounded), the bulk is gone.
	if !strings.Contains(out, "fenced body line 0") {
		t.Errorf("fenced block head line was dropped:\n%s", out)
	}
	if strings.Contains(out, "fenced body line 199") {
		t.Errorf("fenced block tail was reproduced verbatim:\n%s", out)
	}
	// Bounded: label + prose line + head(<=show) + marker + separators.
	if limit := show + len(m[0]) + 40; len(out) > limit {
		t.Errorf("fenced row rendered %d chars, over show(%d)+marker(%d)+overhead: %q",
			len(out), show, len(m[0]), mcHead(out, 200))
	}
	// Deterministic.
	if again := MachineExcerptMegacaveBudget([]utils.Message{mcMsg("assistant", text)}, show); again != out {
		t.Errorf("fenced render is not byte-identical across runs")
	}
}

// TestMegacaveSmallFencedBlockStaysVerbatim: the /caveman-compress contract —
// a code block that fits inside the row budget is kept verbatim, fences and
// all. Only the oversized blocks get the elision treatment.
func TestMegacaveSmallFencedBlockStaysVerbatim(t *testing.T) {
	text := "```go\nfunc main() { println(\"hello\") }\n```"
	out := MachineExcerptMegacaveBudget([]utils.Message{mcMsg("user", text)}, 200)
	if !strings.Contains(out, "```go") || !strings.Contains(out, "func main() { println(\"hello\") }") {
		t.Errorf("small fenced block was not preserved verbatim:\n%s", out)
	}
	if strings.Contains(out, "block elided") {
		t.Errorf("small fenced block was elided:\n%s", out)
	}
}

// TestMegacaveToolCallArgsAreBudgeted: a tool_call always renders its NAME;
// the arguments render verbatim when they fit `show`, and otherwise as a
// bounded head plus a marker naming how many chars were dropped. Small args
// must stay verbatim (that is the common case, and it is lossless).
func TestMegacaveToolCallArgsAreBudgeted(t *testing.T) {
	const show = 200
	fatArgs := `{"content": "` + strings.Repeat("x", 8_000) + `"}`
	smallArgs := `{"op": "read", "path": "/tmp/x"}`
	msg := utils.Message{
		Role:    "assistant",
		Content: utils.NewTextContent("doing it"),
		ToolCalls: []utils.ToolCall{
			{ID: "call_1", Type: "function", Function: utils.FunctionCall{Name: "filesystem_server__fs_write", Arguments: fatArgs}},
			{ID: "call_2", Type: "function", Function: utils.FunctionCall{Name: "fs_read", Arguments: smallArgs}},
		},
	}
	out := MachineExcerptMegacaveBudget([]utils.Message{msg}, show)

	const fatPrefix = "[tool_call filesystem_server__fs_write] args="
	if !strings.Contains(out, fatPrefix) {
		t.Fatalf("fat tool_call lost its name line:\n%s", out)
	}
	if !strings.Contains(out, "[... args elided: ") {
		t.Fatalf("oversized tool_call args were reproduced verbatim (no elision marker), excerpt is %d chars:\n%s",
			len(out), mcHead(out, 300))
	}
	// The args payload of the fat call (head + marker) costs at most show.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, fatPrefix) {
			continue
		}
		payload := strings.TrimPrefix(line, fatPrefix)
		if len(payload) > show {
			t.Errorf("fat tool_call args payload is %d chars, over the %d char show budget", len(payload), show)
		}
		if !strings.Contains(payload, "content") {
			t.Errorf("elided args lost the call shape entirely: %q", payload)
		}
	}
	// Small args render verbatim, compacted, with no marker.
	if !strings.Contains(out, `[tool_call fs_read] args={"op":"read","path":"/tmp/x"}`) {
		t.Errorf("small tool_call args were not rendered verbatim:\n%s", out)
	}
	if strings.Count(out, "[... args elided: ") != 1 {
		t.Errorf("expected exactly one args elision marker (the oversized call):\n%s", out)
	}
	// Deterministic.
	if again := MachineExcerptMegacaveBudget([]utils.Message{msg}, show); again != out {
		t.Errorf("tool_call render is not byte-identical across runs")
	}
}

// mcBlowoutFixture builds the content shape that blew the old budget: many
// fenced blocks + fat tool_call arguments (the two populations the ratio bench
// measured at ratio 1.107 / 1.013). Every row is above the per-row show, so the
// render is entirely budget-sensitive.
func mcBlowoutRows(fenceRows, callRows int) []utils.Message {
	var msgs []utils.Message
	for i := 0; i < fenceRows; i++ {
		msgs = append(msgs, mcMsg("user", "context for change "+strconv.Itoa(i)+":\n"+mcFenced("go", 60)))
		msgs = append(msgs, mcTool("shell_exec", "output "+strconv.Itoa(i)+"\n"+mcFenced("json", 40)))
	}
	for i := 0; i < callRows; i++ {
		msgs = append(msgs, utils.Message{
			Role:    "assistant",
			Content: utils.NewTextContent("writing file " + strconv.Itoa(i)),
			ToolCalls: []utils.ToolCall{{
				ID: "call_" + strconv.Itoa(i), Type: "function",
				Function: utils.FunctionCall{
					Name:      "filesystem_server__fs_write",
					Arguments: `{"content": "` + strings.Repeat("y", 2_000) + `"}`,
				},
			}},
		})
	}
	return msgs
}

// TestMegacaveTotalCeilingBoundsExcerpt: the ceiling the per-row split cannot
// deliver. Against the pre-change renderer this shape rendered 88,449 chars for
// a 12,000 char budget (7x over) — MegacaveRowBudget's doc claimed
// "GUARANTEED not to exceed the budget", and the fenced/args paths ignored the
// budget entirely, so the guarantee was false (fixture: show=64 -> 1,458,172
// chars for a 90,000 char budget, 16x over). With the ceiling passed explicitly
// the finished excerpt never exceeds it, and the truncation is ANNOUNCED by
// exactly one marker naming the pre-truncation size.
func TestMegacaveTotalCeilingBoundsExcerpt(t *testing.T) {
	msgs := mcBlowoutRows(60, 60)
	const budget = 12_000
	show := MegacaveRowBudget(budget, len(msgs))
	if show <= 0 {
		t.Fatalf("expected a derived per-row budget, got %d", show)
	}
	// The ceiling the CALLER knows (the budget), not the per-row split.
	uncapped := MachineExcerptMegacaveBudget(msgs, show)
	if len(uncapped) <= budget {
		t.Fatalf("fixture no longer exercises the defect: render %d <= budget %d", len(uncapped), budget)
	}
	out := MachineExcerptMegacaveCapped(msgs, show, budget)
	if len(out) > budget {
		t.Fatalf("capped excerpt is %d chars, over the %d char ceiling (marker included)", len(out), budget)
	}
	if len(out) >= len(uncapped) {
		t.Fatalf("ceiling did not shrink the excerpt: %d vs %d", len(out), len(uncapped))
	}
	const marker = "[... megacave excerpt truncated at "
	if n := strings.Count(out, marker); n != 1 {
		t.Fatalf("truncation marker appears %d times, want exactly 1:\n%s", n, mcHead(out, 200))
	}
	if !strings.HasSuffix(out, "chars ...]") {
		t.Errorf("excerpt does not end on the truncation marker:\n%s", mcHead(out, 200))
	}
	// The marker names the size BEFORE truncation, so the reader can see how
	// much history was dropped.
	if !strings.Contains(out, marker+strconv.Itoa(len(uncapped))+" chars ...]") {
		t.Errorf("marker does not report the pre-truncation size %d:\n%s", len(uncapped), out)
	}
	// Tiny ceilings: when the ceiling cannot fit the announcing marker, the
	// marker IS the excerpt — the caller's bound is honoured to the marker's own
	// size and the truncation is still NAMED (never a silent drop, never an
	// overflow past the ceiling when the ceiling can hold content).
	reMarker := regexp.MustCompile(`\[... megacave excerpt truncated at \d+ chars \.\.\.\]`)
	for _, cap := range []int{1, 10, 40, 47, 60, 120, 500} {
		tiny := MachineExcerptMegacaveCapped(msgs, show, cap)
		m := reMarker.FindString(tiny)
		if m == "" {
			t.Fatalf("ceiling %d: no truncation marker in a %d char render of a %d char excerpt", cap, len(tiny), len(uncapped))
		}
		if len(tiny) > cap && len(tiny) > len(m) {
			t.Errorf("ceiling %d: render is %d chars, over the ceiling and not marker-only", cap, len(tiny))
		}
		if cap < len(m) && tiny != m {
			t.Errorf("ceiling %d: render must be the marker alone, got %d chars", cap, len(tiny))
		}
		if tiny == "" {
			t.Errorf("ceiling %d: render is empty (history dropped silently)", cap)
		}
	}
	// Determinism: the same input, including the truncation, renders byte-identical.
	if again := MachineExcerptMegacaveCapped(msgs, show, budget); again != out {
		t.Fatalf("capped render is not byte-identical across runs")
	}
	// maxTotalChars <= 0 keeps the FULL excerpt for callers that want it.
	if full := MachineExcerptMegacaveCapped(msgs, show, 0); full != uncapped {
		t.Errorf("maxTotalChars <= 0 must not truncate: %d vs %d", len(full), len(uncapped))
	}
}

// TestMegacaveBudgetedRenderStaysUnderCeilingOnProse: the prose population keeps
// its well-behaved transform AND now honours the ceiling end to end. 50 rows of
// 9KB tool dumps, budgeted render + ceiling: the total is bounded, the rows are
// elided, and the render is smaller than the uncapped one.
func TestMegacaveBudgetedRenderStaysUnderCeilingOnProse(t *testing.T) {
	const rows = 50
	const budget = 90_000
	msgs := make([]utils.Message, 0, rows)
	for i := 0; i < rows; i++ {
		msgs = append(msgs, mcTool("shell", strings.Repeat("prose word ", 900)))
	}
	show := MegacaveRowBudget(budget, rows)
	out := MachineExcerptMegacaveCapped(msgs, show, budget)
	if len(out) > budget {
		t.Errorf("prose render with ceiling is %d chars, over %d", len(out), budget)
	}
	if n := strings.Count(out, "[... megacave elided "); n != rows {
		t.Errorf("expected %d prose elision banners, got %d", rows, n)
	}
	if strings.Contains(out, "[... megacave excerpt truncated at ") {
		t.Errorf("prose shape must fit the ceiling WITHOUT truncation (no history dropped): %d chars", len(out))
	}
}
