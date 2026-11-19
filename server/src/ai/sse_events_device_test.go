package ai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// Byte-compatibility contract for the global status stream. StatusEvent gained
// the device-channel fields (type / request_id / device_tool), and every one of
// them is omitempty, so a legacy frame must serialize EXACTLY as it did before
// the feature. The sidebar parses a frame for every conversation of every user
// on every state change; adding "type": "" would rewrite every frame on the
// wire for no reason and breaks apps that key off the presence of the key.
//
// This is an exact-JSON assertion on purpose: a round-trip / field-by-field
// comparison would not catch a new non-omitempty field.
func TestLegacyStatusEventJSONIsUnchanged(t *testing.T) {
	cases := []struct {
		name string
		ev   StatusEvent
		want string
	}{
		{
			// What RunLLMLoop's BroadcastStatus("typing", "") emits (minus the
			// model_selected snapshot, which is a pointer and omitted when nil).
			name: "typing",
			ev:   StatusEvent{ConversationID: "abc", State: "processing", Activity: "typing"},
			want: `{"conversation_id":"abc","state":"processing","activity":"typing"}`,
		},
		{
			name: "tool_use",
			ev:   StatusEvent{ConversationID: "abc", State: "processing", Activity: "tool_use", ToolName: "shell_client__exec"},
			want: `{"conversation_id":"abc","state":"processing","activity":"tool_use","tool_name":"shell_client__exec"}`,
		},
		{
			name: "idle with title and icon",
			ev:   StatusEvent{ConversationID: "abc", State: "idle", Title: "T", Icon: "base64"},
			want: `{"conversation_id":"abc","state":"idle","title":"T","icon":"base64"}`,
		},
		{
			name: "bare state (sidebar catch-up)",
			ev:   StatusEvent{ConversationID: "abc", State: "processing"},
			want: `{"conversation_id":"abc","state":"processing"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("legacy status frame changed\n got: %s\nwant: %s", got, tc.want)
			}
			for _, banned := range []string{`"type"`, `"request_id"`, `"device_tool"`} {
				if strings.Contains(string(got), banned) {
					t.Fatalf("legacy frame must not carry the device field %s: %s", banned, got)
				}
			}
		})
	}
}

// The device frame's exact shape — the contract the Flutter app parses.
// Pinned verbatim so a field rename on either side fails here first.
func TestDeviceToolRequestFrameJSONShape(t *testing.T) {
	ev := StatusEvent{
		ConversationID: "conv-1",
		State:          "processing",
		Type:           DeviceToolRequestType,
		RequestID:      "req_abc123",
		DeviceTool: &DeviceToolRequest{
			ConversationID: "conv-1",
			ToolCallID:     "call_0",
			Name:           "shell_client__exec",
			Arguments:      `{"command":"dir"}`,
			TimeoutSeconds: 180,
			Source:         "background",
		},
	}
	got, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"conversation_id":"conv-1","state":"processing","type":"device_tool_request","request_id":"req_abc123","device_tool":{"conversation_id":"conv-1","tool_call_id":"call_0","name":"shell_client__exec","arguments":"{\"command\":\"dir\"}","timeout_seconds":180,"source":"background"}}`
	if string(got) != want {
		t.Fatalf("device frame shape changed\n got: %s\nwant: %s", got, want)
	}

	// Round-trip: the app's decoder reads every field it needs.
	var back StatusEvent
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Type != DeviceToolRequestType || back.RequestID != "req_abc123" {
		t.Fatalf("frame identity lost: %+v", back)
	}
	dt := back.DeviceTool
	if dt == nil || dt.ConversationID != "conv-1" || dt.ToolCallID != "call_0" ||
		dt.Name != "shell_client__exec" || dt.Arguments != `{"command":"dir"}` ||
		dt.TimeoutSeconds != 180 || dt.Source != "background" {
		t.Fatalf("device_tool payload lost: %+v", dt)
	}
}

// "ask" needs an interactive human. A background run has nobody to approve the
// call, so the loop synthesizes an explanatory result rather than executing
// (auto-approving would defeat the user's consent setting).
func TestClientToolModeIsAsk(t *testing.T) {
	withMode := func(mode string) ChatPayload {
		return ChatPayload{ModelSelected: utils.ModelSelected{Text: &utils.Model{
			Name:  "m",
			Tools: map[string]string{"shell_client__exec": mode},
		}}}
	}
	cases := []struct {
		mode string
		want bool
	}{
		{"ask", true},
		{"true", false},
		{"on", false},
		{"off", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := clientToolModeIsAsk(withMode(tc.mode), "shell_client__exec"); got != tc.want {
			t.Fatalf("clientToolModeIsAsk(mode=%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
	// No model selection at all: not ask (nothing can be gated on approval).
	if clientToolModeIsAsk(ChatPayload{}, "shell_client__exec") {
		t.Fatal("an empty payload must not be treated as ask mode")
	}
	// A tool absent from the map is not ask.
	if clientToolModeIsAsk(withMode("true"), "filesystem_client__fs_read") {
		t.Fatal("an unlisted tool must not be treated as ask mode")
	}

	msg := deviceToolAskModeError("shell_client__exec")
	for _, want := range []string{"shell_client__exec", "ask", "approval", "system_tools__shell_exec"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("ask-mode result must explain the cause and the remedy (%q): %s", want, msg)
		}
	}
}

// deviceToolSource labels a dispatch so the app can show "a background run
// used your device". Provenance comes from the stored TriggerType, so it
// survives a watchdog resume (which loses the payload it was launched with).
func TestDeviceToolSource(t *testing.T) {
	cases := []struct {
		trigger string
		want    string
	}{
		{"", "chat"},
		{"cron", "background"},
		{"webhook", "background"},
		{"sub_agent", "background"},
		{"conversation", "background"},
		{"something_else", "chat"},
	}
	for _, tc := range cases {
		conv := &utils.Conversation{TriggerType: tc.trigger}
		if got := deviceToolSource(conv); got != tc.want {
			t.Fatalf("deviceToolSource(trigger=%q) = %q, want %q", tc.trigger, got, tc.want)
		}
	}
	if deviceToolSource(nil) != "chat" {
		t.Fatal("nil conversation must be labelled chat, never crash")
	}
}
