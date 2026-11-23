package textsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The load-bearing invariant: truncation is a pure function of the input whose
// result is within the bound, so running it again (the next maintenance pass,
// the next server boot) is a NO-OP. A truncator that is not idempotent rewrites
// stored rows on every boot, and KV prompt caching then discards the whole
// conversation's prefix.
func TestTruncateWithBannerIsIdempotent(t *testing.T) {
	const max = 1000
	s := strings.Repeat("x", 5000)

	once, changed := TruncateWithBanner(s, max)
	if !changed {
		t.Fatal("first pass must truncate an over-bound body")
	}
	if len(once) > max {
		t.Fatalf("result is %d bytes, over the %d bound — the length test never "+
			"settles, so the next pass truncates it again", len(once), max)
	}
	twice, changed2 := TruncateWithBanner(once, max)
	if changed2 {
		t.Fatal("second pass changed the bytes: truncation is not idempotent")
	}
	if twice != once {
		t.Fatal("second pass rewrote the body byte-for-byte")
	}
	for i := 0; i < 25; i++ {
		next, ch := TruncateWithBanner(twice, max)
		if ch || next != once {
			t.Fatalf("pass %d changed the bytes", i+2)
		}
	}
}

func TestTruncateWithBannerReservesRoomForTheBanner(t *testing.T) {
	// The exact shape of the live defect: a 100000 bound whose banner is
	// ~110 bytes produced 100110-byte rows, i.e. rows over their own bound.
	const max = 100_000
	s := strings.Repeat("y", 106_485) // the live row at conv a9df85e3 seq=10
	got, changed := TruncateWithBanner(s, max)
	if !changed {
		t.Fatal("expected truncation")
	}
	if len(got) > max {
		t.Fatalf("got %d bytes, want <= %d", len(got), max)
	}
	if !strings.HasPrefix(got, HeadBytes(s, 100)) {
		t.Fatal("the preserved head must be the head of the original")
	}
	if !strings.Contains(got, BannerMarker) {
		t.Fatal("missing the truncation banner")
	}
	if !strings.Contains(got, "106485") {
		t.Fatal("the banner must state the original length")
	}
}

// A legacy row that already carries stacked banners AND is still over the bound
// must settle in one deterministic pass, then never move again.
func TestTruncateWithBannerSettlesLegacyStackedRowsOnce(t *testing.T) {
	const max = 600
	s := strings.Repeat("z", 400) + Banner(900) + strings.Repeat("q", 400) + Banner(1300)
	if len(s) <= max {
		t.Fatalf("fixture must start over the bound, got %d", len(s))
	}
	once, changed := TruncateWithBanner(s, max)
	if !changed {
		t.Fatal("an over-bound row must be settled, not skipped because it mentions a banner")
	}
	if len(once) > max {
		t.Fatalf("settled row is %d bytes, over the %d bound", len(once), max)
	}
	// Exactly ONE notice is appended; the preserved head may legitimately
	// quote an older one as data.
	if !strings.HasSuffix(once, Banner(len(s))) {
		t.Fatal("the settled row must end with the banner for the original length")
	}
	twice, changed2 := TruncateWithBanner(once, max)
	if changed2 || twice != once {
		t.Fatal("the settled row moved on the second pass — byte rot is back")
	}
}

func TestTruncateWithBannerLeavesSmallBodiesAlone(t *testing.T) {
	s := "short body"
	if got, changed := TruncateWithBanner(s, 1000); changed || got != s {
		t.Fatal("a body within the bound must be returned verbatim")
	}
	if got, changed := TruncateWithBanner(s, 0); changed || got != s {
		t.Fatal("a non-positive bound must be a no-op, not an empty body")
	}
}

// A body that merely QUOTES the banner phrase is a normal body: the decision
// must not branch on the marker, or such a row is never bounded.
func TestTruncateWithBannerDoesNotSkipBodiesQuotingTheBanner(t *testing.T) {
	s := "grep -n 'message truncated by Plurality' *.go\n" + strings.Repeat("out", 3000)
	got, changed := TruncateWithBanner(s, 900)
	if !changed {
		t.Fatal("a body quoting the banner phrase must still be bounded")
	}
	if len(got) > 900 {
		t.Fatalf("got %d bytes, want <= 900", len(got))
	}
	if again, ch := TruncateWithBanner(got, 900); ch || again != got {
		t.Fatal("not idempotent")
	}
}

func TestTruncateWithBannerIsByteStableAcrossRuns(t *testing.T) {
	// The request path calls this per turn; the provider's prefix cache only
	// survives if identical stored bytes render to identical wire bytes.
	s := strings.Repeat("日本語テキスト", 300) // multibyte: the cut must stay on a rune
	a, _ := TruncateWithBanner(s, 900)
	for i := 0; i < 50; i++ {
		b, _ := TruncateWithBanner(s, 900)
		if a != b {
			t.Fatal("truncation is not deterministic")
		}
	}
	if !utf8.ValidString(a) {
		t.Fatal("result is not valid UTF-8: the cut split a rune")
	}
}

func TestTruncateWithBannerTinyBoundIsBounded(t *testing.T) {
	// A bound smaller than the banner must not return the full body.
	got, changed := TruncateWithBanner(strings.Repeat("w", 10_000), 8)
	if !changed {
		t.Fatal("expected truncation")
	}
	if len(got) > 8 {
		t.Fatalf("result %d bytes exceeds the bound", len(got))
	}
	// A bound this small cannot carry the 110-byte notice; the invariants that
	// matter are bounded, deterministic and idempotent.
	if !strings.HasPrefix(got, HeadBytes(Banner(10_000), 8)) {
		t.Fatal("result must be the clamped banner")
	}
	if again, ch := TruncateWithBanner(got, 8); ch || again != got {
		t.Fatal("tiny-bound truncation is not idempotent")
	}
}
