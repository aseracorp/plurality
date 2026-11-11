package textsafe

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// assertValid is the core property of this package: every returned string must
// be valid UTF-8, so it JSON-marshals byte-for-byte and never reaches the
// Dart/Flutter client as U+FFFD or a lone surrogate.
//
// It is applied to results derived from WELL-FORMED input. Inputs that are
// already invalid UTF-8 are covered separately (see
// TestInvalidUTF8InputDoesNotPanic) — no truncation helper can make an
// already-invalid string valid.
func assertValid(t *testing.T, label string, got string) {
	t.Helper()
	if !utf8.ValidString(got) {
		t.Errorf("%s: result is NOT valid UTF-8: %q (% x)", label, got, []byte(got))
	}
}

func TestHeadBytesASCII(t *testing.T) {
	const s = "hello world" // 11 bytes, all ASCII
	cases := []struct {
		n    int
		want string
	}{
		{0, ""},
		{-1, ""},
		{-100, ""},
		{1, "h"},
		{5, "hello"},
		{6, "hello "},
		{11, "hello world"},
		{12, "hello world"},
		{50, "hello world"},
	}
	for _, c := range cases {
		got := HeadBytes(s, c.n)
		assertValid(t, "HeadBytes", got)
		if got != c.want {
			t.Errorf("HeadBytes(%q, %d) = %q, want %q", s, c.n, got, c.want)
		}
		if len(got) > c.n && c.n > 0 {
			t.Errorf("HeadBytes(%q, %d) = %q: %d bytes exceeds budget %d", s, c.n, got, len(got), c.n)
		}
	}
}

func TestTailBytesASCII(t *testing.T) {
	const s = "hello world"
	cases := []struct {
		n    int
		want string
	}{
		{0, ""},
		{-1, ""},
		{-100, ""},
		{1, "d"},
		{5, "world"},
		{6, " world"},
		{11, "hello world"},
		{12, "hello world"},
		{50, "hello world"},
	}
	for _, c := range cases {
		got := TailBytes(s, c.n)
		assertValid(t, "TailBytes", got)
		if got != c.want {
			t.Errorf("TailBytes(%q, %d) = %q, want %q", s, c.n, got, c.want)
		}
		if c.n > 0 && len(got) > c.n {
			t.Errorf("TailBytes(%q, %d) = %q: %d bytes exceeds budget %d", s, c.n, got, len(got), c.n)
		}
	}
}

func TestHeadASCII(t *testing.T) {
	const s = "hello world"
	cases := []struct {
		n    int
		want string
	}{
		{0, ""},
		{-1, ""},
		{1, "h"},
		{5, "hello"},
		{6, "hello "},
		{11, "hello world"},
		{12, "hello world"},
		{50, "hello world"},
	}
	for _, c := range cases {
		got := Head(s, c.n)
		assertValid(t, "Head", got)
		if got != c.want {
			t.Errorf("Head(%q, %d) = %q, want %q", s, c.n, got, c.want)
		}
		if c.n > 0 && utf8.RuneCountInString(got) > c.n {
			t.Errorf("Head(%q, %d) = %q: %d runes exceeds budget %d", s, c.n, got, utf8.RuneCountInString(got), c.n)
		}
	}
}

// emoji is "💾 Apply pve01 memory fix": 24 runes / 27 bytes.
// U+1F4BE (floppy disk) is astral: 4 UTF-8 bytes F0 9F 92 BE = 2 UTF-16 code
// units — exactly the shape that breaks naive byte slicing.
const emoji = "\U0001F4BE Apply pve01 memory fix"

func TestEmojiLayout(t *testing.T) {
	// Pin the layout so the expectations below cannot silently drift.
	if len(emoji) != 27 {
		t.Fatalf("len(emoji) = %d, want 27", len(emoji))
	}
	if utf8.RuneCountInString(emoji) != 24 {
		t.Fatalf("rune count of emoji = %d, want 24", utf8.RuneCountInString(emoji))
	}
	if utf8.RuneLen('💾') != 4 {
		t.Fatalf("floppy emoji is not 4 UTF-8 bytes")
	}
}

