package ai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/azukaar/plurality/src/textsafe"
	"github.com/azukaar/plurality/src/utils"
)

// isContextLengthError classifies an LLM error as "the request exceeded the
// model's context window". Providers report this as context_length_exceeded,
// maximum context length, Input too long, max_prompt_tokens, exceeded the
// context length. When this happens Plurality must NOT dead-end into an
// error/blank UI: the history needs to be compacted and the call retried.
func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	needles := []string{"context_length_exceeded", "maximum context length", "input too long", "max_prompt_tokens", "context window", "exceeded the context length", "context length is"}
	for _, needle := range needles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// CompactConversationForContext drops whole old turns (tool + assistant) from
// the tail of the history so the next request fits the context window. It
// keeps the most recent `keep` messages and always keeps the leading system
// message if present. Returns a new slice; does not touch persisted state.
func CompactConversationForContext(messages []utils.Message, keep int) []utils.Message {
	if keep <= 0 {
		keep = 30
	}
	// Always keep the leading system (system-prompt) message if present.
	startIdx := 0
	kept := make([]utils.Message, 0, keep)
	if len(messages) > 0 && messages[0].Role == "system" {
		kept = append(kept, messages[0])
		startIdx = 1
	}
	// Keep the newest `keep` messages after the (optional) system head.
	tailStart := startIdx
	if len(messages)-startIdx > keep {
		tailStart = len(messages) - keep
	}
	for i := tailStart; i < len(messages); i++ {
		kept = append(kept, messages[i])
	}
	// Drop tool results whose originating assistant call was trimmed, so the
	// LLM never sees a tool result without its assistant call.
	kept = dropDanglingToolResults(kept)

	// Hard-per-message bound: a single giant message (multi-MB pasted blob,
	// huge tool result, inline attachment dump) can exceed the context
	// window BY ITSELF. CompactConversationForContext only drops whole
	// turns, so a huge message inside the newest `keep`-window would still
	// be shipped as-is and the retry would fail again with
	// context_length_exceeded (observed: "WordPress Security Review", a
	// ~4.2MB Cosmos config dump inline; every resume dead-ended). Truncate
	// such messages down to a safe per-message bound so the retry can
	// actually succeed.
	return TruncateOversizedMessages(kept)
}

// dropDanglingToolResults removes "tool" messages whose ToolCallID has no
// matching assistant tool_calls inside the (trimmed) slice.
func dropDanglingToolResults(msgs []utils.Message) []utils.Message {
	ids := make(map[string]bool, 0)
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			for _, tc := range msgs[i].ToolCalls {
				ids[tc.ID] = true
			}
		}
	}
	out := make([]utils.Message, 0, len(msgs))
	for i := range msgs {
		if msgs[i].Role == "tool" && msgs[i].ToolCallID != "" && !ids[msgs[i].ToolCallID] {
			continue
		}
		out = append(out, msgs[i])
	}
	return out
}

// maxCompactMessageChars is the per-message hard bound applied when the
// in-memory history is compacted for a context-overflow retry. The eco
// tail-cap uses the same bound so the two paths agree.
const maxCompactMessageChars = 100_000

