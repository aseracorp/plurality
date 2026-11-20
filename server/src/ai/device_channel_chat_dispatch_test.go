package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// The chat-path gap this fixes (production 2026-11-20, conversation
// f533e88d3dcac84f056cc280): the user chats in the WEB app, which advertises
// no device shell (web has no Process.start), while their Windows app holds a
// live device claim. The loop had a conversation stream attached, so
// clientToolUnreachable said "executable", the device-channel block was gated
// on ar.ClientCount() == 0, and the turn parked on a client that can never run
// the tool — 90s stall, watchdog timeout, identical on every retry. server.log
// shows both shapes of the same conversation: "1 connected clients" + stall at
// 08:31/09:31, and device-channel dispatch answering fine at 08:33 with
// "0 connected clients".

func TestDeviceChannelSteersCall(t *testing.T) {
	cases := []struct {
		name                string
		hasConversationConn bool
		advertisedByStream  bool
		askMode             bool
		want                bool
	}{
		// Background run (no stream): the #196 case, unchanged.
		{"background run steers", false, false, false, true},
		{"background run, ask mode steers (loop synthesizes the ask refusal)", false, false, true, true},
		// Desktop chat where the streaming app IS the executor: byte-identical
		// chat path, including its approval UI.
		{"desktop chat, tool advertised: chat path", true, true, false, false},
		{"desktop chat, ask mode: approval prompt stays on the attached client", true, true, true, false},
		{"attached client, ask mode, tool not advertised: never auto-execute", true, false, true, false},
		// The bug: a stream is attached but that client cannot run the tool.
		{"web tab streaming the chat: steer to the claim", true, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deviceChannelSteersCall(tc.hasConversationConn, tc.advertisedByStream, tc.askMode)
			if got != tc.want {
				t.Fatalf("deviceChannelSteersCall(conv=%v, advertised=%v, ask=%v) = %v, want %v",
					tc.hasConversationConn, tc.advertisedByStream, tc.askMode, got, tc.want)
			}
		})
	}
}

// A live claim rescues an advertised tool even with a stream attached — that
// is what keeps the reattach grace off the chat path and stops the fast-fail.
func TestClientToolUnreachableWithStreamAndClaim(t *testing.T) {
	if clientToolUnreachable(true, true, true, true) {
		t.Fatal("advertised + stream + claim must be executable")
	}
	// The pre-existing chat case (no claim) is unchanged.
	if clientToolUnreachable(true, true, true, false) {
		t.Fatal("advertised + stream must stay executable")
	}
	// An unadvertised name is never rescued by a claim (hallucinated tool).
	if !clientToolUnreachable(false, true, true, true) {
		t.Fatal("an unadvertised name must still fail fast")
	}
}

// sseFrames parses the SSE `data:` lines a writer has received, in order.
func sseFrames(t *testing.T, w *syncWriter) []SSEEvent {
	t.Helper()
	var out []SSEEvent
	for _, line := range strings.Split(w.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev SSEEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("SSE frame is not valid JSON (%v): %s", err, line)
		}
		out = append(out, ev)
	}
	return out
}

func waitForFrames(t *testing.T, w *syncWriter, n int) []SSEEvent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f := sseFrames(t, w); len(f) >= n {
			return f
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d SSE frames (got %d)", n, len(sseFrames(t, w)))
	return nil
}