func TestHeadBytesEmoji(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		// Budgets 1..3 land inside the 4-byte emoji: the partial rune is
		// dropped entirely, so the result is empty — never b"\xf0\x9f\x92".
		{1, ""},
		{2, ""},
		{3, ""},
		{4, "💾"},
		// NOTE: the spec guessed HeadBytes(s,5) == "💾" (4 bytes). The last 5
		// bytes of the prefix are F0 9F 92 BE 20 = emoji + the following
		// space, so the maximal prefix within budget is "💾 " (5 bytes).
		// Contract = "at most n bytes, cut on a rune boundary" = MAXIMAL
		// prefix, so "💾 " is correct and is what we assert.
		{5, "💾 "},
		{6, "💾 A"},
		{7, "💾 Ap"},
		{26, emoji[:26]},
		{27, emoji},
		{28, emoji},
		{0, ""},
		{-5, ""},
	}
	for _, c := range cases {
		got := HeadBytes(emoji, c.n)
		assertValid(t, "HeadBytes(emoji)", got)
		if got != c.want {
			t.Errorf("HeadBytes(emoji, %d) = %q (% x), want %q (% x)",
				c.n, got, []byte(got), c.want, []byte(c.want))
		}
		if c.n > 0 && len(got) > c.n {
			t.Errorf("HeadBytes(emoji, %d) = %q: %d bytes exceeds budget %d",
				c.n, got, len(got), c.n)
		}
	}
}

func TestHeadBytesEmojiFullLength(t *testing.T) {
	if got := HeadBytes(emoji, len(emoji)); got != emoji {
		t.Errorf("HeadBytes(s, len(s)) = %q, want the input unchanged", got)
	}
}

func TestTailBytesEmoji(t *testing.T) {
	// The string ends in ASCII "fix", so small budgets stay on boundaries.
	cases := []struct {
		n    int
		want string
	}{
		{1, "x"},
		{2, "ix"},
		{3, "fix"},
		// NOTE: the spec guessed TailBytes(s,4) == "fix" (3 bytes). The last 4
		// BYTES are 20 66 69 78 = " fix" (space + f + i + x), every one of them
		// a rune start, so the maximal suffix inside a 4-byte budget is " fix".
		// Budget is in bytes and no rune is dropped for free, so " fix" is the
		// correct answer and is what we assert.
		{4, " fix"},
		{5, "y fix"},
		{6, "ry fix"},
		{7, "ory fix"},
		{27, emoji},
		{28, emoji},
		{0, ""},
		{-5, ""},
	}
	for _, c := range cases {
		got := TailBytes(emoji, c.n)
		assertValid(t, "TailBytes(emoji)", got)
		if got != c.want {
			t.Errorf("TailBytes(emoji, %d) = %q (% x), want %q (% x)",
				c.n, got, []byte(got), c.want, []byte(c.want))
		}
		if c.n > 0 && len(got) > c.n {
			t.Errorf("TailBytes(emoji, %d) = %q: %d bytes exceeds budget %d",
				c.n, got, len(got), c.n)
		}
	}
}

