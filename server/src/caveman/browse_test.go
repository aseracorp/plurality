package caveman

import (
	"fmt"
	"strings"
	"testing"
)

// A trimmed-down real playwright__browser_snapshot (shape taken verbatim from
// this deployment's DB: ### Page header + YAML node tree of a table).
var snapshotFixture = strings.Repeat("### Page\n- Page URL: https://www.asera.ch/produkte\n"+
	"- Page Title: Produkte | Asera\n### Snapshot\n```yaml\n"+
	"- generic [active] [ref=e1]:\n"+
	"  - navigation \"Hauptnavigation\" [ref=e2]:\n"+
	"    - link \"Start\" [ref=e3]:\n      - /url: /\n", 40) +
	strings.Repeat(
		"  - row [ref=e9]:\n"+
			"    - cell [ref=e10]\n"+
			"    - cell \"C26361-K644-B520\" [ref=e11]:\n"+
			"      - link \"Tower Server Front Cover\" [ref=e12] [cursor=pointer]:\n"+
			"        - /url: detail.htm?id=12\n"+
			"    - cell \"38044171\" [ref=e13]\n"+
			"  - cell [ref=e14]\n"+
			"  - generic [ref=e15]:\n"+
			"    - paragraph [ref=e16]: Description text goes here\n", 120)

func TestBrowseCompressesSnapshot(t *testing.T) {
	if len(snapshotFixture) < minCompressChars {
		t.Fatalf("fixture too small: %d", len(snapshotFixture))
	}
	p := BrowseProjection(snapshotFixture)
	if p == "" {
		t.Fatal("expected a browse projection")
	}
	if len(p) >= len(snapshotFixture) {
		t.Fatalf("no saving: %d -> %d", len(snapshotFixture), len(p))
	}
	// Content signal must survive.
	for _, want := range []string{
		"Page URL: https://www.asera.ch/produkte",
		"Produkte | Asera",
		"C26361-K644-B520",
		"Tower Server Front Cover",
		"detail.htm?id=12",
		"Description text goes here",
		"Hauptnavigation",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("browse projection dropped content: %q", want)
		}
	}
}

// The safety-critical rule: refs on INTERACTABLE nodes must survive, because
// browser_click/browser_type take the ref as their target. Stripping them
// saves ~3 points more and leaves the agent unable to act.
func TestBrowseKeepsInteractableRefs(t *testing.T) {
	p := BrowseProjection(snapshotFixture)
	for _, want := range []string{
		`link "Start" [ref=e3]`,
		`link "Tower Server Front Cover" [ref=e12] [cursor=pointer]`,
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("interactable ref was stripped, agent could not click: %q", want)
		}
	}
	// Containers keep their LABEL (that's content) and lose their ref (that's
	// a locator for a node you cannot act on — you click the link inside).
	if !strings.Contains(p, `navigation "Hauptnavigation"`) {
		t.Fatal("navigation label lost")
	}
	if strings.Contains(p, `[ref=e2]`) {
		t.Fatal("container ref kept (token waste)")
	}
	// Non-interactable refs (cells, rows, generic wrappers) are pure waste.
	for _, bad := range []string{"[ref=e10]", "[ref=e13]", "[ref=e14]", "[ref=e15]", "[ref=e16]"} {
		if strings.Contains(p, bad) {
			t.Fatalf("non-interactable ref kept: %s", bad)
		}
	}
}

// Empty structural nodes (a cell with no name) carry no information and go.
func TestBrowseDropsEmptyStructure(t *testing.T) {
	p := BrowseProjection(snapshotFixture)
	if strings.Contains(p, "- cell [ref=e10]") {
		t.Fatal("empty cell wrapper retained")
	}
	if strings.Count(p, "- cell\n") > 0 {
		t.Fatal("bare cell lines retained")
	}
}

func TestBrowseIsDeterministic(t *testing.T) {
	a := BrowseProjection(snapshotFixture)
	for i := 0; i < 25; i++ {
		if b := BrowseProjection(snapshotFixture); b != a {
			t.Fatal("browse projection not deterministic")
		}
	}
}

func TestBrowseIgnoresSmallAndNonWeb(t *testing.T) {
	if p := BrowseProjection("tiny page"); p != "" {
		t.Fatalf("small input should not compress: %q", p)
	}
}

func TestWebToolIsBrowsed(t *testing.T) {
	yes := []string{
		"playwright__browser_snapshot",
		"visit_link",
		"mcp__fetch_content",
		"browser_navigate",
	}
	for _, n := range yes {
		if !WebToolIsBrowsed(n) {
			t.Fatalf("%s should be browsed", n)
		}
	}
	no := []string{"system_tools__shell_exec", "search_web", "fs_read", "searxng__search", ""}
	for _, n := range no {
		if WebToolIsBrowsed(n) {
			t.Fatalf("%s should NOT be browsed", n)
		}
	}
}

// The "never worse" guarantee. Caveman's own benchmark table lists HTML as
// "21,670 -> 21,670, none yet" and a whole session 9.9% WORSE: its proxy pays
// overhead and gains nothing on pages it cannot compress.
//
// The Go side cannot lose, because BrowseProjection is total over this
// invariant: it returns "" (no projection stored, raw ships) OR a projection
// at most minGain of the payload, so the banner plus projection is always
// strictly smaller than the raw body it replaces — even at the size floor.
func TestBrowseNeverMakesPayloadsWorse(t *testing.T) {
	var prose strings.Builder
	prose.WriteString("## Content from https://example.com/article\n\n# Title\n\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&prose, "Paragraph %d explains a distinct idea in prose, uniquely worded. See [detail](https://example.com/l%d) for more.\n\n", i, i)
	}

	fixtures := map[string]string{
		"extracted-prose": prose.String(),
		"snapshot-table":  snapshotFixture,
		"near-floor":      strings.Repeat("- cell [ref=e7]\n", 90) + "- link \"x\" [ref=e8]:\n  - /url: /a\n",
		"tiny":            "short",
		"no-structure":    strings.Repeat("unique sentence with no repeats. ", 400),
		"raw-html":        "<html><body>" + strings.Repeat("<div class=\"row\"><span>item</span></div>", 500) + "</body></html>",
	}
	for name, body := range fixtures {
		proj := BrowseProjection(body)
		if proj == "" {
			continue // gain guard rejected it: the raw body ships, never worse
		}
		full := BannerFor("playwright__browser_snapshot", len(body), len(proj), "call_x") + "\n" + proj
		if len(full) >= len(body) {
			t.Fatalf("%s: projected request is not smaller than raw: %d >= %d", name, len(full), len(body))
		}
		t.Logf("%s: %d -> %d (%.1f%% smaller, banner included)", name, len(body), len(full),
			100*(1-float64(len(full))/float64(len(body))))
	}
}
