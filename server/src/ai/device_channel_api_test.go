package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
	"time"

	"github.com/gorilla/mux"
)

// --- targeted addressing ---

// Device frames are addressed to ONE app. Two windows of the same user must
// not both execute the same shell command, and a legacy client (no client_id)
// must never see a device frame at all — it has no executor and no claim.
func TestBroadcastToClientIsTargeted(t *testing.T) {
	_, stat := withCleanRegistries(t)

	wA, wB, wLegacy := &syncWriter{}, &syncWriter{}, &syncWriter{}
	appA := fakeApp(t, stat, "u1", "app-a", wA)
	appB := fakeApp(t, stat, "u1", "app-b", wB)
	legacy := NewStatusClient(wLegacy, wLegacy, "u1", "", nil)
	stat.Add(legacy)
	defer func() {
		stat.Remove(appA)
		stat.Remove(appB)
		stat.Remove(legacy)
	}()

	frame := StatusEvent{
		ConversationID: "conv-1",
		State:          "processing",
		Type:           DeviceToolRequestType,
		RequestID:      "req-1",
		DeviceTool:     &DeviceToolRequest{ConversationID: "conv-1", ToolCallID: "call_0", Name: "shell_client__exec", Arguments: "{}", TimeoutSeconds: 180, Source: "background"},
	}
	if !stat.BroadcastToClient("u1", "app-a", frame) {
		t.Fatal("the addressed client must accept the frame")
	}

	// Legacy status events still go to everyone.
	stat.BroadcastToUser("u1", StatusEvent{ConversationID: "conv-1", State: "processing", Activity: "typing"})

	fA := wA.framesUntil(2)
	fB := wB.framesUntil(1)
	fL := wLegacy.framesUntil(1)

	if len(fA) != 2 {
		t.Fatalf("addressed app got %d frames, want 2 (device frame + status)", len(fA))
	}
	if fA[0].Type != DeviceToolRequestType || fA[0].RequestID != "req-1" {
		t.Fatalf("app A must receive the device frame: %+v", fA[0])
	}
	if len(fB) != 1 || fB[0].Type != "" {
		t.Fatalf("the other app must get only the status event, got %+v", fB)
	}
	if len(fL) != 1 || fL[0].Type != "" {
		t.Fatalf("a legacy client must never receive a device frame, got %+v", fL)
	}

	// Addressing an unknown client_id sends nothing (the caller fails fast).
	if stat.BroadcastToClient("u1", "app-zzz", frame) {
		t.Fatal("no client matches app-zzz")
	}
	// Empty ids are not addressable.
	if stat.BroadcastToClient("", "app-a", frame) || stat.BroadcastToClient("u1", "", frame) {
		t.Fatal("empty userID/clientID must not address anything")
	}
	// A different user's client_id is not addressable either.
	if stat.BroadcastToClient("u2", "app-a", frame) {
		t.Fatal("must not cross users")
	}
}

// ClientIDForUser is the routing lookup dispatchDeviceTool relies on.
func TestClientIDForUser(t *testing.T) {
	dev, stat := withCleanRegistries(t)
	if got := stat.ClientIDForUser("u1"); got != "" {
		t.Fatalf("no client, no id: got %q", got)
	}
	if stat.ClientIDForUser("") != "" {
		t.Fatal("empty userID must yield nothing")
	}

	w := &syncWriter{}
	app := fakeApp(t, stat, "u1", "app-a", w)
	defer stat.Remove(app)
	if got := stat.ClientIDForUser("u1"); got != "app-a" {
		t.Fatalf("fallback to the connected client's id: got %q", got)
	}

	dev.Claim("u1", "app-b", app.ConnID, "", []utils.FunctionToolsRequest{{Name: "shell_client__exec"}}, time.Minute)
	if got := stat.ClientIDForUser("u1"); got != "app-b" {
		t.Fatalf("the live claim wins over the first connected client: got %q", got)
	}
}

// --- POST /client/claim ---

// claimRequest posts to HandleClientClaim with an authenticated userID.
func claimRequest(t *testing.T, userID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/client/claim", strings.NewReader(string(data)))
	r.Header.Set("Content-Type", "application/json")
	if userID != "" {
		r = r.WithContext(withUserID(r.Context(), userID))
	}
	w := httptest.NewRecorder()
	HandleClientClaim(w, r)
	return w
}

