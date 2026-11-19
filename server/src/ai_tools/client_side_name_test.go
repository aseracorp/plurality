package ai_tools

import "testing"

// A device tool the loop cannot reach is a very different failure from a name
// the model invented, and the error text must not blame the model for it.
// Production (2026-11-19, cron chain for the Outlook bridge): a background run
// called shell_client__exec, the tool was not advertised (jobs.RunPrompt builds
// a ChatPayload without ClientSideTools — a cron run has no stream to deliver
// a tool_use to), and the generic "the model may have invented the name" answer
// led the agent to conclude the Windows app was offline. It was running and
// logged in. IsClientSideToolName is what lets the loop name the real cause.
func TestIsClientSideToolName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// The device shell, both as the registry id and as the name the LLM sees.
		{"exec", true},
		{"shell_client__exec", true},
		// Server-side tools are executable in-process, never device tools.
		{"fs_read", false},
		{"filesystem_server__fs_read", false},
		{"fs_write", false},
		{"filesystem_server__fs_write", false},
		{"shell_exec", false},
		{"system_tools__shell_exec", false},
		{"search_web", false},
		// MCP names are not registry entries at all.
		{"github__get_me", false},
		{"outlook__outlook_get_messages", false},
		// Junk the model can produce.
		{"write_file", false},
		{"", false},
		{"__exec", false},
		{"nonexistent__tool", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsClientSideToolName(tc.name); got != tc.want {
				t.Fatalf("IsClientSideToolName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// The namespaced branch must not fire on a name whose prefix is NOT the tool's
// bundle: a bare registry id reached through some other namespace stays a
// non-device tool, so the loop keeps the "invented the name" wording for it.
func TestIsClientSideToolNameRequiresBundleMatch(t *testing.T) {
	if IsClientSideToolName("filesystem_server__exec") {
		t.Fatal(`"filesystem_server__exec" must not classify as a device tool: exec's bundle is shell_client`)
	}
	if !IsClientSideToolName("shell_client__exec") {
		t.Fatal(`"shell_client__exec" must classify as a device tool`)
	}
}
