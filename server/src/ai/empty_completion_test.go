package ai

import (
	"net/http"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// safeFlush must convert a Flush panic into an error instead of letting it
// crash the SSE writer goroutine. A Flush on a ResponseWriter whose handler
// has already returned (or that was closed underneath the stream) can panic;
// before this fix that panic either escaped the writer goroutine (process
// crash) or — when recovered at the goroutine top — left the client
// registered forever as a zombie stream.
func TestSafeFlushConvertsPanicToError(t *testing.T) {
	panicking := panicFlusher{}
	if err := safeFlush(panicking); err == nil {
		t.Fatal("expected error from panicking Flush, got nil")
	} else if !strings.Contains(err.Error(), "flush panicked") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestSafeFlushNoPanicReturnsNil(t *testing.T) {
	ok := okFlusher{}
	if err := safeFlush(ok); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// isEmptyCompletion must catch the degenerate providers: a successful stream
// ([DONE]) that produced neither text nor tool calls. Everything with actual
// content — real text, whitespace-only text that buildAssistantMessage would
// trim away, or tool calls — is a real completion and not retried.
func TestIsEmptyCompletion(t *testing.T) {
	msg := func(text string, toolCalls int) utils.Message {
		m := utils.Message{}
		if text != "" {
			m.Content = utils.NewTextContent(text)
		}
		if toolCalls > 0 {
			m.ToolCalls = make([]utils.ToolCall, toolCalls)
		}
		return m
	}

	cases := []struct {
		name  string
		msg   utils.Message
		empty bool
	}{
		{"no text no tools", msg("", 0), true},
		// buildAssistantMessage trims whitespace BEFORE isEmptyCompletion is
		// consulted (finalizeStream), so a whitespace-only buffer arrives here
		// as empty. Raw whitespace passed in directly is still content and not
		// treated as empty.
		{"whitespace only built empty", msg("", 0), true},
		{"raw whitespace is content", msg("   \n ", 0), false},
		{"real text", msg("hello", 0), false},
		{"tool call no text", msg("", 1), false},
		{"whitespace plus tool call", msg("\n", 1), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEmptyCompletion(tc.msg); got != tc.empty {
				t.Fatalf("isEmptyCompletion = %v, want %v", got, tc.empty)
			}
		})
	}
}

type panicFlusher struct{}

func (panicFlusher) Flush() { panic("http: connection has been hijacked") }

type okFlusher struct{}

func (okFlusher) Flush() {}

var _ http.Flusher = panicFlusher{}
var _ http.Flusher = okFlusher{}
