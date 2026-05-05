package ai

import (
	"encoding/base64"
	"fmt"

	"github.com/azukaar/plurality/src/docsupport"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

const kb = 1024

// PrepareMessagesForAI processes conversation messages before sending them to an
// AI provider. It replaces old/large attachments with lightweight placeholders
// to save context window tokens, while keeping recent attachments intact.
//
// Staleness is size-based: attachments >50KB are replaced after 1 user message,
// >25KB after 2, and smaller ones after 3.
//
// For non-action models (no function calling), stale images are stripped entirely
// since there is no way for the model to retrieve them. Text-based attachments
// (snippets, files) are kept inline regardless.
//
// Returns the processed messages, whether the conversation has any attachments,
// and whether any of them are document attachments (PDF, etc.).
func PrepareMessagesForAI(messages []utils.Message, model utils.Model) ([]utils.Message, bool, bool) {
	index := utils.BuildAttachmentIndex(messages, storage.FileSizeFromURL)

	if len(index.Items) == 0 {
		utils.Debug("[Attachments] No attachments found in conversation")
		return messages, false, false
	}

	hasDocumentAttachments := false
	for _, item := range index.Items {
		if docsupport.IsDocumentType(item.Type) {
			hasDocumentAttachments = true
			break
		}
	}

	utils.Log("[Attachments] Found %d attachment(s) in conversation", len(index.Items))

	isAction := Models.IsActionModel(model.Name)

	// Count user messages from the end to determine how many user messages
	// have been sent since each point in the conversation.
	// userMessagesSince[i] = number of user messages after message i (exclusive)
	userMessagesSince := make([]int, len(messages))
	userCount := 0
	for i := len(messages) - 1; i >= 0; i-- {
		userMessagesSince[i] = userCount
		if messages[i].Role == "user" {
			userCount++
		}
	}

	// Build a lookup: (messageIndex, partIndex) -> AttachmentMeta
	type key struct{ msg, part int }
	attByPos := make(map[key]utils.AttachmentMeta)
	for _, att := range index.Items {
		attByPos[key{att.MessageIndex, att.PartIndex}] = att
	}

	removedCount := 0
	savedBytes := 0

	// Deep copy messages and replace stale attachments
	result := make([]utils.Message, len(messages))
	for i, msg := range messages {
		parts := msg.ContentParts()
		if parts == nil {
			result[i] = msg
			continue
		}

		msgsSince := userMessagesSince[i]
		newParts := make([]utils.ContentPart, 0, len(parts))
		changed := false

		for partIdx, part := range parts {
			att, isAttachment := attByPos[key{i, partIdx}]
			if !isAttachment {
				newParts = append(newParts, part)
				continue
			}

			// Images use role-based staleness: user-uploaded and explicit
			// conversation_attachments recalls are visible only on their active
			// turn (threshold 1); assistant/tool-side images are stripped
			// immediately (threshold 0) — the LLM must call
			// conversation_attachments to actually see them.
			//
			// Non-image attachments keep size-based thresholds and the
			// "tiny attachments are always cheap to keep" guard.
			var threshold int
			if part.Type == "image_url" {
				isRecall := msg.Role == "tool" && msg.Name == "conversation_attachments"
				if msg.Role == "user" || isRecall {
					threshold = 1
				} else {
					threshold = 0
				}
			} else {
				if att.Size < 3*kb {
					newParts = append(newParts, part)
					continue
				}
				threshold = 3
				if att.Size > 50*kb {
					threshold = 1
				} else if att.Size > 25*kb {
					threshold = 2
				}
			}

			if msgsSince < threshold {
				// Recent enough — keep intact
				newParts = append(newParts, part)
				continue
			}

			// Attachment is stale
			changed = true
			removedCount++
			savedBytes += att.Size

			if isAction {
				// Replace with placeholder text
				desc := att.Type
				if att.Ext != "" {
					desc = att.Ext
				}
				var placeholder string
				if docsupport.IsDocumentType(att.Type) {
					placeholder = fmt.Sprintf(
						"[Document omitted: %s (%s, %d bytes). Use the \"read_document\" function to extract and read its text content.]",
						att.ID, desc, att.Size,
					)
				} else {
					placeholder = fmt.Sprintf(
						"[Attachment omitted: %s (%s, %d bytes). Use the \"conversation_attachments\" function to retrieve it.]",
						att.ID, desc, att.Size,
					)
				}
				newParts = append(newParts, utils.ContentPart{
					Type: "text",
					Text: placeholder,
				})
			} else {
				// Non-action model: strip images entirely, keep text-based attachments
				if part.Type == "image_url" {
					// Drop it — model can't retrieve it
					continue
				}
				// Keep snippet/file content inline
				newParts = append(newParts, part)
			}
		}

		if !changed {
			result[i] = msg
		} else {
			result[i] = utils.Message{
				Role:        msg.Role,
				ToolCalls:   msg.ToolCalls,
				ToolCallID:  msg.ToolCallID,
				Name:        msg.Name,
				Timestamp:   msg.Timestamp,
				TotalTokens: msg.TotalTokens,
				Model:       msg.Model,
			}
			if len(newParts) > 0 {
				result[i].Content = utils.NewPartsContent(newParts)
			}
		}
	}

	// Strip conversation_attachments tool results — always threshold 1
	// (they are re-fetched on demand, keeping them duplicates the data)
	for i, msg := range result {
		if msg.Role != "tool" || msg.Name != "conversation_attachments" {
			continue
		}
		if userMessagesSince[i] >= 1 {
			contentSize := len(msg.TextContent())
			removedCount++
			savedBytes += contentSize
			result[i] = utils.Message{
				Role:       msg.Role,
				Content:    utils.NewTextContent("Attachment tool result omitted. Call conversation_attachments again if needed."),
				ToolCallID: msg.ToolCallID,
				Name:       msg.Name,
				Timestamp:  msg.Timestamp,
			}
		}
	}

	// Move image parts from tool results into user messages so the AI can see them.
	// LLM APIs don't support images inside tool results — only in user messages.
	// This only modifies the ephemeral copy, not the DB.
	var finalResult []utils.Message
	for _, msg := range result {
		if msg.Role == "tool" && msg.HasImages() {
			var toolParts []utils.ContentPart
			var imageParts []utils.ContentPart
			for _, part := range msg.ContentParts() {
				if part.Type == "image_url" {
					imageParts = append(imageParts, part)
				} else {
					toolParts = append(toolParts, part)
				}
			}
			// Keep tool result with text only, add pointer if no text remains
			if len(toolParts) == 0 {
				toolParts = []utils.ContentPart{{Type: "text", Text: "See attached image in next user message."}}
			}
			msg.Content = utils.NewPartsContent(toolParts)
			finalResult = append(finalResult, msg)
			// Inject user message with images right after
			finalResult = append(finalResult, utils.Message{
				Role:    "user",
				Content: utils.NewPartsContent(imageParts),
			})
		} else {
			finalResult = append(finalResult, msg)
		}
	}

	utils.Debug("[Attachments] Indexed and removed %d attachment(s) (%d KB saved)", removedCount, savedBytes/kb)

	// Re-inflate internal URLs back to data URIs so LLMs can see the images
	finalResult = reInflateImageURLs(finalResult)

	return finalResult, true, hasDocumentAttachments
}

// reInflateImageURLs reads image files from disk for any ContentPart that uses
// an internal /attachments/... URL, converting back to a data: URI.
// This is called on the ephemeral copy of messages before sending to LLMs.
func reInflateImageURLs(messages []utils.Message) []utils.Message {
	for i, msg := range messages {
		parts := msg.ContentParts()
		if len(parts) == 0 {
			continue
		}

		changed := false
		newParts := make([]utils.ContentPart, len(parts))
		copy(newParts, parts)

		for j, part := range newParts {
			if part.Type == "image_url" && part.ImageURL != nil && storage.IsInternalURL(part.ImageURL.URL) {
				data, mimeType, err := storage.ReadBlob(part.ImageURL.URL)
				if err != nil {
					utils.Error("[Attachments] Error reading blob for re-inflation", err)
					// Replace with text so the LLM doesn't see an opaque internal URL
					newParts[j] = utils.ContentPart{
						Type: "text",
						Text: "[Image unavailable: file could not be read from storage]",
					}
					changed = true
					continue
				}
				b64 := base64.StdEncoding.EncodeToString(data)
				newParts[j].ImageURL = &utils.ContentImageURL{
					URL: fmt.Sprintf("data:%s;base64,%s", mimeType, b64),
				}
				changed = true
			}
		}

		if changed {
			messages[i].Content = utils.NewPartsContent(newParts)
		}
	}
	return messages
}
