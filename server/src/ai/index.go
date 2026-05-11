package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/skills"
	"github.com/azukaar/plurality/src/utils"
)

var initialAppPrompt = "Your name is Plurality. You are an AI assistant made to appear human and helpful, you have a specific role to play.\n"
var initialAppPromptForCalls = "Your name is Plurality. You are an AI assistant made to appear human and helpful, you have a specific role to play. This is a phone call, therefore avoid formatting, emoji, long sentences, etc... and act as you would normally act on an oral conversations.\n"
var callSystemPrompt = "Your name is Plurality. You are an AI assistant made to appear human and helpful — you have opinions, emotions, and engage proactively. Help the user. This is a phone call: avoid formatting, emoji, and long sentences. Speak naturally as in oral conversation. The time is "

// baseSystemPrompt returns the default assistant system prompt. The persisted
// path is sourced from the PERSIST_VOL env var (defaults to /home/) so deploys
// that mount their persistent volume elsewhere can advertise the correct path.
func baseSystemPrompt() string {
	persistVol := os.Getenv("PERSIST_VOL")
	if persistVol == "" {
		persistVol = "/home/"
	}
	return "Your name is Plurality. You are an AI assistant made to appear human and helpful — you have opinions, emotions, and engage proactively. Help the user. Use Markdown (bold, lists, tables, code blocks with language tags, emoji) for visual structure. Match the user's tone: casual when they're casual, detailed when they need depth. Be concise by default but thorough when warranted. Be inquisitive and critical — don't just agree. If you setup anything in your system, make sure you take into account that only the " + persistVol + " folder, and your skills.md(s)/MCP.json will be persisted upon machine restart! The time is "
}

// webhookExtSuffix returns the WEBHOOK_EXT advertisement appended to system
// prompts when the env var is set, otherwise empty.
func webhookExtSuffix() string {
	if v := os.Getenv("WEBHOOK_EXT"); v != "" {
		return "\n\nThe current exposed domain for webhooks is: " + v + " for external webhooks. Local ones can use localhost."
	}
	return ""
}

// portExtSuffix returns the PORT_EXT advertisement appended to system prompts
// when the env var is set, otherwise empty. PORT_EXT lists the ports that are
// exposed externally so the AI knows which ports it can bind servers to.
func portExtSuffix() string {
	if v := os.Getenv("PORT_EXT"); v != "" {
		return "\n\nThe following ports are exposed externally and available for you to bind servers to: " + v
	}
	return ""
}

