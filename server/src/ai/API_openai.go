package ai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// --- OpenAI-Compatible Types ---

type OpenAIChatRequest struct {
	Model       string               `json:"model"`
	Messages    []utils.Message      `json:"messages"`
	Stream      bool                 `json:"stream"`
	Tools       []utils.ToolsRequest `json:"tools,omitempty"`
	Temperature *float64             `json:"temperature,omitempty"`
	MaxTokens   *int                 `json:"max_tokens,omitempty"`
	TopP        *float64             `json:"top_p,omitempty"`
}

type OpenAIChatResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []OpenAIChatChoice `json:"choices"`
	Usage   *OpenAIUsage       `json:"usage,omitempty"`
}

type OpenAIChatChoice struct {
	Index        int            `json:"index"`
	Message      *OpenAIMsg     `json:"message,omitempty"`
	Delta        *OpenAIDelta   `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type OpenAIMsg struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []utils.ToolCall `json:"tool_calls,omitempty"`
}

type OpenAIDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []OpenAIToolDelta `json:"tool_calls,omitempty"`
}

type OpenAIToolDelta struct {
	Index    int               `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function *OpenAIFnDelta    `json:"function,omitempty"`
}

type OpenAIFnDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type OpenAIModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// --- Handlers ---

// HandleListServerTools returns metadata for all server-side tools.
// Clients use this to cache icons and loading strings for display.
func HandleListServerTools(w http.ResponseWriter, r *http.Request) {
	type ToolMeta struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Loading     string `json:"loading"`
		IconURL     string `json:"icon_url"`
	}

	var tools []ToolMeta
	for _, tool := range ai_tools.Registry {
		tools = append(tools, ToolMeta{
			Name:        tool.ToolID,
			Description: tool.Description,
			Loading:     tool.LoadingString,
			IconURL:     tool.IconURL,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tools)
}

// HandleOpenAIListModels returns available models in OpenAI format.
func HandleOpenAIListModels(w http.ResponseWriter, r *http.Request) {
	var models []OpenAIModelEntry
	for _, name := range ValidModels {
		models = append(models, OpenAIModelEntry{
			ID:      name,
			Object:  "model",
			OwnedBy: "plurality",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

// HandleOpenAIChatCompletion handles POST /v1/chat/completions.
// Stateless — no conversation persistence, no state machine.
func HandleOpenAIChatCompletion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.Error("[OpenAI] Method not allowed", nil)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenAIChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.Error("[OpenAI] Invalid request body", err)
		http.Error(w, `{"error":{"message":"Invalid request body"}}`, http.StatusBadRequest)
		return
	}

	if req.Model == "" || len(req.Messages) == 0 {
		utils.Error("[OpenAI] model and messages are required", nil)
		http.Error(w, `{"error":{"message":"model and messages are required"}}`, http.StatusBadRequest)
		return
	}

	// Validate model
	planName, err := db.GetPlanName(r.Context())
	if err != nil {
		utils.Error("[OpenAI] Error getting plan name", err)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if !CheckModel(req.Model, planName) {
		utils.Error("[OpenAI] Invalid model for plan", nil, req.Model)
		http.Error(w, `{"error":{"message":"Invalid model for your plan"}}`, http.StatusBadRequest)
		return
	}

	// Build internal model and conversation
	model := utils.Model{Name: req.Model}
	if req.Temperature != nil || req.TopP != nil || req.MaxTokens != nil {
		model.Params = make(map[string]string)
		if req.Temperature != nil {
			model.Params["temperature"] = fmt.Sprintf("%f", *req.Temperature)
		}
		if req.TopP != nil {
			model.Params["top_p"] = fmt.Sprintf("%f", *req.TopP)
		}
		if req.MaxTokens != nil {
			model.Params["max_tokens"] = fmt.Sprintf("%d", *req.MaxTokens)
		}
	}

	// Extract tool names for model.Tools
	for _, tool := range req.Tools {
		model.Tools = append(model.Tools, tool.Function.Name)
	}

	conversation := utils.Conversation{Messages: req.Messages}
	payload := ChatPayload{
		ModelSelected:   utils.ModelSelected{Text: &model, Vision: &model},
		ClientSideTools: extractClientToolDefs(req.Tools),
	}

	// Call LLM
	response, _, err := SendChatCompletion(r.Context(), model, conversation, payload)
	if err != nil {
		statusCode := http.StatusInternalServerError
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "insufficient credits") {
			statusCode = http.StatusPaymentRequired
		}
		utils.Error("[OpenAI] Chat completion error", err)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, err.Error()), statusCode)
		return
	}

	completionID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	if req.Stream {
		streamOpenAIResponse(w, r, response, req.Model, completionID, created)
	} else {
		collectOpenAIResponse(w, response, req.Model, completionID, created)
	}
}

// --- Streaming ---

func streamOpenAIResponse(w http.ResponseWriter, r *http.Request, response io.ReadCloser, modelName string, completionID string, created int64) {
	defer response.Close()

	SetSSEHeaders(w)

	// Send initial role delta
	writeOpenAIChunk(w, completionID, created, modelName, OpenAIDelta{Role: "assistant"}, nil)

	scanner := bufio.NewScanner(response)
	var toolCalls []utils.ToolCall
	toolCallIndexMap := make(map[string]int) // toolCallID -> index

	for scanner.Scan() {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		text, newToolCalls := parseProviderChunk(data, modelName)

		// Stream text deltas
		if text != "" {
			writeOpenAIChunk(w, completionID, created, modelName, OpenAIDelta{Content: text}, nil)
		}

		// Stream tool call deltas
		for _, tc := range newToolCalls {
			idx, exists := toolCallIndexMap[tc.id]
			if !exists {
				idx = len(toolCalls)
				toolCallIndexMap[tc.id] = idx
				toolCalls = append(toolCalls, utils.ToolCall{ID: tc.id, Type: "function", Function: utils.FunctionCall{Name: tc.name}})

				// Send new tool call delta with name
				writeOpenAIChunk(w, completionID, created, modelName, OpenAIDelta{
					ToolCalls: []OpenAIToolDelta{{
						Index:    idx,
						ID:       tc.id,
						Type:     "function",
						Function: &OpenAIFnDelta{Name: tc.name},
					}},
				}, nil)
			}
			if tc.arguments != "" {
				toolCalls[idx].Function.Arguments += tc.arguments
				writeOpenAIChunk(w, completionID, created, modelName, OpenAIDelta{
					ToolCalls: []OpenAIToolDelta{{
						Index:    idx,
						Function: &OpenAIFnDelta{Arguments: tc.arguments},
					}},
				}, nil)
			}
		}
	}

	// Send finish reason
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	writeOpenAIChunk(w, completionID, created, modelName, OpenAIDelta{}, &finishReason)

	// Send [DONE]
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeOpenAIChunk(w http.ResponseWriter, id string, created int64, model string, delta OpenAIDelta, finishReason *string) {
	chunk := OpenAIChatResponse{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []OpenAIChatChoice{{
			Index:        0,
			Delta:        &delta,
			FinishReason: finishReason,
		}},
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// --- Non-streaming ---

func collectOpenAIResponse(w http.ResponseWriter, response io.ReadCloser, modelName string, completionID string, created int64) {
	defer response.Close()

	var fullText strings.Builder
	var toolCalls []utils.ToolCall
	toolCallMap := make(map[string]*utils.ToolCall)

	scanner := bufio.NewScanner(response)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		text, newToolCalls := parseProviderChunk(data, modelName)
		fullText.WriteString(text)

		for _, tc := range newToolCalls {
			existing, ok := toolCallMap[tc.id]
			if !ok {
				newTC := utils.ToolCall{ID: tc.id, Type: "function", Function: utils.FunctionCall{Name: tc.name}}
				toolCalls = append(toolCalls, newTC)
				toolCallMap[tc.id] = &toolCalls[len(toolCalls)-1]
				existing = toolCallMap[tc.id]
			}
			existing.Function.Arguments += tc.arguments
		}
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	msg := OpenAIMsg{Role: "assistant", Content: fullText.String()}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	resp := OpenAIChatResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: created,
		Model:   modelName,
		Choices: []OpenAIChatChoice{{
			Index:        0,
			Message:      &msg,
			FinishReason: &finishReason,
		}},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// --- Provider Chunk Parsing ---

type parsedToolDelta struct {
	id        string
	name      string
	arguments string
}

// parseProviderChunk extracts text and tool call deltas from a raw LLM chunk.
// All chunks are in OpenAI format since everything goes through the LiteLLM proxy.
func parseProviderChunk(data string, modelName string) (string, []parsedToolDelta) {
	var chunk AIChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return "", nil
	}
	if len(chunk.Choices) == 0 {
		return "", nil
	}

	choice := chunk.Choices[0]
	text := choice.Delta.Content
	if text == "" {
		text = choice.Text
	}

	var tools []parsedToolDelta
	for _, tc := range choice.Delta.ToolCalls {
		tools = append(tools, parsedToolDelta{
			id:        tc.ID,
			name:      tc.Function.Name,
			arguments: tc.Function.Arguments,
		})
	}
	return text, tools
}

// extractClientToolDefs converts ToolsRequest to FunctionToolsRequest for the payload.
func extractClientToolDefs(tools []utils.ToolsRequest) []utils.FunctionToolsRequest {
	var result []utils.FunctionToolsRequest
	for _, t := range tools {
		result = append(result, t.Function)
	}
	return result
}
