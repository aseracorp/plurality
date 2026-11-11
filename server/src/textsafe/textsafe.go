// Package textsafe provides truncation helpers that always cut on a rune
// boundary.
//
// Go strings are UTF-8. Slicing a string at an arbitrary byte index — s[:n],
// s[len(s)-n:] — can split a multi-byte character in half and return invalid
// UTF-8. encoding/json then replaces the broken bytes with U+FFFD (""), so
// API responses turn "Größe" into "Gr??e" and a truncated astral emoji into
// replacement-character garbage. On the Dart/Flutter client the damage is
// worse: a half-emitted astral character becomes a lone surrogate, and
// rendering it (TextPainter / RenderParagraph / a platform text codec) can
// throw and crash the widget.
//
// Every function here guarantees the returned string starts and ends on a
// rune boundary, so it is safe to embed in a JSON payload and safe to render.
package textsafe

import "unicode/utf8"

// HeadBytes returns at most n BYTES of s, cut on a rune boundary.
//
// It is the maximal prefix of s whose byte length is <= n: the cut index is
// walked backwards over UTF-8 continuation bytes (0x80..0xBF) until it lands
// on a rune start, so complete runes are never dropped. This is what callers
// need when n is a byte budget (log lines, tool-output caps, storage columns)
// and the value goes straight into a JSON string.
//
// Prevents: s[:n] slicing through a multi-byte rune, which yields invalid
// UTF-8 that encoding/json re-encodes as U+FFFD, and which the Dart side can
// turn into a lone surrogate that crashes text rendering.
//
// n <= 0 returns "". If len(s) <= n, s is returned unchanged (no copy).
//
// Invalid input is not repaired: if s already contains invalid UTF-8, the
// result is the maximal prefix whose cut index is a rune-start byte.
func HeadBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// s[:n] ends at byte index n-1; for the prefix to end on a rune boundary,
	// s[n] must be the first byte of a rune. Walk back while it is a
	// continuation byte. Slicing does not allocate.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// TailBytes returns the last n BYTES of s, cut on a rune boundary.
//
// It is the maximal suffix of s whose byte length is <= n: the start index is
// walked forward over UTF-8 continuation bytes (0x80..0xBF) using
// utf8.RuneStart until it lands on a rune start, so the tail never begins
// with a partial rune. Use this for byte-budgeted suffixes such as
// keep-the-end-of-a-log-tail, where an invalid leading byte would poison the
// whole JSON field.
//
// Prevents: s[len(s)-n:] slicing into the middle of a multi-byte rune, which
// yields invalid UTF-8 that encoding/json re-encodes as U+FFFD, and which the
// Dart side can turn into a lone surrogate that crashes text rendering.
//
// n <= 0 returns "". If len(s) <= n, s is returned unchanged (no copy).
//
// Invalid input is not repaired: if s already contains invalid UTF-8, the
// result is the maximal suffix whose start index is a rune-start byte (which
// can be the empty string when the whole budget falls inside one rune).
func TailBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	// Skip forward past continuation bytes: the suffix must start at a rune
	// start. Slicing does not allocate.
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// Head returns at most n RUNES of s — use it when n means "characters"
// (display strings, titles, previews, one-line summaries).
//
// It iterates s with `for i := range s` (rune starts) and cuts at the byte
// offset of the n-th rune, so the result is always valid UTF-8.
//
// Prevents: s[:n] byte-slicing on a display budget, which silently chops an
// umlaut, an em-dash, a CJK character or an emoji in half — the JSON response
// shows U+FFFD where the character used to be, and on the Dart side a
// half-cut astral character leaves a lone surrogate that crashes text
// rendering.
//
// n <= 0 returns "". If len(s) <= n, s is returned unchanged (cheap check:
// len(s) bytes <= n implies at most n runes).
func Head(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	// Fewer than n runes in s (byte length > n, so this is a multibyte input).
	return s
}

// Tail returns the last n RUNES of s, cut on a rune boundary — for
// "…last 40 characters" style suffixes.
//
// It decodes runes from the end with utf8.DecodeLastRuneInString, so the cost
// is proportional to n rather than to len(s), and the returned suffix always
// starts on a rune boundary.
//
// Prevents: s[len(s)-n:] byte-slicing on a character budget, which produces
// invalid UTF-8 that encoding/json re-encodes as U+FFFD and that the Dart
// side can turn into a lone surrogate that crashes text rendering.
//
// n <= 0 returns "". If n is at least the rune count of s (checked cheaply
// first with len(s) <= n, then with utf8.RuneCountInString), s is returned
// unchanged.
//
// Invalid input is not repaired: each invalid byte decodes as one rune of
// size 1, so the walk is well defined and never panics.
func Tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		// len(s) bytes <= n implies at most n runes.
		return s
	}
	total := utf8.RuneCountInString(s)
	if n >= total {
		return s
	}
	i := len(s)
	for k := 0; k < n; k++ {
		// Each iteration consumes >= 1 byte (invalid bytes decode as size 1),
		// and n < total guarantees i stays > 0, so s[:i] never goes negative.
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}