// The chat case end to end, with no LLM: a stream IS attached (the web tab),
// the claim belongs to a different app, and the call is dispatched over the
// channel. This is the behaviour the gate on ar.ClientCount() == 0 made
// impossible.
func TestDispatchDeviceToolWithConversationStreamAttached(t *testing.T) {
	useRealTempDB(t)
	dev, stat := withCleanRegistries(t)

	// The executing app, on the status stream.
	appW := &syncWriter{}
	app := fakeApp(t, stat, "u1", "nb03", appW)
	defer stat.Remove(app)
	dev.Claim("u1", "nb03", app.ConnID, `C:\Code`, []utils.FunctionToolsRequest{{Name: "shell_client__exec"}}, time.Minute)

	// The viewer: a conversation SSE client (the web tab). It advertises no
	// device shell, which is exactly the web-app shape.
	viewerW := &syncWriter{}
	viewer := NewSSEClient(viewerW, context.Background())
	if viewer == nil {
		t.Fatal("viewer client needs a flusher")
	}

	ar := NewActiveRequest("conv-1", "u1", utils.Model{Name: "m"}, utils.ModelSelected{})
	defer ar.Cancel()
	ar.AddClient(viewer)
	ar.State = utils.StateProcessing
	if ar.ClientCount() == 0 {
		t.Fatal("precondition: a conversation stream must be attached")
	}

	conv := newTestConv("u1", "conv-1", shellCall())
	seedConversation(t, conv)

	ctx := context.WithValue(context.Background(), "userID", "u1")
	tc := shellCall()

	// The web tab echoes the tool_use back as a refusal, the way ChatService
	// does when the call is not in ITS advertised list. It must be refused:
	// accepting it would cancel this loop and strand the device's real answer.
	refusal := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent("Error: tool 'shell_client__exec' is not advertised by this client"),
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := appW.deviceFrameUntil()
		if req == nil || req.DeviceTool == nil {
			t.Errorf("the claiming app never received a device frame: %s", appW.String())
			return
		}
		if req.DeviceTool.Source != "chat" {
			t.Errorf("source = %q, want %q (a chat turn is not a background run)", req.DeviceTool.Source, "chat")
		}
		// While the loop is blocked on the device, the viewer's echo of this
		// call is refused at the API boundary — accepting it would cancel the
		// loop and strand the real answer.
		if !ar.isDeviceDispatched(req.DeviceTool.ToolCallID) {
			t.Errorf("the dispatched call must be marked in flight before the frame is answered")
		}
		if kept := dropInFlightDeviceResults("conv-1", ar, []utils.Message{refusal}); len(kept) != 0 {
			t.Errorf("the viewer's refusal for an in-flight device call must be dropped, kept %d", len(kept))
		}
		if kept := dropInFlightDeviceResults("conv-1", ar, []utils.Message{refusal, {Role: "tool", ToolCallID: "call_9", Content: utils.NewTextContent("other")}}); len(kept) != 1 || kept[0].ToolCallID != "call_9" {
			t.Errorf("only the in-flight call is dropped: %+v", kept)
		}
		if !dev.Resolve(req.RequestID, "u1", req.DeviceTool.ToolCallID, req.DeviceTool.Name, "nb03 output", "") {
			t.Errorf("resolve failed")
		}
	}()

	result, ok := ar.dispatchDeviceTool(ctx, &conv, tc, dispatchPayload("true"), 5*time.Second)
	<-done
	if !ok {
		t.Fatalf("a chat turn with a live claim must dispatch: result=%q", result)
	}
	if result != "nb03 output" {
		t.Fatalf("result = %q, want the device's output", result)
	}

	// The viewer saw the call, flagged as device-dispatched, so the app knows
	// not to queue it as its own pending client tool.
	frames := sseFrames(t, viewerW)
	var toolUse *SSEEvent
	for i := range frames {
		if frames[i].Type == "tool_use" {
			toolUse = &frames[i]
		}
	}
	if toolUse == nil {
		t.Fatal("the attached viewer must receive the tool_use")
	}
	if !toolUse.DeviceDispatched {
		t.Fatal("tool_use broadcast over the device channel must carry device_dispatched")
	}
	if toolUse.IsServer {
		t.Fatal("a device tool is not a server tool")
	}

	// The call has resolved, so the guard is released and a later result for a
	// reused call id ("call_0" is what providers hand out) is accepted again.
	if ar.isDeviceDispatched(tc.ID) {
		t.Fatal("the guard must be released once the loop stops waiting")
	}
	if kept := dropInFlightDeviceResults("conv-1", ar, []utils.Message{refusal}); len(kept) != 1 {
		t.Fatalf("after the call resolves, a result for that id is accepted again: %+v", kept)
	}
	// nil live request: nothing is filtered (no loop to protect).
	if kept := dropInFlightDeviceResults("conv-1", nil, []utils.Message{refusal}); len(kept) != 1 {
		t.Fatalf("no live loop must filter nothing: %+v", kept)
	}
}

// Wire compatibility: device_dispatched is omitempty, so every frame that
// existed before this change serializes byte-identically. The chat client
// parses every event of every conversation; adding a non-omitempty key would
// rewrite the whole stream for no reason.
func TestSSEEventDeviceDispatchedIsOmitted(t *testing.T) {
	cases := []SSEEvent{
		{Type: "text", Content: "hi", ConversationID: "abc"},
		{Type: "tool_use", ToolCall: &utils.ToolCall{ID: "call_0", Function: utils.FunctionCall{Name: "shell_client__exec"}}, IsServer: false, ConversationID: "abc"},
		{Type: "tool_result", ToolCallID: "call_0", ToolName: "fs_read", ToolResult: "x", ConversationID: "abc"},
		{Type: "done", ConversationID: "abc", State: "idle"},
	}
	for _, ev := range cases {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(b), "device_dispatched") {
			t.Fatalf("frame without the flag must not carry it: %s", b)
		}
	}

	b, err := json.Marshal(SSEEvent{Type: "tool_use", ToolCall: &utils.ToolCall{ID: "call_0"}, ConversationID: "abc", DeviceDispatched: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"device_dispatched":true`) {
		t.Fatalf("the flag must serialize when set: %s", b)
	}
}

// The in-flight guard is per-call-id, set before the broadcast so a viewer
// cannot win the race, and cleared on every exit path of the wait.
func TestMarkAndClearDeviceDispatched(t *testing.T) {
	ar := NewActiveRequest("conv-1", "u1", utils.Model{Name: "m"}, utils.ModelSelected{})
	defer ar.Cancel()
	if ar.isDeviceDispatched("call_0") {
		t.Fatal("nothing is in flight on a fresh request")
	}
	ar.markDeviceDispatched("call_0")
	if !ar.isDeviceDispatched("call_0") {
		t.Fatal("the call must be marked in flight")
	}
	ar.markDeviceDispatched("")
	if ar.isDeviceDispatched("") {
		t.Fatal("an empty tool-call id must not be recorded")
	}
	ar.clearDeviceDispatched("call_0")
	if ar.isDeviceDispatched("call_0") {
		t.Fatal("clearing must release the call")
	}
	ar.clearDeviceDispatched("")
}
