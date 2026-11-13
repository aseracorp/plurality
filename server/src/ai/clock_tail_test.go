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

const clockMark = "The current time is "

// firstPartText returns the text of the first content part, "" for a string
// payload (checked via tailText/HasPrefix by the tests that need it).
func firstPartText(c interface{}) string {
	if parts, ok := c.([]StandardContentReq); ok && len(parts) > 0 {
		return parts[0].Text
	}
	return ""
}

func hasClock(c interface{}) bool {
	return strings.Contains(tailText(c), clockMark)
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
		if !hasClock(tail.Content) {
			t.Fatalf("role %q: clock missing from tail: %q", last.Role, tailText(tail.Content))
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
	if !strings.Contains(parts[1].Text, clockMark) {
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
	if !strings.Contains(s, clockMark) {
		t.Fatalf("clock missing: %q", s)
	}
	if n := partCount(got[0].Content); n != -1 {
		t.Fatalf("string content must not become parts, got %d", n)
	}
}

// THE DEFECT THIS PR FIXES. A request ending on an assistant tool_calls row has
// nil content. The old default branch REPLACED that content with the clock, so
// the message the model reads as its own last utterance became
// "The current time is ..." — live reports of replies opening with a bare
// timestamp, and tool loops narrating the clock. The clock must never become a
// message's whole content, and it must never sit in an assistant row.
func TestAppendCurrentTimeTailNeverReplacesContent(t *testing.T) {
	msgs := []StandardMessageReq{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: []StandardContentReq{{Type: "text", Text: "run the tool"}}},
		{Role: "assistant", ToolCalls: []utils.ToolCall{{ID: "c1", Type: "function"}}},
	}
	got := appendCurrentTimeTail(msgs)
	if len(got) != 3 {
		t.Fatalf("message count changed: %d", len(got))
	}
	assistant := got[2]
	if assistant.Role != "assistant" {
		t.Fatalf("assistant row role changed: %q", assistant.Role)
	}
	if assistant.Content != nil {
		t.Fatalf("assistant content was replaced by the clock: %q", tailText(assistant.Content))
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "c1" {
		t.Fatalf("tool_calls must survive: %+v", assistant.ToolCalls)
	}
	if hasClock(assistant.Content) {
		t.Fatalf("clock landed inside the assistant row")
	}
	// The clock has to ride on the newest NON-assistant message with text.
	if !hasClock(got[1].Content) {
		t.Fatalf("clock missing from the newest user row: %q", tailText(got[1].Content))
	}
	if firstPartText(got[1].Content) != "run the tool" {
		t.Fatalf("user content must stay at the head, got %q", tailText(got[1].Content))
	}
	if got[0].Content != "SYSTEM" {
		t.Fatalf("system prompt mutated: %v", got[0].Content)
	}
}

// An assistant row never carries the clock, even when it is the only thing in
// front: the model reads assistant content as speech it already made.
func TestAppendCurrentTimeTailSkipsAssistantRows(t *testing.T) {
	msgs := []StandardMessageReq{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "my answer"},
		{Role: "user", Content: "and then?"},
		{Role: "assistant", Content: "my second answer"},
	}
	got := appendCurrentTimeTail(msgs)
	for i, m := range got {
		if m.Role == "assistant" && hasClock(m.Content) {
			t.Fatalf("message %d: clock landed in an assistant row: %q", i, tailText(m.Content))
		}
	}
	if !hasClock(got[3].Content) {
		t.Fatalf("clock missing from the newest user row: %q", tailText(got[3].Content))
	}
}

// The clock is APPENDED to content, never written over it. An image-only user
// row keeps its image (the row stays a real user turn) and gains one text part.
func TestAppendCurrentTimeTailAppendsToImageOnlyRow(t *testing.T) {
	msgs := []StandardMessageReq{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: "read this screenshot"},
		{Role: "user", Content: []StandardContentReq{
			{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: "data:image/png;base64,AAA"}},
		}},
	}
	got := appendCurrentTimeTail(msgs)
	parts, ok := got[2].Content.([]StandardContentReq)
	if !ok || len(parts) != 2 {
		t.Fatalf("image row content was replaced: %+v", got[2].Content)
	}
	if parts[0].Type != "image_url" || parts[0].ImageURL == nil ||
		parts[0].ImageURL.URL != "data:image/png;base64,AAA" {
		t.Fatalf("image part must survive untouched: %+v", parts[0])
	}
	if !strings.Contains(parts[1].Text, clockMark) {
		t.Fatalf("clock missing from the newest row: %+v", parts[1])
	}
	if hasClock(got[1].Content) {
		t.Fatalf("older message was mutated: %q", tailText(got[1].Content))
	}
}

// A row with nothing to attach to (empty part list) is skipped, walking back to
// an older message, so the clock never becomes a message's whole content.
func TestAppendCurrentTimeTailSkipsEmptyRows(t *testing.T) {
	msgs := []StandardMessageReq{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: "the real question"},
		{Role: "user", Content: []StandardContentReq{}},
	}
	got := appendCurrentTimeTail(msgs)
	if hasClock(got[2].Content) {
		t.Fatalf("empty row became the clock line: %q", tailText(got[2].Content))
	}
	if !hasClock(got[1].Content) {
		t.Fatalf("clock missing from the newest text row: %q", tailText(got[1].Content))
	}
}

// Nothing carries text at all: skip the clock for this turn. Inventing a row is
// one of the known-bad shapes (trailing system/user/tool each break a template)
// and replacing content is the defect above.
func TestAppendCurrentTimeTailSkipsWhenNothingCarriesText(t *testing.T) {
	msgs := []StandardMessageReq{
		{Role: "assistant", ToolCalls: []utils.ToolCall{{ID: "c1", Type: "function"}}},
		{Role: "user", Content: []StandardContentReq{}},
	}
	got := appendCurrentTimeTail(msgs)
	if len(got) != 2 {
		t.Fatalf("message count changed %d -> %d", len(msgs), len(got))
	}
	for i, m := range got {
		if hasClock(m.Content) {
			t.Fatalf("message %d: clock invented where there is no content: %q", i, tailText(m.Content))
		}
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
