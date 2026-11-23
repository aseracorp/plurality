package textsafe

import (
	"fmt"
	"strings"
)

// BannerMarker is the substring that identifies a truncation notice written by
// Plurality. It is a marker for inspection and tests ONLY — truncation
// decisions must NOT branch on it (see TruncateWithBanner).
const BannerMarker = "message truncated by Plurality"

// bannerFormat is the ONE banner text used by every truncation site: the
// on-disk maintenance pass, the eco tail cap and the context-overflow
// compaction. It used to be TWO different strings — maintenance wrote
// "...exceeded the per-message context budget", the request path wrote
// "...across all content parts exceeded...". The stored form therefore never
// matched what the request path shipped, which is the invariant maintenance.go
// claims to keep.
const bannerFormat = "\n\n[%s: original length %d chars exceeded the per-message context budget]"

// Banner is the truncation notice for a body of originalLen bytes.
func Banner(originalLen int) string {
	return fmt.Sprintf(bannerFormat, BannerMarker, originalLen)
}

// TruncateWithBanner bounds s to AT MOST max bytes, appending Banner(len(s)).
//
// IDEMPOTENT BY CONSTRUCTION, and that is the property every caller depends on:
// the result is always <= max, so the very next pass sees a body within the
// bound and returns it untouched. Two defects made truncation a per-boot byte
// rot:
//
//  1. The result was HeadBytes(s, max) + banner, i.e. max + len(banner) bytes
//     — LONGER than the bound it was supposed to satisfy. The next pass saw the
//     row still over the bound and truncated it again, shaving ~110 bytes off
//     the preserved head and stacking a second banner with a new "original
//     length". Observed live: 345 stored rows carrying the banner while sitting
//     OVER the 100000 bound they had been truncated to, some with FIVE stacked
//     banners.
//  2. Because the length never settled, those rows' BYTES CHANGED ON EVERY
//     SERVER BOOT (the maintenance pass runs from sqlite.go). KV prompt caching
//     reuses the longest common PREFIX, so one changed byte mid-history
//     discards the whole conversation's cached KV. Measured as cache_read
//     pinned FLAT at 38030 tokens across consecutive turns while prompt tokens
//     grew 106710 -> 109378, with the pinned point sitting inside the first
//     banner-bearing row of the conversation.
//
// So room for the banner is reserved INSIDE max. No marker check: a body that
// merely CONTAINS the banner phrase (a shell command that greps for it, a
// quoted log line) is a normal body and must be truncated normally, and a
// legacy row that carries stacked banners and is still over the bound must
// settle — once, deterministically — instead of being skipped forever.
//
// Returns (result, changed). changed is false when s already fits, when max is
// not usable, or when the cut is a no-op.
func TruncateWithBanner(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	banner := Banner(len(s))
	keep := max - len(banner)
	if keep < 0 {
		// The bound is smaller than the banner itself (a degenerate budget).
		// Clamp the banner to max: the result is then EXACTLY max bytes, so
		// the next pass hits the len(s) <= max early-out and is a no-op.
		// Returning the full banner here would be over the bound and would
		// re-truncate to Banner(len(banner)) on the next pass — a different
		// string, i.e. the same byte rot this function exists to remove.
		return HeadBytes(banner, max), true
	}
	out := HeadBytes(s, keep) + banner
	if out == s {
		return s, false
	}
	return out, true
}

// IsTruncated reports whether s carries a Plurality truncation notice.
func IsTruncated(s string) bool {
	return strings.Contains(s, BannerMarker)
}
