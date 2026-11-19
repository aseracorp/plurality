package ai

import (
	"strconv"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/caveman"
	"github.com/azukaar/plurality/src/utils"
)

// Caveman must NEVER compress user (or system) messages. This is not a
// stylistic preference: caveman's own research (Adobe CAVEWOMAN) found that
// rewriting the user's prompt makes answers LONGER and WORSE, and the docs
// state it twice — "Your prompts stay yours. Never rewritten. Research says
// that backfires". Compression also breaks tool_call/tool_result pairing and
// confuses the model about what it was actually asked.
//
// These tests lock the boundary from the request side. The storage side is
// locked by db.TestInsertMessageDoesNotCompressNonToolRoles.

// bigCompressibleUserText is the kind of blob a user pastes: large, repetitive,
// and exactly what a naive "compress everything" implementation would eat.
func stonesBigBlob(kind string) string {
	if kind == "json" {
		var b strings.Builder
		b.WriteString("{\n  \"rows\": [\n")
		for i := 0; i < 120; i++ {
			b.WriteString("    {\"id\": 1, \"name\": \"same\", \"status\": \"ok\", \"note\": \"identical row repeated\"}")
			if i < 119 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString("  ]\n}")
		return b.String()
	}
	// log-shaped: the shape read_less targets hardest
	return strings.Repeat("2026-11-19 12:00:03 [INFO] scanning /usr/simplify/item 100% done\n", 200)
}

// Stones ON, everything on: user + system messages must pass through
// byte-identical while the tool result is projected.
func TestCavemanNeverCompressesUserOrSystemMessages(t *testing.T) {
	seedCavemanModel(t, "cav-all", true, true, true, true)

	userText := stonesBigBlob("json")
	systemText := "You are Plurality. Use Markdown. Be concise."
	toolText := stonesBigBlob("log")
	toolProj := caveman.ToolProjection(toolText)
	if toolProj == "" {
		t.Fatal("fixture tool result should be compressible")
	}
	// Production stores the banner above the projection (db.insertMessage),
	// so the fixture must carry the same shape.
	stored := caveman.BannerFor("system_tools__shell_exec", len(toolText), len(toolProj), "call_1") + "\n" + toolProj

	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent(systemText)},
		{Role: "user", Content: utils.NewTextContent(userText)},
		{Role: "assistant", ToolCalls: []utils.ToolCall{{ID: "call_1", Type: "function", Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_1", Name: "system_tools__shell_exec", Content: utils.NewTextContent(toolText), CompressedContent: stored},
		{Role: "user", Content: utils.NewTextContent("and now summarize it")},
	}

	out, _ := convertMessagesToOpenAI(msgs, utils.Model{Name: "cav-all"})

	// User messages: byte-identical, uncompressed, banner-free.
	var userCount int
	for _, m := range out {
		if m.Role == "user" {
			s := stonesText(t, m.Content)
			userCount++
			if strings.Contains(s, "caveman") {
				t.Fatal("caveman banner leaked into a USER message")
			}
		}
		if m.Role == "system" {
			s := stonesText(t, m.Content)
			if s != systemText {
				t.Fatal("system prompt was modified by caveman")
			}
			if strings.Contains(s, "caveman") {
				t.Fatal("caveman banner leaked into the SYSTEM message")
			}
		}
	}
	if userCount != 2 {
		t.Fatalf("expected 2 user messages, got %d", userCount)
	}
	if s := stonesContentString(t, out, "user"); !strings.Contains(s, "\"rows\"") || strings.Count(s, "identical row repeated") != 120 {
		t.Fatal("user stonesPayload was compressed/lossy")
	}

	// Tool result: projected.
	tool := findToolMessage(t, out)
	if !strings.Contains(tool, "[caveman/read_less:") {
		t.Fatal("tool result was not projected despite read_less on")
	}
}

// say_less injects an instruction; it must never rewrite user text, and it is
// a constant so the prompt-cache prefix cannot churn.
func TestSayLessInjectsVoiceAndNeverTouchesUserText(t *testing.T) {
	seedCavemanModel(t, "cav-say", true, false, false, true)
	seedCavemanModel(t, "cav-nosay", true, false, false, false)

	voice := caveman.Voice()
	if !strings.Contains(voice, "say less") {
		t.Fatal("voice text missing")
	}
	// The voice must itself forbid touching user prompts.
	if !strings.Contains(voice, "NEVER rewrite the user's messages") {
		t.Fatal("voice must explicitly protect user messages")
	}
	for _, neg := range []string{"not", "never", "only"} {
		if !strings.Contains(voice, neg) {
			t.Fatalf("voice dropped negation %q — the known terse-output failure mode", neg)
		}
	}
	// Constant across calls: KV prefix stability.
	if caveman.Voice() != voice {
		t.Fatal("voice is not a constant — would churn the prompt cache")
	}
}

// Stone matrix: browse owns web payloads, read_less owns the rest, and each can
// be disabled independently.
func TestStoneMatrixRoutesByToolName(t *testing.T) {
	seedCavemanModel(t, "cav-browse-only", true, false, true, false)
	seedCavemanModel(t, "cav-readless-only", true, true, false, false)
	seedCavemanModel(t, "cav-none", true, false, false, false)

	snap := stonesSnapshotPayload()
	log := stonesLogPayload()

	cases := []struct {
		model     string
		snapShown string // "projected" | "raw"
		logShown  string
	}{
		{"cav-browse-only", "projected", "raw"},
		{"cav-readless-only", "raw", "projected"},
		{"cav-none", "raw", "raw"},
	}
	for _, tc := range cases {
		msgs := []utils.Message{
			{Role: "user", Content: utils.NewTextContent("check")},
			{Role: "assistant", ToolCalls: []utils.ToolCall{{ID: "c1", Type: "function", Function: utils.FunctionCall{Name: snap.name, Arguments: "{}"}}}},
			{Role: "tool", ToolCallID: "c1", Name: snap.name, Content: utils.NewTextContent(snap.body), CompressedContent: snap.proj},
			{Role: "assistant", ToolCalls: []utils.ToolCall{{ID: "c2", Type: "function", Function: utils.FunctionCall{Name: log.name, Arguments: "{}"}}}},
			{Role: "tool", ToolCallID: "c2", Name: log.name, Content: utils.NewTextContent(log.body), CompressedContent: log.proj},
		}
		out, _ := convertMessagesToOpenAI(msgs, utils.Model{Name: tc.model})
		tools := stonesToolMessages(t, out)
		if len(tools) != 2 {
			t.Fatalf("%s: expected 2 tool msgs, got %d", tc.model, len(tools))
		}
		stonesCheckShown(t, tc.model, "snapshot", tools[0], snap, tc.snapShown)
		stonesCheckShown(t, tc.model, "log", tools[1], log, tc.logShown)
	}
}

type stonesPayload struct {
	name string
	body string
	proj string
	// rawOnly is a token present in the raw body and guaranteed absent from
	// the projection, so "raw shipped" can be asserted positively.
	rawOnly string
	banner  string
}

// stonesLargeSnapshotText builds a Playwright-shaped accessibility snapshot big
// enough to clear the floor: a page header plus a table of repeated cells.
func stonesLargeSnapshotText() string {
	var b strings.Builder
	b.WriteString("### Page\n- Page URL: https://example.com/catalog\n- Page Title: Catalog\n### Snapshot\n```yaml\n")
	b.WriteString("- generic [active] [ref=e1]:\n")
	for i := 0; i < 150; i++ {
		b.WriteString("  - row [ref=e")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("]:\n")
		b.WriteString("    - cell [ref=e")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("]\n")
		b.WriteString("    - cell \"SKU-")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(" [ref=e9]\":\n")
		b.WriteString("      - link \"Product page\":\n        - /url: /p/")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n")
	}
	b.WriteString("```\n")
	return b.String()
}

func stonesSnapshotPayload() stonesPayload {
	body := stonesLargeSnapshotText()
	proj := caveman.BrowseProjection(body)
	if proj == "" {
		panic("snapshot fixture not compressible")
	}
	return stonesPayload{
		name:    "playwright__browser_snapshot",
		body:    body,
		proj:    caveman.BannerFor("playwright__browser_snapshot", len(body), len(proj), "c1") + "\n" + proj,
		rawOnly: "- cell [ref=e42]",
		banner:  "[caveman/browse:",
	}
}

func stonesLogPayload() stonesPayload {
	body := strings.Repeat("2026-11-19 12:00:03 [INFO] scanning /usr/simplify/item 100% done\n", 200)
	proj := caveman.ToolProjection(body)
	if proj == "" {
		panic("log fixture not compressible")
	}
	return stonesPayload{
		name:    "system_tools__shell_exec",
		body:    body,
		proj:    caveman.BannerFor("system_tools__shell_exec", len(body), len(proj), "c2") + "\n" + proj,
		rawOnly: "100% done\n2026-11-19",
		banner:  "[caveman/read_less:",
	}
}

func stonesCheckShown(t *testing.T, model, what string, got string, p stonesPayload, want string) {
	t.Helper()
	switch want {
	case "projected":
		if !strings.Contains(got, p.banner) {
			t.Fatalf("%s/%s: expected %s banner, projection not shipped", model, what, p.banner)
		}
		if strings.Contains(got, p.rawOnly) {
			t.Fatalf("%s/%s: raw body leaked alongside projection", model, what)
		}
	case "raw":
		if strings.Contains(got, p.banner) {
			t.Fatalf("%s/%s: stone is off but projection was shipped", model, what)
		}
		if !strings.Contains(got, p.rawOnly) {
			t.Fatalf("%s/%s: raw body missing — projection applied with the stone off", model, what)
		}
	}
}

func stonesContentString(t *testing.T, msgs []StandardMessageReq, role string) string {
	t.Helper()
	for _, m := range msgs {
		if m.Role == role {
			return stonesText(t, m.Content)
		}
	}
	t.Fatalf("no %s message", role)
	return ""
}

func stonesToolMessages(t *testing.T, msgs []StandardMessageReq) []string {
	t.Helper()
	var out []string
	for _, m := range msgs {
		if m.Role == "tool" {
			out = append(out, stonesText(t, m.Content))
		}
	}
	return out
}

// stonesText renders request content whether it shipped as a plain string or
// as content parts (StandardMessageReq.Content is interface{}).
func stonesText(t *testing.T, c any) string {
	t.Helper()
	switch v := c.(type) {
	case string:
		return v
	case []StandardContentReq:
		var b strings.Builder
		for _, p := range v {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	t.Fatalf("unexpected content type %T", c)
	return ""
}