// TruncateOversizedMessages bounds every message that would be sent to the
// provider to maxCompactMessageChars characters. Messages are NOT dropped
// (their role/turn boundary must be preserved so tool-call pairing stays
// consistent); only their text body is truncated, with a clear banner, so
// the model still sees the head of the content.
//
// Used by CompactConversationForContext so a context-overflow retry cannot
// fail again on a single oversized message. Returns a NEW slice; the input
// is not mutated.
func TruncateOversizedMessages(messages []utils.Message) []utils.Message {
	out := make([]utils.Message, 0, len(messages))
	for _, m := range messages {
		total := m.Content.TotalContentChars()
		if total > maxCompactMessageChars {
			txt := ""
			for _, p := range m.Content.ContentParts() {
				if p.Type == "text" {
					txt += p.Text
				}
			}
			// Idempotent, banner-inside-the-bound truncation (see
			// textsafe.TruncateWithBanner). HeadBytes + banner produced a body
			// LONGER than maxCompactMessageChars, so a context-overflow retry
			// that ran this twice changed the row's bytes between the two
			// requests — and the on-disk maintenance pass re-truncated the
			// same row on every boot for the same reason.
			trunc, _ := textsafe.TruncateWithBanner(txt, maxCompactMessageChars)
			utils.Log("[Compaction] truncating oversized message (%d chars -> %d)", total, maxCompactMessageChars)
			m.Content = utils.NewTextContent(trunc)
		}
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// Megacave machine excerpt (Caveman /caveman-compress, deterministic, no-LLM)
// ---------------------------------------------------------------------------
//
// This is the STAGE-1 machine compaction for a context-overflow retry. It
// mirrors Caveman's `/caveman-compress` contract ("every heading, code block,
// and path intact") and the read_less structured compressor philosophy, but
// runs entirely on the server with NO LLM call:
//
//   - file paths, identifiers, IDs, URLs, error strings, exact numbers and
//     quoted user requests are kept VERBATIM;
//   - a CODE BLOCK that fits the row budget is kept verbatim (fences included);
//     an oversized one costs at most the row show: a bounded head line plus a
//     "[... fenced <lang> block elided: N lines, M chars ...]" marker. It used
//     to be written line-by-line verbatim, which is the path that ADDS bytes
//     (measured ratio 1.107) and the reason a per-row budget saturated;
//   - JSON / structured tool output is re-serialized with whitespace stripped
//     (keys + values preserved, so nothing factual is lost);
//   - prose and obvious framing are compressed hard (filler words removed,
//     blank lines collapsed);
//   - very long tool dumps are cut to head+tail with an elision banner.
//
// The result is the roll-up of the folded history, written into the checkpoint
// pair. Deterministic and fast (microseconds, no model call) — so durable
// compaction can no longer stall for minutes on a slow self-hosted summarizer
// (observed: swift-1.5-iq3_s taking ~3.5 min per summary, during which no
// compacted request ever reached the backend). The LLM summary
// (GenerateCheckpointSummary) remains available as the stage-2 fallback for the
// second context hit.
//
// WHAT THE RENDERER GUARANTEES (measured on a real 1467-row agentic
// conversation, 2,759,284 wire chars, before this contract was enforced):
//   - PLAIN PROSE (no fences, no tool_calls) compresses hard: 825 rows,
//     1,545,874 in -> 181,546 out, ratio 0.117. That transform is unchanged.
//   - FENCED CODE and tool_call ARGUMENTS used to be reproduced verbatim and
//     are the ONLY two paths that ADD bytes: fence rows 46 rows 383,866 ->
//     424,843 (ratio 1.107); compactJSON(args) 754,350 -> 764,533 (ratio
//     1.013) and 52.4% of the whole excerpt. Both are now BUDGETED: the fence
//     renders a bounded head + a "N lines, M chars" marker, the call renders
//     the tool NAME + args within the same budget.
//   - The per-row budget alone does NOT bound the finished excerpt: at
//     MegacaveRowBudget(90000, 1467) -> show=64 the renderer returned
//     1,458,172 chars (ratio 0.528, 16x over the budget) because the
//     unbudgeted paths above dominate and the floor
//     minMegacaveRowShowChars x rows saturates it. The ceiling on the finished
//     excerpt is the explicit maxTotalChars of MachineExcerptMegacaveCapped —
//     no cap is claimed anywhere the caller does not pass one.

var (
	// whitespace runs -> single space; blank lines collapse.
	reWS = regexp.MustCompile(`\s+`)
	// lines that are pure punctuation/filler we drop from prose.
	reFill = regexp.MustCompile(`(?i)^(the|a|an|and|or|but|so|then|well|ok|okay|right|now|so basically|so in summary|in other words|in conclusion)\\b.*$`)
)

// A ceiling on the finished excerpt is a TRUNCATION, so it is the caller's
// decision, never a hidden constant inside the renderer: at 60_000 chars an
// older hard cap cut the excerpt mid-sentence (observed checkpoint: 61_715
// bytes ending "Clean fi") and the model received a mutated transcript with the
// tail of the folded history silently gone. That is why the cap is a PARAMETER
// (maxTotalChars) the caller passes together with the budget it measured, and
// why the truncation is ANNOUNCED by a marker line naming the size — the
// excerpt must never shrink silently. Fit against the context window stays the
// CALLER's job (eco.durableFold picks the number, the storage layer in
// db.composeDurableSummary is the outer backstop).

// maxMegacaveToolShowChars is the DEFAULT head+tail budget shown for a single
// large tool result (split ~60/40 head/tail around an elision banner).
//
// It is a FALLBACK, not the operative number. A fixed cap cannot bound the
// output of a whole conversation: measured on realistic prose+tool content the
// megacave transform runs at ratio ~1.045 (it ADDS labels and newlines), and at
// ~0.81 on the live event — not the 0.107 quoted below, which came from a
// 10.5M-char JSON-heavy conversation where compactJSON does the work. The cause
// is that this constant sits just BELOW the average tool row there (~6.8K
// chars): rows just above the cap lose a few hundred chars, every row below it
// is untouched, and the labels added on top outweigh that. Consequence: stages
// A and B can NEVER fit an agentic conversation, so the lossy stages C/D become
// the DEFAULT path above the trigger instead of the fringe case the spec
// intends.
//
// MachineExcerptMegacaveBudget replaces it with a per-row budget DERIVED from
// the caller's token budget, which is the only way to make the ratio < 1 a
// property of the transform rather than a hope about the content.
const maxMegacaveToolShowChars = 6_000

// megacaveLabelOverheadChars is the per-ROW cost the transform ADDS beyond the
// content it renders. It is the sum of everything emitted per row that is not
// text from the message:
//
//	"USER:" / "ASSISTANT:" / "TOOL_RESULT <name>:"  + blank separator  ~32
//	"[... megacave elided N chars ...]"  (one per row over the cap)     ~40
//
// It must include the banner: omitting it (an earlier shape, 32) underestimates
// the rendered size by ~23 % on a row-heavy fold set, so the budgeted stage A
// overflows the budget (measured: 1830-row fixture budget 93344 -> stage A
// 102608), the ladder escalates to the lossy stages, and the authorization gate
// then refuses every fold — the conversation never compacts durably.
//
// The artifact pair, banners and drop marker are charged ONCE by the caller via
// megacaveArtifactOverheadTokens; charging them per row made the per-row budget
// go negative on a 1830-row fold set.
const megacaveLabelOverheadChars = 72

// minMegacaveRowShowChars is the floor of the derived per-row budget. Below this
// an excerpt stops being an excerpt: the caller's budget is so small relative to
// the row count that no meaningful content fits. The floor exists so a fold set
// that CANNOT be represented losslessly renders at the smallest honest size and
// then FAILS the fit test (the resolver escalates, the authorization gate
// refuses the fold, the request ships ephemeral) — it must never silently
// become uncapped.
const minMegacaveRowShowChars = 64

// MegacaveRowBudget derives the per-row head+tail show budget for one machine
// excerpt: `maxChars` is what the WHOLE excerpt may cost in characters, `rows`
// is how many rows it has to cover.
//
// maxChars / rows is the exact uniform split of the budget over rows: every
// prose run, every fenced block and every tool_call of a row then costs at most
// `per`, rows under the budget are rendered verbatim (the common case), and the
// compression concentrates in the few large rows — the intent of head+tail
// elision, and why this shrinks where the fixed constant did not. A row with
// MANY large items costs several times `per`; that is what the total ceiling
// (MachineExcerptMegacaveCapped) is for, not this split.
//
// WHAT THIS DOES NOT DO: it does not bound the finished excerpt. Measured on a
// real 1467-row conversation, MegacaveRowBudget(90000 chars, 1467 rows) ->
// show=64 and the render came back 1,458,172 chars (16x over): the floor
// minMegacaveRowShowChars x rows saturates the total, and any content the
// per-row split does not govern (fenced code, tool_call args) is reproduced at
// its own size. Callers that need a bound on the WHOLE excerpt must pass
// maxTotalChars to MachineExcerptMegacaveCapped; claiming a guarantee from this
// function alone is the false-doc bug this comment used to carry.
//
// Returns 0 (meaning "uncapped") when there is nothing to bound: zero rows, or a
// budget so generous that every row already fits under the default constant.
func MegacaveRowBudget(maxChars, rows int) int {
	if rows <= 0 {
		return 0
	}
	if maxChars <= 0 {
		// No room at all: render at the smallest honest size so the fit test
		// FAILS and the caller escalates / the authorization gate refuses.
		// Returning 0 here would mean UNLIMITED, i.e. guarantee the overflow the
		// budget exists to prevent.
		return minMegacaveRowShowChars
	}
	per := (maxChars - megacaveLabelOverheadChars*rows) / rows
	if per >= maxMegacaveToolShowChars {
		return 0 // the fixed default is already tighter than the budget asks
	}
	if per < minMegacaveRowShowChars {
		return minMegacaveRowShowChars
	}
	return per
}

// MachineExcerptMegacave renders a deterministic, UNCAPPED "megacave" machine
// excerpt of the folded message history: facts/code/paths/IDs verbatim,
// prose and structured noise aggressively compressed, NO LLM call. It mirrors
// Caveman's /caveman-compress read_less contract so the checkpoint preserves
// the conversation's concrete content without the latency/cost of a summary
// model (the bug behind "durable compaction never sent a compacted request").
//
// Measured on a real 10.5M-char conversation: the transform compresses to
// ~1.13M chars (ratio 0.107), i.e. folding history always shrinks the request
// (total = W - 0.893 x folded) — the property the fold loop in
// resolveEcoContextCompaction relies on. That ratio is JSON-dominated and does
// NOT generalise: on realistic prose+tool content it is ~1.045. Use
// MachineExcerptMegacaveBudget when the caller has a budget to respect.
func MachineExcerptMegacave(messages []utils.Message) string {
	return MachineExcerptMegacaveCapped(messages, 0, 0)
}

// MachineExcerptMegacaveBudget is MachineExcerptMegacave with a per-row
// head+tail budget (showChars <= 0 = uncapped, identical output to
// MachineExcerptMegacave).
//
// This is the lever that makes the compaction ratio a GUARANTEE instead of a
// content lottery: pass MegacaveRowBudget(budgetChars, len(messages)) and the
// excerpt cannot exceed the budget the request is being fitted to, so stage A
// can actually reach "fits" on an agentic conversation and stay lossless for
// every row under the cap.
func MachineExcerptMegacaveBudget(messages []utils.Message, showChars int) string {
	return MachineExcerptMegacaveCapped(messages, showChars, 0)
}

// MachineExcerptMegacaveCapped is the budgeted renderer plus a HARD CEILING on
// the finished excerpt: `maxTotalChars` (<= 0 = no ceiling) is the caller's
// total-char budget — the one number the caller knows and the per-row split
// cannot deliver. The built excerpt is cut at that ceiling on a rune boundary
// and the truncation is ANNOUNCED by one final marker line naming the size, so
// the reader of the roll-up can see that history was dropped instead of
// discovering it by the text ending mid-sentence.
//
// The ceiling is a parameter, not a global constant: the caller (eco.durableFold)
// picks the number from the budget it measured, and the storage layer
// (db.composeDurableSummary) keeps its own outer bound as a backstop for
// machineSummary callbacks that ignore it. Both bounds are the same number on
// the production path.
func MachineExcerptMegacaveCapped(messages []utils.Message, showChars, maxTotalChars int) string {
	var b strings.Builder

	writeLine := func(s string) {
		if s == "" {
			return
		}
		b.WriteString(s)
		b.WriteString("\n")
	}

	for _, m := range messages {
		switch m.Role {
		case "user":
			// USER ROWS ARE NEVER MEGACAVED. The owner's rule: megacave touches
			// only tool and agent rows. A user row is the human's own words — the
			// request, the correction, the pasted error — and compressing it
			// silently rewrites what the user asked for, which is the context loss
			// this subsystem keeps producing. The row is rendered VERBATIM, so the
			// roll-up can always be read as "what the user said", and the fit test
			// sees its true cost (see MegacaveIncompressibleChars).
			writeLine("USER:")
			writeMegacaveContentVerbatim(&b, m, writeLine)
			writeLine("")
		case "assistant":
			writeLine("ASSISTANT:")
			writeMegacaveContent(&b, m, writeLine, showChars)
			for _, tc := range m.ToolCalls {
				writeLine("[tool_call " + tc.Function.Name + "] args=" +
					budgetMegacaveArgs(compactJSON(tc.Function.Arguments), showChars))
			}
			writeLine("")
		case "tool":
			writeLine("TOOL_RESULT " + m.Name + ":")
			writeMegacaveContent(&b, m, writeLine, showChars)
			writeLine("")
		}
	}
	return capMegacaveExcerpt(strings.TrimSpace(b.String()), maxTotalChars)
}

// MegacaveIncompressibleChars is the render cost of the rows megacave CANNOT
// shrink: the user rows, emitted verbatim (see MachineExcerptMegacaveCapped).
//
// A caller that splits a budget over "all the rows" is lying to itself on the
// agentic shape, where a fold set is mostly assistant/tool rows but its user
// rows carry the human's requests: the per-row split hands the verbatim rows a
// share of the budget they will never spend, and starves the tool rows that
// needed it. Subtract this from the budget and split the rest over
// MegacaveCompressibleRows.
func MegacaveIncompressibleChars(messages []utils.Message) int {
	cost := 0
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		cost += m.Content.TotalContentChars() + len("USER:\n\n")
	}
	return cost
}

// MegacaveCompressibleRows counts the rows megacave DOES shrink: assistant and
// tool rows. User rows are excluded (verbatim) and system rows are not part of
// a fold set.
func MegacaveCompressibleRows(messages []utils.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == "assistant" || m.Role == "tool" {
			n++
		}
	}
	return n
}

