package caveman

import (
	"fmt"
	"strings"
	"testing"
)

// buildJSON returns a pretty-printed array of uniform objects large enough to
// clear minCompressChars. Pretty-printing is the realistic producer shape
// (shell tool output, API dumps) and is where indentation waste lives.
func buildJSON(rows int) string {
	var b strings.Builder
	b.WriteString("{\n    \"results\": [\n")
	for i := 0; i < rows; i++ {
		b.WriteString(fmt.Sprintf(
			"        {\"id\": %d, \"name\": \"item-%03d\", \"status\": \"ok\", \"score\": 0.%02d}",
			i, i, i%100))
		if i < rows-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("    ],\n    \"count\": 0\n}")
	return b.String()
}

var jsonPayload = buildJSON(60)

// repeatedText: realistic shell output — a header, thousands of identical
// progress lines interleaved with blanks, and a signal line at the end.
var repeatedText = "2026-11-19 12:00:00 [INFO] starting scan of /usr/simplify\n" +
	strings.Repeat("2026-11-19 12:00:03 [INFO] scanning item 100% done\n\n", 120) +
	"2026-11-19 12:05:00 [ERROR] build failed: exit status 2\n"

var logPayload = strings.Repeat("2026-11-19 12:00:00 [INFO] heartbeat tick\n", 60) +
	strings.Repeat("\n", 20) + "2026-11-19 12:05:00 [ERROR] build failed: exit status 2\n"

func TestPayloadsClearTheFloor(t *testing.T) {
	if len(jsonPayload) < minCompressChars || len(logPayload) < minCompressChars {
		t.Fatalf("fixtures too small: json=%d log=%d floor=%d", len(jsonPayload), len(logPayload), minCompressChars)
	}
}

func TestToolProjectionIsDeterministic(t *testing.T) {
	for _, in := range []string{jsonPayload, logPayload, repeatedText} {
		a := ToolProjection(in)
		if a == "" {
			t.Fatalf("expected a projection, got none (len=%d)", len(in))
		}
		for i := 0; i < 25; i++ {
			if b := ToolProjection(in); b != a {
				t.Fatalf("projection not deterministic on run %d", i)
			}
		}
		if len(a) >= len(in) {
			t.Fatalf("projection did not shrink: %d -> %d", len(in), len(a))
		}
	}
}

// The whole point: the projection is stored at write time, so the same stored
// message must render byte-identically on every later turn (KV prompt-cache
// prefix stability).
func TestProjectionByteStableAcrossRenders(t *testing.T) {
	proj := ToolProjection(jsonPayload)
	render := func() string { return Banner(len(jsonPayload), len(proj), "call_42") + "\n" + proj }
	first := render()
	for i := 0; i < 50; i++ {
		if render() != first {
			t.Fatal("render changed between turns")
		}
	}
}

func TestSmallResultsAreLeftAlone(t *testing.T) {
	if p := ToolProjection(`{"ok":true}`); p != "" {
		t.Fatalf("small result should not compress, got %q", p)
	}
}

func TestJSONShrinksSubstantially(t *testing.T) {
	p := ToolProjection(jsonPayload)
	if len(p) > len(jsonPayload)/2 {
		t.Fatalf("expected >50%% reduction, got %d -> %d", len(jsonPayload), len(p))
	}
	if !strings.Contains(p, `"cols"`) {
		t.Fatalf("expected columnar encoding, got %q", p)
	}
	// Data must survive — compression may not drop rows.
	if !strings.Contains(p, "item-042") || !strings.Contains(p, "item-000") {
		t.Fatal("projection dropped payload rows")
	}
}

func TestLogCompressionCollapsesRepeats(t *testing.T) {
	p := ToolProjection(logPayload)
	if !strings.Contains(p, "^ x") {
		t.Fatalf("expected global repeat collapse, got %q", tail(p, 200))
	}
	if !strings.Contains(p, "build failed") {
		t.Fatal("projection dropped the signal line")
	}
	if strings.Count(p, "heartbeat tick") > 1 {
		t.Fatal("repeated log lines were not collapsed")
	}
}

// Non-adjacent repeats are the real-world shape (43% of bytes in the largest
// live payloads) — adjacent-only RLE misses them.
func TestNonAdjacentRepeatsAreCollapsed(t *testing.T) {
	var b strings.Builder
	b.WriteString("INFO scanning /usr/simplify/path/alpha\nINFO scanning /usr/simplify/path/beta\nINFO scanning /usr/simplify/path/gamma\n")
	for i := 0; i < 200; i++ {
		b.WriteString("INFO scanning /usr/simplify/path/alpha\nINFO scanning /usr/simplify/path/beta\nINFO scanning /usr/simplify/path/gamma\n")
	}
	in := b.String()
	p := ToolProjection(in)
	if p == "" {
		t.Fatal("expected projection of repeating pattern")
	}
	for _, l := range []string{"INFO scanning /usr/simplify/path/alpha", "INFO scanning /usr/simplify/path/beta", "INFO scanning /usr/simplify/path/gamma"} {
		if n := strings.Count(p, l); n != 1 {
			t.Fatalf("%s kept %d times, want 1 (first occurrence only)", l, n)
		}
	}
}

// Code safety: a line repeated exactly twice must survive, since in source a
// second `}` or `return` is meaningful.
func TestCodeTwiceRepeatedLinesSurvive(t *testing.T) {
	var b strings.Builder
	b.WriteString("func main() {\n\tif x {\n\t\treturn\n\t}\n")
	b.WriteString("\treturn\n}\n")
	for i := 0; i < 100; i++ {
		b.WriteString("// padding comment to clear the floor size gate\n")
	}
	in := b.String()
	p := ToolProjection(in)
	if n := strings.Count(p, "return"); n != 2 {
		t.Fatalf("twice-repeated code line collapsed: count=%d (want 2)", n)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestANSIEscapesAreStripped(t *testing.T) {
	in := "\x1b[31mFAIL\x1b[0m " + strings.Repeat("padding padding padding\n", 100)
	p := ToolProjection(in)
	if strings.Contains(p, "\x1b") {
		t.Fatal("ANSI escape survived into the projection")
	}
}

func TestBannerIsNonDestructivePointer(t *testing.T) {
	b := Banner(5000, 400, "call_17")
	for _, want := range []string{"5000", "400", "tool_call_id=\"call_17\"", "caveman_retrieve"} {
		if !strings.Contains(b, want) {
			t.Fatalf("banner missing %q: %q", want, b)
		}
	}
}

func TestSafeTruncateOnRuneBoundary(t *testing.T) {
	s := "héllo wörld ünïcode " + strings.Repeat("z", 500)
	out := SafeTruncate(s, 20)
	if !strings.Contains(out, "caveman_retrieve") {
		t.Fatal("truncate lost the recovery hint")
	}
	if strings.ContainsRune(out, 0xFFFD) {
		t.Fatal("truncate split a multi-byte rune")
	}
}
