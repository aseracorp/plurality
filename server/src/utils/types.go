package utils

import (
	"time"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type MessageContentURL struct {
	URL string `json:"url" bson:"url"`
}

type MessageContentToolCall struct {
	ID string `json:"id" bson:"id"`
	Name string `json:"name" bson:"name"`
	Arguments string `json:"arguments" bson:"arguments"`
	Loading string `json:"loading" bson:"loading"`
	IconURL string `json:"icon_url" bson:"icon_url"`
}

type MessageContent struct {
	Type     string            `json:"type" bson:"type"`
	Text     string            `json:"text",omitempty bson:"text,omitempty"`
	ImageURL MessageContentURL `json:"image_url" bson:"image_url"`
	ToolCall MessageContentToolCall `json:"tool_call" bson:"tool_call"`
	ToolUseId string `json:"tool_use_id" bson:"tool_use_id"`
}

type Message struct {
	Role      string           `json:"role" bson:"role"`
	Timestamp string           `json:"timestamp" bson:"timestamp"`
	Content   []MessageContent `json:"content" bson:"content"`
	TotalTokens int `json:"total_tokens" bson:"total_tokens"`
	Model Model `json:"model" bson:"model"`
}

func (m Message) Text() string {
	for _, c := range m.Content {
		if c.Type == "text" {
			return c.Text
		}
	}
	return ""
}

type Model struct {
	Name   string            `json:"name",omitempty bson:"name,omitempty"`
	Params map[string]string `json:"params",omitempty bson:"params,omitempty"`
}

type ModelSelected struct {
	Text            Model `json:"text"`
	Vision          Model `json:"vision"`
	ImageGen        Model `json:"image_gen"`
	AudioTranscribe Model `json:"audio_transcribe"`
	VoiceGen        Model `json:"voice_gen"`
	AudioGen        Model `json:"audio_gen"`
	VideoGen        Model `json:"video_gen"`
	VideoVision     Model `json:"video_vision"`
	Code            Model `json:"code"`
}

type Conversation struct {
	ID 		        primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	UserID        string    `json:"user_id" bson:"user_id"`
	Messages      []Message `json:"messages" bson:"messages"`
	Title 	      string    `json:"title" bson:"title"`
	LastMessageAt time.Time    `json:"last_message_at" bson:"last_message_at"`
	ModelSelected ModelSelected `json:"model_selected" bson:"model_selected"`
	MiniApp 		  *MiniApp `json:"mini_app,omitempty" bson:"mini_app"`
}

type UserAction struct {
	Type     int `bson:"type"`
	Provider int `bson:"provider"`
	Model    Model `bson:"model"`
}

type MiniAppInput struct {
	Name string `json:"name" bson:"name"`
	Description string `json:"description" bson:"description"`
	Type string `json:"type" bson:"type"`
	Options []string `json:"options" bson:"options"`
}

type MiniApp struct {
	ID          primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	Name        string             `json:"name" bson:"name"`
	Description string             `json:"description" bson:"description"`
	IconURL     string             `json:"icon_url" bson:"icon_url"`
	Author      string             `json:"author" bson:"author"`
	Prompt 	    map[string]string  `json:"-" bson:"prompt"`
	Models      []ModelSelected    `json:"models" bson:"models"`
	Inputs			[]MiniAppInput     `json:"inputs" bson:"inputs"`
	InitialMessage map[string]string `json:"initial_message" bson:"initial_message"`
	InputPlaceholderMessage map[string]string `json:"input_placeholder_message" bson:"input_placeholder_message"`
}

type ToolCall struct {
	Type string `json:"type"`
	Function ToolCallFunction `json:"function"`
	ID string `json:"id,omitempty"`
}

type ToolCallFunction struct {
	Name string `json:"name"`
	Arguments string `json:"arguments"`
	ID string `json:"id,omitempty"`
}

type ToolsRequest struct {
	Type string `json:"type"`
	Function FunctionToolsRequest `json:"function"`
}

type FunctionToolsRequest struct {
	Name string `json:"name"`
	Description string `json:"description"`
	Parameters []ParameterToolsRequest `json:"parameters,omitempty"`
	InputSchema ParameterToolsRequest `json:"input_schema,omitempty"`
}

type AITool struct {
	ID primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	Name string `json:"name" bson:"name"`
	Description string `json:"description" bson:"description"`
	ToolID string `json:"tool_id" bson:"tool_id"`
	ToolRequest ToolsRequest `json:"tool_call" bson:"tool_call"`
	LoadingString string `json:"loading_string" bson:"loading_string"`
	IconURL string `json:"icon_url" bson:"icon_url"`
	Author string `json:"author" bson:"author"`
	Exec   func(string) string `json:"-"`
}

type ParameterToolsRequest struct {
	Type string `json:"type"`
	Properties map[string]PropertyParameterToolsRequest `json:"properties"`
	Required []string `json:"required,omitempty"`
}

type PropertyParameterToolsRequest struct{
	Type string `json:"type"`
	Description string `json:"description"`
	Enum []string `json:"enum,omitempty"`
}