// German umlauts / accented characters: 2-byte runes (C3 84 = Ä, C3 B6 = ö,
// C3 9F = ß, C3 A9 = é). Byte budgets that land on the second byte of a
// 2-byte rune must drop the whole rune, not emit the leading C3.
func TestUmlautsBytes(t *testing.T) {
	const s = "Äpfel Größe café" // 16 runes, 20 bytes
	if len(s) != 20 {
		t.Fatalf("len(s) = %d, want 20", len(s))
	}
	if utf8.RuneCountInString(s) != 16 {
		t.Fatalf("rune count of umlaut string = %d, want 16", utf8.RuneCountInString(s))
	}

	cases := []struct {
		name string
		fn   func(string, int) string
		n    int
		want string
	}{
		{"HeadBytes", HeadBytes, 1, ""}, // budget inside Ä -> drop the rune
		{"HeadBytes", HeadBytes, 2, "Ä"},
		{"HeadBytes", HeadBytes, 3, "Äp"},
		{"HeadBytes", HeadBytes, 6, "Äpfel"},
		{"HeadBytes", HeadBytes, 7, "Äpfel "},
		{"HeadBytes", HeadBytes, 9, "Äpfel Gr"},
		{"HeadBytes", HeadBytes, 10, "Äpfel Gr"}, // budget inside ö -> drop it
		{"HeadBytes", HeadBytes, 11, "Äpfel Grö"},
		{"HeadBytes", HeadBytes, 12, "Äpfel Grö"}, // budget inside ß -> drop it
		{"HeadBytes", HeadBytes, 13, "Äpfel Größ"},
		{"HeadBytes", HeadBytes, 19, "Äpfel Größe caf"},
		{"HeadBytes", HeadBytes, 20, s},
		{"HeadBytes", HeadBytes, 21, s},
		{"HeadBytes", HeadBytes, 0, ""},
		{"HeadBytes", HeadBytes, -3, ""},
		{"TailBytes", TailBytes, 1, ""}, // budget inside é -> drop the rune
		{"TailBytes", TailBytes, 2, "é"},
		{"TailBytes", TailBytes, 3, "fé"},
		{"TailBytes", TailBytes, 4, "afé"},
		{"TailBytes", TailBytes, 5, "café"},
		{"TailBytes", TailBytes, 6, " café"},
		{"TailBytes", TailBytes, 7, "e café"},
		{"TailBytes", TailBytes, 20, s},
		{"TailBytes", TailBytes, 21, s},
		{"TailBytes", TailBytes, 0, ""},
		{"TailBytes", TailBytes, -3, ""},
	}
	for _, c := range cases {
		got := c.fn(s, c.n)
		assertValid(t, c.name+"(umlauts)", got)
		if got != c.want {
			t.Errorf("%s(%q, %d) = %q (% x), want %q (% x)",
				c.name, s, c.n, got, []byte(got), c.want, []byte(c.want))
		}
		if c.n > 0 && len(got) > c.n {
			t.Errorf("%s(%q, %d) = %q: %d bytes exceeds budget %d",
				c.name, s, c.n, got, len(got), c.n)
		}
	}
}

func TestUmlautsRunes(t *testing.T) {
	const s = "Äpfel Größe café" // 16 runes
	cases := []struct {
		name string
		fn   func(string, int) string
		n    int
		want string
	}{
		{"Head", Head, 1, "Ä"},
		{"Head", Head, 2, "Äp"},
		{"Head", Head, 6, "Äpfel "},
		{"Head", Head, 7, "Äpfel G"},
		{"Head", Head, 11, "Äpfel Größe"},
		{"Head", Head, 16, s},
		{"Head", Head, 17, s},
		{"Head", Head, 0, ""},
		{"Head", Head, -1, ""},
		// The last rune is U+00E9 "é", not ASCII "e" — a rune-aware Tail keeps
		// the whole accented character, where s[len(s)-1:] would leave a lone 0xA9.
		{"Tail", Tail, 1, "é"},
		{"Tail", Tail, 2, "fé"},
		{"Tail", Tail, 3, "afé"},
		{"Tail", Tail, 4, "café"},
		{"Tail", Tail, 5, " café"},
		{"Tail", Tail, 16, s},
		{"Tail", Tail, 17, s},
		{"Tail", Tail, 0, ""},
		{"Tail", Tail, -1, ""},
	}
	for _, c := range cases {
		got := c.fn(s, c.n)
		assertValid(t, c.name+"(umlauts)", got)
		if got != c.want {
			t.Errorf("%s(%q, %d) = %q, want %q", c.name, s, c.n, got, c.want)
		}
		if c.n > 0 && utf8.RuneCountInString(got) > c.n {
			t.Errorf("%s(%q, %d) = %q: %d runes exceeds budget %d",
				c.name, s, c.n, got, utf8.RuneCountInString(got), c.n)
		}
	}
}

const ascii = "hello world"

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

