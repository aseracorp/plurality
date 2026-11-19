// Package caveman implements Plurality's own "read less" compression, the
// deterministic projection of tool results that is shipped to the LLM while
// the full original stays in the database.
//
// Why not the caveman-proxy: the in-container proxy is a transparent
// forwarder. Per the caveman middleware contract, an entry point that cannot
// bind its own recovery tool reports `recovery_unbound` and forwards content
// UNCHANGED in `compress` mode. Measured on this instance: identical provider
// prompt_tokens through the proxy and direct LiteLLM (pt 736==736 at 5KB,
// 12896==12896 at 100KB, 196842==196842 at 469KB) with zero middleware_plans
// ever written — while adding 3.9-108s to every request.
//
// So compression happens here, ONCE, at write time:
//
//   - the tool result is stored in the DB exactly as the tool produced it
//     (chat display, audit, and the recovery source for caveman_retrieve),
//   - a deterministic projection is stored in messages.compressed_content,
//   - the request builder prefers that projection, for LLM requests only.
//
// Byte-stability (KV prompt caching): the projection is a pure function of the
// stored content, computed once at write time and never recomputed, so a
// message's rendered form can never drift between turns. No timestamps, no
// random ids, no map iteration order.
//
// Non-destructive: the full original is always reachable via caveman_retrieve.
package caveman

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// minCompressChars: results below this are shipped verbatim. Small tool
// outputs are not worth a projection, and leaving them untouched preserves
// cache prefixes on short conversations.
const minCompressChars = 1200

// minGain: a projection is kept only if it shrinks the text by at least this
// fraction, so the banner's own cost can never make the request bigger.
const minGain = 0.85

// ToolProjection builds the LLM-facing projection of a tool result.
// Returns "" when compression is not applicable or would not pay for itself.
//
// Deterministic: identical input yields byte-identical output, forever.
func ToolProjection(text string) string {
	text = strings.TrimSpace(text)
	if len(text) < minCompressChars {
		return ""
	}
	var body string
	switch {
	case looksLikeJSON(text):
		body = compressJSON(text)
	case looksLikeLog(text):
		body = compressLog(text)
	default:
		body = compressText(text)
	}
	if body == "" {
		return ""
	}
	// Never let the banner push us over the original size.
	if float64(len(body)) > float64(len(text))*minGain {
		return ""
	}
	return body
}

// Banner tells the model the body is compressed and exactly how to get the
// full original back. It carries nothing per-request (no clock, no random id),
// so it stays byte-stable like the projection it fronts.
//
// The recovery key is the tool_call_id, NOT the message seq: inserting an eco
// checkpoint shifts existing rows by +2, so a baked-in seq would silently
// point at the wrong message after compaction. tool_call_id never moves.
func Banner(origChars, projChars int, toolCallID string) string {
	return fmt.Sprintf(
		"[caveman: compressed tool result, %d chars → %d. Full original: call caveman_retrieve with tool_call_id=%q.]",
		origChars, projChars, toolCallID,
	)
}

// ---- JSON ----

// looksLikeJSON: first non-space char opens an object/array and the last char
// closes one.
func looksLikeJSON(text string) bool {
	for _, r := range text {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			continue
		}
		last := text[len(text)-1]
		return (r == '{' || r == '[') && (last == '}' || last == ']')
	}
	return false
}

// jnode is an ordered JSON value. Objects keep insertion order, so encoding is
// deterministic and preserves the producer's key order.
type jnode struct {
	kind    byte // 'o' object, 'a' array, 's' string, 'n' number/literal, 'z' null
	kvs     []jkv
	items   []jnode
	str     string
	literal string
}

type jkv struct {
	key string
	val jnode
}

