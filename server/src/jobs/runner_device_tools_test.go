package jobs

import (
	"testing"
	"time"

	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/utils"
)

// Background runs advertise the user's live device claim, and a sub-agent gets
// the intersection of that claim with the parent's advertised set
// (docs/device-channel.md). With no claim there is nothing to advertise —
// which is exactly the pre-existing behaviour, and the reason the fast-fail
// text for cron device calls stays byte-identical.

func claimTool(name string) utils.FunctionToolsRequest {
	return utils.FunctionToolsRequest{Name: name, Description: name + " def"}
}

// claimAs installs a live claim for the user on the process-wide registry and
// removes it on exit.
func claimAs(t *testing.T, userID string, names ...string) {
	t.Helper()
	tools := make([]utils.FunctionToolsRequest, 0, len(names))
	for _, n := range names {
		tools = append(tools, claimTool(n))
	}
	ai.DeviceRegistry.Claim(userID, "app-"+userID, "conn-"+userID, "", tools, time.Minute)
	t.Cleanup(ai.DeviceRegistry.Reset)
}

func namesOf(tools []utils.FunctionToolsRequest) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func TestBackgroundDeviceTools(t *testing.T) {
	// No claim: a cron run advertises nothing (nil, not an empty slice, so the
	// payload is byte-identical to what RunPrompt built before this feature).
	ai.DeviceRegistry.Reset()
	if got := backgroundDeviceTools("no-claim-user"); got != nil {
		t.Fatalf("no claim must advertise nothing, got %v", namesOf(got))
	}

	claimAs(t, "u1", "shell_client__exec", "filesystem_client__fs_read")
	got := namesOf(backgroundDeviceTools("u1"))
	want := []string{"filesystem_client__fs_read", "shell_client__exec"} // sorted (prompt-cache contract)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (must be sorted by name)", got, want)
		}
	}
	// A different user's run advertises nothing.
	if got := backgroundDeviceTools("u2"); got != nil {
		t.Fatalf("claims are per-user, got %v", namesOf(got))
	}
}

// A sub-agent can never gain a device tool its parent did not advertise.
func TestSubAgentDeviceToolsIntersection(t *testing.T) {
	claimAs(t, "u1", "shell_client__exec", "filesystem_client__fs_read", "filesystem_client__fs_write")

	cases := []struct {
		name        string
		parentTools []utils.FunctionToolsRequest
		want        []string
	}{
		{
			name:        "parent had only the shell",
			parentTools: []utils.FunctionToolsRequest{claimTool("shell_client__exec")},
			want:        []string{"shell_client__exec"},
		},
		{
			name:        "parent had two, output sorted",
			parentTools: []utils.FunctionToolsRequest{claimTool("filesystem_client__fs_write"), claimTool("filesystem_client__fs_read")},
			want:        []string{"filesystem_client__fs_read", "filesystem_client__fs_write"},
		},
		{
			// The parent advertised nothing (the loop's payload is not readable
			// from outside it), so the claim passes through; model.Tools gating
			// in ai_tools.GetRequests is what keeps the child from seeing a tool
			// the parent's preset did not enable.
			name:        "no parent set known",
			parentTools: nil,
			want:        []string{"filesystem_client__fs_read", "filesystem_client__fs_write", "shell_client__exec"},
		},
		{
			name:        "parent asked for a tool the app does not claim",
			parentTools: []utils.FunctionToolsRequest{claimTool("search_web")},
			want:        nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := namesOf(subAgentDeviceTools("u1", tc.parentTools))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}

	// A background run of a user with no app connected spawns a sub-agent with
	// no device tools at all.
	ai.DeviceRegistry.Reset()
	if got := subAgentDeviceTools("nobody", []utils.FunctionToolsRequest{claimTool("shell_client__exec")}); got != nil {
		t.Fatalf("no claim -> no tools, got %v", namesOf(got))
	}
}
