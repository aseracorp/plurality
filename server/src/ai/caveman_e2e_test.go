package ai

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// End-to-end proof of the caveman stones, through the REAL path:
//
//	db.PushMessage (write-time compression) -> SQLite -> GetConversationByIdInternal
//	  -> convertMessagesToOpenAI (exact request bytes) -> live provider prompt_tokens
//
// Skipped unless CAVEMAN_E2E=1, because it opens a real DB and (for the live
// case) spends real money on a provider. Run with:
//
//	CAVEMAN_E2E=1 go test -tags fts5 -run E2E ./ai/ -v
//
// The live half additionally needs OPENROUTER_API_KEY.

const e2eUserMarker = "ZEBRA-42-KERNEL-KEEP-VERBATIM"

func e2eLogPayload(repeats int) string {
	var b strings.Builder
	b.WriteString("2026-11-19 12:00:00 [INFO] starting scan of /usr/simplify\n")
	for i := 0; i < repeats; i++ {
		b.WriteString("2026-11-19 12:00:03 [INFO] scanning /usr/simplify/item 100% done\n")
	}
	b.WriteString("2026-11-19 12:05:00 [ERROR] build failed: exit status 2\n")
	return b.String()
}

func e2eSnapshotPayload(rows int) string {
	var b strings.Builder
	b.WriteString("### Page\n- Page URL: https://example.com/catalog\n- Page Title: Catalog\n### Snapshot\n```yaml\n")
	b.WriteString("- generic [active] [ref=e1]:\n")
	for i := 0; i < rows; i++ {
		n := fmt.Sprint(i)
		b.WriteString("  - row [ref=r" + n + "]:\n")
		b.WriteString("    - cell [ref=c" + n + "a]\n")
		b.WriteString("    - cell [ref=c" + n + "b]\n")
		b.WriteString("    - cell \"SKU-" + n + "\" [ref=c" + n + "c]:\n")
		b.WriteString("      - link \"Open product " + n + "\" [ref=l" + n + "] [cursor=pointer]:\n")
		b.WriteString("        - /url: /p/" + n + "\n")
	}
	b.WriteString("```\n")
	return b.String()
}

// e2eSeedDB points the db package at a throwaway directory and returns a ctx
// bound to a test user.
func e2eSeedDB(t *testing.T) context.Context {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USER_DATA_STORAGE", dir)
	db.InitSQLite()
	return context.WithValue(context.Background(), "userID", "e2e-user")
}

// e2eConversation pushes a full turn set (user, tool call, tool result, second
// tool call, second tool result, follow-up user) and returns the conversation.
func e2eConversation(t *testing.T, ctx context.Context) utils.Conversation {
	t.Helper()
	log := e2eLogPayload(600)
	snap := e2eSnapshotPayload(160)

	conv := utils.Conversation{Title: "e2e"}
	push := func(m utils.Message) {
		t.Helper()
		c, _, err := db.PushMessage(ctx, conv, m)
		if err != nil {
			t.Fatalf("PushMessage(%s): %v", m.Role, err)
		}
		conv = c
	}

	push(utils.Message{Role: "user", Content: utils.NewTextContent(
		"Check the build log and the product page for me. Marker: " + e2eUserMarker)})
	push(utils.Message{Role: "assistant", ToolCalls: []utils.ToolCall{{
		ID: "call_log", Type: "function",
		Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: `{"command":"make"}`},
	}}})
	push(utils.Message{Role: "tool", ToolCallID: "call_log", Name: "system_tools__shell_exec",
		Content: utils.NewTextContent(log)})
	push(utils.Message{Role: "assistant", ToolCalls: []utils.ToolCall{{
		ID: "call_snap", Type: "function",
		Function: utils.FunctionCall{Name: "playwright__browser_snapshot", Arguments: `{}`},
	}}})
	push(utils.Message{Role: "tool", ToolCallID: "call_snap", Name: "playwright__browser_snapshot",
		Content: utils.NewTextContent(snap)})
	push(utils.Message{Role: "user", Content: utils.NewTextContent("now summarize the failures in one line")})
	return conv
}