func TestHandleClientClaim(t *testing.T) {
	dev, _ := withCleanRegistries(t)

	w := claimRequest(t, "u1", map[string]interface{}{
		"client_id":          "app-1",
		"tools":              []map[string]string{{"name": "shell_client__exec", "description": "device shell"}},
		"client_folder_path": `C:\Code\Outlook`,
		"ttl_seconds":        120,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %s", w.Body.String())
	}
	if resp["ok"] != true {
		t.Fatalf("ok must be true: %s", w.Body.String())
	}
	expStr, _ := resp["expires_at"].(string)
	if expStr == "" {
		t.Fatalf("expires_at must be present: %s", w.Body.String())
	}
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil {
		t.Fatalf("expires_at must be RFC3339 (%q): %v", expStr, err)
	}
	if d := time.Until(exp); d < 100*time.Second || d > 130*time.Second {
		t.Fatalf("ttl_seconds=120 must yield ~120s of expiry, got %v", d)
	}
	if !dev.ClaimedTool("shell_client__exec", "u1") {
		t.Fatal("the claim must be stored")
	}
	if dev.ClientFolderPathForUser("u1") != `C:\Code\Outlook` {
		t.Fatalf("folder path = %q", dev.ClientFolderPathForUser("u1"))
	}

	// Missing client_id -> 400.
	if w := claimRequest(t, "u1", map[string]interface{}{"tools": []interface{}{}}); w.Code != http.StatusBadRequest {
		t.Fatalf("missing client_id: status %d, want 400", w.Code)
	}
	// Unauthenticated -> 401.
	if w := claimRequest(t, "", map[string]interface{}{"client_id": "x"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status %d, want 401", w.Code)
	}
	// Malformed body -> 400.
	r := httptest.NewRequest(http.MethodPost, "/client/claim", strings.NewReader("{not json"))
	r = r.WithContext(withUserID(r.Context(), "u1"))
	wb := httptest.NewRecorder()
	HandleClientClaim(wb, r)
	if wb.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: status %d, want 400", wb.Code)
	}
	// GET is not the claim endpoint.
	rg := httptest.NewRequest(http.MethodGet, "/client/claim", nil)
	rg = rg.WithContext(withUserID(rg.Context(), "u1"))
	wg := httptest.NewRecorder()
	HandleClientClaim(wg, rg)
	if wg.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must be refused with 405, got %d", wg.Code)
	}

	// ttl_seconds is clamped: a 1s claim cannot be used to starve the user's
	// background runs, and 100000s cannot outlive the app's connection.
	claimRequest(t, "u1", map[string]interface{}{"client_id": "app-1", "ttl_seconds": 1})
	if c := dev.ClaimForUser("u1"); c == nil || time.Until(c.ExpiresAt) < 10*time.Second {
		t.Fatalf("ttl_seconds=1 must clamp to %v", deviceClaimTTLMin)
	}
	claimRequest(t, "u1", map[string]interface{}{"client_id": "app-1", "ttl_seconds": 100000})
	if c := dev.ClaimForUser("u1"); c == nil || time.Until(c.ExpiresAt) > deviceClaimTTLMax+5*time.Second {
		t.Fatalf("ttl_seconds=100000 must clamp to %v", deviceClaimTTLMax)
	}
}

// --- POST /chat/device-tool-result/{request_id} ---

func resultRequest(t *testing.T, userID, requestID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/chat/device-tool-result/"+requestID, strings.NewReader(string(data)))
	r = r.WithContext(withUserID(r.Context(), userID))
	r = mux.SetURLVars(r, map[string]string{"request_id": requestID})
	w := httptest.NewRecorder()
	HandleDeviceToolResult(w, r)
	return w
}

func TestHandleDeviceToolResult(t *testing.T) {
	dev, _ := withCleanRegistries(t)

	results, cleanup := dev.RegisterPending("req-1", "u1", "conv-1", "call_0", "shell_client__exec", time.Minute)
	defer cleanup()

	w := resultRequest(t, "u1", "req-1", map[string]string{
		"tool_call_id": "call_0",
		"name":         "shell_client__exec",
		"result":       "dir output",
		"error":        "",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %s", w.Body.String())
	}
	if resp["ok"] != true {
		t.Fatalf("ok must be true: %s", w.Body.String())
	}
	select {
	case got := <-results:
		if got.Result != "dir output" {
			t.Fatalf("delivered %+v", got)
		}
	default:
		t.Fatal("result must reach the waiting loop")
	}

	// Unknown request -> 404 (the app ignores it).
	if w := resultRequest(t, "u1", "nope", map[string]string{"result": "x"}); w.Code != http.StatusNotFound {
		t.Fatalf("unknown request: status %d, want 404", w.Code)
	}
	// Another user's request -> 404, and the pending entry is untouched.
	_, cleanup2 := dev.RegisterPending("req-2", "u1", "conv-1", "call_1", "shell_client__exec", time.Minute)
	defer cleanup2()
	if w := resultRequest(t, "intruder", "req-2", map[string]string{"result": "x"}); w.Code != http.StatusNotFound {
		t.Fatalf("cross-user result: status %d, want 404", w.Code)
	}
	if dev.PendingCount() != 1 {
		t.Fatalf("a rejected result must not consume the pending request, %d held", dev.PendingCount())
	}
	// Unauthenticated -> 401.
	if w := resultRequest(t, "", "req-2", map[string]string{"result": "x"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status %d, want 401", w.Code)
	}
}

// withUserID mimics auth.AuthMiddleware's context injection.
func withUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, "userID", userID)
}
