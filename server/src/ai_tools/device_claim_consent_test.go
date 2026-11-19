package ai_tools

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The consent gate for the device channel (docs/device-channel.md).
//
// A background run now puts the user's live claim into
// ChatPayload.ClientSideTools, so the app decides WHICH device tools exist —
// not the preset. GetRequests' selection filtering is therefore the gate that
// keeps a device tool invisible to a run whose model.Tools does not enable it,
// and this test pins it: the filtering itself must not be relaxed for claimed
// tools, and a claimed tool the model enables is emitted exactly once (the
// client's enriched definition, not the Registry stub).

func claimDef(names ...string) []utils.FunctionToolsRequest {
	out := make([]utils.FunctionToolsRequest, 0, len(names))
	for _, n := range names {
		out = append(out, utils.FunctionToolsRequest{Name: n, Description: "client enriched " + n})
	}
	return out
}

func emittedNames(reqs []utils.ToolsRequest) []string {
	names := make([]string, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, r.Function.Name)
	}
	return names
}

func TestClaimedToolIsGatedByModelSelection(t *testing.T) {
	claimed := claimDef("shell_client__exec", "filesystem_client__fs_read")

	// The preset enables only the device shell. The claim also advertises the
	// device filesystem tool — it must NOT reach the LLM.
	model := utils.Model{Name: "m", Tools: map[string]string{
		"shell_client__exec": "true",
		"search_web":         "true",
	}}
	got := emittedNames(GetRequests("consent-user", model, claimed, false, false, false, false))

	seen := map[string]int{}
	for _, n := range got {
		seen[n]++
	}
	if seen["shell_client__exec"] != 1 {
		t.Fatalf("enabled claimed tool must appear exactly once, got %d in %v", seen["shell_client__exec"], got)
	}
	if seen["filesystem_client__fs_read"] != 0 {
		t.Fatalf("a claimed tool the model selection does not enable must not be advertised: %v", got)
	}
	// A claimed tool that is DISABLED also lands in the disabled-tools
	// summary (it is available, just off) — that is the pre-existing contract.
	summary := GetDisabledToolsSummary("consent-user", model, claimed)
	if !strings.Contains(summary, "filesystem_client__fs_read") {
		t.Fatalf("disabled claimed tool should be listed as available-but-disabled: %s", summary)
	}
	if strings.Contains(summary, "shell_client__exec") {
		t.Fatalf("enabled tool must not be listed as disabled: %s", summary)
	}
}

// The client's enriched definition wins over the Registry stub, and the emitted
// order stays canonical — the claim list arrives sorted from
// ai.DeviceRegistry.ToolsForUser, and GetRequests re-sorts the selected subset
// (KV prompt-cache contract, see registry.go).
func TestClaimedToolsAreEmittedInCanonicalOrder(t *testing.T) {
	sel := map[string]string{
		"shell_client__exec":          "true",
		"filesystem_client__fs_read":  "true",
		"filesystem_client__fs_write": "true",
	}
	for i := 0; i < 20; i++ {
		// Unsorted input (a claim from an app that posted them in any order).
		names := emittedNames(GetRequests("consent-user", utils.Model{Tools: sel}, claimDef(
			"shell_client__exec", "filesystem_client__fs_write", "filesystem_client__fs_read"), false, false, false, false))
		var device []string
		for _, n := range names {
			if strings.HasPrefix(n, "shell_client__") || strings.HasPrefix(n, "filesystem_client__") {
				device = append(device, n)
			}
		}
		want := []string{"filesystem_client__fs_read", "filesystem_client__fs_write", "shell_client__exec"}
		if len(device) != len(want) {
			t.Fatalf("iteration %d: device tools %v, want %v", i, device, want)
		}
		for j := range want {
			if device[j] != want[j] {
				t.Fatalf("iteration %d: device tools not sorted: %v (want %v)", i, device, want)
			}
		}
	}
}
