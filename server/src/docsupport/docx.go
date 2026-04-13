package docsupport

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// ParseDOCX extracts text from a DOCX byte slice.
// DOCX files are ZIP archives containing XML; text lives in word/document.xml
// inside <w:t> elements.
func ParseDOCX(data []byte, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 10000
	}

	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("opening DOCX: %w", err)
	}

	var docFile *zip.File
	for _, f := range r.File {
		if f.Name == "word/document.xml" {
			docFile = f
			break
		}
	}
	if docFile == nil {
		return "", fmt.Errorf("DOCX has no word/document.xml")
	}

	rc, err := docFile.Open()
	if err != nil {
		return "", fmt.Errorf("reading DOCX document.xml: %w", err)
	}
	defer rc.Close()

	text, err := extractDOCXText(rc, maxChars)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("no text could be extracted from this DOCX")
	}

	return text, nil
}

// extractDOCXText walks the XML in word/document.xml and collects text from
// <w:t> elements, joining paragraphs (<w:p>) with newlines.
func extractDOCXText(r io.Reader, maxChars int) (string, error) {
	decoder := xml.NewDecoder(r)
	var b strings.Builder
	charCount := 0
	inParagraph := false
	paragraphHasText := false

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return b.String(), nil // return what we have so far
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "p" && t.Name.Space == "http://schemas.openxmlformats.org/wordprocessingml/2006/main" {
				if inParagraph && paragraphHasText {
					b.WriteString("\n")
					charCount++
				}
				inParagraph = true
				paragraphHasText = false
			}
		case xml.CharData:
			text := string(t)
			if charCount+len(text) > maxChars {
				remaining := maxChars - charCount
				if remaining > 0 {
					b.WriteString(text[:remaining])
				}
				b.WriteString("\n[truncated]")
				return b.String(), nil
			}
			b.WriteString(text)
			charCount += len(text)
			if strings.TrimSpace(text) != "" {
				paragraphHasText = true
			}
		}
	}

	return strings.TrimSpace(b.String()), nil
}