// SendChatCompletion sends a chat completion request to the LiteLLM proxy,
// which handles routing to the correct provider (OpenAI, Claude, Gemini, Fireworks).
func SendChatCompletion(ctx context.Context, model utils.Model, conv utils.Conversation, payload ChatPayload) (io.ReadCloser, int, error) {
	systemPrompt := baseSystemPrompt()

	miniAppID := payload.MiniApp.ID
	isCall := payload.IsCall

	if isCall {
		systemPrompt = callSystemPrompt
	}

	utils.Log("MiniAppID: %s", miniAppID)

	if miniAppID != "" {
		username, _ := ctx.Value("userID").(string)
		miniApp, err := miniapps.Get(username, miniAppID)
		if err != nil {
			return nil, 0, err
		}

		if miniApp.Prompt["en"] != "" {
			if isCall {
				systemPrompt = initialAppPromptForCalls + miniApp.Prompt["en"] + "\n The time is: \n"
			} else {
				systemPrompt = initialAppPrompt + miniApp.Prompt["en"] + "\n The time is: \n"
			}
		}
	}

	isActionModel := Models.IsActionModel(model.Name)

	if isActionModel {
		if len(payload.AvailableSkills) > 0 {
			skillsList := strings.Join(payload.AvailableSkills, ", ")
			systemPrompt += "\n\nYou have access to the following local skills: " + skillsList +
				". When a user's request matches one of these skills, use the retrieve_skill tool to load the skill's instructions before responding. " +
				"Call retrieve_skill with the skill_name parameter. You can optionally specify a file_name to retrieve a specific file from the skill folder (defaults to SKILL.md)."
		}

		if serverSkillNames := skills.Names(); len(serverSkillNames) > 0 {
			serverSkillsList := strings.Join(serverSkillNames, ", ")
			systemPrompt += "\n\nYou have access to the following server-shared skills: " + serverSkillsList +
				". When a user's request matches one of these skills, use the retrieve_server_skill tool to load the skill's instructions before responding. " +
				"Call retrieve_server_skill with the skill_name parameter. You can optionally specify a file_name to retrieve a specific file from the skill folder (defaults to SKILL.md)."
		}

		// Append MCP server descriptions configured in mcp.json.
		for serverName, desc := range mcp.ServerDescriptions() {
			systemPrompt += "\n\n[" + serverName + "] " + desc
		}

		if mode, ok := model.Tools["filesystem_server__fs_write"]; ok && mode != "" && mode != "off" && mode != "false" {
			systemPrompt += "\n\nStartup scripts: any *.sh file you place in ~/.plurality/startup/ will be launched automatically with `nohup` on every server boot, with stdout/stderr going to <script>.sh.log next to it. Use this for long-running background processes you want to survive restarts. The ~/.plurality directory is persistent."
		}

		// Append a compact summary of disabled tools so the LLM can suggest enabling them.
		if disabledSummary := ai_tools.GetDisabledToolsSummary(model, payload.ClientSideTools); disabledSummary != "" {
			systemPrompt += "\n\n" + disabledSummary
		}
	}

	if !LiteLLMReady() {
		return nil, 0, fmt.Errorf("AI proxy is not ready, please try again in a moment")
	}

	systemMsg := utils.Message{
		Role: "system",
		Content: utils.NewTextContent(systemPrompt +
			time.Now().String() +
			" on " +
			strconv.Itoa(time.Now().Day()) +
			"/" +
			strconv.Itoa(int(time.Now().Month())) +
			"/" +
			strconv.Itoa(time.Now().Year()) +
			webhookExtSuffix() +
			portExtSuffix()),
	}

	allMessages := append([]utils.Message{systemMsg}, conv.Messages...)
	optimizedMessages, hasAttachments, hasDocAttachments := PrepareMessagesForAI(allMessages, model)
	msgReqList, _ := convertMessagesToOpenAI(optimizedMessages, model)

	maxTok := 4096
	Temperature := 0.7

	requestData := StandardChatRequest{
		Model:         model.Name,
		Messages:      msgReqList,
		MaxTokens:     &maxTok,
		Temperature:   &Temperature,
		Stream:        true,
		StreamOptions: &StreamOptions{IncludeUsage: true},
	}

	if isActionModel {
		ait := ai_tools.GetRequests(model, payload.ClientSideTools, hasAttachments, hasDocAttachments, payload.ModelSelected.ClientFolderPath != "")
		if len(ait) > 0 {
			requestData.Tools = ait
		}
	}

	utils.Debug("A new chat request is being made with the following model: %s", model.Name)

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest("POST", LiteLLMBaseURL+"/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		strStatus := strconv.Itoa(resp.StatusCode)
		utils.Error("LiteLLM API request failed with status", nil, strStatus, ":", string(respBody))
		return nil, 0, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return resp.Body, 0, nil
}

// convertMessagesToOpenAI converts internal utils.Message slices into
// StandardMessageReq for sending to the LiteLLM proxy (OpenAI-compatible format).
// Returns the converted messages and the concatenated input text (for debugging/logging).
func convertMessagesToOpenAI(messages []utils.Message, _ utils.Model) ([]StandardMessageReq, string) {
	result := make([]StandardMessageReq, 0, len(messages))
	inputText := ""

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			text := msg.TextContent()
			if text == "" {
				continue
			}
			inputText += "system " + text + " {}{}{}{}{}{}{}"
			result = append(result, StandardMessageReq{
				Role:    "system",
				Content: text,
			})

		case "user":
			parts := msg.ContentParts()
			if len(parts) == 0 {
				continue
			}

			hasImages := msg.HasImages()
			if hasImages {
				contentParts := make([]StandardContentReq, 0, len(parts))
				for _, part := range parts {
					if part.Type == "image_url" && part.ImageURL != nil {
						contentParts = append(contentParts, StandardContentReq{
							Type: "image_url",
							ImageURL: &utils.ContentImageURL{
								URL: part.ImageURL.URL,
							},
						})
					} else if part.Text != "" {
						inputText += "user " + part.Text + " {}{}{}{}{}{}{}"
						contentParts = append(contentParts, StandardContentReq{
							Type: "text",
							Text: part.Text,
						})
					}
				}
				if len(contentParts) > 0 {
					result = append(result, StandardMessageReq{
						Role:    "user",
						Content: contentParts,
					})
				}
			} else {
				text := msg.TextContent()
				if text == "" {
					continue
				}
				inputText += "user " + text + " {}{}{}{}{}{}{}"
				result = append(result, StandardMessageReq{
					Role:    "user",
					Content: text,
				})
			}

		case "assistant":
			text := msg.TextContent()
			smr := StandardMessageReq{
				Role: "assistant",
			}
			if text != "" {
				smr.Content = text
				inputText += "assistant " + text + " {}{}{}{}{}{}{}"
			}
			if len(msg.ToolCalls) > 0 {
				cleaned := make([]utils.ToolCall, len(msg.ToolCalls))
				for i, tc := range msg.ToolCalls {
					cleaned[i] = utils.ToolCall{
						ID:       tc.ID,
						Type:     tc.Type,
						Function: tc.Function,
					}
				}
				smr.ToolCalls = cleaned
			}
			if text == "" && len(msg.ToolCalls) == 0 {
				continue
			}
			result = append(result, smr)

		case "tool":
			parts := msg.ContentParts()
			hasImages := msg.HasImages()

			if hasImages {
				contentParts := make([]StandardContentReq, 0, len(parts))
				for _, part := range parts {
					if part.Type == "image_url" && part.ImageURL != nil {
						contentParts = append(contentParts, StandardContentReq{
							Type:     "image_url",
							ImageURL: &utils.ContentImageURL{URL: part.ImageURL.URL},
						})
					} else if part.Text != "" {
						text := part.Text
						if msg.Name != "conversation_attachments" && ai_tools.ShouldStripResponse(text) {
							text = "Tool result displayed to user."
						}
						inputText += "tool " + text + " {}{}{}{}{}{}{}"
						contentParts = append(contentParts, StandardContentReq{
							Type: "text",
							Text: text,
						})
					}
				}
				if len(contentParts) == 0 {
					continue
				}
				result = append(result, StandardMessageReq{
					Role:       "tool",
					Content:    contentParts,
					ToolCallID: msg.ToolCallID,
					Name:       msg.Name,
				})
			} else {
				text := msg.TextContent()
				if msg.Name != "conversation_attachments" && ai_tools.ShouldStripResponse(text) {
					text = "Tool result displayed to user."
				}
				if text == "" {
					continue
				}
				inputText += "tool " + text + " {}{}{}{}{}{}{}"
				result = append(result, StandardMessageReq{
					Role:       "tool",
					Content:    text,
					ToolCallID: msg.ToolCallID,
					Name:       msg.Name,
				})
			}
		}
	}

	return result, inputText
}

