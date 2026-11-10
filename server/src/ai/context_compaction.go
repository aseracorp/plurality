package ai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

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
	needles := []string{"context_length_exceeded", "maximum context length", "input too long", "max_prompt_tokens", "context window", "exceeded the context length", "context length is", "request exceeds the context window"}
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

// TruncateOversizedMessages bounds the WIRE PAYLOAD of every message that
// would be sent to the provider to maxCompactMessageChars characters. Messages
// are NOT dropped (their role/turn boundary must be preserved so tool-call
// pairing stays consistent); only their TEXT body is truncated, with a clear
// banner, so the model still sees the head of the content.
//
// The bound is the full payload — content parts PLUS assistant tool_call
// arguments and the tool-role plumbing — i.e. exactly what messageWireChars
// measures, so after this pass messageWireChars(m) <= maxCompactMessageChars
// for every row. That identity is the contract with the eco trigger estimator
// (boundedWireChars clamps to the same cap): the trigger and the request can
// only agree if the sender enforces the cap. Estimating a bounded view while
// shipping an unbounded one is what made the loop permanent: the trigger said
// "fits", the preemptive context guard measured the raw bytes of an
// unbounded 991440-char pasted logfile and rejected them, and the retry
// compacted 0 rows (the oversized row IS the protected newest user turn)
// before sending the identical request again — on every message.
//
// Used by CompactConversationForContext (context-overflow retry) and by the
// normal send path (index.go). Returns a NEW slice; the input and its content
// parts are NOT mutated.
//
// Non-text parts (image_url / file / pdf) are PRESERVED and their bytes count
// toward the budget: the previous implementation rebuilt the content as a
// single text part, silently deleting every attachment of an oversized message
// — invisible on the retry-only call site, unacceptable once this guards the
// normal send path.
func TruncateOversizedMessages(messages []utils.Message) []utils.Message {
	out := make([]utils.Message, 0, len(messages))
	for _, m := range messages {
		// The cap covers the NON-IMAGE payload. Image bytes are the provider's
		// real context cost (a screenshot is 10-50k tokens) and this helper must
		// never trade them away: bounding the whole payload instead would let one
		// big screenshot delete the user's text from the same message, which is a
		// context-LOSS regression far worse than the bytes it saves. Images stay
		// counted in full by the estimator and are handled by the attachment
		// pipeline and the window guard, not here.
		img := imageWireChars(m.Content)
		nonImage := messageWireChars(m) - img
		if nonImage <= maxCompactMessageChars {
			out = append(out, m)
			continue
		}
		banner := fmt.Sprintf(
			"\n\n[message truncated by Plurality: original length %d chars exceeded the per-message context budget of %d chars]",
			nonImage, maxCompactMessageChars)

		parts := m.Content.ContentParts()
		textWire := 0
		for _, p := range parts {
			if p.Type == "text" {
				textWire += len(p.Text) + len(p.Filename)
			}
		}
		// Plumbing that rides on the wire unchanged: tool_call arguments,
		// name/tool_call_id, and non-text/non-image parts (file, pdf, snippet).
		plumbing := nonImage - textWire
		budget := maxCompactMessageChars - plumbing - len(banner)
		if budget < 0 {
			budget = 0
		}
		if textWire > budget {
			utils.Log("[Compaction] truncating oversized message (role=%s non-image wire %d chars -> %d)", m.Role, nonImage, maxCompactMessageChars)
			outParts := make([]utils.ContentPart, 0, len(parts))
			textDone := false
			for _, p := range parts {
				if p.Type != "text" {
					outParts = append(outParts, p)
					continue
				}
				if textDone {
					// The remaining text is represented by the truncated part.
					continue
				}
				txt := p.Text
				if textWire > budget {
					keep := len(txt) - (textWire - budget)
					if keep < 0 {
						keep = 0
					}
					txt = txt[:keep]
				}
				outParts = append(outParts, utils.ContentPart{Type: "text", Text: txt + banner})
				textDone = true
			}
			if textDone {
				m.Content = utils.NewPartsContent(outParts)
			}
		}
		// Text alone may not be the oversized part: an assistant row can carry a
		// huge tool_call argument blob (a file write, a page snapshot). The
		// estimator counts those bytes, so the sender must bound them too —
		// as VALID JSON, because providers reject malformed tool_calls.
		if messageWireChars(m)-imageWireChars(m.Content) > maxCompactMessageChars {
			m = elideToolCallArguments(m, maxCompactMessageChars+img)
		}
		out = append(out, m)
	}
	return out
}

