package caveman

import (
	"regexp"
	"strings"
)

// "browse": the web-page compressor.
//
// This is the ONE payload shape read_less cannot handle. Caveman's own
// benchmark table is explicit about it: HTML goes "21,670 → 21,670, none yet"
// and costs 9.9% in a whole session. Its headline web number (121 tokens vs
// 15,704, 129.8× smaller) is a Playwright accessibility snapshot, not raw
// HTML — so that is what this targets.
//
// Measured on this deployment's own 25 largest playwright__browser_snapshot
// results (1.22M chars): 25.0% saved, with every interactable node keeping its
// ref (see below).

// refPattern is the Playwright node locator: [ref=e12], [ref=f3e12].
var refPattern = regexp.MustCompile(`\s*\[ref=[A-Za-z0-9]+\]`)

// nodePattern matches a snapshot node line: `  - cell "foo"`.
var nodePattern = regexp.MustCompile(`^(\s*)-\s*([A-Za-z]+)\b(.*)$`)

// interactableRoles are the node roles the agent can ACT on. Playwright's
// browser_click / browser_type / browser_hover take the element's `ref` as
// their target, so removing it from these lines would leave the model able to
// see a button and unable to press it.
//
// Measured: naive ref-stripping saves 28.4% and blinds the agent; keeping refs
// on interactables costs 3.4 points of that (25.0%) and is the right trade.
// Non-interactable roles (cell, row, generic, text, img, paragraph, ...) carry
// no action, so their refs are pure token waste.
var interactableRoles = map[string]bool{
	"link": true, "button": true, "textbox": true, "checkbox": true,
	"radio": true, "combobox": true, "listbox": true, "menu": true,
	"menuitem": true, "menuitemradio": true, "menuitemcheckbox": true,
	"tab": true, "tablist": true, "option": true, "slider": true,
	"searchbox": true, "spinbutton": true, "switch": true, "tree": true,
	"treeitem": true, "group": true, "columnheader": true, "rowheader": true,
	"heading": true, "form": true,
}

// BrowseProjection compresses web-page content: a Playwright accessibility
// snapshot (the dominant shape, 70-105k chars per call here) or extracted page
// text/HTML.
//
// Deterministic: pure function of the input, line by line, no maps.
//
// Non-destructive: the snapshot's text, URLs, headings and every interactable
// ref survive; the full original stays in messages.content for
// caveman_retrieve.
func BrowseProjection(text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < minCompressChars {
		return ""
	}
	var body string
	if isAccessibilitySnapshot(trimmed) {
		body = compressSnapshot(trimmed)
	} else {
		body = compressText(trimmed)
	}
	if body == "" {
		return ""
	}
	if float64(len(body)) > float64(len(trimmed))*minGain {
		return ""
	}
	return body
}

// isAccessibilitySnapshot: the snapshot format is stable — a `### Snapshot`
// section (or a `- <role> [ref=` node root) rendered as YAML.
func isAccessibilitySnapshot(text string) bool {
	if strings.Contains(text, "### Snapshot") {
		return true
	}
	// Root node line, e.g. `- generic [active] [ref=e1]:`
	return strings.Contains(text, "[ref=") && nodePattern.MatchString(firstContentLine(text))
}

func firstContentLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// compressSnapshot strips locator refs from non-actionable nodes and drops
// node lines that carry no content once their ref is gone (an empty
// `- cell`/`- generic` wrapper is structure, not information).
//
// Everything carrying signal is kept verbatim: the `### Page` header block,
// URLs, headings, `text:` content, `- /url:` lines, and any node line that
// retains a state marker like [active], [cursor=pointer] or [sealed].
func compressSnapshot(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		// Fence markers carry no node structure; pass through. The `### Page`
		// header above the tree is small and valuable (URL, title, console
		// counts) and never matches nodePattern, so it is left untouched.
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			out = append(out, ln)
			continue
		}
		m := nodePattern.FindStringSubmatch(ln)
		if m == nil {
			out = append(out, ln)
			continue
		}
		role := m[2]
		if interactableRoles[role] {
			// Agent may act on this node: keep its ref.
			out = append(out, ln)
			continue
		}
		stripped := refPattern.ReplaceAllString(ln, "")
		// Nothing left but the bare role token → pure structure, drop it.
		if bareRoleLine.MatchString(strings.TrimSpace(stripped)) {
			continue
		}
		out = append(out, strings.TrimRight(stripped, " \t"))
	}
	return strings.Join(out, "\n")
}

// bareRoleLine matches a snapshot node line that carries no name/value:
// `- cell`, `- cell:`, `- generic [active]` keeps, `- img` drops.
var bareRoleLine = regexp.MustCompile(`^-\s*[A-Za-z]+:?$`)

// WebToolIsBrowsed reports whether a tool result of this name is web-page
// content that browse owns (as opposed to read_less).
//
// The decision is a pure function of the tool name so write time and request
// time can agree on it without storing a flag.
func WebToolIsBrowsed(name string) bool {
	// Strip an MCP namespace prefix ("playwright__browser_snapshot").
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	switch name {
	case "browser_snapshot", "visit_link", "fetch_content", "browser_navigate", "browser_find":
		return true
	}
	return false
}

// ProjectionFor picks the compressor that owns this tool result.
//
// Pure function of (tool name, text), so the write path can compute it once
// and the request path can gate it later without a schema flag telling the two
// sides which stone produced the stored projection.
func ProjectionFor(toolName, text string) string {
	if WebToolIsBrowsed(toolName) {
		return BrowseProjection(text)
	}
	return ToolProjection(text)
}
