package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"

	"github.com/azukaar/plurality/src/utils"
)

// prefix_watch — the instrument that ends the guessing.
//
// WHY. The deployed signature of the cache bug is a cache_read FROZEN at one
// value (54289) across 15 consecutive turns while prompt_tokens grew
// 162615 -> 195999. A frozen cache_read is not a cold cache and not a moved
// tail: it means every request shares a prefix of EXACTLY the same length and
// then diverges at the same offset. Six rounds of fixes (#197..#205) each
// changed a plausible suspect — clock tail, request pacing, tail budget, fold
// identity, truncation idempotence — and each left the frozen offset
// untouched, because every theory was reasoned about in the abstract and none
// was measured against the wire bytes of two consecutive requests.
//
// WHAT IT DOES. It keeps the outgoing request bytes of the previous turn per
// conversation (bounded, and only the head is retained since that is where
// divergence is reported) and reports the byte offset where this request stops
// sharing a prefix with it, attributed to a region of the request. That names
// the mutation instead of guessing at it.
//
// Pure observer: it never alters the request. Enabled with PREFIX_WATCH=1.

// prefixWatchKeep is how many leading bytes of the previous request are
// retained for byte-exact comparison. The cache break is a prefix property, so
// the head is all that is needed, and this bounds memory.
const prefixWatchKeep = 1 << 20

type prefixWatchEntry struct {
	head []byte // leading bytes of the previous request
	len  int    // its full length
}

var (
	prefixWatchMu    sync.Mutex
	prefixWatchState = map[string]prefixWatchEntry{}
	prefixWatchOn    = os.Getenv("PREFIX_WATCH") == "1"
)

// prefixWatchPartSize marshals a slice of the request and returns its byte
// size, used to attribute a divergence offset to a region of the prompt.
// Returns 0 on failure so the observer can never break the request path.
func prefixWatchPartSize(v interface{}) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// prefixWatchCompare reports where this request stops sharing a prefix with
// the previous request for the same conversation. systemBytes and toolsBytes
// are the marshalled sizes of the parts built before the history, so the
// offset is attributed to a region: a break inside the system prompt or the
// tool schemas is a per-turn mutation of the stable prefix; a break in the
// history means a stored row was rewritten.
func prefixWatchCompare(convID string, request []byte, systemBytes, toolsBytes int) {
	if !prefixWatchOn || convID == "" {
		return
	}
	keep := len(request)
	if keep > prefixWatchKeep {
		keep = prefixWatchKeep
	}
	head := make([]byte, keep)
	copy(head, request[:keep])

	prefixWatchMu.Lock()
	prev, had := prefixWatchState[convID]
	prefixWatchState[convID] = prefixWatchEntry{head: head, len: len(request)}
	prefixWatchMu.Unlock()

	if !had {
		utils.Log("[PrefixWatch] %s baseline: %d bytes (system %d, tools %d)", convID, len(request), systemBytes, toolsBytes)
		return
	}

	n := len(prev.head)
	if n > len(head) {
		n = len(head)
	}
	div := n
	for i := 0; i < n; i++ {
		if prev.head[i] != head[i] {
			div = i
			break
		}
	}
	if div == n && len(request) >= prev.len {
		utils.Log("[PrefixWatch] %s STABLE: previous %d bytes fully reused, request grew to %d", convID, prev.len, len(request))
		return
	}

	region := "history"
	switch {
	case div <= systemBytes:
		region = "SYSTEM PROMPT"
	case div <= systemBytes+toolsBytes:
		region = "TOOL SCHEMAS"
	}
	utils.Log("[PrefixWatch] %s CACHE BREAK at byte %d (region: %s; system=%d, tools=%d) prev=%d bytes now=%d bytes",
		convID, div, region, systemBytes, toolsBytes, prev.len, len(request))
}

// prefixWatchHashes is used by tests to fingerprint a byte slice.
func prefixWatchHashes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}
