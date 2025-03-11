package utils

import (
	"time"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type MessageContentURL struct {
	URL string `json:"url" bson:"url"`
}

type MessageContent struct {
	Type     string            `json:"type" bson:"type"`
	Text     string            `json:"text",omitempty bson:"text,omitempty"`
	ImageURL MessageContentURL `json:"image_url" bson:"image_url"`
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
}

type UserAction struct {
	Type     int `bson:"type"`
	Provider int `bson:"provider"`
	Model    Model `bson:"model"`
}
