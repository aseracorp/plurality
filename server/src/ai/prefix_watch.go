package ai

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"

	"github.com/azukaar/plurality/src/utils"
)

// prefix_watch — the instrument that ends the guessing.
//
// WHY. The deployed signature of the cache bug is a cache_read FROZEN at one
// value across consecutive turns while prompt_tokens grows. A frozen
// cache_read is not a cold cache: every request shares a prefix of EXACTLY the
// same length and diverges at the same offset. Six rounds of fixes
// (#197..#206) each changed a plausible suspect, and none was measured against
// the wire bytes of two consecutive requests.
//
// WHAT IT DOES. Keeps the outgoing request bytes of the previous turn per
// conversation and reports the byte offset where this request stops sharing a
// prefix with it — then LOCATES that offset: which history row contains it,
// its role, its tool name and its leading bytes. That names the mutation
// instead of reasoning about it.
//
// LAYOUT, measured, not assumed. StandardChatRequest marshals in struct order:
// model, messages, max_tokens, ... , stream, tools, reasoning_effort,
// cache_prompt, n_keep. The tools block is therefore at the END of the body,
// not after the system prompt. The first version of this file attributed a
// divergence to "TOOL SCHEMAS" when the offset fell in the first
// systemBytes+toolsBytes of the body — which is the SYSTEM PROMPT and the
// FIRST HISTORY ROWS, not the schemas. Every break was reported as "history"
// by that arithmetic. Region is now derived from the real offsets: the body is
// scanned for the messages array, rows are walked in order, and the trailing
// fields (including the schemas) are located by their own byte spans.
//
// Pure observer: it never alters the request. Enabled with PREFIX_WATCH=1.

// prefixWatchKeep bounds the retained head of the previous request.
const prefixWatchKeep = 1 << 21

type prefixWatchEntry struct {
	head []byte
	len  int
}

var (
	prefixWatchMu    sync.Mutex
	prefixWatchState = map[string]prefixWatchEntry{}
	prefixWatchOn    = os.Getenv("PREFIX_WATCH") == "1"
)

// prefixWatchPartSize marshals v, returning 0 on failure so the observer can
// never break the request path.
func prefixWatchPartSize(v interface{}) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// messagesArrayOffset returns the byte offset just past `"messages":[` in the
// marshalled body, or -1. This is the anchor the row walk needs; assuming it
// is what broke the first version.
func messagesArrayOffset(request []byte) int {
	i := bytes.Index(request, []byte(`"messages":[`))
	if i < 0 {
		return -1
	}
	return i + len(`"messages":[`)
}

// prefixWatchLocate walks the history rows from the start of the messages
// array and returns a description of the row containing `div`, plus the byte
// offset where the history ends. Row sizes are the marshalled row plus the
// separating comma, which is exactly how encoding/json lays the array out.
func prefixWatchLocate(rows []StandardMessageReq, request []byte, div int) (string, int) {
	off := messagesArrayOffset(request)
	if off < 0 {
		return "messages array not found", -1
	}
	for i, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			break
		}
		n := len(b)
		if i < len(rows)-1 {
			n++ // the comma between array elements
		}
		if div < off+n {
			head := ""
			if s, ok := r.Content.(string); ok {
				head = s
			} else if parts, ok := r.Content.([]StandardContentReq); ok {
				for _, p := range parts {
					head += p.Text
				}
			}
			if len(head) > 90 {
				head = head[:90]
			}
			return "row #" + pwItoa(i) + " role=" + r.Role + " name=" + r.Name +
				" row_bytes=" + pwItoa(len(b)) + " row_starts_at=" + pwItoa(off) +
				" head=" + pwQuoteOneLine(head), off + n
		}
		off += n
	}
	return "offset is past the history (trailing fields / tool schemas)", off
}

func pwQuoteOneLine(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return "?"
	}
	return string(b)
}

func pwItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// prefixWatchCompare reports where this request stops sharing a prefix with
// the previous request for the same conversation, and which row that is.
func prefixWatchCompare(convID string, request []byte, rows []StandardMessageReq, systemBytes int) {
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
		utils.Log("[PrefixWatch] %s baseline: %d bytes (system %d, %d history rows)",
			convID, len(request), systemBytes, len(rows))
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

	where, histEnd := prefixWatchLocate(rows, request, div)
	region := "HISTORY"
	if div <= systemBytes {
		region = "SYSTEM PROMPT"
	} else if histEnd >= 0 && div >= histEnd {
		region = "TRAILING FIELDS / TOOL SCHEMAS"
	}
	utils.Log("[PrefixWatch] %s CACHE BREAK at byte %d (region: %s; %d bytes from the END of the previous %d-byte request) -> %s",
		convID, div, region, prev.len-div, prev.len, where)
}
