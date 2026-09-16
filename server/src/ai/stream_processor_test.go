package ai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// newTestStreamProcessor builds a StreamProcessor bound to a bare ActiveRequest
// with no SSE clients — enough to exercise accumulateToolCall and
// buildAssistantMessage without any network or DB.
func newTestStreamProcessor() *StreamProcessor {
	ar := &ActiveRequest{
		ConversationID: "test-conv",
		UserID:         "test-user",
		Ctx:            context.Background(),
		Model:          utils.Model{Name: "deepseek-v4-flash-0731"},
	}
	return &StreamProcessor{
		request:      ar,
		model:        utils.Model{Name: "deepseek-v4-flash-0731"},
		conversation: utils.Conversation{},
	}
}

// feedChunk simulates one SSE "data: {...}" payload processed by the
// ProcessStandardStream loop's tool-call handling.
func (sp *StreamProcessor) feedChunk(t *testing.T, raw string) {
	t.Helper()
	var chunk AIChunk
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("bad test chunk: %v", err)
	}
	if len(chunk.Choices) == 0 {
		return
	}
	choice := chunk.Choices[0]
	if len(choice.Delta.ToolCalls) > 0 {
		for _, tc := range choice.Delta.ToolCalls {
			sp.accumulateToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
	}
}

// TestAccumulateToolCallInterleaved replays the exact DeepSeek v4 Flash
// streaming pattern that previously corrupted tool call arguments: two tool
// calls whose deltas are interleaved, with continuation deltas carrying
// id=null / name=null.
func TestAccumulateToolCallInterleaved(t *testing.T) {
	sp := newTestStreamProcessor()

	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"ls","arguments":""}}]}}]}`)
	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"{\"path\": \""}}]}}]}`)
	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_B","type":"function","function":{"name":"ls","arguments":""}}]}}]}`)
	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"/tmp\"}"}}]}}]}`)
	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"{\"path\": \""}}]}}]}`)
	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"/var/log\"}"}}]}}]}`)

	buf := sp.request.ToolCallBuffer

	// Older logic produced 6 garbage entries. New logic must give exactly 2.
	if len(buf) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %+v", len(buf), buf)
	}

	for i, tc := range buf {
		if tc.Function.Name != "ls" {
			t.Errorf("call %d: expected name 'ls', got %q", i, tc.Function.Name)
		}
		if !json.Valid([]byte(tc.Function.Arguments)) {
			t.Errorf("call %d: arguments are invalid JSON: %q", i, tc.Function.Arguments)
		}
	}

	if buf[0].Function.Arguments != `{"path": "/tmp"}` {
		t.Errorf("call 0 args = %q, want {\"path\": \"/tmp\"}", buf[0].Function.Arguments)
	}
	if buf[1].Function.Arguments != `{"path": "/var/log"}` {
		t.Errorf("call 1 args = %q, want {\"path\": \"/var/log\"}", buf[1].Function.Arguments)
	}
	if buf[0].ID != "call_A" || buf[1].ID != "call_B" {
		t.Errorf("wrong ids: %q %q", buf[0].ID, buf[1].ID)
	}
}

// TestAccumulateToolCallGapFill ensures a first chunk with a non-zero index
// (e.g. index 2 with no 0/1 present yet) doesn't panic or corrupt slots.
func TestAccumulateToolCallGapFill(t *testing.T) {
	sp := newTestStreamProcessor()
	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":2,"id":"call_C","type":"function","function":{"name":"get","arguments":""}}]}}]}`)
	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":2,"id":null,"type":"function","function":{"name":null,"arguments":"{\"url\": \"x\"}"}}]}}]}`)

	buf := sp.request.ToolCallBuffer
	if len(buf) != 3 {
		t.Fatalf("expected gap-filled len 3, got %d", len(buf))
	}
	if buf[2].Function.Name != "get" || buf[2].Function.Arguments != `{"url": "x"}` {
		t.Errorf("slot 2 wrong: %+v", buf[2])
	}
}

// TestAccumulateToolCallSingle covers the plain single-tool-call case.
func TestAccumulateToolCallSingle(t *testing.T) {
	sp := newTestStreamProcessor()
	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_D","type":"function","function":{"name":"search","arguments":""}}]}}]}`)
	sp.feedChunk(t, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"{\"q\": \"hello\"}"}}]}}]}`)

	buf := sp.request.ToolCallBuffer
	if len(buf) != 1 {
		t.Fatalf("expected 1 call, got %d", len(buf))
	}
	if buf[0].Function.Arguments != `{"q": "hello"}` {
		t.Errorf("args wrong: %q", buf[0].Function.Arguments)
	}
}
