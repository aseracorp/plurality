package ai_tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

// attachmentMessages is the message slice the attachment index must be built
// over: EVERY stored row, folded (compacted) rows included.
//
// Why not the slice the tool loop hands in: that is the LLM-facing view, which
// omits folded rows so a compacted conversation does not re-inflate every
// request. Using it for FETCH would make the fetch placeholder a promise the
// server cannot keep — an attachment whose row got folded is not in that index,
// so "call conversation_attachments to read it again" would answer "not
// found" for exactly the content compaction promised to keep available.
// Retrieval is the one path that must see the whole history.
func attachmentMessages(ctx context.Context, conv utils.Conversation) []utils.Message {
	if full, err := db.GetConversationById(ctx, conv.ID); err == nil && full != nil && len(full.Messages) > 0 {
		return full.Messages
	}
	// Unsaved/in-memory conversation, or a context without a user: the caller's
	// slice is all there is.
	return conv.Messages
}

// ResolveAttachment resolves an attachment ID (e.g. "att_0") from a conversation
// to its content. For binary attachments (images, documents) stored with internal
// URLs, the content is returned as a data URI. For text-based attachments
// (snippets, files), the plain text is returned. The attachment metadata is also
// returned so callers can inspect the type.
func ResolveAttachment(ctx context.Context, attachmentID string, conv utils.Conversation) (content string, meta *utils.AttachmentMeta, err error) {
	index := utils.BuildAttachmentIndex(attachmentMessages(ctx, conv), storage.FileSizeFromURL)

	raw, ok := index.ContentMap[attachmentID]
	if !ok {
		return "", nil, fmt.Errorf("attachment %s not found in conversation", attachmentID)
	}

	// Find metadata
	for _, item := range index.Items {
		if item.ID == attachmentID {
			meta = &item
			break
		}
	}

	if storage.IsInternalURL(raw) {
		data, mimeType, readErr := storage.ReadBlob(raw)
		if readErr != nil {
			return "", meta, fmt.Errorf("error reading attachment %s from storage: %w", attachmentID, readErr)
		}
		b64 := base64.StdEncoding.EncodeToString(data)
		return fmt.Sprintf("data:%s;base64,%s", mimeType, b64), meta, nil
	}

	// Already a data URI or plain text — return as-is
	return raw, meta, nil
}

// ResolveAttachmentImage resolves an image attachment and returns its data URI
// plus aspect ratio (width / height). Returns an error if the attachment is not
// an image.
func ResolveAttachmentImage(ctx context.Context, attachmentID string, conv utils.Conversation) (dataURI string, aspectRatio float64, err error) {
	content, meta, err := ResolveAttachment(ctx, attachmentID, conv)
	if err != nil {
		return "", 0, err
	}

	if meta != nil && meta.Type != "image_url" {
		return "", 0, fmt.Errorf("attachment %s is type %q, not an image", attachmentID, meta.Type)
	}

	// Decode image bytes to detect dimensions
	var rawBytes []byte
	if strings.HasPrefix(content, "data:") {
		parts := strings.SplitN(content, ",", 2)
		if len(parts) == 2 {
			rawBytes, _ = base64.StdEncoding.DecodeString(parts[1])
		}
	}

	aspectRatio = 4.0 / 3.0 // default fallback (matches current 1024x768)
	if len(rawBytes) > 0 {
		cfg, _, decErr := image.DecodeConfig(bytes.NewReader(rawBytes))
		if decErr == nil && cfg.Width > 0 && cfg.Height > 0 {
			aspectRatio = float64(cfg.Width) / float64(cfg.Height)
		}
	}

	return content, aspectRatio, nil
}
