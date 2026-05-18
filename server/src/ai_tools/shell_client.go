package ai_tools

import (
	"github.com/azukaar/plurality/src/utils"
)

// ShellClientExecTool runs shell commands on the user's *device* (not the
// server). The server never executes this tool — it's marked ClientSide so the
// tool loop routes calls to the connected Flutter client, which dispatches to
// PowerShell on Windows or sh on macOS/Linux.
//
// Parameter shape mirrors shellExec.go's ShellExecTool so the LLM sees an
// identical contract regardless of whether the call landed on the server or
// the client. The description here is a generic fallback; the client overrides
// it with a richer environment block (OS, default shell, attached folder, git
// branch, platform gotchas) when it advertises the tool in clientSideTools.
var ShellClientExecTool = utils.AITool{
	Name:              "Shell Execute (Device)",
	Description:       "Execute shell commands on the user's device",
	ToolID:            "exec",
	BundleName:        "shell_client",
	Cost:              0,
	PickerLabel:       "Shell Execute (Device)",
	PickerDescription: "Run shell commands on the user's device (PowerShell on Windows, sh on macOS/Linux)",
	PickerDefault:     "ask",
	PickerOrder:       115,
	ClientSide:        true,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "shell_client__exec",
			Description: "[device] Execute a shell command on the USER'S DEVICE and return its output. The user's OS, default shell, and platform-specific syntax gotchas are filled in by the client at request time — when this generic description is showing, the client hasn't enriched it and you should ask the user about their OS before writing platform-specific commands. Operation 'exec' (default) runs synchronously with a 60s timeout. For long-running processes use 'start' to spawn a background task, then 'status' to poll, 'kill' to terminate, and 'list' to see all tasks. Background tasks live in client memory only — a client restart loses them.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"operation": {
						Type:        "string",
						Description: "What to do. 'exec' (default) runs the command and waits up to 60s. 'start' spawns a background task and returns a task_id immediately. 'status' returns state + tail of output for a task_id. 'kill' terminates a task_id. 'list' returns all known tasks.",
						Enum:        []string{"exec", "start", "status", "kill", "list"},
					},
					"command": {
						Type:        "string",
						Description: "The shell command to execute. Required for 'exec' and 'start'.",
					},
					"pwd": {
						Type:        "string",
						Description: "Working directory. Absolute paths used as-is; relative paths resolve against the attached folder if any, else the user's home directory.",
					},
					"task_id": {
						Type:        "string",
						Description: "Task identifier returned by 'start'. Required for 'status' and 'kill'.",
					},
					"tail_bytes": {
						Type:        "integer",
						Description: "How many bytes of the most recent stdout/stderr to return on 'status' (default 4096). Older bytes beyond ~1 MiB stdout / 256 KiB stderr are not retained.",
					},
				},
			},
		},
	},
	LoadingString: "Executing command on device...",
	IconURL:       "",
	// No Exec — execution happens on the client. The tool loop sees
	// ClientSide: true and routes the call to the device.
}
