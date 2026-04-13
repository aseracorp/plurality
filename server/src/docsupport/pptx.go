package docsupport

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ParsePPTX extracts text from a PPTX byte slice.
// PPTX files are ZIP archives; slide text lives in ppt/slide{N}.xml inside
// <a:t> elements.
func ParsePPTX(data []byte, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 10000
	}

	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("opening PPTX: %w", err)
	}

	// Collect slide files and sort by slide number
	type slideEntry struct {
		num  int
		file *zip.File
	}
	var slides []slideEntry

	for _, f := range r.File {
		name := filepath.Base(f.Name)
		dir := filepath.Dir(f.Name)
		if dir != "ppt/slides" && dir != "ppt\\slides" {
			continue
		}
		if !strings.HasPrefix(name, "slide") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "slide"), ".xml")
		num, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		slides = append(slides, slideEntry{num: num, file: f})
	}

	sort.Slice(slides, func(i, j int) bool {
		return slides[i].num < slides[j].num
	})

	if len(slides) == 0 {
		return "", fmt.Errorf("PPTX has no slides")
	}

	var b strings.Builder
	charCount := 0

	for _, s := range slides {
		header := fmt.Sprintf("--- Slide %d ---\n", s.num)
		if charCount+len(header) > maxChars {
			b.WriteString("\n[truncated]")
			break
		}
		b.WriteString(header)
		charCount += len(header)

		rc, err := s.file.Open()
		if err != nil {
			continue
		}

		text, truncated := extractPPTXSlideText(rc, maxChars-charCount)
		rc.Close()

		b.WriteString(text)
		charCount += len(text)

		if truncated {
			b.WriteString("\n[truncated]")
			break
		}

		b.WriteString("\n")
		charCount++
	}

	result := strings.TrimSpace(b.String())
	if result == "" {
		return "", fmt.Errorf("no text could be extracted from this PPTX")
	}

	return result, nil
}

// extractPPTXSlideText walks slide XML and collects text from <a:t> elements,
// joining paragraphs (<a:p>) with newlines.
func extractPPTXSlideText(r io.Reader, remaining int) (string, bool) {
	decoder := xml.NewDecoder(r)
	var b strings.Builder
	charCount := 0
	inParagraph := false
	paragraphHasText := false

	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "p" && t.Name.Space == "http://schemas.openxmlformats.org/drawingml/2006/main" {
				if inParagraph && paragraphHasText {
					b.WriteString("\n")
					charCount++
				}
				inParagraph = true
				paragraphHasText = false
			}
		case xml.CharData:
			text := string(t)
			if charCount+len(text) > remaining {
				left := remaining - charCount
				if left > 0 {
					b.WriteString(text[:left])
				}
				return b.String(), true
			}
			b.WriteString(text)
			charCount += len(text)
			if strings.TrimSpace(text) != "" {
				paragraphHasText = true
			}
		}
	}

	return b.String(), false
}
