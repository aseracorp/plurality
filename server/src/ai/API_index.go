package ai

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
	"bufio"
	"fmt"

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

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
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
  
  response, err := SendChatCompletion(SelectModel(payload.ModelSelected, payload.Messages[0]), conv)
  if err != nil {
    utils.Error("[HandleChat] Error sending chat completion", err)
    utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
    return
  }
  defer response.Close()
  
	tokenUsage := 0
	stringProduced := ""

  // Process the SSE chunks
  scanner := bufio.NewScanner(response)
  for scanner.Scan() {
    line := scanner.Text()
    
    // Skip empty lines or non-data lines
    if !strings.HasPrefix(line, "data: ") {
      continue
    }
    
    // Extract the JSON payload
    jsonData := strings.TrimPrefix(line, "data: ")
    
    // Parse the JSON
    var chunk AIChunk;

		if jsonData == "[DONE]" {
			if tokenUsage == 0 {
				tokenUsage = strings.Count(stringProduced, " ")
			}
          
      if isNew {
        // Try title
        pp := payload.Messages[0].Content[0].Text + " \n Response from AI: " + stringProduced
        if len(pp) > 1024 {
          pp = pp[:1024]
        }
        title, err := GenerateTitleForMessage(pp)
        if err != nil {
          utils.Error("[HandleChat] Error generating title", err)
          title = "Unnamed Cookie"
        }
        utils.Log("[HandleChat] Generated title for message", title)

        // Send the final response
        responseObj := map[string]interface{}{
          "content":       "",
          "model":         chunk.Model,
          "totalTokens":   tokenUsage,
          "conversationID": conv.ID.Hex(),
          "conversationTitle": title,
        }

        responseJSON, err := json.Marshal(responseObj)
        if err == nil {
          // Write the original line to the response
          fmt.Fprintf(w, "%s\n\n", ([]byte)("data: " + string(responseJSON)))
          w.(http.Flusher).Flush()
        }

        // update title in DB
        conv.Title = title
        if err := db.UpdateConversationMetadata(r.Context(), conv.ID, conv.Title); err != nil {
          utils.Error("[HandleChat] Error updating conversation", err)
        }
      }

			fmt.Fprintf(w, "%s\n\n", line)
			w.(http.Flusher).Flush() 

			// Push message to DB
			_, _, err := db.PushMessage(r.Context(), conv, utils.Message{
				Role:      "assistant",
				Timestamp: time.Now().Format(time.RFC3339),
				Content: []utils.MessageContent{
					{
						Type: "text",
						Text: stringProduced,
					},
				},
			})
			if err != nil {
				utils.Error("[HandleChat] Error pushing message", err)
				utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
				return
			}

			utils.Log("[HandleChat] Successfully completed chat using %d tokens", tokenUsage)
			break
		}
    
    if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
      utils.Error("[HandleChat] Error parsing chunk", err, jsonData)
      continue
    }

		if chunk.Usage.TotalTokens > 0 {
			tokenUsage = chunk.Usage.TotalTokens
		}
    
    // Extract content from the chunk
    if len(chunk.Choices) > 0 {
      var content string
      
      // Try to get content from delta first (streaming format)
      if chunk.Choices[0].Delta.Content != "" {
        content = chunk.Choices[0].Delta.Content
      } else if chunk.Choices[0].Text != "" {
        // Fall back to text if available
        content = chunk.Choices[0].Text
      }
      
      // Only process non-empty content
      if content != "" {
				stringProduced += content

				if os.Getenv("LOG_LEVEL") == "DEBUG" {
					// utils.Debug("[HandleChat] Sending response", content)
				}

				responseObj := map[string]interface{}{
          "content":       content,
          "model":         chunk.Model,
          "totalTokens":   tokenUsage,
          "conversationID": conv.ID.Hex(),
          "conversationTitle": conv.Title,
        }
      	
				responseJSON, err := json.Marshal(responseObj)
        if err != nil {
          utils.Error("[HandleChat] Error marshaling response", err)
          continue
        }

        // Write the original line to the response
        fmt.Fprintf(w, "%s\n\n", ([]byte)("data: " + string(responseJSON)))
        w.(http.Flusher).Flush()
      }
    }
  }
  
  if err := scanner.Err(); err != nil {
    utils.Error("[HandleChat] Error reading response", err)
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

    utils.Log("[API_HandleConversation] Returning conversation", string(response))

    w.Header().Set("Content-Type", "application/json")
    w.Write(response)
    return
  } else {
    utils.Error("[API_HandleConversation] Method not allowed", nil)
    utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
    return
  }
}