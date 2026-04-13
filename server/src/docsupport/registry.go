package docsupport

import "fmt"

// DocumentExtensions is the single source of truth for which content-part types
// are treated as binary documents. To add a new format, add an entry here and
// create a corresponding parser file.
var DocumentExtensions = map[string]bool{
	"pdf":  true,
	"docx": true,
	"xlsx": true,
	"pptx": true,
}

// IsDocumentType reports whether a ContentPart.Type value is a supported
// document format.
func IsDocumentType(partType string) bool {
	return DocumentExtensions[partType]
}

// SearchMatch holds a single regex match result from a document.
type SearchMatch struct {
	Page    int    `json:"page,omitempty"`
	Section string `json:"section,omitempty"`
	Match   string `json:"match"`
	Context string `json:"context"`
}

// ParseDocument extracts readable text from a document byte slice.
// ext determines which parser to use (e.g. "pdf", "docx").
// pages is only meaningful for PDF; ignored for other formats.
func ParseDocument(data []byte, ext, pages string, maxChars int) (string, error) {
	switch ext {
	case "pdf":
		return ParsePDF(data, pages, maxChars)
	case "docx":
		return ParseDOCX(data, maxChars)
	case "xlsx":
		return ParseXLSX(data, maxChars)
	case "pptx":
		return ParsePPTX(data, maxChars)
	default:
		return "", fmt.Errorf("unsupported document format: %s", ext)
	}
}

// SearchDocument runs a regex pattern against a document and returns matches
// with context.
func SearchDocument(data []byte, ext, pattern string, maxResults int) ([]SearchMatch, error) {
	switch ext {
	case "pdf":
		return SearchPDF(data, pattern, maxResults)
	case "docx", "xlsx", "pptx":
		return searchByFullText(data, ext, pattern, maxResults)
	default:
		return nil, fmt.Errorf("unsupported document format: %s", ext)
	}
}

// searchByFullText is a generic search implementation for formats that don't
// have native page-level search. It extracts all text, then runs the regex.
func searchByFullText(data []byte, ext, pattern string, maxResults int) ([]SearchMatch, error) {
	text, err := ParseDocument(data, ext, "", 0)
	if err != nil {
		return nil, err
	}
	return searchText(text, pattern, maxResults)
}
