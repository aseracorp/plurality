package ai_tools

import (
	"sort"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// TestGetDisabledToolsSummaryIsDeterministic guards the KV prompt-cache
// contract for the SECOND map-iteration site in the prompt prefix.
//
// GetRequests emits the tools array in canonical sorted order (see
// TestGetRequestsOrderIsDeterministic). GetDisabledToolsSummary lives in the
// SYSTEM PROMPT and ranged the Registry map directly, so Go's per-call
// randomized map iteration re-shuffled that block on every request.
//
// The system prompt is rendered first by the chat template, so the shuffle
// landed at roughly byte 2801 of the serialized request (~600 tokens in).
// A prompt cache can only reuse up to the first divergent byte, so prefix
// reuse collapsed to the 4-checkpoint floor of exactly 16384 tokens: observed
// live on strata as 42 of 198 turns reporting "16384 reused", and identical
// prompt sizes reporting 0 / 16384 / 26394 reused depending on the draw.
//
// The summary block is ~400 tokens, which is exactly the 4096-token checkpoint
// quantum strata rounds to — 4 checkpoints = 16384.
func TestGetDisabledToolsSummaryIsDeterministic(t *testing.T) {
	// Disable a subset so the summary is non-empty: enable only fs_read,
	// leaving most builtins + MCP tools in the disabled list.
	partial := map[string]string{
		"filesystem_server__fs_read": "true",
	}
	model := utils.Model{Tools: partial}

	const trials = 40
	var first string
	for i := 0; i < trials; i++ {
		got := GetDisabledToolsSummary("order-test-user", model, nil)
		if got == "" {
			t.Fatal("expected a non-empty disabled-tools summary")
		}
		if i == 0 {
			first = got
			t.Logf("baseline: %d bytes", len(got))
			continue
		}
		if got != first {
			t.Fatalf("iteration %d: disabled-tools summary is not byte-stable\nfirst:\n%s\ngot:\n%s",
				i, first, got)
		}
	}
}

// TestGetDisabledToolsSummaryIsCanonical pins the sorted contract: lines
// ascend by the name each tool is emitted under, matching GetRequests.
func TestGetDisabledToolsSummaryIsCanonical(t *testing.T) {
	model := utils.Model{Tools: map[string]string{"filesystem_server__fs_read": "true"}}
	got := GetDisabledToolsSummary("order-test-user", model, nil)

	lines := strings.Split(got, "\n")
	var names []string
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "- ") {
			continue
		}
		name := strings.SplitN(strings.TrimPrefix(ln, "- "), ":", 2)[0]
		names = append(names, name)
	}
	if len(names) < 2 {
		t.Fatalf("expected several disabled tools, got %d", len(names))
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("disabled-tools summary not sorted by emitted name: %s", strings.Join(names, ","))
	}

	// No duplicate names: the same tool can be present as both a Registry
	// stub and a client-side enriched copy.
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Fatalf("duplicate tool in disabled summary: %q", n)
		}
		seen[n] = true
	}
}
