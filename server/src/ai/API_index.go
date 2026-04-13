package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// HandleChat is the unified endpoint for sending messages and tool results.
// It creates an ActiveRequest, launches the LLM loop in a goroutine, and
// connects the HTTP client as an SSE listener.
func HandleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload ChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate plan and model access
	planName, err := db.GetPlanName(r.Context())
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !CheckModel(payload.ModelSelected.Text.Name, planName) || !CheckModel(payload.ModelSelected.Vision.Name, planName) {
		http.Error(w, "Invalid model for your plan", http.StatusBadRequest)
		return
	}

	partialConversation := utils.Conversation{
		ID:            payload.ConversationID,
		Title:         "New Chat",
		ModelSelected: payload.ModelSelected,
	}
	if payload.MiniApp.ID != primitive.NilObjectID {
		partialConversation.MiniApp = &payload.MiniApp
	}

	var conversation utils.Conversation

	if len(payload.ToolResults) > 0 {
		// Client is submitting tool results — push each to DB, then resume LLM loop
		for _, toolResult := range payload.ToolResults {
			toolResult.Timestamp = time.Now().Format(time.RFC3339)
			updatedConversation, _, err := db.PushMessage(r.Context(), partialConversation, toolResult)
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

	// Select model based on content (text vs vision)
	model := SelectModel(payload.ModelSelected, conversation.Messages)

	// Create the ActiveRequest with a cancelable context that carries the userID
	persistCtx := CopyUserContext(r)
	activeRequest := NewActiveRequest(conversation.ID, conversation.UserID, model, payload.ModelSelected)
	cancelCtx, cancelFunc := context.WithCancel(persistCtx)
	activeRequest.Ctx = cancelCtx
	activeRequest.Cancel = cancelFunc
	RequestRegistry.Set(conversation.ID.Hex(), activeRequest)
	if err := db.UpdateConversationState(persistCtx, conversation.ID, utils.StateProcessing); err != nil {
		utils.Error("[HandleChat] Error setting conversation state", err)
	}

	// Connect this HTTP client as SSE listener
	SetSSEHeaders(w)
	sseClient := NewSSEClient(w)
	if sseClient == nil {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	activeRequest.AddClient(sseClient)

	// Launch the LLM loop in a goroutine that outlives this HTTP connection
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

// HandleStreamReconnect allows a client to reconnect to an active conversation's SSE stream.
func HandleStreamReconnect(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	conversationID := vars["id"]

	activeRequest := RequestRegistry.Get(conversationID)
	if activeRequest == nil {
		http.Error(w, "No active request for this conversation", http.StatusNotFound)
		return
	}

	SetSSEHeaders(w)
	sseClient := NewSSEClient(w)
	if sseClient == nil {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	activeRequest.AddClient(sseClient)

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
		http.Error(w, "No active request for this conversation", http.StatusNotFound)
		return
	}

	activeRequest.Cancel()
	w.WriteHeader(http.StatusOK)
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

	client := &StatusClient{
		writer:  w,
		flusher: flusher,
		UserID:  userID,
		Done:    make(chan struct{}),
	}
	StatusRegistry.Add(client)
	defer StatusRegistry.Remove(client)

	// Send initial catch-up: current state of all active conversations
	for _, ar := range RequestRegistry.GetForUser(userID) {
		client.Send(StatusEvent{
			ConversationID: ar.ConversationID.Hex(),
			State:          string(ar.State),
		})
	}

	// Block until client disconnects
	select {
	case <-client.Done:
	case <-r.Context().Done():
	}
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
		titlePayload += message.Role + ": \n"
		titlePayload += message.TextContent() + "\n\n"
	}

	if len(titlePayload) > 500 {
		titlePayload = titlePayload[:500]
	}

	title, err := GenerateTitleForMessage(titlePayload)
	if err != nil {
		return "", "", fmt.Errorf("generating title: %w", err)
	}

	iconPrompt := "Simple illustration, visual, " + title
	iconData, err := GenerateIconForConversation(iconPrompt)
	if err != nil {
		return "", "", fmt.Errorf("generating icon: %w", err)
	}

	if err := db.UpdateConversationMetadata(ctx, conversation.ID, title, iconData); err != nil {
		return "", "", fmt.Errorf("updating metadata: %w", err)
	}

	db.RemoveCredits(ctx, 100, utils.UserAction{Type: TITLE, Provider: NONE, Model: utils.Model{}})

	return title, iconData, nil
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

func GenerateIconForConversation(prompt string) (string, error) {
	request := ImageGenerationRequest{
		Model:          "black-forest-labs/FLUX.2-dev",
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

func API_GetUserBalance(w http.ResponseWriter, r *http.Request) {
	balance, err := db.GetUserBalance(r.Context())
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(balance)
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

	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		utils.SendHTTPError(w, "Invalid conversation ID", http.StatusBadRequest)
		return
	}

	if err := db.UpdateConversationFolder(r.Context(), oid, request.Folder); err != nil {
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

	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		utils.SendHTTPError(w, "Invalid conversation ID", http.StatusBadRequest)
		return
	}

	if err := db.UpdateConversationMetadata(r.Context(), oid, request.Title, ""); err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