func parseJNode(dec *json.Decoder) (jnode, bool) {
	tok, err := dec.Token()
	if err != nil {
		return jnode{}, false
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			n := jnode{kind: 'o'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return jnode{}, false
				}
				key, ok := kt.(string)
				if !ok {
					return jnode{}, false
				}
				v, ok := parseJNode(dec)
				if !ok {
					return jnode{}, false
				}
				n.kvs = append(n.kvs, jkv{key: key, val: v})
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return jnode{}, false
			}
			return n, true
		}
		if t == '[' {
			n := jnode{kind: 'a'}
			for dec.More() {
				v, ok := parseJNode(dec)
				if !ok {
					return jnode{}, false
				}
				n.items = append(n.items, v)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return jnode{}, false
			}
			return n, true
		}
		return jnode{}, false
	case string:
		return jnode{kind: 's', str: t}, true
	case bool:
		return jnode{kind: 'n', literal: strconv.FormatBool(t)}, true
	case json.Number:
		// UseNumber() keeps the original literal — no float round-trip drift.
		return jnode{kind: 'n', literal: t.String()}, true
	case nil:
		return jnode{kind: 'z'}, true
	}
	return jnode{}, false
}

func parseJRoot(text string) (jnode, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	n, ok := parseJNode(dec)
	if !ok {
		return jnode{}, false
	}
	return n, true
}

func (n jnode) marshal(b *strings.Builder) {
	switch n.kind {
	case 'o':
		b.WriteByte('{')
		for i, kv := range n.kvs {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(kv.key))
			b.WriteByte(':')
			kv.val.marshal(b)
		}
		b.WriteByte('}')
	case 'a':
		b.WriteByte('[')
		for i, it := range n.items {
			if i > 0 {
				b.WriteByte(',')
			}
			it.marshal(b)
		}
		b.WriteByte(']')
	case 's':
		b.WriteString(strconv.Quote(n.str))
	case 'z':
		b.WriteString("null")
	default:
		b.WriteString(n.literal)
	}
}

// compressJSON: minify first (pretty-printed JSON loses 40-60% to indentation
// alone), then columnar-encode arrays of uniform objects — the benchmark shape
// in caveman's own docs (CSV/JSON at 98.5-98.9%).
func compressJSON(text string) string {
	root, ok := parseJRoot(text)
	if !ok {
		var buf bytes.Buffer
		if err := json.Compact(&buf, []byte(text)); err != nil {
			return ""
		}
		return buf.String()
	}
	var b strings.Builder
	jsonColumnar(&b, root)
	return b.String()
}

// jsonColumnar writes a compact JSON encoding, replacing an array whose
// elements are objects with one identical key sequence by a key header plus
// value rows. Deterministic: same input, same bytes.
func jsonColumnar(b *strings.Builder, n jnode) {
	if n.kind == 'a' {
		if keys, ok := uniformObjectKeys(n.items); ok {
			b.WriteString(`[{"cols":`)
			writeKeyHeader(b, keys)
			b.WriteString(`,"rows":[`)
			for i, it := range n.items {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteByte('[')
				for j, k := range keys {
					if j > 0 {
						b.WriteByte(',')
					}
					findKey(it, k).marshal(b)
				}
				b.WriteByte(']')
			}
			b.WriteString("]}")
			return
		}
	}
	if n.kind == 'o' {
		b.WriteByte('{')
		for i, kv := range n.kvs {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(kv.key))
			b.WriteByte(':')
			jsonColumnar(b, kv.val)
		}
		b.WriteByte('}')
		return
	}
	if n.kind == 'a' {
		b.WriteByte('[')
		for i, it := range n.items {
			if i > 0 {
				b.WriteByte(',')
			}
			jsonColumnar(b, it)
		}
		b.WriteByte(']')
		return
	}
	n.marshal(b)
}

func writeKeyHeader(b *strings.Builder, keys []string) {
	b.WriteByte('[')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(k))
	}
	b.WriteByte(']')
}

// uniformObjectKeys returns the shared key sequence when every element is an
// object with the identical ordered key list, else false.
func uniformObjectKeys(items []jnode) ([]string, bool) {
	if len(items) < 2 {
		return nil, false
	}
	base := items[0]
	if base.kind != 'o' {
		return nil, false
	}
	keys := make([]string, 0, len(base.kvs))
	for _, kv := range base.kvs {
		keys = append(keys, kv.key)
	}
	if len(keys) < 2 {
		return nil, false
	}
	for _, it := range items[1:] {
		if it.kind != 'o' || len(it.kvs) != len(keys) {
			return nil, false
		}
		for i, kv := range it.kvs {
			if kv.key != keys[i] {
				return nil, false
			}
		}
	}
	return keys, true
}

