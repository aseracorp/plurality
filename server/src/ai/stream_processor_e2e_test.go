package ai

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// TestProcessStandardStreamRealDeepSeek replays a REAL DeepSeek v4 Flash
// streaming capture (two interleaved tool calls, null-id continuations, usage
// chunk) through ProcessStandardStream — the exact server code path — and
// asserts both tool calls come out with valid JSON arguments and correct ids.
func TestProcessStandardStreamRealDeepSeek(t *testing.T) {
	path := os.Getenv("DEEPSEEK_CAPTURE")
	if path == "" {
		t.Skip("set DEEPSEEK_CAPTURE to a saved stream capture to run")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}

	sp := newTestStreamProcessor()
	reader := io.NopCloser(strings.NewReader(string(raw)))
	msg, err := sp.ProcessStandardStream(context.Background(), reader)
	if err != nil {
		t.Fatalf("ProcessStandardStream error: %v", err)
	}

	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %+v", len(msg.ToolCalls), msg.ToolCalls)
	}
	for i, tc := range msg.ToolCalls {
		if tc.Function.Name != "ls" {
			t.Errorf("call %d name = %q, want ls", i, tc.Function.Name)
		}
		if !json.Valid([]byte(tc.Function.Arguments)) {
			t.Errorf("call %d args invalid JSON: %q", i, tc.Function.Arguments)
		}
		if tc.ID == "" {
			t.Errorf("call %d has empty id", i)
		}
		t.Logf("call %d: id=%s name=%s args=%s", i, tc.ID, tc.Function.Name, tc.Function.Arguments)
	}
}
