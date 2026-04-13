package storage

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

const maxBlobSize = 100 * 1024 * 1024 // 100MB

// ExtractBlobsFromMessage scans a Message's ContentParts for data: URIs,
// saves them to disk, and replaces the URL with the internal path.
func ExtractBlobsFromMessage(userID string, msg *utils.Message) error {
	parts := msg.ContentParts()
	if len(parts) == 0 {
		return nil
	}

	changed := false
	newParts := make([]utils.ContentPart, len(parts))
	copy(newParts, parts)

	for i, part := range newParts {
		if part.Type == "image_url" && part.ImageURL != nil && strings.HasPrefix(part.ImageURL.URL, "data:") {
			data, mimeType, ext, err := ExtractBlobFromDataURI(part.ImageURL.URL)
			if err != nil {
				utils.Error("[Storage] Failed to extract image blob", err)
				continue
			}
			if len(data) > maxBlobSize {
				return fmt.Errorf("blob exceeds maximum size of %d bytes", maxBlobSize)
			}
			_ = mimeType
			urlPath, err := SaveBlob(userID, data, ext)
			if err != nil {
				return fmt.Errorf("saving image blob: %w", err)
			}
			newParts[i].ImageURL = &utils.ContentImageURL{URL: urlPath}
			changed = true
		}

		if part.Type == "pdf" && strings.HasPrefix(part.Text, "data:") {
			data, _, ext, err := ExtractBlobFromDataURI(part.Text)
			if err != nil {
				utils.Error("[Storage] Failed to extract PDF blob", err)
				continue
			}
			if len(data) > maxBlobSize {
				return fmt.Errorf("blob exceeds maximum size of %d bytes", maxBlobSize)
			}
			urlPath, err := SaveBlob(userID, data, ext)
			if err != nil {
				return fmt.Errorf("saving PDF blob: %w", err)
			}
			newParts[i].Text = urlPath
			changed = true
		}
	}

	if changed {
		msg.Content = utils.NewPartsContent(newParts)
	}
	return nil
}

// ExtractBlobFromDataURI parses "data:mime/type;base64,AAAA..." and returns
// the decoded bytes, MIME type, and file extension.
func ExtractBlobFromDataURI(dataURI string) ([]byte, string, string, error) {
	// Format: data:mime/type;base64,<data>
	if !strings.HasPrefix(dataURI, "data:") {
		return nil, "", "", fmt.Errorf("not a data URI")
	}

	commaIdx := strings.Index(dataURI, ",")
	if commaIdx < 0 {
		return nil, "", "", fmt.Errorf("malformed data URI: no comma")
	}

	header := dataURI[5:commaIdx] // strip "data:"
	b64Data := dataURI[commaIdx+1:]

	// Parse MIME type from header (e.g. "image/png;base64")
	mimeType := strings.TrimSuffix(header, ";base64")

	ext := extFromMIME(mimeType)

	data, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return nil, "", "", fmt.Errorf("decoding base64: %w", err)
	}

	return data, mimeType, ext, nil
}

// ExtractIconBlob saves a raw base64 icon string (no data: prefix) as icon.png
// and returns the URL path.
func ExtractIconBlob(userID, iconBase64 string) (string, error) {
	if iconBase64 == "" {
		return "", nil
	}

	data, err := base64.StdEncoding.DecodeString(iconBase64)
	if err != nil {
		return "", fmt.Errorf("decoding icon base64: %w", err)
	}

	return SaveBlob(userID, data, "png")
}

func extFromMIME(mimeType string) string {
	switch mimeType {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/svg+xml":
		return "svg"
	case "application/pdf":
		return "pdf"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case "application/vnd.ms-excel":
		return "xls"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/msword":
		return "doc"
	default:
		// Try to extract subtype: "image/bmp" -> "bmp"
		if idx := strings.LastIndex(mimeType, "/"); idx >= 0 {
			sub := mimeType[idx+1:]
			// Strip parameters like "+xml"
			if plus := strings.Index(sub, "+"); plus >= 0 {
				sub = sub[:plus]
			}
			return sub
		}
		return "bin"
	}
}
