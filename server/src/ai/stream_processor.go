package ai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// ErrStreamIdleTimeout is returned when a provider accepted a streaming
// request (200 OK) but then sent no bytes within the idle window. The window
// is model-aware (see StreamProcessor.streamIdleTimeout): some self-hosted
// model families can legitimately take minutes to prefill a large cached
// prompt before the first token, especially when the GPU is busy with other
// concurrent requests, so they get a much longer budget than cloud models.
// A zero-byte idle timeout must never be treated as a clean end-of-stream.
var ErrStreamIdleTimeout = errors.New("SSE stream idle timeout (no bytes received)")

// StreamProcessor reads an LLM streaming response and writes events to an ActiveRequest.
// It accumulates text and tool calls, broadcasts SSE events, and returns the
// assembled assistant Message when the stream ends.
type StreamProcessor struct {
	request      *ActiveRequest
	model        utils.Model
	conversation utils.Conversation
}

// streamIdleTimeout returns how long ProcessStandardStream will wait for the
// first/next byte from the provider before giving up. Self-hosted model
// families with slow prefills (matched by isSlowPrefillModel) get 6 minutes:
// a long prefill on a big prompt at native speed takes minutes, and the GPU
// can be occupied by another conversation's generation at the same time.
// Cloud models get a tighter 2 minutes — a cloud endpoint silent for that
// long is virtually always broken, and retrying earlier (see RunLLMLoop)
// hides the failure faster.
func (sp *StreamProcessor) streamIdleTimeout() time.Duration {
	name := strings.ToLower(sp.model.Name)
	if isSlowPrefillModel(name) {
		return 6 * time.Minute
	}
	return 2 * time.Minute
}

// slowPrefillModelMarkers are substrings matched (case-insensitively) against
// model names to identify families that prefill large prompts slowly — the
// "long idle" class of self-hosted/local deployments. Add your own local
// model families here; cloud models must stay on the 2-minute timeout.
var slowPrefillModelMarkers = []string{"strata", "swift"}

