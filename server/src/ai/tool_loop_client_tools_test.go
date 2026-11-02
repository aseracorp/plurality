package ai

import (
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// A client tool the client did not advertise in this request can never be
// executed: the device has no handler registered for it. This is the
// "wrong tool call" case — the model hallucinates a name (seen in
// production: `write_file` and `filesystem_server__fs_edit` from
// strata-swift when the real tools are fs_read / fs_write), categorize
// routes it to the client, and the loop would otherwise park for the full
// 90s stall timeout before synthesizing a generic "did not respond"
// result. The advertised-name check lets the loop fail these fast with a
// precise error so the model self-corrects on the next iteration.
func TestClientToolIsAdvertised(t *testing.T) {
	advertised := []utils.FunctionToolsRequest{
		{Name: "filesystem_client__fs_read"},
		{Name: "filesystem_client__fs_write"},
		{Name: "retrieve_skill"},
		{Name: "shell_client__exec"},
	}

	cases := []struct {
		name string
		tool string
		want bool
	}{
		{"exact advertised read", "filesystem_client__fs_read", true},
		{"exact advertised write", "filesystem_client__fs_write", true},
		{"skill tool", "retrieve_skill", true},
		{"shell on desktop", "shell_client__exec", true},
		{"hallucinated bare write_file", "write_file", false},
		{"hallucinated fs_edit", "filesystem_server__fs_edit", false},
		{"near-miss server read", "filesystem_server__fs_read", false},
		{"empty string", "", false},
		{"no tools advertised at all", "anything", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientToolIsAdvertised(tc.tool, advertised); got != tc.want {
				t.Fatalf("clientToolIsAdvertised(%q) = %v, want %v", tc.tool, got, tc.want)
			}
		})
	}

	// Empty advertisement list: nothing is executable client-side, so
	// everything must fail fast (this is the watchdog-resume case where
	// capabilities are not carried over).
	if clientToolIsAdvertised("shell_client__exec", nil) {
		t.Fatal("clientToolIsAdvertised must be false when no tools are advertised")
	}
}
