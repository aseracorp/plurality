package utils

import (
	"fmt"
	"strings"
)

// AttachmentMeta holds lightweight metadata about a single attachment
// extracted from conversation messages.
type AttachmentMeta struct {
	ID           string `json:"id"`
	Type         string `json:"type"`                    // "image_url", "snippet", "file"
	Filename     string `json:"filename,omitempty"`       // original filename if available
	Ext          string `json:"ext,omitempty"`            // file extension or MIME type
	Size         int    `json:"size"`                     // byte length of the content string
	MessageIndex int    `json:"message_index"`            // position in Messages array
	PartIndex    int    `json:"part_index"`               // position within the message's content parts
}

// AttachmentIndex is a runtime-built index of all attachments in a conversation.
type AttachmentIndex struct {
	Items      []AttachmentMeta  `json:"items"`
	ContentMap map[string]string `json:"-"` // ID -> full content (not serialized)
}

// BuildAttachmentIndex scans conversation messages and builds an index of all
// non-text attachments. This includes user-uploaded images, snippets, files,
// and tool results that contain base64 data (e.g. generated images).
func BuildAttachmentIndex(messages []Message) AttachmentIndex {
	index := AttachmentIndex{
		Items:      []AttachmentMeta{},
		ContentMap: make(map[string]string),
	}

	counter := 0

	for msgIdx, msg := range messages {
		// Skip conversation_attachments tool results — they are ephemeral
		// retrievals, not original attachments. Indexing them would duplicate data.
		if msg.Role == "tool" && msg.Name == "conversation_attachments" {
			continue
		}

		parts := msg.ContentParts()

		for partIdx, part := range parts {
			content := ""
			if part.Type == "image_url" && part.ImageURL != nil {
				content = part.ImageURL.URL
			} else if part.Text != "" {
				content = part.Text
			}

			if content == "" || len(content) < 3*1024 {
				continue
			}

			id := fmt.Sprintf("att_%d", counter)
			counter++

			meta := AttachmentMeta{
				ID:           id,
				Type:         part.Type,
				Size:         len(content),
				MessageIndex: msgIdx,
				PartIndex:    partIdx,
			}

			// Try to extract extension from content
			if part.Type == "image_url" {
				meta.Ext = guessImageExt(content)
			}

			index.Items = append(index.Items, meta)
			index.ContentMap[id] = content
		}

	}

	return index
}

// guessImageExt tries to determine the image format from a data URI or URL.
func guessImageExt(content string) string {
	if strings.HasPrefix(content, "data:image/png") {
		return "png"
	} else if strings.HasPrefix(content, "data:image/jpeg") || strings.HasPrefix(content, "data:image/jpg") {
		return "jpeg"
	} else if strings.HasPrefix(content, "data:image/gif") {
		return "gif"
	} else if strings.HasPrefix(content, "data:image/webp") {
		return "webp"
	} else if strings.HasPrefix(content, "data:image/") {
		// Extract MIME subtype
		parts := strings.SplitN(content, ";", 2)
		if len(parts) > 0 {
			mime := strings.TrimPrefix(parts[0], "data:image/")
			return mime
		}
	}
	return "image"
}
