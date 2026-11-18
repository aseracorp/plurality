package utils

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/azukaar/plurality/src/docsupport"
)

// AttachmentMeta holds lightweight metadata about a single attachment
// extracted from conversation messages.
type AttachmentMeta struct {
	ID           string `json:"id"`
	Type         string `json:"type"`               // "image_url", "snippet", "file"
	Filename     string `json:"filename,omitempty"` // original filename if available
	Ext          string `json:"ext,omitempty"`      // file extension or MIME type
	Size         int    `json:"size"`               // byte length of the content string
	MessageIndex int    `json:"message_index"`      // position in Messages array
	PartIndex    int    `json:"part_index"`         // position within the message's content parts
}

// AttachmentIndex is a runtime-built index of all attachments in a conversation.
type AttachmentIndex struct {
	Items      []AttachmentMeta  `json:"items"`
	ContentMap map[string]string `json:"-"` // ID -> full content (not serialized)
}

// FileSizeFunc resolves the size of a file-backed attachment given its internal
// URL path (e.g. "/attachments/uid/cid/file.png"). Callers inject this so that
// the utils package doesn't depend on the storage package.
type FileSizeFunc func(urlPath string) int64

// BuildAttachmentIndex scans conversation messages and builds an index of all
// non-text attachments. fileSizeFn is called for internal-URL attachments to
// get their on-disk size; pass nil if file-backed attachments are not expected.
func BuildAttachmentIndex(messages []Message, fileSizeFn FileSizeFunc) AttachmentIndex {
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
			isInternalURL := false
			if part.Type == "image_url" && part.ImageURL != nil {
				content = part.ImageURL.URL
				isInternalURL = strings.HasPrefix(content, "/attachments/")
			} else if docsupport.IsDocumentType(part.Type) && part.Text != "" {
				content = part.Text
				isInternalURL = strings.HasPrefix(content, "/attachments/")
			} else if part.Text != "" {
				content = part.Text
			}

			if content == "" {
				continue
			}

			// For internal URLs, get size from disk; for inline, check minimum size
			contentSize := len(content)
			if isInternalURL {
				if fileSizeFn != nil {
					contentSize = int(fileSizeFn(content))
				}
				if contentSize == 0 {
					continue
				}
			} else if contentSize < 3*1024 {
				continue
			}

			id := fmt.Sprintf("att_%d", counter)
			counter++

			meta := AttachmentMeta{
				ID:           id,
				Type:         part.Type,
				Filename:     part.Filename,
				Size:         contentSize,
				MessageIndex: msgIdx,
				PartIndex:    partIdx,
			}

			if part.Type == "image_url" {
				if isInternalURL {
					meta.Ext = guessExtFromPath(content)
				} else {
					meta.Ext = guessImageExt(content)
				}
			} else if docsupport.IsDocumentType(part.Type) {
				meta.Ext = part.Type
			}

			index.Items = append(index.Items, meta)
			index.ContentMap[id] = content
		}
	}

	return index
}

// guessExtFromPath extracts the file extension from an internal URL path.
func guessExtFromPath(path string) string {
	ext := filepath.Ext(path)
	if ext != "" {
		return strings.TrimPrefix(ext, ".")
	}
	return "image"
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
		parts := strings.SplitN(content, ";", 2)
		if len(parts) > 0 {
			mime := strings.TrimPrefix(parts[0], "data:image/")
			return mime
		}
	}
	return "image"
}