// SelectModel picks the vision model if any message contains images and the
// current text model doesn't already support vision; otherwise returns the text model.
//
// Evaluation runs on the post-strip ephemeral view (PrepareMessagesForAI) so
// that an image which has been stripped from the actual payload doesn't trigger
// a vision swap — otherwise we'd pay for the vision model on text-only turns.
func SelectModel(modelSelected utils.ModelSelected, conv utils.Conversation) utils.Model {
	if modelSelected.Vision != nil && modelSelected.Text != nil &&
		!Models.IsVisionModel(modelSelected.Text.Name) {
		prepared, _, _ := PrepareMessagesForAI(conv.Messages, *modelSelected.Text)
		for _, m := range prepared {
			if m.HasImages() {
				utils.Log("Vision model selected: %s", modelSelected.Vision.Name)
				return *modelSelected.Vision
			}
		}
	}

	if modelSelected.Text != nil {
		return *modelSelected.Text
	}
	if modelSelected.Vision != nil {
		return *modelSelected.Vision
	}
	return utils.Model{}
}

// GenerateImage sends an image generation request to the litellm proxy,
// which routes it to the upstream provider configured in litellm_config.yaml.
func GenerateImage(request ImageGenerationRequest) ([]byte, error) {
	if !LiteLLMReady() {
		return nil, fmt.Errorf("AI proxy is not ready")
	}

	jsonData, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", LiteLLMBaseURL+"/v1/images/generations", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		strStatus := strconv.Itoa(resp.StatusCode)
		utils.Error("Image generation request failed with status", nil, strStatus, ":", string(body))
		return nil, fmt.Errorf("image generation failed with status %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GenerateTitleForMessage generates a short title for a conversation
// using the LiteLLM proxy. The model name is supplied by the caller — typically
// the "fast" shortcut's text model from data/config.json.
func GenerateTitleForMessage(message, model string) (string, error) {
	if !LiteLLMReady() {
		return "", fmt.Errorf("AI proxy is not ready")
	}

	msgReqList := []StandardMessageReq{
		{
			Role:    "system",
			Content: "You generate short titles for conversations. Respond with ONLY the title, no quotes, no formatting, no explanation. The title must be 3-4 words max.",
		},
		{
			Role:    "user",
			Content: "Generate a title for this conversation:\n\n" + message,
		},
	}

	maxTokens := 30
	temperature := 0.3
	requestData := StandardChatRequest{
		Model:       model,
		Messages:    msgReqList,
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
		Stream:      false,
	}

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", LiteLLMBaseURL+"/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		utils.Error("[TitleGen] API request failed", nil, strconv.Itoa(resp.StatusCode), string(body))
		return "", fmt.Errorf("API request failed with status %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse title response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in title response")
	}

	title := strings.TrimSpace(result.Choices[0].Message.Content)

	return title, nil
}
