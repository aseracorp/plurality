package ai

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// Opt-in measurement harness: ULTRACAVE_RATIO_FIXTURE=/path/to/fixture.json
// runs the REAL megacave renderer against a REAL conversation dump and reports
// the compression rate at each budget the fold path can pick.
//
// Fixture rows are {role, text, tool_calls:[{name,arguments}], tool_call_id,
// name} — the shape /tmp/ultracave-fixture.json is dumped in.

func ulLoadFixture(t *testing.T) []utils.Message {
	t.Helper()
	path := os.Getenv("ULTRACAVE_RATIO_FIXTURE")
	if path == "" {
		t.Skip("ULTRACAVE_RATIO_FIXTURE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var rows []struct {
		Role      string `json:"role"`
		Text      string `json:"text"`
		ToolCalls []struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"tool_calls"`
		ToolCallID string `json:"tool_call_id"`
		Name       string `json:"name"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	out := make([]utils.Message, 0, len(rows))
	for _, r := range rows {
		m := utils.Message{
			Role:       r.Role,
			Content:    utils.NewTextContent(r.Text),
			ToolCallID: r.ToolCallID,
			Name:       r.Name,
		}
		for i, tc := range r.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, utils.ToolCall{
				ID:   "fixture_call_" + itoa(i),
				Type: "function",
				Function: utils.FunctionCall{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		out = append(out, m)
	}
	return out
}

func ulWireChars(msgs []utils.Message) int {
	n := 0
	for i := range msgs {
		n += SentViewRowChars(msgs[i])
	}
	for i := range msgs {
		for _, tc := range msgs[i].ToolCalls {
			n += len(tc.Function.Arguments)
		}
	}
	return n
}

func TestUltracaveCompressionRateOnRealConversation(t *testing.T) {
	msgs := ulLoadFixture(t)
	if len(msgs) < 100 {
		t.Fatalf("fixture too small to be representative: %d rows", len(msgs))
	}

	in := ulWireChars(msgs)
	t.Logf("FIXTURE: %d rows, %d wire chars (~%d tokens @3 chars)", len(msgs), in, in/3)

	// The budget the production fold path picks: durableFold's
	// chars = min(room, foldedChars/2), show = MegacaveRowBudget(chars, rows).
	// Reported for several plausible target budgets so the curve is visible.
	type probe struct {
		label string
		show  int
	}
	probes := []probe{
		{"uncapped (fixed 6000 default)", 0},
	}
	for _, tc := range []int{200_000, 100_000, 50_000, 25_000, 10_000} {
		chars := tc * 3
		if half := in / 2; half < chars {
			chars = half
		}
		probes = append(probes, probe{
			label: "budget " + itoa(tc) + " tokens (prod min(room, folded/2))",
			show:  MegacaveRowBudget(chars, len(msgs)),
		})
	}

	for _, p := range probes {
		out := MachineExcerptMegacaveBudget(msgs, p.show)
		ratio := float64(len(out)) / float64(in)
		t.Logf("%-46s show=%-6d out=%-9d ratio=%.3f  shrink=%.1f%%",
			p.label, p.show, len(out), ratio, 100*(1-ratio))
		if ratio >= 1.0 && p.show == 0 {
			t.Logf("   ^ uncapped ADDS bytes: this is the ratio>=1 finding the per-row budget exists to fix")
		}
	}

	// Production shape end-to-end: what the fold ACTUALLY ships = prior
	// (empty on a first fold) + excerpt, measured against the view it replaces.
	chars := ecoTargetTokens() * 3
	if half := in / 2; half < chars {
		chars = half
	}
	show := MegacaveRowBudget(chars, len(msgs))
	exc := MachineExcerptMegacaveBudget(msgs, show)
	t.Logf("PRODUCTION SHAPE: target=%d tokens -> show=%d, excerpt=%d chars vs %d wire = ratio %.3f",
		ecoTargetTokens(), show, len(exc), in, float64(len(exc))/float64(in))
}

// Why the ratio SATURATES: the floor of the per-row budget is
// minMegacaveRowShowChars=64, so at any budget below rows*64 the output stops
// shrinking. This attributes the residual bytes: fenced code blocks are written
// VERBATIM by design (code/paths/IDs verbatim), so they bypass the budget.
func TestUltracaveRatioFloorAttribution(t *testing.T) {
	msgs := ulLoadFixture(t)
	in := ulWireChars(msgs)

	fenced, prose := 0, 0
	for i := range msgs {
		for _, p := range msgs[i].Content.ContentParts() {
			txt := p.Text
			for strings.Contains(txt, "```") {
				head := txt[:strings.Index(txt, "```")]
				prose += len(head)
				txt = txt[strings.Index(txt, "```")+3:]
				if j := strings.Index(txt, "```"); j >= 0 {
					fenced += j
					txt = txt[j+3:]
				} else {
					fenced += len(txt)
					txt = ""
				}
			}
			prose += len(txt)
		}
	}
	t.Logf("INPUT: total=%d fenced(verbatim, unbudgeted)=%d prose=%d", in, fenced, prose)

	show := MegacaveRowBudget(30_000*3, len(msgs))
	out := MachineExcerptMegacaveBudget(msgs, show)
	t.Logf("OUT at show=%d: %d chars -> ratio %.3f", show, len(out), float64(len(out))/float64(in))

	// Rows-with-fences vs rows-without, rendered separately.
	var withFence, noFence []utils.Message
	for i := range msgs {
		all := ""
		for _, p := range msgs[i].Content.ContentParts() {
			all += p.Text
		}
		if strings.Contains(all, "```") {
			withFence = append(withFence, msgs[i])
		} else {
			noFence = append(noFence, msgs[i])
		}
	}
	for _, part := range []struct {
		label string
		rows  []utils.Message
	}{{"fence rows", withFence}, {"no-fence rows", noFence}} {
		i := ulWireChars(part.rows)
		o := len(MachineExcerptMegacaveBudget(part.rows, show))
		t.Logf("  %-14s %4d rows in=%-9d out=%-9d ratio=%.3f", part.label, len(part.rows), i, o, float64(o)/float64(i))
	}

	// Floor math: the smallest the excerpt CAN be at the budget floor.
	floor := len(msgs) * (minMegacaveRowShowChars + megacaveLabelOverheadChars)
	t.Logf("FLOOR MATH: %d rows x (%d show + %d label) = %d chars = ratio %.3f (budget cannot go below this)",
		len(msgs), minMegacaveRowShowChars, megacaveLabelOverheadChars, floor, float64(floor)/float64(in))
}

// End-to-end: run the PRODUCTION fold path over the real conversation in a temp
// DB and measure the LLM view before vs after. This is the number that matters:
// the view the next request carries, not just the excerpt in isolation.
func TestUltracaveEndToEndFoldOnRealConversation(t *testing.T) {
	msgs := ulLoadFixture(t)
	ctx, _, convID := ucDB(t)

	// Stamp cumulative prompt_tokens so the eco arithmetic has a curve.
	cum := 1_000
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			cum += SentViewRowChars(msgs[i]) / 3
			msgs[i].PromptTokens = cum
		}
	}
	// The fixture dump has no user rows at the fold boundary shape the storage
	// layer needs; ucHistory builds that shape. Here the real dump already has
	// user rows, so push it as-is.
	ucPush(t, ctx, convID, msgs...)

	view := ucLLMView(t, ctx, convID)
	before := SentViewTokens(view)
	beforeChars := ulWireChars(view)
	prior := ucSummaryText(t, ctx, convID)

	folded, err := CompactConversationMachineOnly(ctx, convID, view, prior, 12)
	if err != nil {
		t.Fatalf("CompactConversationMachineOnly: %v", err)
	}
	after := ucLLMView(t, ctx, convID)
	afterTokens := SentViewTokens(after)
	summary := ucSummaryText(t, ctx, convID)

	if folded == 0 {
		t.Fatal("fold removed 0 rows on the real conversation — compaction does not converge")
	}
	ratio := float64(afterTokens) / float64(before)
	t.Logf("END-TO-END: %d rows -> folded %d; view %d -> %d tokens; ratio %.3f (shrink %.1f%%)",
		len(view), folded, before, afterTokens, ratio, 100*(1-ratio))
	t.Logf("  wire chars %d -> %d (%.3f)", beforeChars, ulWireChars(after),
		float64(ulWireChars(after))/float64(beforeChars))
	t.Logf("  stored roll-up: %d chars (~%d tokens), rows kept live=%d, client rows=%d",
		len(summary), len(summary)/3, len(after)-2, len(ucClientRows(t, ctx, convID)))
}

func ucClientRows(t *testing.T, ctx context.Context, convID string) []utils.Message {
	t.Helper()
	c, err := db.GetConversationById(ctx, convID)
	if err != nil {
		t.Fatalf("client view: %v", err)
	}
	return c.Messages
}

// Pin the bypass: which paths write UNBUDGETED bytes.
func TestUltracaveRatioBypassAttribution(t *testing.T) {
	msgs := ulLoadFixture(t)
	in := ulWireChars(msgs)
	show := MegacaveRowBudget(30_000*3, len(msgs))

	argsRaw, argsCompact := 0, 0
	for i := range msgs {
		for _, tc := range msgs[i].ToolCalls {
			argsRaw += len(tc.Function.Arguments)
			argsCompact += len(compactJSON(tc.Function.Arguments))
		}
	}
	out := len(MachineExcerptMegacaveBudget(msgs, show))
	t.Logf("BUDGET ASKED: MegacaveRowBudget(%d chars, %d rows) -> show=%d; doc claims total <= maxChars",
		30_000*3, len(msgs), show)
	t.Logf("tool_call ARGS raw=%d, after compactJSON=%d  (compactJSON is NOT budgeted by show)", argsRaw, argsCompact)
	t.Logf("rendered total=%d  -> args are %.1f%% of the output", out, 100*float64(argsCompact)/float64(out))

	// Rows with NO tool_calls and NO fences: the fully budgeted population.
	var plain []utils.Message
	for i := range msgs {
		if len(msgs[i].ToolCalls) > 0 {
			continue
		}
		all := ""
		for _, p := range msgs[i].Content.ContentParts() {
			all += p.Text
		}
		if strings.Contains(all, "```") {
			continue
		}
		plain = append(plain, msgs[i])
	}
	i := ulWireChars(plain)
	o := len(MachineExcerptMegacaveBudget(plain, show))
	t.Logf("plain rows (no tool_calls, no fences) %4d rows in=%-9d out=%-9d ratio=%.3f  per-row out=%d (show=%d)",
		len(plain), i, o, float64(o)/float64(i), o/len(plain), show)

	// A hard cap on the finished excerpt: what the budget would then deliver.
	exc := MachineExcerptMegacaveBudget(msgs, show)
	for _, cap := range []int{150_000, 80_000, 40_000} {
		if len(exc) <= cap {
			continue
		}
		t.Logf("HARD CAP %d chars: ratio %.3f (shrink %.1f%%) — the excerpt would then honour the budget",
			cap, float64(cap)/float64(in), 100*(1-float64(cap)/float64(in)))
	}
}

// Per-population table of the CHANGE: each population rendered by the CURRENT
// renderer at the production show, plus the emitted tool_call args payload.
// Baselines measured before the change (fixture, show=64):
//
//	plain rows      825 rows in=1,545,874 out=181,546  ratio=0.117
//	fence rows       46 rows in=  383,866 out=424,843  ratio=1.107 (ADDS bytes)
//	tool_call args       754,350 raw -> 764,533 out    ratio=1.013 (ADDS bytes,
//	                 52.4% of the whole excerpt)
//	whole excerpt   2,759,284 -> 1,458,172              ratio=0.528 vs a 90,000
//	                 char budget ask = 16x over, SATURATED (50k->10k tokens,
//	                 no change).
func TestUltracavePopulationTable(t *testing.T) {
	msgs := ulLoadFixture(t)
	show := MegacaveRowBudget(30_000*3, len(msgs))

	var plain, fence, calls []utils.Message
	argsRaw, argsCompact := 0, 0
	for i := range msgs {
		for _, tc := range msgs[i].ToolCalls {
			argsRaw += len(tc.Function.Arguments)
			argsCompact += len(compactJSON(tc.Function.Arguments))
		}
		if len(msgs[i].ToolCalls) > 0 {
			calls = append(calls, msgs[i])
			continue
		}
		all := ""
		for _, p := range msgs[i].Content.ContentParts() {
			all += p.Text
		}
		if strings.Contains(all, "```") {
			fence = append(fence, msgs[i])
		} else {
			plain = append(plain, msgs[i])
		}
	}
	totalIn := ulWireChars(msgs)

	// Emitted args payload: what the renderer actually writes after "args=".
	emittedArgs := func(rows []utils.Message) int {
		out := MachineExcerptMegacaveBudget(rows, show)
		n := 0
		for _, line := range strings.Split(out, "\n") {
			if i := strings.Index(line, "] args="); i >= 0 {
				n += len(line) - i - len("] args=")
			}
		}
		return n
	}

	for _, part := range []struct {
		label string
		rows  []utils.Message
	}{{"plain rows", plain}, {"fence rows", fence}, {"tool_call rows", calls}} {
		i := ulWireChars(part.rows)
		o := len(MachineExcerptMegacaveBudget(part.rows, show))
		t.Logf("POST-CHANGE %-15s %4d rows in=%-9d out=%-9d ratio=%.3f", part.label, len(part.rows), i, o, float64(o)/float64(i))
	}
	t.Logf("POST-CHANGE tool_call ARGS   raw=%-9d compactJSON=%-9d emitted-in-excerpt=%-9d ratio(vs raw)=%.3f",
		argsRaw, argsCompact, emittedArgs(calls), float64(emittedArgs(calls))/float64(argsRaw))

	// Whole excerpt, production shape: per-row show + the caller's ceiling.
	uncapped := len(MachineExcerptMegacaveBudget(msgs, show))
	const ceiling = 80_000 // db.MaxDurableSummaryChars, what durableFold passes
	capped := MachineExcerptMegacaveCapped(msgs, show, ceiling)
	t.Logf("POST-CHANGE WHOLE EXCERPT show=%d: uncapped=%d ratio=%.3f | ceiling %d -> %d ratio=%.3f (marker=%v)",
		show, uncapped, float64(uncapped)/float64(totalIn), ceiling, len(capped),
		float64(len(capped))/float64(totalIn), strings.Contains(capped, "[... megacave excerpt truncated at "))
	if len(capped) > ceiling {
		t.Fatalf("ceiling violated: %d > %d", len(capped), ceiling)
	}

	// Determinism on the real fixture: byte-identical, twice.
	if a, b := MachineExcerptMegacaveCapped(msgs, show, ceiling), MachineExcerptMegacaveCapped(msgs, show, ceiling); a != b {
		t.Fatal("fixture render is not byte-identical across runs")
	}
}

// The owner's design question, measured: megacave FIRST, then the 100k cap —
// how much MORE history does 100k of prompt buy than 100k of raw tail?
func TestUltracaveCap100kCoverage(t *testing.T) {
	msgs := ulLoadFixture(t)
	const cap100k = 100_000

	// (a) capTail with NO megacave: keep the newest rows that fit 100k chars.
	chars, start := 0, len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		chars += SentViewRowChars(msgs[i])
		if chars > cap100k {
			start = i + 1
			break
		}
	}
	rawRows := len(msgs) - start
	t.Logf("RAW 100k cap (no megacave): keeps %d/%d rows VERBATIM, DROPS %d rows entirely (%d chars of history unseen)",
		rawRows, len(msgs), len(msgs)-rawRows, chars)

	// (b) megacave first, THEN the 100k cap.
	show := MegacaveRowBudget(cap100k, len(msgs))
	exc := MachineExcerptMegacaveCapped(msgs, show, cap100k)
	t.Logf("MEGACAVE then 100k cap: excerpt=%d chars (cap honoured=%v), per-row show=%d",
		len(exc), len(exc) <= cap100k, show)

	// Coverage: the raw cap DROPS rows outright; megacave labels EVERY row and
	// elides its bulk, so the same 100k chars carries the whole conversation's
	// shape instead of only its newest slice.
	t.Logf("COVERAGE: raw cap represents %d of %d rows (~%d tokens); megacave+cap represents ALL %d rows = %.1fx more history inside the SAME %d chars",
		rawRows, len(msgs), chars/3, len(msgs), float64(len(msgs))/float64(rawRows), cap100k)

	in := ulWireChars(msgs)
	t.Logf("HISTORY VISIBLE: raw cap sends ~%d chars of %d stored (%.2f%%); megacave+cap sends %d chars covering all rows (%.2f%% of stored, every row named)",
		chars, in, 100*float64(chars)/float64(in), cap100k, 100*float64(cap100k)/float64(in))
}