// isSlowPrefillModel reports whether a (lowercased) model name belongs to a
// slow-prefill family, e.g. "strata-swift:foo" or "swift-1.5".
func isSlowPrefillModel(name string) bool {
	for _, marker := range slowPrefillModelMarkers {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// NewStreamProcessor creates a StreamProcessor bound to an ActiveRequest.
func NewStreamProcessor(request *ActiveRequest, model utils.Model, conversation utils.Conversation) *StreamProcessor {
	return &StreamProcessor{
		request:      request,
		model:        model,
		conversation: conversation,
	}
}

// broadcastText sends a text chunk to all SSE clients and accumulates it in the buffer.
func (sp *StreamProcessor) broadcastText(content string) {
	if strings.TrimSpace(content) == "" {
		// Whitespace-only chunk: skip the SSE broadcast AND the buffer. Without
		// this check, a model that emits leading newline(s) before its first
		// token (common with DeepSeek over OpenRouter) would render an empty
		// "assistant" bubble on the client and persist whitespace-only text
		// that then re-enters history on every turn.
		return
	}
	sp.request.TextBuffer.WriteString(content)
	sp.request.Broadcast(SSEEvent{
		Type:             "text",
		Content:          content,
		ConversationID:   sp.request.ConversationID,
		Model:            &sp.model,
		TotalTokens:      sp.request.TokenUsage,
		PromptTokens:     sp.request.PromptTokens,
		CompletionTokens: sp.request.CompletionTokens,
		ResponseCost:     sp.request.ResponseCost,
		Title:            sp.conversation.Title,
	})
}

// accumulateToolCall collects a new tool call or appends arguments to the last one.
func (sp *StreamProcessor) accumulateToolCall(id, name, arguments string) {
	if name != "" {
		sp.request.ToolCallBuffer = append(sp.request.ToolCallBuffer, utils.ToolCall{
			ID:   id,
			Type: "function",
			Function: utils.FunctionCall{
				Name:      name,
				Arguments: "",
			},
		})
	}
	if arguments != "" && len(sp.request.ToolCallBuffer) > 0 {
		last := &sp.request.ToolCallBuffer[len(sp.request.ToolCallBuffer)-1]
		last.Function.Arguments += arguments
	}
}

// mintProviderCallID makes an assistant tool-call id unique across the WHOLE
// conversation, not just this turn. Providers (e.g. DeepSeek v4 Flash over
// OpenRouter) recycle short ids like "call_0" on every turn, so an id that was
// already persisted in an earlier assistant message collides with later turns.
// Namespacing on the number of already-persisted messages guarantees two tool
// calls can never share an id (even the same provider id twice in one turn),
// while staying short and deterministic.
func (sp *StreamProcessor) mintProviderCallID(providerID string) string {
	// One namespace per persisted message: len() is the number of messages
	// already persisted, and each new assistant message appends to the array,
	// so the namespace strictly increases turn over turn.
	ns := len(sp.conversation.Messages) + 1000
	if providerID == "" {
		return fmt.Sprintf("call_%d_a", ns)
	}
	return fmt.Sprintf("call_%d_%s", ns, providerID)
}

// parseXMLToolCall converts a Claude/Anthropic-style XML tool call snippet
// (emitted by some self-hosted models in the plain-text stream instead of
// the native OpenAI tool_calls delta channel) into a structured ToolCall.
//
// Accepted shapes (whitespace-tolerant):
//
//	<tool_call>
//	<function=filesystem_server__fs_write>
//	<parameter=path>
//	/usr/simplify/foo.py
//	</parameter>
//	<parameter=content>
//	...
//	</parameter>
//	</function>
//	</tool_call>
//
// and the Claude shorthand with named open/close:
//
//	<function_calls>
//	<invoke name="tool_name">
//	<parameter name="arg">value</parameter>
//	</invoke>
//	</function_calls>
//
// The function/parameter delimiters are parsed as raw string ranges (NOT
// re-escaped as HTML entities), so the values are byte-identical to what the
// model wrote — matching how the native tool_calls channel would carry them.
func parseXMLToolCall(snippet string) (utils.ToolCall, bool) {
	// Extract the tool name from either <function=NAME> or <invoke name="NAME">.
	var fnName string
	lower := strings.ToLower(snippet)
	if i := strings.Index(lower, "<function="); i >= 0 {
		start := i + len("<function=")
		if end := strings.Index(snippet[start:], ">"); end >= 0 {
			fnName = strings.TrimSpace(snippet[start : start+end])
		}
	} else if i := strings.Index(lower, "<invoke"); i >= 0 {
		// name="NAME" or name='NAME'
		if j := strings.Index(lower[i:], "name="); j >= 0 {
			ns := i + j + len("name=")
			quote := snippet[ns]
			if quote == '"' || quote == '\'' {
				rest := snippet[ns+1:]
				if e := strings.Index(rest, string(quote)); e >= 0 {
					fnName = strings.TrimSpace(rest[:e])
				}
			}
		}
	}
	if fnName == "" {
		return utils.ToolCall{}, false
	}

	// Extract all <parameter=KEY>VALUE</parameter> pairs (or <parameter name="KEY">VALUE</parameter>).
	args := map[string]string{}
	paramRe := regexp.MustCompile(`(?is)<parameter(?:\s+name)?\s*=\s*["']?([A-Za-z0-9_]+)["']?\s*>(.*?)</parameter>`)
	for _, m := range paramRe.FindAllStringSubmatch(snippet, -1) {
		key := m[1]
		val := strings.TrimSpace(m[2])
		args[key] = val
	}

	// Marshal args to JSON in insertion order.
	var buf strings.Builder
	buf.WriteByte('{')
	first := true
	for k, v := range args {
		if !first {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(v)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
		first = false
	}
	buf.WriteByte('}')

	return utils.ToolCall{
		ID:   "xml_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		Type: "function",
		Function: utils.FunctionCall{
			Name:      fnName,
			Arguments: buf.String(),
		},
	}, true
}

// extractXMLToolCalls scans the raw accumulated text for XML-format tool call
// snippets and returns (call, rest) where rest is the text with those snippets
// removed. It converts the Claude-style multi-call wrapper as well:
//
//	<function_calls>
//	<invoke name="a"><parameter name="x">1</parameter></invoke>
//	<invoke name="b"><parameter name="y">2</parameter></invoke>
//	</function_calls>
//
// Returns nil calls if no XML tool-call block is found.
func extractXMLToolCalls(text string) ([]utils.ToolCall, string) {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "<tool_call") && !strings.Contains(lower, "<function_calls") && !strings.Contains(lower, "<invoke") {
		return nil, text
	}
	var calls []utils.ToolCall
	rest := text

	// 1) <tool_call>...</tool_call> blocks.
	blockRe := regexp.MustCompile(`(?is)<tool_call\s*>.*?</tool_call\s*>`)
	rest = blockRe.ReplaceAllStringFunc(rest, func(m string) string {
		if tc, ok := parseXMLToolCall(m); ok {
			calls = append(calls, tc)
			return "\x00" // marker removed below
		}
		return m
	})

	// 2) <function_calls>...</function_calls> wrappers containing <invoke>.
	wrapperRe := regexp.MustCompile(`(?is)<function_calls\s*>.*?</function_calls\s*>`)
	rest = wrapperRe.ReplaceAllStringFunc(rest, func(m string) string {
		inner := m
		invokeRe := regexp.MustCompile(`(?is)<invoke\b[^>]*>.*?</invoke\s*>`)
		found := false
		inner = invokeRe.ReplaceAllStringFunc(inner, func(inv string) string {
			if tc, ok := parseXMLToolCall(inv); ok {
				calls = append(calls, tc)
				found = true
				return "\x00"
			}
			return inv
		})
		if found {
			return inner
		}
		return m
	})

	// Lone <invoke> without wrapper.
	if len(calls) == 0 {
		invokeRe := regexp.MustCompile(`(?is)<invoke\b[^>]*>.*?</invoke\s*>`)
		rest = invokeRe.ReplaceAllStringFunc(rest, func(inv string) string {
			if tc, ok := parseXMLToolCall(inv); ok {
				calls = append(calls, tc)
				return "\x00"
			}
			return inv
		})
	}

	// Remove \x00 markers and tidy.
	rest = strings.ReplaceAll(rest, "\x00", "")
	rest = strings.TrimSpace(rest)
	return calls, rest
}

// buildAssistantMessage creates the final assistant Message from accumulated buffers.
func (sp *StreamProcessor) buildAssistantMessage() utils.Message {
	message := utils.Message{
		Role:             "assistant",
		Timestamp:        time.Now().Format(time.RFC3339),
		TotalTokens:      sp.request.TokenUsage,
		PromptTokens:     sp.request.PromptTokens,
		CompletionTokens: sp.request.CompletionTokens,
		ResponseCost:     sp.request.ResponseCost,
		Model:            sp.model,
	}

	text := strings.TrimSpace(sp.request.TextBuffer.String())

	// Self-heal Claude/Anthropic-style XML tool calls that some self-hosted
	// models emit in the plain-text stream instead of the native tool_calls
	// delta channel. Left alone, the XML renders as raw chat text and the
	// tool never executes — the conversation stalls with the model
	// "spilling" the call into the chat and going idle (observed: WordPress
	// Hover Image Fix, seq 2967/2975). Parse them into real ToolCalls and
	// strip them from the visible text.
	var xmlCalls []utils.ToolCall
	if text != "" {
		var rest string
		xmlCalls, rest = extractXMLToolCalls(text)
		if len(xmlCalls) > 0 {
			for i := range xmlCalls {
				utils.Log("[StreamProcessor] recovered %d XML-format tool call(s) from text stream (first: %s)", len(xmlCalls), xmlCalls[0].Function.Name)
				xmlCalls[i].ID = sp.mintProviderCallID("xml_" + xmlCalls[i].Function.Name)
				enrichToolCallMetadata(&xmlCalls[i])
			}
			text = strings.TrimSpace(rest)
		}
	}

	if text != "" {
		message.Content = utils.NewTextContent(text)
	}

	if len(sp.request.ToolCallBuffer) > 0 {
		for i := range sp.request.ToolCallBuffer {
			// Namespace the provider id with this conversation's seq so ids
			// are unique across turns (providers reuse call_0) — the persisted
			// assistant message and every result pushed for it must carry the
			// same minted id, otherwise the tool loop cannot match them.
			sp.request.ToolCallBuffer[i].ID = sp.mintProviderCallID(sp.request.ToolCallBuffer[i].ID)
			enrichToolCallMetadata(&sp.request.ToolCallBuffer[i])
		}
		message.ToolCalls = sp.request.ToolCallBuffer
	}

	// Recovered XML calls ride on the same assistant message as any native
	// calls (appended after). The tool loop executes each ToolCall and pushes
	// a result for every one, so the persisted block stays consistent.
	if len(xmlCalls) > 0 {
		message.ToolCalls = append(message.ToolCalls, xmlCalls...)
	}

	return message
}

// finalizeStream stores total tokens on the active request and persists the
// assistant message. Tool calls whose accumulated arguments are not valid JSON
// (typically because max_tokens cut the stream mid-call) get their args
// sanitized to "{}" and an explicit failure tool result pushed, so the
// outbound payload to the LLM never carries a malformed or dangling call.
//
// A completely empty completion (no text, no tool calls) is NOT persisted:
// it carries no information, and letting it accumulate in history makes the
// conversation look halted while the loop quietly retries. RunLLMLoop owns
// the retry/surface decision; all we do here is keep the DB clean so a
// retry has nothing to roll back.
func (sp *StreamProcessor) finalizeStream(ctx context.Context) utils.Message {
	sp.request.TokenUsage = sp.request.PromptTokens + sp.request.CompletionTokens

	message := sp.buildAssistantMessage()

	type failedCall struct{ ID, Name string }
	var failed []failedCall
	for i := range message.ToolCalls {
		tc := &message.ToolCalls[i]
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			tc.Function.Arguments = "{}"
			continue
		}
		if !json.Valid([]byte(args)) {
			preview := args
			if len(preview) > 120 {
				preview = preview[:120]
			}
			utils.Log("[StreamProcessor] tool_call %s (%s) args truncated/invalid; failing call. First 120 chars: %s",
				tc.ID, tc.Function.Name, preview)
			tc.Function.Arguments = "{}"
			failed = append(failed, failedCall{ID: tc.ID, Name: tc.Function.Name})
		}
	}

	// Skip the DB push entirely for a truly empty completion: nothing to
	// persist, and the loop treats "no persisted assistant message" as an
	// incomplete turn it can retry without cleanup.
	if isEmptyCompletion(message) {
		utils.Log("[StreamProcessor] empty completion (no text, no tool calls) for conversation %s — not persisting", sp.request.ConversationID)
		return message
	}

	if updated, _, err := db.PushMessage(ctx, sp.conversation, message); err != nil {
		utils.Error("[StreamProcessor] Error saving assistant message to DB", err)
	} else {
		sp.conversation = updated
	}

	if len(failed) > 0 {
		failBody := "The tool call failed either because the content was not a valid JSON or was too large to fit in a single toolcall. Retry with valid content, or smaller content."
		for _, f := range failed {
			toolMsg := utils.Message{
				Role:       "tool",
				Content:    utils.NewTextContent(failBody),
				ToolCallID: f.ID,
				Name:       f.Name,
				Timestamp:  time.Now().Format(time.RFC3339),
			}
			updatedConv, _, pushErr := db.PushMessage(ctx, sp.conversation, toolMsg)
			if pushErr != nil {
				utils.Error("[StreamProcessor] Error saving failed-call tool result", pushErr)
				continue
			}
			sp.conversation = updatedConv

			sp.request.Broadcast(SSEEvent{
				Type:           "tool_result",
				ToolCallID:     f.ID,
				ToolName:       f.Name,
				ToolResult:     failBody,
				IsServer:       true,
				ConversationID: sp.request.ConversationID,
			})
		}
	}

	return message
}

// ProcessStandardStream reads an OpenAI-compatible SSE stream from the LiteLLM proxy.
// The response is wrapped in an idleTimeoutReader so a provider that accepted the
// request (200 OK) but then stops sending bytes cannot wedge this goroutine
// forever inside bufio.Scanner.Scan (the recurring "conversation ends but the
// workflow never completes" stall, previously stuck 80-200 min per crash dump).
func (sp *StreamProcessor) ProcessStandardStream(ctx context.Context, response io.ReadCloser) (utils.Message, error) {
	defer response.Close()

	scanner := bufio.NewScanner(&idleTimeoutReader{ctx: ctx, r: response, timeout: sp.streamIdleTimeout()})
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return sp.buildAssistantMessage(), ctx.Err()
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		jsonData := strings.TrimPrefix(line, "data: ")

		if jsonData == "[DONE]" {
			return sp.finalizeStream(ctx), nil
		}

		var chunk AIChunk
		if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
			utils.Error("[StreamProcessor] Error parsing chunk", err)
			continue
		}

		// Track actual token usage and cost from LiteLLM
		if chunk.Usage.CompletionTokens > 0 {
			sp.request.CompletionTokens = chunk.Usage.CompletionTokens
		}
		if chunk.Usage.PromptTokens > 0 {
			sp.request.PromptTokens = chunk.Usage.PromptTokens
		}
		if chunk.Usage.ResponseCost > 0 {
			sp.request.ResponseCost = chunk.Usage.ResponseCost
		}

		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			if choice.Delta.Content != "" {
				sp.broadcastText(choice.Delta.Content)
			} else if choice.Text != "" {
				sp.broadcastText(choice.Text)
			} else if len(choice.Delta.ToolCalls) > 0 {
				tc := choice.Delta.ToolCalls[0]
				sp.accumulateToolCall(tc.ID, tc.Function.Name, tc.Function.Arguments)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		// A bounded idle-timeout error (see idleTimeoutReader) must NOT be
		// treated as a clean end-of-stream: the response was incomplete. Return
		// it so RunLLMLoop marks the turn as errored (state idle + error event)
		// instead of finalizing a truncated / empty "done".
		return sp.buildAssistantMessage(), err
	}
	return sp.buildAssistantMessage(), nil
}

// idleTimeoutReader wraps an io.Reader so every Read gives up if no byte arrives
// within timeout. This bounds the time a RunLLMLoop goroutine can block reading
// a stalled SSE stream (see ProcessStandardStream), preventing the multi-hour
// wedges observed in crash dumps (goroutines stuck in Scanner.Scan / chunked
// reader poll wait holding the ActiveRequest and the single SQLite connection).
//
// Unlike the pre-#36 version it also watches the run context: when the request
// is cancelled (user stop / client disconnect) the reader unblocks immediately
// instead of waiting out the full timeout.
type idleTimeoutReader struct {
	ctx     context.Context
	r       io.Reader
	timeout time.Duration
}

func (t *idleTimeoutReader) Read(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := t.r.Read(p)
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(t.timeout):
		err := fmt.Errorf("%w after %v (no bytes received)", ErrStreamIdleTimeout, t.timeout)
		utils.Error("[StreamProcessor] %v", err)
		return 0, err
	case <-t.ctx.Done():
		return 0, t.ctx.Err()
	}
}
