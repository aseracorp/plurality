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
	if request.Steps == 0 {
		request.Steps = 12
	}
	if request.N == 0 || request.N > 1 {
		request.N = 1
	}
	if request.ResponseFormat == "" {
		request.ResponseFormat = "b64_json"
	}

	if !CheckModel(request.Model) {
		utils.Error("[HandleImageGeneration] Invalid model %s", nil, request.Model)
		utils.SendHTTPError(w, "Invalid model", http.StatusBadRequest)
		return
	}
  
	// check balance before sending request
	priceToken := GetImageGenPrice(request.Model, (request.Width * request.Height), request.Steps)

	canPerform, err := db.CheckSufficientCredits(r.Context(), priceToken)
	if err != nil {	
		utils.Error("[HandleImageGeneration] Error checking credits", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	if !canPerform {
		utils.Error("[HandleImageGeneration] Insufficient credits", err)
		utils.SendHTTPError(w, "Insufficient credits", http.StatusPaymentRequired)
		return
	}
	
  utils.Log("[HandleImageGeneration] Generating image with model %s and steps %d for %f credits", request.Model, request.Steps, priceToken)

	_, err = db.RemoveCredits(r.Context(), priceToken, utils.UserAction{
		Type: IMAGE_GEN,
		Provider: TOGETHER,
		Model: utils.Model{
			Name: request.Model,
			Params: map[string]string{
				"width": strconv.Itoa(request.Width),
				"height": strconv.Itoa(request.Height),
				"steps": strconv.Itoa(request.Steps),
			},
		},
	})

	if err != nil {
		utils.Error("[HandleImageGeneration] Error removing credits", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

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
	
	
	// Select the appropriate model
	model := SelectModel(payload.ModelSelected, payload.Messages[0])

	if !CheckModel(model.Name) {
		utils.Error("[HandleImageGeneration] Invalid model %s", nil, model.Name)
		utils.SendHTTPError(w, "Invalid model", http.StatusBadRequest)
		return
	}

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
	
	// Get the response from the model's API
	response, err := SendChatCompletion(r.Context(), model, conv)
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
		processErr = chunkProcessor.ProcessClaudeChunk(r.Context(), response, model)
	} else {
		// Use standard chunk processor for OpenAI and Together AI
		processErr = chunkProcessor.ProcessStandardChunk(r.Context(), response, model)
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

// Get first two messages of a conversation and return the title
func API_HandleTitleGeneration(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	conv, err := db.GetConversationById(r.Context(), id)
	if err != nil {
		utils.Error("[API_HandleTitleGeneration] Error getting conversation", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var messages []utils.Message

	if len(conv.Messages) > 2 {
		messages = conv.Messages[:2]
	} else {
		messages = conv.Messages
	}

	title := ""
	payload := ""

	for _, message := range messages {
		payload += message.Role + ": \n"
		for _, content := range message.Content {
			if content.Type == "text" {
				payload += content.Text + "\n\n"
			}
		}
	}

	if len(payload) > 500 {
		payload = payload[:500]
	}

	title, err = GenerateTitleForMessage(payload)
	if err != nil {
		utils.Error("[API_HandleTitleGeneration] Error generating title", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = db.UpdateConversationMetadata(r.Context(), conv.ID, title)
	if err != nil {
		utils.Error("[API_HandleTitleGeneration] Error updating conversation metadata", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	utils.Log("[API_HandleTitleGeneration] Title generated and saved to DB")

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(title))
}

func API_GetUserBalance(w http.ResponseWriter, r *http.Request) {
	balance, err := db.GetUserBalance(r.Context())
	if err != nil {
		utils.Error("[API_GetUserBalance] Error getting user balance", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	utils.Log("[API_GetUserBalance] User balance: %f", balance)

	response, err := json.Marshal(balance)
	if err != nil {
		utils.Error("[API_GetUserBalance] Error marshaling response", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}