package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

// The device channel's dispatch path (docs/device-channel.md): a client tool
// call in a run with NO conversation SSE client is routed to the user's app
// over the status stream, and the loop blocks in place until the app POSTs the
// result. The server never executes the tool — it routes, waits, persists.
//
// These tests drive dispatchDeviceTool directly (no LLM). The fake app is a
// real StatusClient registered on StatusRegistry with a matching client_id,
// writing into a buffered writer, so the frames asserted here are the bytes
// the app actually reads.

// useRealTempDB points db + storage at a throwaway directory so tool results
// persist through the real SQLite path, exactly as in production. Mirrors
// db.InitSQLite()'s USER_DATA_STORAGE contract; restored on exit so other
// tests in the package are unaffected.
func useRealTempDB(t *testing.T) {
	t.Helper()
	old := os.Getenv("USER_DATA_STORAGE")
	dir := t.TempDir()
	os.Setenv("USER_DATA_STORAGE", dir)
	// GetUserDB caches handles per user, so the existing pool must be closed
	// before the new path takes effect — otherwise a test writes into the DB
	// opened by an earlier test and the seeded conversation row collides.
	db.CloseAllUserDBs()
	db.InitSQLite()
	storage.Init()
	t.Cleanup(func() {
		db.CloseAllUserDBs()
		os.Setenv("USER_DATA_STORAGE", old)
		db.InitSQLite()
		storage.Init()
	})
}

