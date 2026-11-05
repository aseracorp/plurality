package ai_tools

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// selectionKeys builds the model.Tools selection map that enables every tool
// the registry exposes (namespaced for bundled tools, bare for standalone),
// mirroring how GetRequests computes its selKey.
func selectionKeys() map[string]string {
	sel := make(map[string]string, len(Registry))
	for id, tool := range Registry {
		key := id
		if tool.BundleName != "" {
			key = tool.BundleName + "__" + id
		}
		sel[key] = "true"
	}
	return sel
}

func requestNames(model utils.Model) []string {
	reqs := GetRequests("order-test-user", model, nil, false, false, false)
	names := make([]string, len(reqs))
	for i, r := range reqs {
		names[i] = r.Function.Name
	}
	return names
}

// TestGetRequestsOrderIsDeterministic guards the KV prompt-cache contract.
//
// The tools array is rendered into the prompt by the chat template, so its
// byte order is part of the prompt's byte-prefix. GetRequests used to iterate
// the Registry map (and mcp.ToolsRequests the merged-tools map) with `range`;
// Go randomizes map iteration order on EVERY call, so the tools block — and
// with it the whole prompt prefix — was byte-shuffled on every request.
// Observed live on strata: prompt-cache reuse collapsed to exactly 16384
// tokens (4 checkpoints = the stable system block) and every agent turn
// re-prefilled 40-70k tokens (38-50 s) instead of reusing the full prefix.
func TestGetRequestsOrderIsDeterministic(t *testing.T) {
	model := utils.Model{Tools: selectionKeys()}

	const trials = 40
	var first []string
	for i := 0; i < trials; i++ {
		got := requestNames(model)
		if i == 0 {
			first = got
			t.Logf("baseline: %d tools", len(got))
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("iteration %d: tool count changed %d -> %d", i, len(first), len(got))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf(
					"iteration %d: tools order diverges at index %d: %q vs %q\n"+
						"first: %s\n  got: %s",
					i, j, got[j], first[j],
					strings.Join(first, ","), strings.Join(got, ","),
				)
			}
		}
	}
}

// TestGetRequestsOrderIsCanonical pins the sorted contract: namespaced,
// alphabetical by emitted name. Sorting (rather than "whatever range gives")
// is what makes the order stable ACROSS processes, restarts and versions.
func TestGetRequestsOrderIsCanonical(t *testing.T) {
	model := utils.Model{Tools: selectionKeys()}
	names := requestNames(model)
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("tools not sorted: index %d %q > index %d %q", i-1, names[i-1], i, names[i])
		}
	}
}