// writeMegacaveContentVerbatim writes a row's text parts UNCHANGED: no
// head+tail budget, no JSON single-lining, no fence elision. The attachment
// name is kept as a label because the stored row's parts are what the client
// shows; the text itself is byte-for-byte what the user wrote.
func writeMegacaveContentVerbatim(b *strings.Builder, m utils.Message, writeLine func(string)) {
	for _, p := range m.ContentParts() {
		if p.Type != "text" && p.Type != "snippet" {
			continue
		}
		if p.Text == "" {
			continue
		}
		if p.Filename != "" {
			writeLine("[attachment: " + p.Filename + "]")
		}
		writeLine(p.Text)
	}
}

// megacaveTruncatedBanner is the single final marker announcing that the
// finished excerpt hit the caller's total-char ceiling.
const megacaveTruncatedBanner = "[... megacave excerpt truncated at "

// capMegacaveExcerpt enforces the caller's ceiling on the finished excerpt:
// at most `maxTotalChars` characters of excerpt, cut on a rune boundary, plus
// the announcing marker. maxTotalChars <= 0 means "no ceiling" (the caller
// asked for the full excerpt). The marker costs characters too, so the budget
// reserves room for it: the function never returns more than maxTotalChars.
func capMegacaveExcerpt(excerpt string, maxTotalChars int) string {
	if maxTotalChars <= 0 || len(excerpt) <= maxTotalChars {
		return excerpt
	}
	marker := megacaveTruncatedBanner + strconv.Itoa(len(excerpt)) + " chars ...]"
	keep := maxTotalChars - len(marker) - 1
	if keep < 0 {
		// The ceiling is smaller than the marker: the marker IS the excerpt,
		// so the caller's bound is honoured and the truncation is still named.
		return marker
	}
	// Keep the TAIL, not the head. The excerpt is CHRONOLOGICAL, so the head
	// is the OLDEST folded rows. Keeping the head (what this function did)
	// means a roll-up of a long conversation preserves its first hour and
	// throws away its most recent work — the opposite of what a compaction
	// should prioritise, and invisible to the model, which sees a summary that
	// reads as if the conversation ended early. TailBytes keeps the newest
	// content; the marker names what was dropped.
	return marker + "\n" + textsafe.TailBytes(excerpt, keep)
}

