package ai

import (
	"regexp"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// clockTailText is the per-turn time-awareness line, appended to the outgoing
// request only (never persisted).
func clockTailText() string {
	return "\n\nThe current time is " + time.Now().Format("2006-01-02 15:04")
}

// clockTailSuffixRE matches the clock line at the END of a string-content
// message, and clockOnlyRE the standalone form (the synthetic row
// appendCurrentTimeTail invents when a request has no text-bearing message).
// The timestamp is pinned to clockTailText's exact layout, so the strip is
// pattern-exact and cannot eat a user message that merely talks about the time.
var (
	clockTailSuffixRE = regexp.MustCompile(`(?s)\n\nThe current time is \d{4}-\d{2}-\d{2} \d{2}:\d{2}$`)
	clockOnlyRE       = regexp.MustCompile(`^The current time is \d{4}-\d{2}-\d{2} \d{2}:\d{2}$`)
)

// stripCurrentTimeTail is the INVERSE of appendCurrentTimeTail, used by the
// durable-fold summarisation replay (ai/eco_summarize.go).
//
// Why it exists: the fold's summary request must replay the CHAT request's
// serialized prefix byte-for-byte so the backend's KV prompt cache is reused
// for the summary call (one final prefill of the evictable context). The clock
// is the ONLY per-turn mutation the request path makes, so it is the one thing
// that must come back off. It also pollutes the roll-up: a model asked to write
// the durable checkpoint of a history that ends in "The current time is ..."
// narrates the timestamp into the summary that is stored forever.
//
// The rules mirror attachClock exactly, in reverse:
//   - string content ending in the clock line -> drop the clock line, keep the
//     message (the string stays a string);
//   - []StandardContentReq whose LAST part is a text part equal to the clock
//     line -> drop exactly that part (appendClock appends it as the last part);
//   - a standalone "system" row whose whole content IS the clock line (the
//     invented empty-request shape) -> drop the row;
//   - everything else, verbatim. Nothing is removed from the middle of a
//     message, and a user message about the current time is left alone: the
//     pattern is anchored to the exact "The current time is <timestamp>" line.
//
// It does NOT mutate the input slice or its messages: rows are copied, parts
// slices rebuilt. It is idempotent (a second pass finds nothing).
func stripCurrentTimeTail(msgs []StandardMessageReq) []StandardMessageReq {
	if len(msgs) == 0 {
		return msgs
	}
	out := make([]StandardMessageReq, 0, len(msgs))
	for i := range msgs {
		m := msgs[i]
		switch content := m.Content.(type) {
		case string:
			if m.Role == "system" && clockOnlyRE.MatchString(strings.TrimSpace(content)) {
				// The invented clock-only row: no message claimed a turn for it,
				// so it goes away entirely.
				continue
			}
			if stripped := clockTailSuffixRE.ReplaceAllString(content, ""); stripped != content {
				m.Content = stripped
			}
		case []StandardContentReq:
			if len(content) > 0 {
				last := content[len(content)-1]
				if last.Type == "text" && last.ImageURL == nil &&
					clockOnlyRE.MatchString(strings.TrimSpace(last.Text)) {
					parts := make([]StandardContentReq, len(content)-1)
					copy(parts, content[:len(content)-1])
					m.Content = parts
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// appendCurrentTimeTail attaches the current time to the OUTGOING request
// without claiming a conversational turn.
//
// The role/shape of a trailing message is not a free choice — chat templates
// assign meaning to the last message, and every wrong variant is a live
// defect. Measured against the deployed strata Qwen/Unsloth template
// (serve/chat_template.jinja, rendered with jinja2):
//
//   - trailing "system"    -> the template RAISES "System message must be at
//     the beginning." Self-hosted ChatML templates only tolerate system at
//     index 0 (they merge a leading system run), so the whole request 500s.
//   - trailing "user"      -> renders, but moves the template's last user turn
//     onto the clock line, so the model's final instruction reads only "The
//     current time is ...". The model narrates the timestamp back, stalls on
//     it, and in tool loops the injection reads as an apparent user turn.
//   - trailing "assistant" -> renders, and the model reads the clock as its
//     own prior utterance, so it keeps narrating the clock.
//   - separate trailing "tool" row: safe ONLY when the request already ends on
//     tool rows (it merges into the open tool-response group). When the
//     request ends on a user message the template opens a fresh user block
//     around the tool row, so the clock again becomes the entire final user
//     turn. It also needs a synthetic tool_call_id, which strict hosted
//     providers reject (every tool message must answer a real tool_call).
//
// So the clock rides INSIDE an existing message that already carries real
// content: no message is added, no role is claimed, no id is invented, and the
// clock is always a fragment of a message about the conversation rather than a
// message of its own.
//
// Which message: the newest one that already carries content and is not an
// "assistant" row. Assistant rows are skipped deliberately — a clock inside an
// assistant message reads as something the model already said, and that is the
// narrating-the-clock defect. Concretely, a request ending on an assistant
// tool_calls row has nil content; writing the clock there (what this function
// used to do in its default branch) turns the whole message into
// "The current time is ..." and the model imitates it (observed live: replies
// opening with a bare timestamp, tool loops narrating the clock).
//
// The invariant is "the clock is always APPENDED to content that is already
// there", never written over it: a row whose content is nil, empty, or an
// image-only part list keeps what it had (an image-only user turn stays a real
// user turn that gains one text part), so no message ever becomes the clock
// line. A row with nothing to attach to is skipped, walking back to an older
// message.
//
// Prompt caching is preserved: the system prompt and the messages before the
// attachment point stay byte-identical, so the KV prefix is reused; only the
// tail from the attachment point on is (re-)processed, which for the rare
// non-tail case is a handful of trailing messages.
func appendCurrentTimeTail(msgs []StandardMessageReq) []StandardMessageReq {
	clock := clockTailText()

	if len(msgs) == 0 {
		// No history at all (title generation, auxiliary calls): nothing can
		// claim a turn, so a plain system row is the cheapest safe shape.
		return append(msgs, StandardMessageReq{Role: "system", Content: strings.TrimSpace(clock)})
	}

	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			// The clock must never read as the model's own past speech.
			continue
		}
		if attachClock(&msgs[i], clock) {
			return msgs
		}
	}

	// Nothing in the request carries text (tool schemas only, image-only
	// rows): skip the clock for this turn. There is no content to attach it
	// to, and inventing a row is one of the known-bad shapes above.
	utils.Log("[clock_tail] no text-bearing message in this request; skipping the current-time tail this turn")
	return msgs
}

// attachClock appends the clock to a message that already carries content,
// preserving its wire shape (string stays string, parts stay parts). Returns
// false when the message has no usable text, so the caller can keep walking
// back to an older message.
func attachClock(msg *StandardMessageReq, clock string) bool {
	switch content := msg.Content.(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			return false
		}
		// A string-content message stays a string: no shape switch, so the
		// wire shape a strict provider validated is never altered.
		msg.Content = content + clock
		return true
	case []StandardContentReq:
		if len(content) == 0 {
			return false
		}
		parts := make([]StandardContentReq, 0, len(content)+1)
		parts = append(parts, content...)
		parts = append(parts, StandardContentReq{Type: "text", Text: clock})
		msg.Content = parts
		return true
	default:
		// nil / unknown shape: this message has no text of its own to carry
		// the clock. Never REPLACE its content — that is how a message
		// becomes "the clock line" and the model starts narrating it.
		return false
	}
}
