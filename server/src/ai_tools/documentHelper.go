package ai_tools

import (
	"fmt"

	"github.com/azukaar/plurality/src/docsupport"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

// resolveDocumentAttachment looks up a document attachment by ID, validates it
// is a supported document type, and reads its bytes from storage.
// Returns the attachment metadata, raw bytes, or an error content to return to the AI.
func resolveDocumentAttachment(conv utils.Conversation, attachmentID string) (*utils.AttachmentMeta, []byte, *utils.MessageContent) {
	index := utils.BuildAttachmentIndex(conv.Messages, storage.FileSizeFromURL)

	var att *utils.AttachmentMeta
	for _, item := range index.Items {
		if item.ID == attachmentID {
			att = &item
			break
		}
	}
	if att == nil {
		errMsg := utils.NewTextContent(fmt.Sprintf("Attachment %s not found.", attachmentID))
		return nil, nil, &errMsg
	}

	if !docsupport.IsDocumentType(att.Type) {
		errMsg := utils.NewTextContent(fmt.Sprintf("Attachment %s is not a supported document (type: %s). Supported formats: PDF, DOCX, XLSX, PPTX.", attachmentID, att.Type))
		return nil, nil, &errMsg
	}

	content, ok := index.ContentMap[attachmentID]
	if !ok {
		errMsg := utils.NewTextContent(fmt.Sprintf("Content for attachment %s not found.", attachmentID))
		return nil, nil, &errMsg
	}

	var data []byte
	if storage.IsInternalURL(content) {
		d, _, err := storage.ReadBlob(content)
		if err != nil {
			errMsg := utils.NewTextContent(fmt.Sprintf("Error reading document file: %s", err.Error()))
			return nil, nil, &errMsg
		}
		data = d
	} else {
		d, _, _, err := storage.ExtractBlobFromDataURI(content)
		if err != nil {
			errMsg := utils.NewTextContent(fmt.Sprintf("Error decoding document data: %s", err.Error()))
			return nil, nil, &errMsg
		}
		data = d
	}

	return att, data, nil
}