// seedConversation creates the conversation row (and its messages) in the
// test user's SQLite DB, so db.PushMessage in the dispatch path hits an
// existing conversation instead of failing the FK constraint. Mirrors what a
// real run has already done: jobs.RunPrompt pushes the prompt before the loop
// starts, so the row always exists by the time a tool result is persisted.
func seedConversation(t *testing.T, conv utils.Conversation) {
	t.Helper()
	userDB, err := db.GetUserDB(conv.UserID)
	if err != nil {
		t.Fatalf("get user db: %v", err)
	}
	if _, err := userDB.Exec(
		`INSERT INTO conversations (id, title, last_message_at, model_selected, state, folder, icon)
		 VALUES (?, ?, ?, ?, ?, '', '')`,
		conv.ID, conv.Title, time.Now().Format(time.RFC3339Nano), "{}", string(conv.State),
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	for i, m := range conv.Messages {
		content, _ := json.Marshal(m.Content)
		var toolCallID, name interface{}
		if m.ToolCallID != "" {
			toolCallID = m.ToolCallID
		}
		if m.Name != "" {
			name = m.Name
		}
		if _, err := userDB.Exec(
			`INSERT INTO messages (conversation_id, seq, role, content, tool_calls, tool_call_id, name, timestamp)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			conv.ID, i, m.Role, string(content), nullToolCalls(m.ToolCalls), toolCallID, name, m.Timestamp,
		); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
}

func nullToolCalls(tcs []utils.ToolCall) interface{} {
	if len(tcs) == 0 {
		return nil
	}
	b, _ := json.Marshal(tcs)
	return string(b)
}

// withCleanRegistries isolates the two process-wide registries a dispatch
// touches (DeviceRegistry, StatusRegistry) and the device-unresponsive memo
// from other tests in the package, restoring them on exit.
func withCleanRegistries(t *testing.T) (*deviceRegistry, *statusRegistry) {
	t.Helper()
	devOld := DeviceRegistry
	statOld := StatusRegistry
	memoOld := clientToolTimeouts
	dev := &deviceRegistry{claims: make(map[string]*deviceClaim), pending: make(map[string]*devicePending)}
	stat := &statusRegistry{clients: make(map[*StatusClient]bool)}
	DeviceRegistry = dev
	StatusRegistry = stat
	clientToolTimeouts = &clientToolTimeoutMemo{seen: make(map[string]time.Time)}
	t.Cleanup(func() {
		DeviceRegistry = devOld
		StatusRegistry = statOld
		clientToolTimeouts = memoOld
	})
	return dev, stat
}

// syncWriter is a mutex-protected ResponseWriter + Flusher: the StatusClient
// writer goroutine writes concurrently with assertions, which -race (and a
// plain data race on bytes.Buffer) would otherwise flag.
type syncWriter struct {
	mu   sync.Mutex
	buf  strings.Builder
	head http.Header
}

func (w *syncWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.head == nil {
		w.head = http.Header{}
	}
	return w.head
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) WriteHeader(int) {}

func (w *syncWriter) Flush() {}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// statusFrames parses the SSE `data:` lines written so far, in order.
func (w *syncWriter) statusFrames(t *testing.T) []StatusEvent {
	var out []StatusEvent
	for _, line := range strings.Split(w.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev StatusEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			if t != nil {
				t.Fatalf("status frame is not valid JSON (%v): %s", err, line)
			}
			continue
		}
		out = append(out, ev)
	}
	return out
}

// framesUntil waits for at least n frames to have been written, returning nil
// on timeout. It returns nil rather than failing: it is called from helper
// goroutines (a simulated device), where t.Fatalf would call runtime.Goexit on
// the test goroutine from the wrong place.
func (w *syncWriter) framesUntil(n int) []StatusEvent {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f := w.statusFrames(nil); len(f) >= n {
			return f
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// fakeApp registers a status-stream client exactly the way
// HandleStatusStream does (constructor + Add, which also registers the
// placeholder claim) and returns it. The claim's tool list is then posted
// through DeviceRegistry.Claim, mirroring POST /client/claim.
func fakeApp(t *testing.T, stat *statusRegistry, userID, clientID string, w *syncWriter) *StatusClient {
	t.Helper()
	c := NewStatusClient(w, w, userID, clientID, context.Background())
	stat.Add(c)
	return c
}

// newTestConv builds the minimal conversation a background run has: a user
// message, an assistant message with the tool call, and no tool result yet.
func newTestConv(userID, convID string, tc utils.ToolCall) utils.Conversation {
	return utils.Conversation{
		ID:     convID,
		UserID: userID,
		Title:  "Cron: demo",
		State:  utils.StateProcessing,
		Messages: []utils.Message{
			{Role: "user", Content: utils.NewTextContent("do the thing"), Timestamp: time.Now().Format(time.RFC3339)},
			{Role: "assistant", ToolCalls: []utils.ToolCall{tc}, Timestamp: time.Now().Format(time.RFC3339)},
		},
	}
}

func shellCall() utils.ToolCall {
	return utils.ToolCall{
		ID:       "call_0",
		Type:     "function",
		Function: utils.FunctionCall{Name: "shell_client__exec", Arguments: `{"command":"dir"}`},
	}
}

// dispatchPayload is what jobs.RunPrompt builds for a background run with a
// live claim: the claimed tool advertised, enabled in the model selection.
func dispatchPayload(mode string) ChatPayload {
	ms := utils.ModelSelected{Text: &utils.Model{Name: "test-model", Tools: map[string]string{"shell_client__exec": mode}}}
	return ChatPayload{
		ConversationID:  "conv-1",
		ModelSelected:   ms,
		ClientSideTools: []utils.FunctionToolsRequest{{Name: "shell_client__exec", Description: "device shell"}},
	}
}

// TestDispatchDeviceToolHappyPath is the feature's core: the frame reaches the
// claiming app, the loop blocks, the app's answer is returned AND persisted as
// a tool row, and tool_result is broadcast for any viewer.
func TestDispatchDeviceToolHappyPath(t *testing.T) {
	useRealTempDB(t)
	dev, stat := withCleanRegistries(t)

	w := &syncWriter{}
	app := fakeApp(t, stat, "u1", "app-1", w)
	defer stat.Remove(app)
	dev.Claim("u1", "app-1", app.ConnID, `C:\Code`, []utils.FunctionToolsRequest{{Name: "shell_client__exec"}}, time.Minute)

	ar := NewActiveRequest("conv-1", "u1", utils.Model{Name: "m"}, utils.ModelSelected{})
	defer ar.Cancel()
	conv := newTestConv("u1", "conv-1", shellCall())
	seedConversation(t, conv)
	ar.State = utils.StateProcessing

	ctx := context.WithValue(context.Background(), "userID", "u1")
	tc := shellCall()

	// Answer the call the moment the frame arrives, like the app does.
	done := make(chan struct{})
	go func() {
		defer close(done)
		frames := w.framesUntil(1)
		if frames == nil {
			t.Errorf("no status frames written")
			return
		}
		var req StatusEvent
		for _, f := range frames {
			if f.Type == DeviceToolRequestType {
				req = f
				break
			}
		}
		if req.DeviceTool == nil {
			t.Errorf("frame has no device_tool payload: %+v", req)
			return
		}
		if req.RequestID == "" {
			t.Errorf("frame has no request_id")
			return
		}
		if !stat.BroadcastToClient("u1", "app-1", StatusEvent{ConversationID: "conv-1", State: "processing", Activity: "typing"}) {
			t.Errorf("secondary frame send failed")
		}
		if !dev.Resolve(req.RequestID, "u1", req.DeviceTool.ToolCallID, req.DeviceTool.Name, "dir output", "") {
			t.Errorf("resolve failed")
		}
	}()

	result, ok := ar.dispatchDeviceTool(ctx, &conv, tc, dispatchPayload("true"), 5*time.Second)
	<-done
	if !ok {
		t.Fatalf("dispatch must succeed when the device answers; result=%q", result)
	}
	if result != "dir output" {
		t.Fatalf("result = %q, want the device's output", result)
	}

	// The frame the app read.
	frames := w.statusFrames(t)
	var req *StatusEvent
	for i := range frames {
		if frames[i].Type == DeviceToolRequestType {
			req = &frames[i]
		}
	}
	if req == nil {
		t.Fatalf("no device_tool_request frame written: %s", w.String())
	}
	if req.ConversationID != "conv-1" {
		t.Fatalf("conversation_id = %q", req.ConversationID)
	}
	dt := req.DeviceTool
	if dt.ConversationID != "conv-1" || dt.ToolCallID != "call_0" || dt.Name != "shell_client__exec" {
		t.Fatalf("device_tool identity wrong: %+v", dt)
	}
	if dt.Arguments != `{"command":"dir"}` {
		t.Fatalf("arguments must be the raw JSON string from the model, got %q", dt.Arguments)
	}
	if dt.TimeoutSeconds != 5 {
		t.Fatalf("timeout_seconds = %d, want 5", dt.TimeoutSeconds)
	}
	if dt.Source != "chat" {
		t.Fatalf("source = %q, want chat (no trigger provenance)", dt.Source)
	}

	// The result is a persisted tool row (the loop continues the LLM turn with
	// it) and a tool_result SSE event on the conversation.
	var toolRows int
	var lastText string
	for _, m := range conv.Messages {
		if m.Role == "tool" {
			toolRows++
			lastText = m.TextContent()
			if m.ToolCallID != "call_0" {
				t.Fatalf("tool row tool_call_id = %q, want call_0", m.ToolCallID)
			}
			if m.Name != "shell_client__exec" {
				t.Fatalf("tool row name = %q", m.Name)
			}
		}
	}
	if toolRows != 1 {
		t.Fatalf("exactly 1 tool row must be persisted, got %d", toolRows)
	}
	if lastText != "dir output" {
		t.Fatalf("persisted tool row = %q", lastText)
	}

	// The pending entry must not outlive the dispatch (deferred cleanup), so a
	// late replayed POST can never answer into a later turn.
	if dev.PendingCount() != 0 {
		t.Fatalf("pending request must be cleaned up, %d held", dev.PendingCount())
	}

}

// An answered call clears the device-unresponsive memo (the device proved it
// answers); a timed-out call arms it, so the loop does not burn the whole
// dispatch budget again on the next device call. Same semantics as the stall
// watchdog on the chat path.
func TestDispatchDeviceToolArmsAndClearsTheMemo(t *testing.T) {
	useRealTempDB(t)
	dev, stat := withCleanRegistries(t)

	w := &syncWriter{}
	app := fakeApp(t, stat, "u1", "app-1", w)
	defer stat.Remove(app)
	dev.Claim("u1", "app-1", app.ConnID, "", []utils.FunctionToolsRequest{{Name: "shell_client__exec"}}, time.Minute)

	ctx := context.WithValue(context.Background(), "userID", "u1")

	// Timeout path: nobody answers.
	ar := NewActiveRequest("conv-memo", "u1", utils.Model{Name: "m"}, utils.ModelSelected{})
	defer ar.Cancel()
	conv := newTestConv("u1", "conv-memo", shellCall())
	seedConversation(t, conv)
	result, ok := ar.dispatchDeviceTool(ctx, &conv, tc0(), dispatchPayload("true"), 120*time.Millisecond)
	if ok {
		t.Fatal("an unanswered call must not report success")
	}
	if result != "" {
		t.Fatalf("a timed-out call returns no result, got %q", result)
	}
	if !clientToolTimeouts.recentlyTimedOut("conv-memo") {
		t.Fatal("a device timeout must arm the memo, exactly like the watchdog")
	}
	if !strings.Contains(lastToolText(conv), "did not answer within") {
		t.Fatalf("timeout result must explain the miss: %q", lastToolText(conv))
	}

	// Answer path clears it. The app answers the NEXT frame whose request is
	// still pending, so the first dispatch's (already-expired) request is not
	// replayed into it.
	clientToolTimeouts.note("conv-memo")
	ar2 := NewActiveRequest("conv-memo", "u1", utils.Model{Name: "m"}, utils.ModelSelected{})
	defer ar2.Cancel()
	// Same conversation row (already seeded above); only the in-memory copy is
	// fresh, mirroring a loop that reloads from the DB each iteration.
	conv2 := newTestConv("u1", "conv-memo", shellCall())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Poll rather than read once. The SSE writer is its own goroutine, so
		// "frames written so far" is not a stable index: the FIRST dispatch's
		// frame can land after this goroutine starts, and resolving that
		// already-expired request returns false — the simulated device would
		// answer nothing, and the second dispatch would burn its full 5s
		// timeout (observed as a flaky ~5.8s failure). Keep scanning until a
		// Resolve lands on a LIVE pending entry: that frame is the one the
		// loop is actually blocked on.
		deadline := time.Now().Add(3 * time.Second)
		seen := 0
		for time.Now().Before(deadline) {
			frames := w.statusFrames(nil)
			for i := seen; i < len(frames); i++ {
				if frames[i].Type != DeviceToolRequestType || frames[i].DeviceTool == nil {
					continue
				}
				if dev.Resolve(frames[i].RequestID, "u1", frames[i].DeviceTool.ToolCallID, frames[i].DeviceTool.Name, "ok", "") {
					return
				}
			}
			seen = len(frames)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if _, ok := ar2.dispatchDeviceTool(ctx, &conv2, tc0(), dispatchPayload("true"), 5*time.Second); !ok {
		t.Fatal("dispatch must succeed when the device answers")
	}
	<-done
	if clientToolTimeouts.recentlyTimedOut("conv-memo") {
		t.Fatal("an answered call must clear the memo — the device is alive again")
	}
}

func tc0() utils.ToolCall { return shellCall() }

func lastToolText(conv utils.Conversation) string {
	for i := len(conv.Messages) - 1; i >= 0; i-- {
		if conv.Messages[i].Role == "tool" {
			return conv.Messages[i].TextContent()
		}
	}
	return ""
}

// No socket behind the claim: the app disconnected (or its status client is
// stalled and was dropped). dispatchDeviceTool must fail fast — a 180s wait on
// a frame nobody can read is pure dead time in a background run.
func TestDispatchDeviceToolFailsFastWhenSocketIsGone(t *testing.T) {
	useRealTempDB(t)
	dev, _ := withCleanRegistries(t)

	// Claim exists, but no status client is registered for it.
	dev.Claim("u1", "app-1", "conn-dead", "", []utils.FunctionToolsRequest{{Name: "shell_client__exec"}}, time.Minute)

	ar := NewActiveRequest("conv-1", "u1", utils.Model{Name: "m"}, utils.ModelSelected{})
	defer ar.Cancel()
	conv := newTestConv("u1", "conv-1", shellCall())
	seedConversation(t, conv)

	ctx := context.WithValue(context.Background(), "userID", "u1")
	start := time.Now()
	result, ok := ar.dispatchDeviceTool(ctx, &conv, shellCall(), dispatchPayload("true"), 30*time.Second)
	if ok {
		t.Fatal("dispatch into an unwritable socket must not report success")
	}
	if result != "" {
		t.Fatalf("expected no result, got %q", result)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("must fail fast, waited %v", d)
	}
	text := lastToolText(conv)
	if !strings.Contains(text, "not writable") {
		t.Fatalf("result must name the real cause: %q", text)
	}
	if dev.PendingCount() != 0 {
		t.Fatalf("pending entry must be cleaned up, %d held", dev.PendingCount())
	}
}

// A claim that lapsed between the loop's check and the dispatch (lazy expiry)
// dispatches nothing and says so.
func TestDispatchDeviceToolWithExpiredClaim(t *testing.T) {
	useRealTempDB(t)
	dev, stat := withCleanRegistries(t)
	_ = stat
	dev.Claim("u1", "app-1", "conn1", "", []utils.FunctionToolsRequest{{Name: "shell_client__exec"}}, time.Minute)
	dev.claims["u1"].ExpiresAt = time.Now().Add(-time.Second)

	ar := NewActiveRequest("conv-1", "u1", utils.Model{Name: "m"}, utils.ModelSelected{})
	defer ar.Cancel()
	conv := newTestConv("u1", "conv-1", shellCall())
	seedConversation(t, conv)

	ctx := context.WithValue(context.Background(), "userID", "u1")
	if _, ok := ar.dispatchDeviceTool(ctx, &conv, shellCall(), dispatchPayload("true"), 5*time.Second); ok {
		t.Fatal("an expired claim must not dispatch")
	}
	if !strings.Contains(lastToolText(conv), "claim expired") {
		t.Fatalf("result must explain the lapsed claim: %q", lastToolText(conv))
	}
}

// Cancellation (user stop / superseded loop) fills the call in as
// "Cancelled by user", mirroring the loop's cancel path, so the history never
// carries a dangling tool call.
func TestDispatchDeviceToolRespectsCancellation(t *testing.T) {
	useRealTempDB(t)
	dev, stat := withCleanRegistries(t)

	w := &syncWriter{}
	app := fakeApp(t, stat, "u1", "app-1", w)
	defer stat.Remove(app)
	dev.Claim("u1", "app-1", app.ConnID, "", []utils.FunctionToolsRequest{{Name: "shell_client__exec"}}, time.Minute)

	ar := NewActiveRequest("conv-1", "u1", utils.Model{Name: "m"}, utils.ModelSelected{})
	conv := newTestConv("u1", "conv-1", shellCall())
	seedConversation(t, conv)

	go func() {
		time.Sleep(120 * time.Millisecond)
		ar.Cancel()
	}()
	ctx := context.WithValue(context.Background(), "userID", "u1")
	start := time.Now()
	if _, ok := ar.dispatchDeviceTool(ctx, &conv, shellCall(), dispatchPayload("true"), 60*time.Second); ok {
		t.Fatal("a cancelled request must not report success")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("cancellation must end the wait promptly, took %v", d)
	}
	if !strings.Contains(lastToolText(conv), "Cancelled by user") {
		t.Fatalf("cancelled call must be filled in: %q", lastToolText(conv))
	}
	ar.Cancel()
}