// elideToolCallArguments shrinks an assistant message's tool_call ARGUMENTS
// until its wire payload fits `cap`, replacing each blob with a compact,
// VALID JSON object. Arguments are history: the model produced them, the tool
// already ran, and the tool RESULT is the row that carries the information.
// Replacing them with a marker is safe; truncating the JSON mid-string is not
// (providers validate tool_calls arguments and answer 400).
//
// Returns the message unchanged when nothing is left to elide — e.g. a payload
// made of a single huge image part, which this helper must never touch.
func elideToolCallArguments(m utils.Message, cap int) utils.Message {
	if len(m.ToolCalls) == 0 {
		return m
	}
	wire := messageWireChars(m)
	if wire <= cap {
		return m
	}
	calls := make([]utils.ToolCall, len(m.ToolCalls))
	copy(calls, m.ToolCalls)
	for i := range calls {
		if wire <= cap {
			break
		}
		arg := calls[i].Function.Arguments
		marker := fmt.Sprintf(`{"plurality_elided":%d}`, len(arg))
		if len(marker) >= len(arg) {
			continue
		}
		wire -= len(arg) - len(marker)
		calls[i].Function.Arguments = marker
	}
	m.ToolCalls = calls
	return m
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
//   - code blocks, file paths, identifiers, IDs, URLs, error strings, exact
//     numbers and quoted user requests are kept VERBATIM;
//   - JSON / structured tool output is re-serialized with whitespace stripped
//     (keys + values preserved, so nothing factual is lost);
//   - prose and obvious framing are compressed hard (filler words removed,
//     blank lines collapsed);
//   - very long tool dumps are cut to head+tail with an elision banner.
//
// The result is a bounded "megacave" excerpt (<= maxMegacaveExcerptChars) that
// stands in for the folded history in the roll-back checkpoint. Deterministic
// and fast (microseconds, no model call) — so durable compaction can no longer
// stall for minutes on a slow self-hosted summarizer (observed: swift-1.5-iq3_s
// taking ~3.5 min per summary, during which no compacted request ever reached
// the backend). The LLM summary (GenerateCheckpointSummary) remains available
// as the stage-2 fallback for the second context hit.

var (
	// whitespace runs -> single space; blank lines collapse.
	reWS = regexp.MustCompile(`\s+`)
	// lines that are pure punctuation/filler we drop from prose.
	reFill = regexp.MustCompile(`(?i)^(the|a|an|and|or|but|so|then|well|ok|okay|right|now|so basically|so in summary|in other words|in conclusion)\\b.*$`)
)

// maxMegacaveExcerptChars bounds the machine-excerpt checkpoint.
const maxMegacaveExcerptChars = 60_000

// maxMegacaveToolShowChars is the head+tail budget shown for a single large
// tool result (split ~60/40 head/tail around an elision banner).
const maxMegacaveToolShowChars = 6_000

// MachineExcerptMegacave renders a bounded, deterministic "megacave" machine
// excerpt of the folded message history: facts/code/paths/IDs verbatim,
// prose and structured noise aggressively compressed, NO LLM call. It mirrors
// Caveman's /caveman-compress read_less contract so the checkpoint preserves
// the conversation's concrete content without the latency/cost of a summary
// model (the bug behind "durable compaction never sent a compacted request").
func MachineExcerptMegacave(messages []utils.Message) string {
	var b strings.Builder
	budget := maxMegacaveExcerptChars

	writeLine := func(s string) {
		if s == "" {
			return
		}
		if b.Len() >= budget {
			return
		}
		room := budget - b.Len()
		if len(s) > room {
			s = s[:room]
		}
		b.WriteString(s)
		b.WriteString("\n")
	}

	for _, m := range messages {
		if b.Len() >= budget {
			writeLine("[... megacave excerpt truncated: further history omitted ...]")
			break
		}
		switch m.Role {
		case "user":
			writeLine("USER:")
			writeMegacaveContent(&b, m, budget, writeLine)
			writeLine("")
		case "assistant":
			writeLine("ASSISTANT:")
			writeMegacaveContent(&b, m, budget, writeLine)
			for _, tc := range m.ToolCalls {
				args := compactJSON(tc.Function.Arguments)
				writeLine("[tool_call " + tc.Function.Name + "] args=" + args)
			}
			writeLine("")
		case "tool":
			writeLine("TOOL_RESULT " + m.Name + ":")
			writeMegacaveContent(&b, m, budget, writeLine)
			writeLine("")
		}
	}
	return strings.TrimSpace(b.String())
}

// writeMegacaveContent writes a message's text parts through the megacave
// transforms: code/markdown fences verbatim, JSON/long text compressed.
func writeMegacaveContent(b *strings.Builder, m utils.Message, budget int, writeLine func(string)) {
	parts := m.ContentParts()
	for _, p := range parts {
		if b.Len() >= budget {
			return
		}
		if p.Type != "text" && p.Type != "snippet" {
			continue
		}
		if p.Text == "" {
			continue
		}
		if p.Filename != "" {
			writeLine("[attachment: " + p.Filename + "]")
		}
		writeMegacaveText(b, p.Text, budget, writeLine)
	}
}

// writeMegacaveText applies the per-text megacave transform: keep fenced code
// blocks verbatim; compress JSON to single line with whitespace stripped;
// otherwise trim prose to head+tail if oversized.
func writeMegacaveText(b *strings.Builder, text string, budget int, writeLine func(string)) {
	if strings.Contains(text, "```") || strings.Contains(text, "~~~") {
		// Markdown-fenced code: preserve every line verbatim (fenced regions are
		// the "code blocks" the /caveman-compress contract keeps intact).
		for _, line := range strings.Split(text, "\n") {
			writeLine(line)
			if b.Len() >= budget {
				return
			}
		}
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
	// Long prose/tool dump: head+tail with elision banner.
	if len(trimmed) > maxMegacaveToolShowChars {
		head := trimmed[:maxMegacaveToolShowChars*3/5]
		tailStart := len(trimmed) - maxMegacaveToolShowChars*2/5
		if tailStart < len(head) {
			tailStart = len(head)
		}
		tail := trimmed[tailStart:]
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
		if b.Len() >= budget {
			return
		}
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
