package docsupport

import (
	"fmt"
	"regexp"
	"strings"
)

// searchText runs a regex against plain text and returns matches with context.
func searchText(text, pattern string, maxResults int) ([]SearchMatch, error) {
	if maxResults <= 0 {
		maxResults = 50
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	locs := re.FindAllStringIndex(text, -1)
	var matches []SearchMatch

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
			Match:   text[loc[0]:loc[1]],
			Context: strings.TrimSpace(text[ctxStart:ctxEnd]),
		})

		if len(matches) >= maxResults {
			break
		}
	}

	return matches, nil
}
