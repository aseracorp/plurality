package ai

import (
	"github.com/azukaar/plurality/src/utils"
)

// The eco trigger has to predict the size of the request that goes ON THE WIRE.
// utils.MessageContent.TotalContentChars counts ONLY text parts — right for UI
// rendering, wrong for a size estimate. Two payload classes ride on the wire and
// are invisible to it:
//
//   - image / document parts. PrepareMessagesForAI re-inflates attachments to
//     base64; a single screenshot is 10-50k tokens.
//   - assistant tool_call ARGUMENTS: a shell command, a file-write payload, a
//     page snapshot — marshalled into the request like any other content.
//
// Live evidence, one request, two estimates (conversation aeb657c899e600ff1a822ffb):
//
//	[Eco] content ≈52508 tokens ≤ trigger 209715 — sending unchanged (stable prefix)
//	[Eco] preemptive context guard: estimated 438246 tokens vs window 262144
//	[LLMLoop] context length exceeded — compacting history (durable) and retrying
//	[LLMLoop] compacted conversation ...: durable removed 0, in-memory 6 -> 6 messages
//	[LLMLoop] Error calling LLM : request estimated to exceed the context window
//
// The trigger shipped the history UNCHANGED because its 52k reading sat under
// the threshold, while the very same request measured 438k on the wire. The
// staged resolver's `fits` used that same blind estimator, concluded "full
// history fits", durable compaction folded 0 messages, and every retry
// re-shipped the identical oversized request — the conversation dead-ended
// exactly as the user reported: it grew to the context size and then failed to
// continue, with compaction never kicking in.
//
// strata rejects as soon as prompt + reserved output reaches max_context
// (262144): the observed prompt sizes cluster at 79066 / 79829 / 80621, each
// with max_tokens = 262144 - prompt_tokens — eight tokens of headroom. Compaction
// must therefore fire on the COMPLETE request size with the output budget
// reserved (see ecoReplyReserve).

// contentWireChars counts every byte of message content that reaches the
// provider: text parts, image data-URLs, and document/file parts.
func contentWireChars(mc utils.MessageContent) int {
	total := 0
	for _, p := range mc.ContentParts() {
		if p.ImageURL != nil {
			total += len(p.ImageURL.URL)
			continue
		}
		total += len(p.Text) + len(p.Filename)
	}
	return total
}

// messageWireChars is the full wire payload of one message: its content parts
// PLUS the assistant's tool_call arguments and the tool-role plumbing
// (name / tool_call_id) that is marshalled alongside them.
func messageWireChars(m utils.Message) int {
	chars := contentWireChars(m.Content)
	for i := range m.ToolCalls {
		tc := &m.ToolCalls[i]
		chars += len(tc.Function.Name) + len(tc.Function.Arguments) + len(tc.ID)
	}
	chars += len(m.Name) + len(m.ToolCallID)
	return chars
}

// imageWireChars is the share of a message payload contributed by image parts.
// The request path STRIPS images for a non-vision action model
// (stripImagesForNonVisionModel), so that call site subtracts this volume to
// keep the estimate aligned with what the model actually receives. For a vision
// model the image bytes ARE sent and MUST count.
func imageWireChars(mc utils.MessageContent) int {
	total := 0
	for _, p := range mc.ContentParts() {
		if p.ImageURL != nil {
			total += len(p.ImageURL.URL)
		}
	}
	return total
}

// sentViewImageTokens estimates the image volume of the view that would be
// sent — compacted rows excluded, same rule as SentViewTokens.
func sentViewImageTokens(messages []utils.Message) int {
	chars := 0
	for i := range messages {
		if messages[i].Compacted {
			continue
		}
		chars += imageWireChars(messages[i].Content)
	}
	return chars / 3
}
