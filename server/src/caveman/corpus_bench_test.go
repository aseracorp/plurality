package caveman

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// TestCorpusBenchmark measures the compressor against a corpus of REAL stored
// tool results (a JSON file of {"chars","text"} rows). It is a measurement
// harness, not an assertion: it skips when no corpus is present, so CI is
// unaffected.
//
// Build a corpus from a live deployment with:
//
//	SELECT content FROM messages WHERE role='tool' AND length(content)>1200
//
// (decode the message-content JSON to plain text), write it to
// /tmp/tool_corpus.json, then:
//
//	go test -run CorpusBenchmark -v ./caveman/
//
// Documented result on this deployment's own 1000 largest tool results
// (73.4M chars): 9.1% aggregate saving, ~0.62 median per-message ratio, up to
// 77% on the most repetitive payloads. Aggregate is modest because the corpus
// is dominated by pretty-printed JSON config dumps and large code reads, where
// the safety floors deliberately leave content intact.
func TestCorpusBenchmark(t *testing.T) {
	b, err := os.ReadFile("/tmp/tool_corpus.json")
	if err != nil {
		t.Skip("no corpus at /tmp/tool_corpus.json")
	}
	var rows []struct {
		Chars int    `json:"chars"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	var orig, comp, compressed, skipped int
	var ratios []float64
	type res struct{ o, c int }
	var top []res
	for _, r := range rows {
		p := ToolProjection(r.Text)
		if p == "" {
			skipped++
			orig += len(r.Text)
			comp += len(r.Text)
			continue
		}
		compressed++
		orig += len(r.Text)
		comp += len(p)
		ratios = append(ratios, float64(len(p))/float64(len(r.Text)))
		top = append(top, res{len(r.Text), len(p)})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].o > top[j].o })
	t.Logf("messages: %d  compressed: %d  untouched: %d", len(rows), compressed, skipped)
	t.Logf("chars: %d -> %d (%.1f%% saved)", orig, comp, 100*(1-float64(comp)/float64(orig)))
	sort.Float64s(ratios)
	if len(ratios) > 0 {
		t.Logf("per-msg ratio (proj/orig): median %.3f p90 %.3f best %.3f",
			ratios[len(ratios)/2], ratios[int(float64(len(ratios))*0.9)], ratios[0])
	}
	for i := 0; i < 8 && i < len(top); i++ {
		t.Logf("  top: %7d -> %7d (%.1f%%)", top[i].o, top[i].c, 100*(1-float64(top[i].c)/float64(top[i].o)))
	}
	t.Logf("approx tokens (@4 chars): %d -> %d, saved ~%d", orig/4, comp/4, (orig-comp)/4)
}
