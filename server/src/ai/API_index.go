package ai

import (
	"encoding/json"
	"net/http"
	"time"
	"strings"
	"strconv"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"github.com/gorilla/mux"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

func HandleImageGeneration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var request ImageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		utils.Error("[HandleImageGeneration] Invalid request body", err)
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if request.ConversationID == primitive.NilObjectID {
		utils.Error("[HandleImageGeneration] No conversation ID provided", nil)
		utils.SendHTTPError(w, "No conversation ID provided", http.StatusBadRequest)
		return
	}

	// Set default values if not provided
	if request.Model == "" {
		request.Model = "black-forest-labs/FLUX.1-schnell"
	}
	if request.Width == 0 || request.Width > 1024 {
		request.Width = 1024
	}
	if request.Height == 0 || request.Height > 768 {
		request.Height = 768
	}
	if request.Steps == 0 || request.Steps > 40 {
		request.Steps = 40
	}
	if request.N == 0 || request.N > 1 {
		request.N = 1
	}
	if request.ResponseFormat == "" {
		request.ResponseFormat = "b64_json"
	}
  
  utils.Log("[HandleImageGeneration] Generating image with model %s and steps %d", request.Model, request.Steps)

	response, err := GenerateImage(request)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	partialConv := utils.Conversation{
		ID: request.ConversationID,
	}

	// parse response
	type JsonResponse struct {
    Data []struct {
			B64Json  string `json:"b64_json"`
			Timings  struct {
					Inference float64 `json:"inference"`
			} `json:"timings"`
    } `json:"data"`
	}

	var jsonResponse JsonResponse
	
	if err := json.Unmarshal(response, &jsonResponse); err != nil {
		utils.Error("[HandleImageGeneration] Error unmarshaling response", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := jsonResponse.Data[0].B64Json

	infTime := jsonResponse.Data[0].Timings.Inference

	message := utils.Message{
		Role: "assistant",
		Timestamp: time.Now().Format(time.RFC3339),
		Content: []utils.MessageContent{
			utils.MessageContent{
				Type: "image_url",
				ImageURL: utils.MessageContentURL{
					URL: data,
				},
			},
			utils.MessageContent{
				Type: "text",
				Text: "Generated in " + strconv.FormatFloat(infTime, 'f', 2, 64) + "s",
			},
		},
	}

	// Save to DB
	_, _, err = db.PushMessage(r.Context(), partialConv, message)
	if err != nil {
		utils.Error("[HandleChat] Error pushing message", err)
	}

	utils.Log("[HandleImageGeneration] Image generated and saved to DB")

	w.Header().Set("Content-Type", "application/json")
	messageAsBytes, _ := json.Marshal(message)
	w.Write(messageAsBytes)
}

func HandleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.Error("[HandleChat] Method not allowed", nil)
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var payload ChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		utils.Error("[HandleChat] Invalid request body", err)
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	
	if len(payload.Messages) == 0 {
		utils.Error("[HandleChat] No messages provided", nil)
		utils.SendHTTPError(w, "No messages provided", http.StatusBadRequest)
		return
	}

	utils.Debug("[HandleChat] Received chat payload for model", payload.ModelSelected)
	
	partialConv := utils.Conversation{
		ID: payload.ConversationID,
		ModelSelected: payload.ModelSelected,
	}

	utils.Log("[HandleChat] Pushing message to conversation ID: ", payload.ConversationID)
	
	conv, isNew, err := db.PushMessage(r.Context(), partialConv, payload.Messages[0])
	if err != nil {
		utils.Error("[HandleChat] Error pushing message", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Transfer-Encoding", "chunked")
	
	// Select the appropriate model
	model := SelectModel(payload.ModelSelected, payload.Messages[0])
	
	// Get the response from the model's API
	response, err := SendChatCompletion(model, conv)
	if err != nil {
		utils.Error("[HandleChat] Error sending chat completion", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	// Create a chunk processor for handling the response
	chunkProcessor := NewChunkProcessor(w, conv, isNew)
	
	// Process the response based on the model type
	var processErr error
	if strings.HasPrefix(model.Name, "Claude/") {
		// Use Claude-specific chunk processor
		processErr = chunkProcessor.ProcessClaudeChunk(r.Context(), response)
	} else {
		// Use standard chunk processor for OpenAI and Together AI
		processErr = chunkProcessor.ProcessStandardChunk(r.Context(), response)
	}
	
	if processErr != nil {
		utils.Error("[HandleChat] Error processing response", processErr)
	}
}

func API_ListConversation(w http.ResponseWriter, r *http.Request) {
  if r.Method != http.MethodGet {
    utils.Error("[API_ListConversation] Method not allowed", nil)
    utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
    return
  }

  conversations, err := db.ListConversations(r.Context())
  if err != nil {
    utils.Error("[API_ListConversation] Error listing conversations", err)
    utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
    return
  }

  response, err := json.Marshal(conversations)
  if err != nil {
    utils.Error("[API_ListConversation] Error marshaling response", err)
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

  if r.Method == http.MethodDelete {
    if err := db.DeleteConversation(r.Context(), id); err != nil {
      utils.Error("[API_HandleConversation] Error deleting conversation", err)
      utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
      return
    }

    utils.Log("[API_HandleConversation] Conversation deleted", id)

    w.WriteHeader(http.StatusNoContent)
    return
  } else if r.Method == http.MethodGet {
    conv, err := db.GetConversationById(r.Context(), id); 
    
    if err != nil {
      utils.Error("[API_HandleConversation] Error getting conversation", err)
      utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
      return
    }
    
    response, err := json.Marshal(conv)
    if err != nil {
      utils.Error("[API_HandleConversation] Error marshaling response", err)
      utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
      return
    }

    utils.Log("[API_HandleConversation] Returning conversation %s", id)

    w.Header().Set("Content-Type", "application/json")
    w.Write(response)
    return
  } else {
    utils.Error("[API_HandleConversation] Method not allowed", nil)
    utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
    return
  }
}