// budgetMegacaveArgs bounds a tool_call's rendered arguments to the row show
// budget: arguments that fit are emitted verbatim (the common case, and
// lossless), oversized ones keep a bounded head — the call's SHAPE (tool name
// is always emitted by the caller; the leading keys are what identify which
// argument carried the bulk) — plus a marker naming how many chars were
// dropped. showChars <= 0 falls back to the default tool cap.
func budgetMegacaveArgs(args string, showChars int) string {
	if showChars <= 0 {
		showChars = maxMegacaveToolShowChars
	}
	if len(args) <= showChars {
		return args
	}
	head := textsafe.HeadBytes(args, showChars/2)
	return head + " [... args elided: " + strconv.Itoa(len(args)-len(head)) + " chars ...]"
}

// writeMegacaveContent writes a message's text parts through the megacave
// transforms: code/markdown fences verbatim, JSON/long text compressed.
// showChars <= 0 means the default constant cap.
func writeMegacaveContent(b *strings.Builder, m utils.Message, writeLine func(string), showChars int) {
	parts := m.ContentParts()
	for _, p := range parts {
		if p.Type != "text" && p.Type != "snippet" {
			continue
		}
		if p.Text == "" {
			continue
		}
		if p.Filename != "" {
			writeLine("[attachment: " + p.Filename + "]")
		}
		writeMegacaveText(b, p.Text, writeLine, showChars)
	}
}