// The write path: projections are stored for tool rows, the raw body survives
// beside them, and NON-tool rows get nothing at all.
func TestE2ECavemanWritePathStoresProjections(t *testing.T) {
	if os.Getenv("CAVEMAN_E2E") != "1" {
		t.Skip("set CAVEMAN_E2E=1 to run")
	}
	ctx := e2eSeedDB(t)
	conv := e2eConversation(t, ctx)

	loaded, err := db.GetConversationByIdInternal(ctx, conv.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Messages) != 6 {
		t.Fatalf("expected 6 messages, got %d", len(loaded.Messages))
	}

	// Raw SQL, straight from the file on disk — not the Go struct.
	path := filepath.Join(os.Getenv("USER_DATA_STORAGE"), "e2e-user", "conversations.db")
	odb, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer odb.Close()
	rows, err := odb.Query(`SELECT role, COALESCE(name, ''), COALESCE(length(content), 0),
		COALESCE(length(compressed_content), -1) FROM messages WHERE conversation_id=? ORDER BY seq`, conv.ID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var seen []string
	for rows.Next() {
		var role, name string
		var clen, complen int
		if err := rows.Scan(&role, &name, &clen, &complen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen = append(seen, fmt.Sprintf("%s/%s raw=%d comp=%d", role, name, clen, complen))
		switch role {
		case "tool":
			if complen < 0 {
				t.Fatalf("tool row %s has NO compressed_content column value", name)
			}
			if complen >= clen {
				t.Fatalf("tool row %s: projection (%d) not smaller than raw (%d)", name, complen, clen)
			}
			if clen < 1000 {
				t.Fatalf("tool row %s: raw body was mutated, len=%d", name, clen)
			}
		default:
			// user / assistant: NEVER compressed. This is the hard rule.
			if complen != -1 && complen != 0 {
				t.Fatalf("%s row was compressed (comp=%d) — user/assistant text must stay verbatim", role, complen)
			}
		}
	}
	t.Logf("db rows:\n  %s", strings.Join(seen, "\n  "))
}

// The request path, reading the REAL stored messages: stones on ships
// projections, stones off ships raw, user text is byte-identical either way.
func TestE2ECavemanRequestPathFromStoredDB(t *testing.T) {
	if os.Getenv("CAVEMAN_E2E") != "1" {
		t.Skip("set CAVEMAN_E2E=1 to run")
	}
	ctx := e2eSeedDB(t)
	conv := e2eConversation(t, ctx)
	loaded, err := db.GetConversationByIdInternal(ctx, conv.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	seedCavemanModel(t, "e2e-on", true, true, true, false)
	seedCavemanModel(t, "e2e-off", true, false, false, false)

	on, _ := convertMessagesToOpenAI(loaded.Messages, utils.Model{Name: "e2e-on"})
	off, _ := convertMessagesToOpenAI(loaded.Messages, utils.Model{Name: "e2e-off"})

	onBytes, offBytes := e2eMarshal(t, on), e2eMarshal(t, off)

	// Projections shipped, raw bodies gone.
	if !strings.Contains(onBytes, "[caveman/read_less:") {
		t.Fatal("read_less projection missing from request")
	}
	if !strings.Contains(onBytes, "[caveman/browse:") {
		t.Fatal("browse projection missing from request")
	}
	// The projection keeps a single exemplar line plus a repeat marker, so the
	// repetition count — not mere presence — is the discriminator.
	if n := strings.Count(onBytes, "100% done"); n > 2 {
		t.Fatalf("raw log repetitions leaked: %d occurrences in compressed request", n)
	}
	if n := strings.Count(offBytes, "100% done"); n < 500 {
		t.Fatalf("raw path should carry every repetition, got %d", n)
	}
	// browse: the ref-only wrapper lines must be gone from the compressed request
	if strings.Contains(onBytes, `- cell [ref=c42a]`) {
		t.Fatal("browse left a ref-only structural line in the request")
	}
	// Raw path is intact and bigger.
	if !strings.Contains(offBytes, "100% done") {
		t.Fatal("raw path lost the log body")
	}
	if strings.Contains(offBytes, "[caveman/") {
		t.Fatal("stones off but projection shipped")
	}
	save := float64(len(offBytes)-len(onBytes)) / float64(len(offBytes))
	if save < 0.30 {
		t.Fatalf("compression saved only %.1f%% of the request (%d -> %d bytes)",
			100*save, len(offBytes), len(onBytes))
	}
	t.Logf("request bytes %d -> %d (%.1f%% saved)", len(offBytes), len(onBytes), 100*save)

	// Every interactable ref survived browse — the agent can still click.
	for _, want := range []string{"Open product 7", "/url: /p/7", "SKU-42", "Page URL: https://example.com/catalog"} {
		if !strings.Contains(onBytes, want) {
			t.Fatalf("browse dropped content the agent needs: %q", want)
		}
	}

	// The hard rule, measured on the rendered request: user text is byte-
	// identical with stones on and off, and never carries a caveman banner.
	allUser := strings.Join(e2eUserBlocks(on), "\x00")
	if !strings.Contains(allUser, e2eUserMarker) {
		t.Fatalf("user marker lost from the request: %q", allUser)
	}
	if strings.Count(allUser, e2eUserMarker) != 1 {
		t.Fatal("user marker duplicated — a user message was rewritten")
	}
	for _, m := range on {
		if m.Role != "user" {
			continue
		}
		if s := stonesText(t, m.Content); strings.Contains(s, "[caveman/") {
			t.Fatal("caveman banner in a USER message")
		}
	}
	onU, offU := strings.Join(e2eUserBlocks(on), "\x00"), strings.Join(e2eUserBlocks(off), "\x00")
	if onU != offU {
		t.Fatal("user messages differ between stones-on and stones-off")
	}
}

func e2eUserBlocks(msgs []StandardMessageReq) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "user" {
			out = append(out, fmt.Sprintf("%v", m.Content))
		}
	}
	return out
}

func e2eMarshal(t *testing.T, msgs []StandardMessageReq) string {
	t.Helper()
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// say_less is a prompt instruction, not a compressor. It appends a constant,
// and leaves user text and tool payloads untouched.
func TestE2ECavemanVoiceIsAppendOnly(t *testing.T) {
	if os.Getenv("CAVEMAN_E2E") != "1" {
		t.Skip("set CAVEMAN_E2E=1 to run")
	}
	seedCavemanModel(t, "e2e-say", true, false, false, true)
	base := "You are Plurality. Be helpful."

	with := applyCavemanVoice(base, utils.Model{Name: "e2e-say"}, false)
	if !strings.HasPrefix(with, base) {
		t.Fatal("voice must APPEND; it changed the existing prompt")
	}
	if !strings.Contains(with, "Caveman mode (say less)") {
		t.Fatal("voice not injected")
	}
	if strings.Contains(with, "rewrite the user's messages") {
		t.Fatal("voice must not instruct on user-message handling — the model cannot rewrite context")
	}
	if without := applyCavemanVoice(base, utils.Model{Name: "e2e-say"}, true); without != base {
		t.Fatal("voice must be skipped on calls")
	}
	// Constant => KV prompt-cache prefix stays byte-stable across turns.
	if applyCavemanVoice(base, utils.Model{Name: "e2e-say"}, false) != with {
		t.Fatal("voice is not constant")
	}
}

// ---- live provider -------------------------------------------------------

// The full loop, measured by the provider itself: identical conversation,
// stones off vs on, real prompt_tokens.
func TestE2ECavemanLiveProviderTokenDrop(t *testing.T) {
	if os.Getenv("CAVEMAN_E2E") != "1" {
		t.Skip("set CAVEMAN_E2E=1 to run")
	}
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	model := os.Getenv("CAVEMAN_E2E_MODEL")
	if model == "" {
		model = "openai/gpt-4o-mini"
	}

	ctx := e2eSeedDB(t)
	conv := e2eConversation(t, ctx)
	loaded, err := db.GetConversationByIdInternal(ctx, conv.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	seedCavemanModel(t, "live-on", true, true, true, false)
	seedCavemanModel(t, "live-off", true, false, false, false)
	// The registry wants a real entry for context-window math; mirror the
	// seeded name onto the provider model id.
	for _, n := range []string{"live-on", "live-off"} {
		e := Models.byName[n]
		e.Name = model
		Models.byName[model+"|"+n] = e
	}

	on, _ := convertMessagesToOpenAI(loaded.Messages, utils.Model{Name: "live-on"})
	off, _ := convertMessagesToOpenAI(loaded.Messages, utils.Model{Name: "live-off"})

	onUsage := e2eLive(t, key, model, on)
	offUsage := e2eLive(t, key, model, off)

	t.Logf("provider usage  raw: prompt=%d completion=%d  |  caveman: prompt=%d completion=%d",
		offUsage.Prompt, offUsage.Completion, onUsage.Prompt, onUsage.Completion)

	if onUsage.Prompt >= offUsage.Prompt {
		t.Fatalf("provider saw no saving: %d (caveman) vs %d (raw) prompt tokens", onUsage.Prompt, offUsage.Prompt)
	}
	saved := float64(offUsage.Prompt-onUsage.Prompt) / float64(offUsage.Prompt)
	if saved < 0.15 {
		t.Fatalf("provider saved only %.1f%% (%d -> %d)", 100*saved, offUsage.Prompt, onUsage.Prompt)
	}
	// The user's words reached the model intact.
	if !strings.Contains(onUsage.Text, e2eUserMarker) && onUsage.Echoes != e2eUserMarker {
		t.Logf("note: model did not echo the marker (answer=%q); marker integrity asserted on the request bytes", onUsage.Text)
	}
	t.Logf("live provider prompt tokens: %d -> %d (%.1f%% saved)", offUsage.Prompt, onUsage.Prompt, 100*saved)
}

type e2eUsage struct {
	Prompt     int    `json:"prompt_tokens"`
	Completion int    `json:"completion_tokens"`
	Text       string `json:"-"`
	Echoes     string `json:"-"`
}

func e2eLive(t *testing.T, key, model string, msgs []StandardMessageReq) e2eUsage {
	t.Helper()
	body := map[string]any{
		"model":       model,
		"messages":    msgs,
		"max_tokens":  48,
		"temperature": 0,
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest("POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("http: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provider HTTP %d: %s", resp.StatusCode, string(raw[:min(len(raw), 400)]))
	}
	var out struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, string(raw[:min(len(raw), 400)]))
	}
	u := e2eUsage{Prompt: out.Usage.PromptTokens, Completion: out.Usage.CompletionTokens}
	if len(out.Choices) > 0 {
		u.Text = out.Choices[0].Message.Content
	}
	if u.Prompt == 0 {
		t.Fatalf("provider reported 0 prompt tokens: %s", string(raw[:min(len(raw), 300)]))
	}
	return u
}

// Routing: a caveman-owned model must NOT hop through the proxy by default.
// Measured on this deployment: the binary's compress mode forwards input
// unchanged (no bound recovery tool) at 10.6x latency, so the hop is pure
// cost. The Go stones (say_less/read_less/browse) need no hop.
func TestE2ECavemanOwnedModelDoesNotHopByDefault(t *testing.T) {
	// These are package-level atomics shared by every test in the package:
	// save and restore, or this test leaks routing state into its siblings.
	prevCaveman, prevHeadroom := cavemanReady.Load(), headroomReady.Load()
	t.Cleanup(func() {
		cavemanReady.Store(prevCaveman)
		headroomReady.Store(prevHeadroom)
	})

	// seedCavemanModel writes whole entries, so headroom is set by mutating a
	// copy and writing it back — map values are not addressable.
	seedCavemanModel(t, "e2e-hop-off", true, true, true, true)
	seedCavemanModel(t, "e2e-hop-headroom", true, false, false, false)
	hr := Models.byName["e2e-hop-headroom"]
	hr.Headroom = HeadroomSettings{Enabled: true}
	Models.byName["e2e-hop-headroom"] = hr

	// read_less owns content in Go, so the request goes straight to LiteLLM.
	cavemanReady.Store(true)
	headroomReady.Store(true)
	if got := ChatCompletionURLForName("e2e-hop-off"); got != LiteLLMBaseURL {
		t.Fatalf("caveman-owned model must talk to LiteLLM directly, got %q", got)
	}

	// Explicit opt-in puts the binary in the path.
	e := Models.byName["e2e-hop-off"]
	e.Caveman.Proxy = true
	Models.byName["e2e-hop-off"] = e
	if got := ChatCompletionURLForName("e2e-hop-off"); !strings.HasPrefix(got, CavemanBaseURL) {
		t.Fatalf("caveman.proxy: true must route through the proxy, got %q", got)
	}

	// An unhealthy proxy degrades to direct — never to a doubled hop.
	cavemanReady.Store(false)
	if got := ChatCompletionURLForName("e2e-hop-off"); got != LiteLLMBaseURL {
		t.Fatalf("unhealthy proxy must fall back to direct LiteLLM, got %q", got)
	}

	// headroom ownership is untouched by the caveman change.
	if got := ChatCompletionURLForName("e2e-hop-headroom"); got != HeadroomBaseURL {
		t.Fatalf("headroom-owned model must keep its hop, got %q", got)
	}
}
