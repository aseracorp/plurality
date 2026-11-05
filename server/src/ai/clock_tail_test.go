package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The current-time tail must never claim a conversational turn. Each test
// asserts the shape property that stops chat templates from reading the clock
// as the model's instruction (see clock_tail.go for the measured rationale).

func tailText(c interface{}) string {
	switch v := c.(type) {
	case string:
		return v
	case []StandardContentReq:
		var b []byte
		for _, p := range v {
			b = append(b, p.Text...)
		}
		return string(b)
	}
	return ""
}

func partCount(c interface{}) int {
	if parts, ok := c.([]StandardContentReq); ok {
		return len(parts)
	}
	return -1
}

// The tail must never add a message: adding one is exactly what claims a role.
func TestAppendCurrentTimeTailKeepsMessageCount(t *testing.T) {
	msgs := []StandardMessageReq{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: []StandardContentReq{{Type: "text", Text: "hello"}}},
	}
	got := appendCurrentTimeTail(msgs)
	if len(got) != len(msgs) {
		t.Fatalf("message count changed %d -> %d: the tail must not add a message", len(msgs), len(got))
	}
}

// Whatever the newest message is, the clock lands on it and nothing else moves.
func TestAppendCurrentTimeTailLandsOnTheNewestMessage(t *testing.T) {
	cases := []StandardMessageReq{
		{Role: "user", Content: []StandardContentReq{{Type: "text", Text: "question"}}},
		{Role: "user", Content: "plain string request"},
		{Role: "tool", Content: "tool output", ToolCallID: "call_1", Name: "sh"},
		{Role: "assistant", Content: "my answer"},
		{Role: "assistant", ToolCalls: []utils.ToolCall{{ID: "call_9", Type: "function"}}},
	}
	for _, last := range cases {
		msgs := []StandardMessageReq{{Role: "system", Content: "SYSTEM"}, last}
		got := appendCurrentTimeTail(msgs)
		if len(got) != 2 {
			t.Fatalf("role %q: message count became %d", last.Role, len(got))
		}
		// The prefix must stay byte-identical — that is what keeps the KV cache
		// warm across turns.
		if got[0].Content != "SYSTEM" {
			t.Fatalf("role %q: prefix was mutated: %v", last.Role, got[0].Content)
		}
		tail := got[1]
		if tail.Role != last.Role {
			t.Fatalf("role %q: tail role became %q", last.Role, tail.Role)
		}
		if tail.ToolCallID != last.ToolCallID {
			t.Fatalf("role %q: tool_call_id changed %q -> %q", last.Role, last.ToolCallID, tail.ToolCallID)
		}
		if text := tailText(tail.Content); !strings.Contains(text, "The current time is ") {
			t.Fatalf("role %q: clock missing from tail: %q", last.Role, text)
		}
	}
}

// The clock rides along with real content: existing text keeps its place.
func TestAppendCurrentTimeTailPreservesExistingContent(t *testing.T) {
	msgs := []StandardMessageReq{
		{Role: "user", Content: []StandardContentReq{{Type: "text", Text: "the real request"}}},
	}
	got := appendCurrentTimeTail(msgs)
	parts, ok := got[0].Content.([]StandardContentReq)
	if !ok || len(parts) != 2 {
		t.Fatalf("expected the clock appended as one extra part, got %+v", got[0].Content)
	}
	if parts[0].Text != "the real request" {
		t.Fatalf("existing content rewritten: %q", parts[0].Text)
	}
	if !strings.Contains(parts[1].Text, "The current time is ") {
		t.Fatalf("clock part missing: %q", parts[1].Text)
	}
}

// A string-content message stays a string (no shape switch, no new part), so
// the wire shape a strict provider validated is never altered.
func TestAppendCurrentTimeTailKeepsStringShape(t *testing.T) {
	msgs := []StandardMessageReq{{Role: "user", Content: "the real request"}}
	got := appendCurrentTimeTail(msgs)
	s, ok := got[0].Content.(string)
	if !ok {
		t.Fatalf("string content must stay a string, got %T", got[0].Content)
	}
	if !strings.HasPrefix(s, "the real request") {
		t.Fatalf("real request must stay at the head: %q", s)
	}
	if !strings.Contains(s, "The current time is ") {
		t.Fatalf("clock missing: %q", s)
	}
	if n := partCount(got[0].Content); n != -1 {
		t.Fatalf("string content must not become parts, got %d", n)
	}
}

// An assistant message carrying only tool_calls has nil content: it must get
// text content, not a part array (arrays are only reliably accepted for user).
func TestAppendCurrentTimeTailHandlesToolCallOnlyAssistant(t *testing.T) {
	msgs := []StandardMessageReq{{Role: "assistant", ToolCalls: []utils.ToolCall{{ID: "c1"}}}}
	got := appendCurrentTimeTail(msgs)
	s, ok := got[0].Content.(string)
	if !ok {
		t.Fatalf("nil content must become a string, got %T", got[0].Content)
	}
	if !strings.Contains(s, "The current time is ") {
		t.Fatalf("clock missing: %q", s)
	}
	if len(got[0].ToolCalls) != 1 {
		t.Fatalf("tool_calls must survive: %+v", got[0].ToolCalls)
	}
}

func TestAppendCurrentTimeTailEmptyHistory(t *testing.T) {
	got := appendCurrentTimeTail(nil)
	if len(got) != 1 || got[0].Role != "system" {
		t.Fatalf("empty history must yield one system row, got %+v", got)
	}
}

// Two adjacent turns each add exactly one part — the tail stays bounded, it
// never nests (content is rebuilt from the persisted slice, not accumulated).
func TestAppendCurrentTimeTailIsBounded(t *testing.T) {
	build := func() []StandardMessageReq {
		return []StandardMessageReq{
			{Role: "user", Content: []StandardContentReq{{Type: "text", Text: "hello"}}},
		}
	}
	for i := 0; i < 3; i++ {
		if n := partCount(appendCurrentTimeTail(build())[0].Content); n != 2 {
			t.Fatalf("turn %d: expected 2 parts, got %d", i, n)
		}
	}
}
