package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/textsafe"
	"github.com/azukaar/plurality/src/utils"
)

// legacyModelPrefixes were used by an older Go-side naming scheme; the
// canonical names now live in litellm_config.yaml without any prefix.
var legacyModelPrefixes = []string{"ChatGPT/", "Claude/", "Gemini/"}

// stripLegacyPrefix normalises a model name in place by removing any of the
// legacy provider prefixes carried by older conversations.
func stripLegacyPrefix(m *utils.Model) {
	if m == nil {
		return
	}
	for _, p := range legacyModelPrefixes {
		if strings.HasPrefix(m.Name, p) {
			m.Name = strings.TrimPrefix(m.Name, p)
			return
		}
	}
}

// HandleChat is the unified endpoint for sending messages and tool results.
// It creates an ActiveRequest, launches the LLM loop in a goroutine, and
// connects the HTTP client as an SSE listener.
func HandleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request body to 100MB
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024*1024)

	var payload ChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Compatibility shim: older conversations may carry "ChatGPT/", "Claude/", or
	// "Gemini/" prefixes from when the Go side maintained its own naming scheme.
	// Strip them so they match the bare names that come from the litellm config.
	stripLegacyPrefix(payload.ModelSelected.Text)
	stripLegacyPrefix(payload.ModelSelected.Vision)
	stripLegacyPrefix(payload.ModelSelected.ImageGen)
	stripLegacyPrefix(payload.ModelSelected.ImageEdit)

	if payload.ModelSelected.Text == nil || !Models.IsKnown(payload.ModelSelected.Text.Name) {
		utils.Error("[HandleChat] Unknown text model", nil)
		http.Error(w, "Unknown text model", http.StatusBadRequest)
		return
	}
	if payload.ModelSelected.Vision != nil && !Models.IsKnown(payload.ModelSelected.Vision.Name) {
		utils.Error("[HandleChat] Unknown vision model", nil)
		http.Error(w, "Unknown vision model", http.StatusBadRequest)
		return
	}

	partialConversation := utils.Conversation{
		ID:            payload.ConversationID,
		ModelSelected: payload.ModelSelected,
	}
	if payload.ConversationID == "" {
		partialConversation.Title = "New Chat"
	}
	if payload.MiniApp.ID != "" {
		partialConversation.MiniApp = &payload.MiniApp
	}

	userID, _ := r.Context().Value("userID").(string)

	var conversation utils.Conversation

	if len(payload.ToolResults) > 0 {
		// Client is submitting tool results — push each to DB, then resume LLM
		// loop. An arriving device result is also the proof that the device is
		// answering again, so drop its "unresponsive" memo: without this, a
		// recovered isolate would keep being fast-failed for the rest of the
		// window even though it just answered.
		clientToolTimeouts.clear(payload.ConversationID)

		// Drop results for a call this conversation's live loop dispatched to
		// the device channel and is still waiting on. The tool_use for such a
		// call is broadcast to every viewer, and a viewer that is NOT the
		// executing app (a browser tab, a phone, a second window) treats it as
		// its own work: it validates the call against ITS advertised tools,
		// fails — a web tab has no shell def at all — and POSTs the refusal
		// back. That POST is "tool_results", so the path below cancels the live
		// loop to resume on it, the pending device request is torn down, and
		// the device's real answer lands on a 404: the turn is answered by a
		// refusal nobody typed. The only legitimate answer for an in-flight
		// dispatched call is the device's POST to
		// /chat/device-tool-result/{request_id}.
		if live := RequestRegistry.Get(payload.ConversationID); live != nil {
			payload.ToolResults = dropInFlightDeviceResults(payload.ConversationID, live, payload.ToolResults)
		}

		for i := range payload.ToolResults {
			payload.ToolResults[i].Timestamp = time.Now().Format(time.RFC3339)
			if err := storage.ExtractBlobsFromMessage(userID, &payload.ToolResults[i]); err != nil {
				utils.Error("[HandleChat] Error extracting blobs from tool result", err)
			}
			updatedConversation, _, err := db.PushMessage(r.Context(), partialConversation, payload.ToolResults[i])
			if err != nil {
				utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			conversation = updatedConversation
		}
	} else if len(payload.Messages) > 0 {
		// New user message
		userMessage := payload.Messages[0]
		userMessage.Timestamp = time.Now().Format(time.RFC3339)
		if err := storage.ExtractBlobsFromMessage(userID, &userMessage); err != nil {
			utils.Error("[HandleChat] Error extracting blobs from user message", err)
		}
		updatedConversation, _, err := db.PushMessage(r.Context(), partialConversation, userMessage)
		if err != nil {
			utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		conversation = updatedConversation
	} else {
		utils.SendHTTPError(w, "Either messages or tool_results must be provided", http.StatusBadRequest)
		return
	}

	// Every submitted result belonged to a device-channel call in flight, so
	// nothing was pushed and there is no turn to resume. Answer 200 (accepted,
	// ignored) and keep the live loop's stream attached: it is still waiting
	// for the device, and cancelling/resuming here would be the exact bug the
	// filter above exists to prevent.
	if len(payload.ToolResults) == 0 && len(payload.Messages) == 0 {
		if live := RequestRegistry.Get(payload.ConversationID); live != nil {
			SetSSEHeaders(w)
			sseClient := NewSSEClient(w, r.Context())
			if sseClient == nil {
				w.WriteHeader(http.StatusOK)
				return
			}
			if live.AddClientIfOpen(sseClient) {
				select {
				case <-sseClient.Done:
				case <-r.Context().Done():
					live.RemoveClient(sseClient)
				}
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// Select model based on content (text vs vision)
	model := SelectModel(payload.ModelSelected, conversation)

	persistCtx := CopyUserContext(r)

	// Connect this HTTP client as SSE listener. Built before any registry
	// decision so the adoption path can hand it straight to the live loop.
	SetSSEHeaders(w)
	sseClient := NewSSEClient(w, r.Context())
	if sseClient == nil {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// A conversation can already have a live LLM loop: the user sent a message
	// while the agent was still working (the client keeps its own "one send in
	// flight" guard, but a message typed during a long tool chain is a normal,
	// expected thing to do), or a second tab / a reconnecting device is
	// posting. Blindly registering a second ActiveRequest and launching a
	// second RunLLMLoop is what made the agent answer the same message twice
	// at once: both loops broadcast to every attached client.
	//
	// Two kinds of POST behave differently:
	//   - tool_results / approvals resume a loop that PARKED itself waiting for
	//     them, so its registry entry is already gone. If one is still
	//     registered, it is stale (e.g. a watchdog loop racing us) and must be
	//     cancelled: two loops feeding on the same tool results would emit
	//     duplicated turns.
	//   - a new user message is data the live loop can consume — its next
	//     iteration reloads the conversation from the DB, and the loop adopts a
	//     tail user message before going idle. So we QUEUE on the live loop and
	//     attach to its stream instead of starting a second one.
	if live := RequestRegistry.Get(conversation.ID); live != nil {
		isToolResultPOST := len(payload.ToolResults) > 0
		if isToolResultPOST {
			live.Cancel()
		} else if attachToLiveLoop(live, sseClient, conversation.ID, persistCtx) {
			select {
			case <-sseClient.Done:
			case <-r.Context().Done():
				live.RemoveClient(sseClient)
			}
			return
		}
	}

	// Create the ActiveRequest with a cancelable context that carries the userID
	activeRequest := NewActiveRequest(conversation.ID, conversation.UserID, model, payload.ModelSelected)
	cancelCtx, cancelFunc := context.WithCancel(persistCtx)
	activeRequest.Ctx = cancelCtx
	activeRequest.Cancel = cancelFunc
	previous, replaced := RequestRegistry.Register(conversation.ID, activeRequest)
	if replaced && previous != nil {
		// We own the slot now; silence the loop we displaced so only one
		// stream keeps writing to this conversation.
		previous.Cancel()
	}
	if err := db.UpdateConversationState(persistCtx, conversation.ID, utils.StateProcessing); err != nil {
		utils.Error("[HandleChat] Error setting conversation state", err)
	}

	activeRequest.AddClient(sseClient)

	// Launch the LLM loop in a goroutine that outlives this HTTP connection.
	// The eco-mode rolling-checkpoint summary is spawned at the END of the
	// LLM loop (in tool_loop.go) so it can read the just-recorded assistant
	// message's prompt_tokens.
	go activeRequest.RunLLMLoop(persistCtx, conversation, payload)

	// Block until the client disconnects or the stream ends
	select {
	case <-sseClient.Done:
		// Stream ended normally or client disconnected
	case <-r.Context().Done():
		// HTTP connection closed by client
		activeRequest.RemoveClient(sseClient)
	}
}

// attachToLiveLoop queues a freshly pushed user message onto a loop that is
// already running for the conversation, by attaching this request's SSE client
// to it. The message is already in the DB (HandleChat pushed it before
// calling), so the live loop's next iteration will pick it up; the loop's idle
// path also adopts a tail user message, so it cannot finish and drop it.
//
// Returns false when the live loop lost the registry slot (superseded) or had
// already closed its clients: the caller must then start its own loop. That
// check is what keeps this race-free — AddClientIfOpen refuses a request whose
// clients have been torn down, so we never park a client on a dead stream.
func attachToLiveLoop(live *ActiveRequest, sseClient *SSEClient, conversationID string, persistCtx context.Context) bool {
	if RequestRegistry.Get(conversationID) != live {
		return false
	}
	if !live.AddClientIfOpen(sseClient) {
		return false
	}
	// The queue is now the live loop's responsibility: it owes this client a
	// reply (and the "processing" state the client is showing).
	if err := db.UpdateConversationState(persistCtx, conversationID, utils.StateProcessing); err != nil {
		utils.Error("[HandleChat] Error setting conversation state on adopted queue", err)
	}
	utils.Log("[HandleChat] queued message on live loop for %s (%d client(s) attached)", conversationID, live.ClientCount())
	live.BroadcastStatus("typing", "")
	return true
}

// dropInFlightDeviceResults removes tool results a viewer posted for a call
// this conversation's live loop dispatched to the device channel and is still
// waiting on. See the call site in HandleChat for the full reason: the refusal
// a non-executing viewer posts must not resume (and thereby cancel) the loop
// that is blocked on the device's real answer.
//
// Returns the results to keep, in order. Extracted so the rule is
// unit-testable without an HTTP request, a model registry, or a DB.
func dropInFlightDeviceResults(conversationID string, live *ActiveRequest, results []utils.Message) []utils.Message {
	if live == nil || len(results) == 0 {
		return results
	}
	kept := results[:0]
	for i := range results {
		if live.isDeviceDispatched(results[i].ToolCallID) {
			utils.Log("[HandleChat] dropping tool result for %s: call %s is in flight on the device channel", conversationID, results[i].ToolCallID)
			continue
		}
		kept = append(kept, results[i])
	}
	return kept
}

// HandleStreamReconnect allows a client to reconnect to an active conversation's SSE stream.
func HandleStreamReconnect(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	conversationID := vars["id"]

	activeRequest := RequestRegistry.Get(conversationID)
	if activeRequest == nil {
		// Expected: the client probes this endpoint on every conversation
		// open / switch / app-resume to detect a live stream, so 404 here
		// is normal control flow rather than an error condition.
		http.Error(w, "No active request for this conversation", http.StatusNotFound)
		return
	}

	SetSSEHeaders(w)
	sseClient := NewSSEClient(w, r.Context())
	if sseClient == nil {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	// Attach only while the loop is still live. A loop that has just finished
	// closes its clients in cleanup; registering here would reset that and
	// leave this reconnect probe hanging on a stream that never emits again.
	if !activeRequest.AddClientIfOpen(sseClient) {
		http.Error(w, "No active request for this conversation", http.StatusNotFound)
		return
	}

	// Send current buffer as catch-up
	currentText := activeRequest.TextBuffer.String()
	if currentText != "" {
		sseClient.Send(SSEEvent{
			Type:           "text",
			Content:        currentText,
			ConversationID: conversationID,
		})
	}

	select {
	case <-sseClient.Done:
	case <-r.Context().Done():
		activeRequest.RemoveClient(sseClient)
	}
}

// HandleCancel cancels an active LLM request for a conversation.
func HandleCancel(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	conversationID := vars["id"]

	activeRequest := RequestRegistry.Get(conversationID)
	if activeRequest == nil {
		utils.Error("[HandleCancel] No active request for conversation", nil, conversationID)
		http.Error(w, "No active request for this conversation", http.StatusNotFound)
		return
	}

	activeRequest.Cancel()
	w.WriteHeader(http.StatusOK)
}

// HandleApprove handles user approval/denial of server-side "ask" tools.
// Executes approved tools, pushes rejection for denied ones, then relaunches the LLM loop.
// Client-side "ask" tools are handled by the client separately via HandleChat + ToolResults.
func HandleApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(r)
	conversationID := vars["id"]

	var payload struct {
		Approvals []struct {
			ToolCallID string `json:"tool_call_id"`
			ToolName   string `json:"tool_name"`
			Arguments  string `json:"arguments"`
			Approved   bool   `json:"approved"`
		} `json:"approvals"`
		ModelSelected   utils.ModelSelected          `json:"model_selected"`
		ClientSideTools []utils.FunctionToolsRequest `json:"client_side_tools"`
		AvailableSkills []string                     `json:"available_skills,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Verify user owns this conversation. Use the internal variant so the
	// downstream tool-call processing (and the LLM loop it resumes) sees
	// the raw history including any eco checkpoint pair.
	conversation, err := db.GetConversationByIdInternal(r.Context(), conversationID)
	if err != nil {
		utils.SendHTTPError(w, "Conversation not found", http.StatusNotFound)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	if conversation.UserID != userID {
		utils.SendHTTPError(w, "Unauthorized", http.StatusForbidden)
		return
	}

	// Process each server-side tool approval (MCP checked first, then builtins).
	for _, approval := range payload.Approvals {
		var toolMessage utils.Message
		if approval.Approved {
			args := approval.Arguments
			if args == "" {
				args = "{}"
			}

			if mcp.IsMCPTool(userID, approval.ToolName) {
				resultContent := mcp.CallTool(r.Context(), userID, approval.ToolName, args, conversationID)
				toolMessage = utils.Message{
					Role: "tool", Content: resultContent,
					ToolCallID: approval.ToolCallID, Name: approval.ToolName,
					Timestamp: time.Now().Format(time.RFC3339),
				}
			} else if tool, ok := ai_tools.GetTool(approval.ToolName); ok {
				resultContent := tool.Exec(r.Context(), args, *conversation)
				toolMessage = utils.Message{
					Role: "tool", Content: resultContent,
					ToolCallID: approval.ToolCallID, Name: approval.ToolName,
					Timestamp: time.Now().Format(time.RFC3339),
				}
			} else {
				toolMessage = utils.Message{
					Role: "tool", Content: utils.NewTextContent("Tool not found: " + approval.ToolName),
					ToolCallID: approval.ToolCallID, Name: approval.ToolName,
					Timestamp: time.Now().Format(time.RFC3339),
				}
			}
		} else {
			toolMessage = utils.Message{
				Role: "tool", Content: utils.NewTextContent("Tool call rejected by user."),
				ToolCallID: approval.ToolCallID, Name: approval.ToolName,
				Timestamp: time.Now().Format(time.RFC3339),
			}
		}
		if err := storage.ExtractBlobsFromMessage(userID, &toolMessage); err != nil {
			utils.Error("[HandleApprove] Error extracting blobs", err)
		}
		updatedConv, _, pushErr := db.PushMessage(r.Context(), *conversation, toolMessage)
		if pushErr != nil {
			utils.Error("[HandleApprove] Error saving tool result", pushErr)
		} else {
			*conversation = updatedConv
		}
	}

	// Relaunch LLM loop
	model := SelectModel(payload.ModelSelected, *conversation)
	persistCtx := CopyUserContext(r)
	activeRequest := NewActiveRequest(conversation.ID, conversation.UserID, model, payload.ModelSelected)
	cancelCtx, cancelFunc := context.WithCancel(persistCtx)
	activeRequest.Ctx = cancelCtx
	activeRequest.Cancel = cancelFunc
	previous, replaced := RequestRegistry.Register(conversation.ID, activeRequest)
	if replaced && previous != nil {
		// Approvals resume a loop that parked in waiting_for_approval (which
		// unregisters itself). Anything still registered is a loop we are
		// replacing — cancel it so a single stream answers.
		previous.Cancel()
	}
	if err := db.UpdateConversationState(persistCtx, conversation.ID, utils.StateProcessing); err != nil {
		utils.Error("[HandleApprove] Error setting conversation state", err)
	}

	SetSSEHeaders(w)
	sseClient := NewSSEClient(w, r.Context())
	if sseClient == nil {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	activeRequest.AddClient(sseClient)

	chatPayload := ChatPayload{
		ConversationID:  conversationID,
		ModelSelected:   payload.ModelSelected,
		ClientSideTools: payload.ClientSideTools,
		AvailableSkills: payload.AvailableSkills,
	}
	go activeRequest.RunLLMLoop(persistCtx, *conversation, chatPayload)

	select {
	case <-sseClient.Done:
	case <-r.Context().Done():
		activeRequest.RemoveClient(sseClient)
	}
}

// HandleStatusStream is a long-lived SSE connection that broadcasts compact
// status updates for all of the user's active conversations.
func HandleStatusStream(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value("userID").(string)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}

	SetSSEHeaders(w)
	flusher, ok := w.(http.Flusher)
	if !ok {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// client_id turns this connection into a DEVICE CHANNEL (docs/
	// device-channel.md): the app's ClientIdentity().id, recorded on the
	// StatusClient so device tool frames can be addressed to exactly this app
	// and so the connection owns a claim. Empty means a legacy client: status
	// events only, no device channel, and it never sees a device_tool_request.
	clientID := r.URL.Query().Get("client_id")

	// Build through the constructor: it initializes the heartbeatStop channel
	// (and its sync.Once) that stopWriter() closes. A raw struct literal
	// here leaves heartbeatStop nil, so every disconnect panics with
	// "close of nil channel" — and when that panic fires inside the LLM loop
	// goroutine (via StatusRegistry.BroadcastToUser → stopWriter) it kills
	// the conversation's loop mid-turn, wedging it in "processing" forever.
	client := NewStatusClient(w, flusher, userID, clientID, r.Context())
	StatusRegistry.Add(client)
	defer StatusRegistry.Remove(client)

	// Send initial catch-up: current state of all active conversations
	for _, ar := range RequestRegistry.GetForUser(userID) {
		client.Send(StatusEvent{
			ConversationID: ar.ConversationID,
			State:          string(ar.State),
		})
	}

	// Block until client disconnects
	select {
	case <-client.Done:
	case <-r.Context().Done():
	}
}

// --- Device channel endpoints (docs/device-channel.md) ---

// deviceClaimRequest is the body of POST /client/claim.
type deviceClaimRequest struct {
	ClientID         string                       `json:"client_id"`
	Tools            []utils.FunctionToolsRequest `json:"tools"`
	ClientFolderPath string                       `json:"client_folder_path"`
	TTLSeconds       int                          `json:"ttl_seconds"`
}

// HandleClientClaim stores or refreshes the app's device-tool advertisement for
// the authenticated user.
//
// The claim is what lets a BACKGROUND run (cron, webhook, sub-agent) advertise
// and dispatch device tools at all: before this existed jobs.RunPrompt built a
// payload with no ClientSideTools, so every device call fast-failed with
// "cannot be executed by this client" while the app was running and logged in
// (2026-11-19, the Outlook bridge cron chain — the agent concluded the Windows
// app was offline, which was a server-side routing gap, not an offline app).
//
// The app re-claims every ~30s while its status stream is open. The TTL is the
// safety bound: a device that stops refreshing lapses within one TTL (lazy
// expiry on read), so background runs fail fast on an unreachable device
// instead of dispatching into a dead socket.
func HandleClientClaim(w http.ResponseWriter, r *http.Request) {
	// Plain http.Error (not utils.SendHTTPError) for the client-facing 4xx
	// paths: SendHTTPError rewrites every message into a 500 with a random
	// user code, which would make an app-side bug ("I forgot client_id") look
	// like a server failure and hide the reason from the app's log.
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID, ok := r.Context().Value("userID").(string)
	if !ok || userID == "" {
		http.Error(w, "User not authenticated", http.StatusUnauthorized)
		return
	}

	var req deviceClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.ClientID == "" {
		http.Error(w, "client_id is required", http.StatusBadRequest)
		return
	}

	expires := DeviceRegistry.Claim(userID, req.ClientID, StatusRegistry.ConnIDForClient(userID, req.ClientID), req.ClientFolderPath, req.Tools, clampDeviceClaimTTL(req.TTLSeconds))
	if expires.IsZero() {
		http.Error(w, "Claim rejected", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":         true,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

// deviceToolResultRequest is the body of POST /chat/device-tool-result/{request_id}.
type deviceToolResultRequest struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Result     string `json:"result"`
	Error      string `json:"error"`
}

// HandleDeviceToolResult resolves a pending device tool call dispatched over
// the device channel, unblocking the LLM loop waiting in dispatchDeviceTool.
//
// Answered 404 when the request is unknown, expired, already answered, or
// belongs to a different user — the app ignores the response, and an expired
// request is one the loop already synthesized an error for, so delivering it
// into a turn that moved on would duplicate a tool row.
func HandleDeviceToolResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID, ok := r.Context().Value("userID").(string)
	if !ok || userID == "" {
		http.Error(w, "User not authenticated", http.StatusUnauthorized)
		return
	}
	requestID := mux.Vars(r)["request_id"]
	if requestID == "" {
		http.Error(w, "request_id is required", http.StatusBadRequest)
		return
	}

	var req deviceToolResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if !DeviceRegistry.Resolve(requestID, userID, req.ToolCallID, req.Name, req.Result, req.Error) {
		// Plain 404: this is normal control flow — a result racing the
		// dispatch timeout, a replayed frame, a request the app never owned.
		// The app ignores the response.
		http.Error(w, "No pending device tool request", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// --- Non-Chat Endpoints (kept from original, adapted to new types) ---

func API_ListConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	conversations, err := db.ListConversations(r.Context())
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response, err := json.Marshal(conversations)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if string(response) == "null" {
		response = []byte("[]")
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

func API_HandleConversation(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	switch r.Method {
	case http.MethodDelete:
		if err := db.DeleteConversation(r.Context(), id); err != nil {
			utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		conversation, err := db.GetConversationById(r.Context(), id)
		if err != nil {
			utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(conversation)

	default:
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// fastShortcutModels returns the text and imagegen model names from the "fast"
// shortcut in data/config.json. validateShortcuts guarantees the shortcut and
// both slots always exist.
func fastShortcutModels() (textModel, imageModel string) {
	ms := ShortcutModelSelected("fast")
	return ms.Text.Name, ms.ImageGen.Name
}

// generateTitleAndIcon generates a title and icon for a conversation and persists them.
// Returns the generated title and icon (base64), or an error.
func generateTitleAndIcon(ctx context.Context, conversation utils.Conversation) (string, string, error) {
	// Build a payload from the first 2 messages for title generation
	messages := conversation.Messages
	if len(messages) > 2 {
		messages = messages[:2]
	}

	titlePayload := ""
	for _, message := range messages {
		content := message.TextContent()
		if len(content) > 2000 {
			content = textsafe.HeadBytes(content, 2000)
		}
		titlePayload += message.Role + ": \n"
		titlePayload += content + "\n\n"
	}

	if len(titlePayload) > 500 {
		titlePayload = textsafe.HeadBytes(titlePayload, 500)
	}

	textModel, imageModel := fastShortcutModels()

	title, err := GenerateTitleForMessage(titlePayload, textModel)
	if err != nil {
		return "", "", fmt.Errorf("generating title: %w", err)
	}

	iconPrompt := "Simple illustration, visual, " + title
	iconData, err := GenerateIconForConversation(iconPrompt, imageModel)
	if err != nil {
		return "", "", fmt.Errorf("generating icon: %w", err)
	}

	// Save icon to file storage
	iconURL, err := storage.ExtractIconBlob(conversation.UserID, iconData)
	if err != nil {
		utils.Error("[generateTitleAndIcon] Error saving icon to storage", err)
		iconURL = "" // non-fatal
	}

	if err := db.UpdateConversationMetadata(ctx, conversation.ID, title, iconURL); err != nil {
		return "", "", fmt.Errorf("updating metadata: %w", err)
	}

	return title, iconURL, nil
}

func API_HandleTitleGeneration(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	conversation, err := db.GetConversationById(r.Context(), id)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	title, iconData, err := generateTitleAndIcon(r.Context(), *conversation)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"title": title, "icon": iconData})
}

func GenerateIconForConversation(prompt, model string) (string, error) {
	request := ImageGenerationRequest{
		Model:          model,
		Prompt:         prompt,
		Width:          128,
		Height:         128,
		N:              1,
		ResponseFormat: "b64_json",
	}

	response, err := GenerateImage(request)
	if err != nil {
		return "", err
	}

	var jsonResponse struct {
		Data []struct {
			B64Json string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &jsonResponse); err != nil {
		return "", err
	}

	if len(jsonResponse.Data) > 0 {
		return jsonResponse.Data[0].B64Json, nil
	}

	return "", fmt.Errorf("no image data returned")
}

func API_UpdateConversationFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	var request struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if strings.Contains(request.Folder, "/") || strings.Contains(request.Folder, "\\") {
		utils.SendHTTPError(w, "Invalid folder name", http.StatusBadRequest)
		return
	}

	if err := db.UpdateConversationFolder(r.Context(), id, request.Folder); err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func API_UpdateConversationTitle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	var request struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if strings.Contains(request.Title, "/") || strings.Contains(request.Title, "\\") {
		utils.SendHTTPError(w, "Invalid title", http.StatusBadRequest)
		return
	}

	if err := db.UpdateConversationMetadata(r.Context(), id, request.Title, ""); err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
