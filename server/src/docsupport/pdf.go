package docsupport

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ledongthuc/pdf"
)

// ParsePDF extracts text from a PDF byte slice.
//
// pages selects which pages to extract: "" for all, "3" for a single page,
// "1-5" for a range. maxChars truncates the output (0 = default 10000).
func ParsePDF(data []byte, pages string, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 10000
	}

	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("opening PDF: %w", err)
	}

	totalPages := reader.NumPage()
	if totalPages == 0 {
		return "", fmt.Errorf("PDF has no pages")
	}

	from, to, err := parsePageRange(pages, totalPages)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	charCount := 0

	for i := from; i <= to; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}

		text, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}

		header := fmt.Sprintf("--- Page %d ---\n", i)
		b.WriteString(header)
		charCount += len(header)

		if charCount+len(text) > maxChars {
			remaining := maxChars - charCount
			if remaining > 0 {
				b.WriteString(text[:remaining])
			}
			b.WriteString("\n[truncated]")
			break
		}

		b.WriteString(text)
		b.WriteString("\n")
		charCount += len(text) + 1
	}

	result := strings.TrimSpace(b.String())
	if result == "" {
		return "", fmt.Errorf("no text could be extracted from this PDF (it may be scanned/image-based)")
	}

	return result, nil
}

// SearchPDF runs a regex pattern against all pages of a PDF and returns matches
// with page numbers and surrounding context.
func SearchPDF(data []byte, pattern string, maxResults int) ([]SearchMatch, error) {
	if maxResults <= 0 {
		maxResults = 50
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("opening PDF: %w", err)
	}

	totalPages := reader.NumPage()
	if totalPages == 0 {
		return nil, fmt.Errorf("PDF has no pages")
	}

	var matches []SearchMatch

	for i := 1; i <= totalPages; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}

		text, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}

		locs := re.FindAllStringIndex(text, -1)
		for _, loc := range locs {
			ctxStart := loc[0] - 80
			if ctxStart < 0 {
				ctxStart = 0
			}
			ctxEnd := loc[1] + 80
			if ctxEnd > len(text) {
				ctxEnd = len(text)
			}

			matches = append(matches, SearchMatch{
				Page:    i,
				Match:   text[loc[0]:loc[1]],
				Context: strings.TrimSpace(text[ctxStart:ctxEnd]),
			})

			if len(matches) >= maxResults {
				return matches, nil
			}
		}
	}

	return matches, nil
}

// parsePageRange interprets a page selection string.
// Returns 1-based from/to inclusive.
func parsePageRange(pages string, total int) (int, int, error) {
	pages = strings.TrimSpace(pages)
	if pages == "" {
		return 1, total, nil
	}

	if strings.Contains(pages, "-") {
		parts := strings.SplitN(pages, "-", 2)
		from, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid page range start: %s", parts[0])
		}
		to, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid page range end: %s", parts[1])
		}
		if from < 1 {
			from = 1
		}
		if to > total {
			to = total
		}
		if from > to {
			return 0, 0, fmt.Errorf("invalid page range: %d-%d (PDF has %d pages)", from, to, total)
		}
		return from, to, nil
	}

	page, err := strconv.Atoi(pages)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid page number: %s", pages)
	}
	if page < 1 || page > total {
		return 0, 0, fmt.Errorf("page %d out of range (PDF has %d pages)", page, total)
	}
	return page, page, nil
}
