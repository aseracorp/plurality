package ai

import (
	"strings"
	"time"
)

// clockTailText is the per-turn time-awareness line, appended to the outgoing
// request only (never persisted).
func clockTailText() string {
	return "\n\nThe current time is " + time.Now().Format("2006-01-02 15:04")
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
// Appending the text into the newest message sidesteps all of it: no message
// is added, so no role is claimed, no id is invented, every backend accepts
// the request, and the clock rides along with real content.
//
// Prompt caching is preserved: the system prompt and the whole history stay
// byte-identical, so the KV prefix is reused; only the tail of the newest
// message (and the model's reply) is (re-)processed. Appending at the END of
// the newest message costs only the handful of tokens after the append point.
func appendCurrentTimeTail(msgs []StandardMessageReq) []StandardMessageReq {
	clock := clockTailText()

	if len(msgs) == 0 {
		// No history at all (title generation, auxiliary calls): nothing can
		// claim a turn, so a plain system row is the cheapest safe shape.
		return append(msgs, StandardMessageReq{Role: "system", Content: strings.TrimSpace(clock)})
	}

	last := msgs[len(msgs)-1]
	switch content := last.Content.(type) {
	case string:
		last.Content = content + clock
	case []StandardContentReq:
		parts := make([]StandardContentReq, 0, len(content)+1)
		parts = append(parts, content...)
		parts = append(parts, StandardContentReq{Type: "text", Text: clock})
		last.Content = parts
	default:
		// nil / unknown shape (e.g. an assistant message carrying only
		// tool_calls): a plain string is the safest wire shape — providers
		// accept string content for every role, part arrays only reliably for
		// user messages.
		last.Content = strings.TrimSpace(clock)
	}
	msgs[len(msgs)-1] = last
	return msgs
}