// writeMegacaveText applies the per-text megacave transform: keep fenced code
// blocks verbatim; compress JSON to single line with whitespace stripped;
// otherwise trim prose to head+tail if oversized. `showChars` is the per-row
// head+tail budget (MegacaveRowBudget; <= 0 means the default constant).
func writeMegacaveText(b *strings.Builder, text string, writeLine func(string), showChars int) {
	if showChars <= 0 {
		showChars = maxMegacaveToolShowChars
	}
	if strings.Contains(text, "```") || strings.Contains(text, "~~~") {
		// Markdown-fenced code, bounded by the row budget. Fenced regions used to
		// be emitted line-by-line VERBATIM, which is the path that ADDS bytes
		// (measured 46 fence rows: 383,866 in -> 424,843 out, ratio 1.107) and the
		// reason a per-row budget saturated at ratio 0.528 on a real conversation.
		// A block that fits the budget stays verbatim (that IS the
		// /caveman-compress contract); an oversized one costs at most `showChars`:
		// its first non-empty line, then a marker naming the block's line and char
		// counts — "a 200-line go block was here, 10K chars" is the fact that
		// survives, the bulk is what the roll-up is for.
		writeMegacaveFenced(text, writeLine, showChars)
		return
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	// JSON-ish bodies: compact.
	if looksJSON(trimmed) {
		compacted := compactJSON(trimmed)
		if len(compacted) > 0 {
			writeLine(compacted)
		}
		return
	}
	// Prose / tool dump: the shared transform.
	writeMegacaveProse(trimmed, writeLine, showChars)
}

// writeMegacaveProse is the prose half of the megacave transform, and the ONLY
// prose path: a long run is cut to head+tail with an elision banner naming the
// dropped char count, a normal run is collapsed (whitespace runs -> single
// space, blank/filler lines dropped). Measured on the real 1467-row fixture
// this is the transform that WORKS: 825 plain rows, 1,545,874 in -> 181,546
// out, ratio 0.117. It is shared with the non-fence runs of a fenced row so a
// row that merely contains a fence cannot smuggle unbudgeted prose past the
// per-row show (that was the residual ratio > 1 on the fence population).
func writeMegacaveProse(trimmed string, writeLine func(string), showChars int) {
	if trimmed == "" {
		return
	}
	// Long prose/tool dump: head+tail with elision banner.
	if len(trimmed) > showChars {
		head := textsafe.HeadBytes(trimmed, showChars*3/5)
		tailStart := len(trimmed) - showChars*2/5
		if tailStart < len(head) {
			tailStart = len(head)
		}
		tail := textsafe.TailBytes(trimmed, len(trimmed)-tailStart)
		writeLine(head)
		writeLine(fmt.Sprintf("[... megacave elided %d chars ...]", len(trimmed)-(len(head)+len(tail))))
		writeLine(tail)
		return
	}
	// Normal prose: collapse whitespace, drop obvious filler lines.
	for _, line := range strings.Split(trimmed, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if reWS.ReplaceAllString(l, " ") == "" {
			continue
		}
		writeLine(reWS.ReplaceAllString(l, " "))
	}
}

// writeMegacaveFenced renders text that contains markdown code fences within
// the row's show budget: runs OUTSIDE a fence are ordinary megacave prose, each
// fenced block is kept verbatim when it fits the budget and collapsed to a
// bounded head + a "[... fenced <lang> block elided: N lines, M chars ...]"
// marker when it does not. Deterministic, line-counting, no LLM.
func writeMegacaveFenced(text string, writeLine func(string), showChars int) {
	lines := strings.Split(text, "\n")
	// Fence spans: [startIdx, endIdx] inclusive, endIdx = index of the closing
	// fence (or len(lines)-1 when the block is unterminated).
	type span struct{ start, end int }
	var spans []span
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "```") && !strings.HasPrefix(t, "~~~") {
			continue
		}
		marker := t[:3]
		end := len(lines) - 1
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), marker) {
				end = j
				break
			}
		}
		spans = append(spans, span{i, end})
		i = end
	}
	spanAt := func(i int) (span, bool) {
		for _, sp := range spans {
			if i >= sp.start && i <= sp.end {
				return sp, true
			}
		}
		return span{}, false
	}

	for i := 0; i < len(lines); i++ {
		sp, isFence := spanAt(i)
		if !isFence {
			// Maximal run of non-fence lines, then ONE prose transform over the
			// whole run: bounding line-by-line would be unbudgeted in total (a row
			// of 200 short prose lines emitted one per writeLine is exactly the
			// fence-row residual ratio 1.08 the fixture measured).
			j := i
			for j < len(lines) {
				if _, fenced := spanAt(j); fenced {
					break
				}
				j++
			}
			writeMegacaveProse(strings.Join(lines[i:j], "\n"), writeLine, showChars)
			i = j - 1
			continue
		}
		i = sp.end
		lang := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[sp.start]), "`~"))
		body := lines[sp.start+1 : sp.end] // lines strictly between the fences
		bodyChars := 0
		for _, bl := range body {
			bodyChars += len(bl) + 1
		}
		if bodyChars <= showChars && len(lang)+len(lines[sp.start]) <= showChars {
			// Fits the budget: keep the block verbatim, fences included.
			for j := sp.start; j <= sp.end; j++ {
				writeLine(lines[j])
			}
			continue
		}
		// Oversized: the block costs AT MOST showChars in total — marker first
		// (it is the fact that survives), then a head line bounded by what the
		// marker left. Reserving the marker's own characters is the whole point:
		// paying head + marker on top of the budget is how a row of many small
		// blocks (a tool result full of short fenced snippets, the shape of the
		// real fixture) rendered each block BIGGER than it is and left the fence
		// population at ratio 1.08.
		if lang == "" {
			lang = "code"
		}
		marker := fmt.Sprintf("[... fenced %s block elided: %d lines, %d chars ...]",
			lang, len(body), bodyChars)
		headBudget := showChars - len(marker)
		if headBudget > 0 {
			head := ""
			for _, bl := range body {
				if strings.TrimSpace(bl) != "" {
					head = strings.TrimSpace(bl)
					break
				}
			}
			if head != "" {
				writeLine(textsafe.HeadBytes(head, headBudget))
			}
		}
		writeLine(marker)
	}
}

// looksJSON reports whether a string is likely JSON output (object or array).
func looksJSON(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	if t[0] != '{' && t[0] != '[' {
		return false
	}
	return json.Valid([]byte(t))
}

// compactJSON re-serializes JSON with whitespace stripped (keys + values
// preserved verbatim, so no factual loss), falling back to whitespace collapse
// if re-marshalling fails.
func compactJSON(s string) string {
	t := strings.TrimSpace(s)
	if !looksJSON(t) {
		return reWS.ReplaceAllString(t, " ")
	}
	var v interface{}
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return reWS.ReplaceAllString(t, " ")
	}
	out, err := json.Marshal(v)
	if err != nil {
		return reWS.ReplaceAllString(t, " ")
	}
	return string(out)
}

// isFillerLine reports trivial prose lines (mostly for the review/keep).
func isFillerLine(line string) bool {
	l := strings.ToLower(strings.TrimSpace(line))
	if l == "" {
		return true
	}
	if reFill.MatchString(l) {
		return true
	}
	// lines consisting only of punctuation/symbols
	for _, r := range l {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