// Mixed 1-, 2-, 3- and 4-byte runes in one string. Every byte budget from -3
// to len(s)+3 is swept, so every "the budget lands mid-rune" position in every
// input is exercised, and the result must be valid UTF-8 every single time.
func TestMixedMultibyteFullSweep(t *testing.T) {
	inputs := []string{
		ascii,
		emoji,
		"日本語のテキスト",
		"café ☕ résumé",
		"a\U0001F600b\U0001F601c\U0001F4BE",
		"\U0001F4BEÄößé\U0001F600",
		"emoji\U0001F4BE",
		"\U0001F4BEmixedÄ text\U0001F600",
		"GrößeÄ",
		"a",
		"\U0001F4BE",
	}
	for _, s := range inputs {
		if !utf8.ValidString(s) {
			t.Fatalf("test input is not valid UTF-8: %q", s)
		}
		total := utf8.RuneCountInString(s)
		for n := -3; n <= len(s)+3; n++ {
			hb, tb, hr, tr := HeadBytes(s, n), TailBytes(s, n), Head(s, n), Tail(s, n)
			where := func(fn string) string {
				return fmt.Sprintf("%s(%q, %d)", fn, s, n)
			}
			assertValid(t, where("HeadBytes"), hb)
			assertValid(t, where("TailBytes"), tb)
			assertValid(t, where("Head"), hr)
			assertValid(t, where("Tail"), tr)

			if n <= 0 {
				if hb != "" || tb != "" || hr != "" || tr != "" {
					t.Errorf("n=%d on %q: expected \"\" from all four helpers, got %q/%q/%q/%q",
						n, s, hb, tb, hr, tr)
				}
				continue
			}
			if n >= len(s) {
				if hb != s || tb != s || hr != s || tr != s {
					t.Errorf("budget %d >= len(%q)=%d: expected the input back, got %q/%q/%q/%q",
						n, s, len(s), hb, tb, hr, tr)
				}
				continue
			}

			if len(hb) > n {
				t.Errorf("%s = %q: %d bytes exceeds budget %d", where("HeadBytes"), hb, len(hb), n)
			}
			if len(tb) > n {
				t.Errorf("%s = %q: %d bytes exceeds budget %d", where("TailBytes"), tb, len(tb), n)
			}
			if utf8.RuneCountInString(hr) > n {
				t.Errorf("%s = %q: %d runes exceeds budget %d", where("Head"), hr, utf8.RuneCountInString(hr), n)
			}
			if utf8.RuneCountInString(tr) > n {
				t.Errorf("%s = %q: %d runes exceeds budget %d", where("Tail"), tr, utf8.RuneCountInString(tr), n)
			}
			if !strings.HasPrefix(s, hb) {
				t.Errorf("%s = %q is not a prefix of the input", where("HeadBytes"), hb)
			}
			if !strings.HasSuffix(s, tb) {
				t.Errorf("%s = %q is not a suffix of the input", where("TailBytes"), tb)
			}
			if !strings.HasPrefix(s, hr) {
				t.Errorf("%s = %q is not a prefix of the input", where("Head"), hr)
			}
			if !strings.HasSuffix(s, tr) {
				t.Errorf("%s = %q is not a suffix of the input", where("Tail"), tr)
			}

			// Maximality: HeadBytes must be the LARGEST valid prefix that fits,
			// so the next rune must not fit in the remaining budget.
			if len(hb) < len(s) {
				nextSize := utf8.RuneLen(firstRune(s[len(hb):]))
				if len(hb)+nextSize <= n {
					t.Errorf("%s = %q is not maximal: next rune (%d bytes) fits in budget %d",
						where("HeadBytes"), hb, nextSize, n)
				}
			}
			// Maximality for the tail: the rune just before the suffix must not
			// fit in the budget.
			if start := len(s) - len(tb); start > 0 && start < len(s) {
				prevSize := utf8.RuneLen(lastRune(s[:start]))
				if start-prevSize >= len(s)-n {
					t.Errorf("%s = %q is not maximal: previous rune (%d bytes) fits in budget %d",
						where("TailBytes"), tb, prevSize, n)
				}
			}

			// Rune budgets: Head/Tail at the full rune count return everything.
			if n >= total && hr != s {
				t.Errorf("%s: n=%d >= rune count %d, expected the input back, got %q",
					where("Head"), n, total, hr)
			}
			if n >= total && tr != s {
				t.Errorf("%s: n=%d >= rune count %d, expected the input back, got %q",
					where("Tail"), n, total, tr)
			}
		}
	}
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

// Input that is a single astral emoji: 4 bytes, 1 rune. Every budget 1..3 must
// yield "" (the whole rune is dropped) — never 1-3 bytes of a surrogate pair.
func TestSingleAstralEmoji(t *testing.T) {
	const s = "\U0001F4BE"
	if len(s) != 4 || utf8.RuneCountInString(s) != 1 {
		t.Fatalf("layout: len=%d runes=%d", len(s), utf8.RuneCountInString(s))
	}
	byteHelpers := []struct {
		name string
		fn   func(string, int) string
	}{{"HeadBytes", HeadBytes}, {"TailBytes", TailBytes}}
	// BYTE budgets 1..3 fall inside the 4-byte rune: the whole rune is
	// dropped, so the answer is "" -- never 1-3 bytes of a surrogate pair.
	for n := -5; n <= 3; n++ {
		for _, c := range byteHelpers {
			got := c.fn(s, n)
			assertValid(t, c.name, got)
			if got != "" {
				t.Errorf("%s(%q, %d) = %q, want \"\"", c.name, s, n, got)
			}
		}
	}
	// RUNE budgets: n is a character count, so Head("emoji", 1) is the WHOLE
	// emoji (it is 4 bytes, but only 1 rune). Only n <= 0 is empty here.
	runeHelpers := []struct {
		name string
		fn   func(string, int) string
	}{{"Head", Head}, {"Tail", Tail}}
	for _, n := range []int{-5, -1, 0} {
		for _, c := range runeHelpers {
			if got := c.fn(s, n); got != "" {
				t.Errorf("%s(%q, %d) = %q, want \"\" (rune budget <= 0)", c.name, s, n, got)
			}
		}
	}
	for _, n := range []int{4, 5, 100} {
		if got := HeadBytes(s, n); got != s {
			t.Errorf("HeadBytes(emoji, %d) = %q, want the emoji", n, got)
		}
		if got := TailBytes(s, n); got != s {
			t.Errorf("TailBytes(emoji, %d) = %q, want the emoji", n, got)
		}
	}
	// Rune budgets: 1 rune is the whole string, 2 is more than it has.
	for _, n := range []int{1, 2, 99} {
		if got := Head(s, n); got != s {
			t.Errorf("Head(emoji, %d) = %q, want the emoji", n, got)
		}
		if got := Tail(s, n); got != s {
			t.Errorf("Tail(emoji, %d) = %q, want the emoji", n, got)
		}
	}
}

// Boundary cases for Tail(s, n) around the rune count, including inputs where
// the rune count is strictly less than the byte length (so the cheap
// len(s) <= n shortcut does NOT fire and utf8.RuneCountInString must).
func TestTailRuneCountBoundaries(t *testing.T) {
	const s = "Äpfel Größe café" // 20 bytes, 16 runes
	cases := []struct {
		n    int
		want string
	}{
		{16, s},                 // n == rune count
		{17, s},                 // n > rune count
		{1000, s},               // n >> rune count
		{15, "pfel Größe café"}, // n == rune count - 1
		{1, "é"},
		{0, ""},
		{-1, ""},
	}
	for _, c := range cases {
		got := Tail(s, c.n)
		assertValid(t, "Tail", got)
		if got != c.want {
			t.Errorf("Tail(%q, %d) = %q, want %q", s, c.n, got, c.want)
		}
		if c.n > 0 && utf8.RuneCountInString(got) > c.n {
			t.Errorf("Tail(%q, %d) = %q: rune budget exceeded", s, c.n, got)
		}
	}
	// Same boundary shape for Head.
	if got := Head(s, 16); got != s {
		t.Errorf("Head(s, 16) = %q, want the input unchanged", got)
	}
	if got := Head(s, 17); got != s {
		t.Errorf("Head(s, 17) = %q, want the input unchanged", got)
	}
}

// Boundary cases: empty input, n == 0, n < 0, n == len(s), n > len(s).
func TestBoundaries(t *testing.T) {
	const s = "abc"
	helpers := []struct {
		name string
		fn   func(string, int) string
	}{
		{"HeadBytes", HeadBytes},
		{"TailBytes", TailBytes},
		{"Head", Head},
		{"Tail", Tail},
	}
	for _, h := range helpers {
		// Empty input, any budget.
		for _, n := range []int{-1, 0, 1, 100} {
			if got := h.fn("", n); got != "" {
				t.Errorf("%s(\"\", %d) = %q, want \"\"", h.name, n, got)
			}
		}
		// n == len(s) returns the input unchanged; n > len(s) likewise.
		if got := h.fn(s, len(s)); got != s {
			t.Errorf("%s(s, len(s)) = %q, want %q", h.name, got, s)
		}
		if got := h.fn(s, len(s)+1); got != s {
			t.Errorf("%s(s, len(s)+1) = %q, want %q", h.name, got, s)
		}
		// n == 0 and n < 0 return "".
		for _, n := range []int{0, -1, -1000} {
			if got := h.fn(s, n); got != "" {
				t.Errorf("%s(s, %d) = %q, want \"\"", h.name, n, got)
			}
		}
	}
}

// Input that is ALREADY invalid UTF-8, e.g. a raw 0xFF byte: the helpers must
// not panic, must not invent bytes, and must return a prefix/suffix that lands
// on a rune-start byte.
//
// Documented behaviour (this is a truncation helper, not a sanitiser): invalid
// bytes are NOT repaired and NOT replaced. The walk stops at the first byte
// that can legally start a rune (utf8.RuneStart = b < 0x80 || b >= 0xC0), so:
//   - HeadBytes(string([]byte{0xff, 0x41}), 2) == "\xff\x41" — the whole string
//     fits in the budget, so it is returned verbatim, invalid byte included.
//   - HeadBytes(..., 1) == "\xff" — index 1 (0x41) IS a rune start, so the cut
//     at 1 is already on a boundary and 0xff is kept.
//   - TailBytes(..., 1) == "A" — the byte budget lands on the ASCII rune.
//
// Callers that need clean output must sanitise separately (e.g.
// strings.ToValidUTF8) before or after truncating.
func TestInvalidUTF8InputDoesNotPanic(t *testing.T) {
	invalid := []string{
		string([]byte{0xff, 0x41}),
		string([]byte{0x41, 0xff}),
		string([]byte{0x41, 0xc3, 0x41}),
		string([]byte{0xf0, 0x9f, 0x92}),
		string([]byte{0xf0, 0x9f, 0x92, 0x38}),
		string([]byte{0x80}),
		string([]byte{0xc0, 0x80}),
		string([]byte{0xed, 0xa0, 0x80}), // CESU-8 encoded surrogate
		"A" + string([]byte{0xff}) + "B",
	}
	for _, s := range invalid {
		for n := -2; n <= len(s)+2; n++ {
			hb := HeadBytes(s, n)
			tb := TailBytes(s, n)
			hr := Head(s, n)
			tr := Tail(s, n)
			if !strings.HasPrefix(s, hb) {
				t.Errorf("HeadBytes(% x, %d) = % x is not a prefix", []byte(s), n, []byte(hb))
			}
			if !strings.HasSuffix(s, tb) {
				t.Errorf("TailBytes(% x, %d) = % x is not a suffix", []byte(s), n, []byte(tb))
			}
			if !strings.HasPrefix(s, hr) || !strings.HasSuffix(s, tr) {
				t.Errorf("Head/Tail(% x, %d) = % x / % x must stay inside the input",
					[]byte(s), n, []byte(hr), []byte(tr))
			}
			if n <= 0 && (hb != "" || tb != "" || hr != "" || tr != "") {
				t.Errorf("n=%d on % x: expected empty results", n, []byte(s))
			}
			if n > 0 && (len(hb) > n || len(tb) > n) {
				t.Errorf("n=%d on % x: byte budgets exceeded (%d/%d)", n, []byte(s), len(hb), len(tb))
			}
			if n >= len(s) && (hb != s || tb != s || hr != s || tr != s) {
				t.Errorf("n=%d >= len(% x): expected the input back verbatim", n, []byte(s))
			}
		}
	}
}

func TestInvalidUTF8DocumentedValues(t *testing.T) {
	s := string([]byte{0xff, 0x41})
	// Documented in TestInvalidUTF8InputDoesNotPanic: the invalid byte is kept,
	// never replaced, and no panic happens.
	if got := HeadBytes(s, 2); got != "\xff\x41" {
		t.Errorf("HeadBytes(0xff41, 2) = % x, want ff 41", []byte(got))
	}
	if got := HeadBytes(s, 1); got != "\xff" {
		t.Errorf("HeadBytes(0xff41, 1) = % x, want ff", []byte(got))
	}
	if got := HeadBytes(s, 0); got != "" {
		t.Errorf("HeadBytes(0xff41, 0) = % x, want empty", []byte(got))
	}
	if got := TailBytes(s, 1); got != "A" {
		t.Errorf("TailBytes(0xff41, 1) = % q, want \"A\"", got)
	}
	if got := TailBytes(s, 2); got != "\xff\x41" {
		t.Errorf("TailBytes(0xff41, 2) = % x, want ff 41", []byte(got))
	}
	// Budget lands on the continuation byte 0x9f -> skip forward to 0x92, which
	// is also a continuation, so the suffix collapses to empty.
	b := string([]byte{0xf0, 0x9f, 0x92})
	if got := TailBytes(b, 2); got != "" {
		t.Errorf("TailBytes(f0 9f 92, 2) = % x, want empty", []byte(got))
	}
}

// The end-to-end point of the package: a truncated string goes through
// encoding/json and then through UTF-16 (what the Dart/Flutter client sees).
// Byte slicing at an arbitrary index injects U+FFFD and can leave a lone
// surrogate; the textsafe helpers must do neither.
func TestJSONRoundTripNoReplacementCharNoLoneSurrogate(t *testing.T) {
	inputs := []string{
		emoji,
		"Äpfel Größe café",
		"日本語のテキスト",
		"a\U0001F600b\U0001F601c",
		"\U0001F4BEÄößé\U0001F600",
	}
	for _, s := range inputs {
		for n := 1; n < len(s); n++ {
			for _, c := range []struct {
				name string
				fn   func(string, int) string
			}{{"HeadBytes", HeadBytes}, {"TailBytes", TailBytes}, {"Head", Head}, {"Tail", Tail}} {
				v := c.fn(s, n)
				// 1. JSON must be byte-identical to the input's own JSON
				// escaping: a U+FFFD would appear as "\ufffd" = "".
				b, err := json.Marshal(map[string]string{"v": v})
				if err != nil {
					t.Fatalf("json.Marshal of %s(%q, %d) = %q: %v", c.name, s, n, v, err)
				}
				if strings.Contains(string(b), "\ufffd") {
					t.Errorf("%s(%q, %d) = %q marshals to %s: contains U+FFFD",
						c.name, s, n, v, string(b))
				}
				// 2. UTF-16 encoding must contain NO lone surrogate: a leading
				// surrogate must be immediately followed by a trailing one.
				units := utf16.Encode([]rune(v))
				for i, u := range units {
					if u >= 0xD800 && u <= 0xDBFF {
						if i+1 >= len(units) || units[i+1] < 0xDC00 || units[i+1] > 0xDFFF {
							t.Errorf("%s(%q, %d) = %q yields a LONE LEADING surrogate at %d: % x",
								c.name, s, n, v, i, units)
						}
					}
					if u >= 0xDC00 && u <= 0xDFFF && (i == 0 || units[i-1] < 0xD800 || units[i-1] > 0xDBFF) {
						t.Errorf("%s(%q, %d) = %q yields a LONE TRAILING surrogate at %d: % x",
							c.name, s, n, v, i, units)
					}
				}
				// 3. Demonstrate that the naive byte slice WOULD have broken:
				// for every n where s[:n] is invalid, our helper must be valid.
				if !utf8.ValidString(s[:n]) && !utf8.ValidString(v) {
					t.Errorf("%s(%q, %d) = %q: helper must be valid where s[:n] is not",
						c.name, s, n, v)
				}
			}
		}
	}
}

// HeadBytes/TailBytes must not allocate: they are used on hot paths (tool
// output caps, log tails) and are pure string slices sharing the backing array.
func TestByteHelpersDoNotAllocate(t *testing.T) {
	s := emoji + " — longer payload with multibyte runes: Größe 日本語 \U0001F600"
	if len(s) < 40 {
		t.Fatalf("payload too short: %d bytes", len(s))
	}
	// Budgets chosen so every call must walk past continuation bytes:
	// 2/3 land inside the leading 4-byte emoji, len-2/len-3 inside the
	// trailing one. None of them is a plain slice.
	budgets := []int{2, 3, len(s) - 2, len(s) - 3}
	for _, n := range budgets {
		if hb := HeadBytes(s, n); len(hb) == n {
			t.Fatalf("HeadBytes(s, %d) = %q: expected a boundary cut (len == budget)", n, hb)
		}
		if tb := TailBytes(s, n); len(tb) == n {
			t.Fatalf("TailBytes(s, %d) = %q: expected a boundary cut (len == budget)", n, tb)
		}
	}
	for _, n := range budgets {
		allocs := testing.AllocsPerRun(200, func() {
			_ = HeadBytes(s, n)
			_ = TailBytes(s, n)
		})
		if allocs != 0 {
			t.Errorf("HeadBytes/TailBytes (budget %d) allocated %v times per run, want 0", n, allocs)
		}
	}
}