func findKey(n jnode, key string) jnode {
	for _, kv := range n.kvs {
		if kv.key == key {
			return kv.val
		}
	}
	return jnode{kind: 'z'}
}

// ---- logs / text ----

func looksLikeLog(text string) bool {
	return strings.Count(text, "\n") >= 8
}

// compressLog: drop ANSI, collapse blank runs, and de-duplicate repeated
// lines. Measured against a real corpus of this server's own stored tool
// results:43% of the bytes in the 40 largest payloads are lines that repeat
// NON-adjacently (progress spam, repeated status rows, stack frames). Adjacent
// run-length coding alone caught almost none of it, so dedup is global and
// keeps the FIRST occurrence in place.
//
// Deterministic: first-occurrence order is the input order, so output is a
// pure function of the input.
func compressLog(text string) string {
	return collapseLines(stripANSI(text))
}

// compressText: same line collapse for prose/code-ish payloads. It never
// rewrites non-whitespace bytes, so code, paths and numbers stay verbatim.
//
// The duplicate-line rule is deliberately conservative for code: a line is
// dropped only when it repeats AT LEAST 3 times, because in source code a
// twice-repeated `}` or `return` is meaningful and must not vanish. Log spam
// (>=3) is the shape worth collapsing.
func compressText(text string) string {
	return collapseLinesParam(stripANSI(text), 3)
}

func collapseLines(text string) string { return collapseLinesParam(text, 2) }

// collapseLinesParam trims trailing whitespace, collapses blank runs to a
// single marker, and replaces the 2nd+ occurrence of a repeated line with a
// compact back-reference. `minRepeat` is how many total occurrences justify
// collapsing.
//
// Safety: a line is only collapsed when it is at least minDedupLineLen chars
// long. Without that rule, `fs_read` on a source file (which lands here, since
// 8+ newlines routes to the log path) would collapse every repeated `}` and
// `return` — actively breaking coding agents, which is the opposite of safe
// compression. Long lines are the log-spam/JSON-row shape; short lines stay
// verbatim.
func collapseLinesParam(text string, minRepeat int) string {
	const minDedupLineLen = 24
	lines := strings.Split(text, "\n")
	counts := make(map[string]int, len(lines))
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" || len(ln) < minDedupLineLen {
			continue
		}
		counts[ln]++
	}
	seen := make(map[string]bool, len(counts))
	out := make([]string, 0, len(lines))
	blank := 0
	for _, raw := range lines {
		ln := strings.TrimRight(raw, " \t\r")
		if ln == "" {
			blank++
			// One marker per blank run, and never two markers back to back:
			// emitting one per run made blank-heavy payloads come out LARGER
			// than the original (the gain guard then refused to compress them
			// at all).
			if blank == 2 && (len(out) == 0 || out[len(out)-1] != "...") {
				out = append(out, "...")
			}
			continue
		}
		blank = 0
		if counts[ln] >= minRepeat {
			if seen[ln] {
				// Later occurrence: replace with a short back-reference so the
				// model can tell the line recurred without paying its bytes
				// again. First occurrence stays verbatim, in place.
				out = append(out, "^")
				continue
			}
			seen[ln] = true
		}
		out = append(out, ln)
	}
	return dedupeMarkers(strings.Join(out, "\n"))
}

// dedupeMarkers collapses runs of consecutive "^" back-references into one
// line with a count, so a burst of repeated lines costs ~3 bytes total.
func dedupeMarkers(s string) string {
	if !strings.Contains(s, "^") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	run := 0
	flush := func() {
		if run == 0 {
			return
		}
		if run == 1 {
			out = append(out, "^")
		} else {
			out = append(out, "^ x"+strconv.Itoa(run))
		}
		run = 0
	}
	for _, ln := range lines {
		if ln == "^" {
			run++
			continue
		}
		flush()
		out = append(out, ln)
	}
	flush()
	return strings.Join(out, "\n")
}

func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			for j < len(s) && !(s[j] >= 'a' && s[j] <= 'z') {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// SafeTruncate is the last-resort single-message bound: cut on a rune
// boundary and state exactly how much is missing, so the model can ask for the
// rest with caveman_retrieve instead of guessing.
func SafeTruncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf(
		"\n[caveman: %d of %d chars elided — full text via caveman_retrieve]",
		len(s)-cut, len(s),
	)
}
