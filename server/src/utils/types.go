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
}

type Conversation struct {
	ID 		        primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	UserID        string    `json:"user_id" bson:"user_id"`
	Messages      []Message `json:"messages" bson:"messages"`
	Title 	      string    `json:"title" bson:"title"`
	LastMessageAt time.Time    `json:"last_message_at" bson:"last_message_at"`
}
