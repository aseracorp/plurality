package ai_tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

const defaultStatusTailBytes = 4096

// ShellExecTool allows the AI to execute shell commands. Commands can be
// blocking (`exec`, default) or detached background tasks (`start`/`status`/
// `kill`/`list`) so long-running processes don't burn a full turn.
var ShellExecTool = utils.AITool{
	Name:              "Shell Execute",
	Description:       "Execute shell commands, with optional background tasks",
	ToolID:            "shell_exec",
	BundleName:        "system_tools",
	Cost:              0,
	PickerLabel:       "Shell Execute",
	PickerDescription: "Execute shell commands on the server",
	PickerDefault:     "ask",
	PickerOrder:       110,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "shell_exec",
			Description: "Execute a shell command and return its output. Default operation 'exec' runs the command synchronously with a 60s timeout — use it for quick one-shot commands. For processes that won't finish in seconds (servers, watchers, builds, training jobs, log tails), use 'start' to spawn a detached background task, then 'status' to poll output, 'kill' to terminate, and 'list' to see all known tasks. Background tasks live in memory only — a server restart loses them. Commands run with server permissions; use with caution.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"operation": {
						Type:        "string",
						Description: "What to do. 'exec' (default) runs the command and waits up to 60s. 'start' spawns a background task and returns a task_id immediately. 'status' returns state + tail of output for a task_id. 'kill' terminates a task_id. 'list' returns all known tasks. Omit for backward-compatible blocking exec.",
						Enum:        []string{"exec", "start", "status", "kill", "list"},
					},
					"command": {
						Type:        "string",
						Description: "The shell command to execute. Required for 'exec' and 'start'.",
					},
					"pwd": {
						Type:        "string",
						Description: "Working directory for the command (optional, applies to 'exec' and 'start'; defaults to server directory).",
					},
					"task_id": {
						Type:        "string",
						Description: "Task identifier returned by 'start'. Required for 'status' and 'kill'.",
					},
					"tail_bytes": {
						Type:        "integer",
						Description: "How many bytes of the most recent stdout/stderr to return on 'status' (default 4096). Set higher to read more history; older bytes beyond ~1 MiB stdout / 256 KiB stderr are not retained.",
					},
				},
				// Intentionally no Required — different operations need different args.
				// Validation is done in the handler so error messages can be specific.
			},
		},
	},
	LoadingString: "Executing command...",
	IconURL:       "",
	Exec:          execShellExec,
}

type shellExecParams struct {
	Operation string `json:"operation"`
	Command   string `json:"command"`
	Pwd       string `json:"pwd"`
	TaskID    string `json:"task_id"`
	TailBytes int    `json:"tail_bytes"`
}

func execShellExec(ctx context.Context, input string, _ utils.Conversation) utils.MessageContent {
	var params shellExecParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	op := params.Operation
	if op == "" {
		op = "exec"
	}

	switch op {
	case "exec":
		return runShellForeground(ctx, params)
	case "start":
		return runShellStart(params)
	case "status":
		return runShellStatus(params)
	case "kill":
		return runShellKill(params)
	case "list":
		return runShellList()
	default:
		return utils.NewTextContent(fmt.Sprintf("Error: unknown operation %q. Use one of: exec, start, status, kill, list.", op))
	}
}

func runShellForeground(ctx context.Context, params shellExecParams) utils.MessageContent {
	if params.Command == "" {
		return utils.NewTextContent("Error: 'command' parameter is required for operation 'exec'.")
	}

	// Create command based on OS
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", params.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", params.Command)
	}

	if params.Pwd != "" {
		cmd.Dir = params.Pwd
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	timeoutCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd.WaitDelay = 60 * 5 * time.Second

	startTime := time.Now()
	err := cmd.Start()
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error starting command: %s", err.Error()))
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	timedOut := false
	select {
	case <-timeoutCtx.Done():
		timedOut = true
		cmd.Process.Kill()
		<-done
	case err = <-done:
	}

	duration := time.Since(startTime)

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Command: %s\n", params.Command))
	if params.Pwd != "" {
		result.WriteString(fmt.Sprintf("Working directory: %s\n", params.Pwd))
	}
	result.WriteString(fmt.Sprintf("Duration: %s\n", duration.Round(time.Millisecond)))
	if timedOut {
		result.WriteString("Status: TIMED OUT after 60 seconds (process killed, partial output below). For long-running processes use operation 'start' instead.\n")
	} else {
		result.WriteString(fmt.Sprintf("Exit code: %d\n", cmd.ProcessState.ExitCode()))
	}
	result.WriteString("\n--- STDOUT ---\n")
	if stdout.Len() > 0 {
		out := stdout.String()
		if len(out) > 50000 {
			out = out[:50000] + "\n... (truncated)"
		}
		result.WriteString(out)
	} else {
		result.WriteString("(empty)")
	}
	result.WriteString("\n\n--- STDERR ---\n")
	if stderr.Len() > 0 {
		errOut := stderr.String()
		if len(errOut) > 10000 {
			errOut = errOut[:10000] + "\n... (truncated)"
		}
		result.WriteString(errOut)
	} else {
		result.WriteString("(empty)")
	}

	if err != nil && !timedOut {
		result.WriteString(fmt.Sprintf("\n\nError: %s", err.Error()))
	}

	return utils.NewTextContent(result.String())
}

func runShellStart(params shellExecParams) utils.MessageContent {
	if params.Command == "" {
		return utils.NewTextContent("Error: 'command' parameter is required for operation 'start'.")
	}
	bp, err := registerBackground(params.Command, params.Pwd)
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error starting background task: %s", err.Error()))
	}
	snap := bp.snapshot()
	var result strings.Builder
	result.WriteString("Background task started.\n")
	result.WriteString(fmt.Sprintf("task_id: %s\n", snap.TaskID))
	result.WriteString(fmt.Sprintf("pid: %d\n", snap.PID))
	result.WriteString(fmt.Sprintf("command: %s\n", snap.Command))
	if snap.Pwd != "" {
		result.WriteString(fmt.Sprintf("working directory: %s\n", snap.Pwd))
	}
	result.WriteString(fmt.Sprintf("started_at: %s\n", snap.StartedAt.Format(time.RFC3339)))
	result.WriteString("\nUse operation 'status' with this task_id to read output, or 'kill' to terminate it.")
	return utils.NewTextContent(result.String())
}

func runShellStatus(params shellExecParams) utils.MessageContent {
	if params.TaskID == "" {
		return utils.NewTextContent("Error: 'task_id' parameter is required for operation 'status'.")
	}
	bp, ok := getBackground(params.TaskID)
	if !ok {
		return utils.NewTextContent(fmt.Sprintf("Error: no background task with task_id %q. It may have been garbage-collected (tasks are retained for 1 hour after completion) or the server was restarted.", params.TaskID))
	}
	tail := params.TailBytes
	if tail <= 0 {
		tail = defaultStatusTailBytes
	}
	return utils.NewTextContent(formatBackgroundStatus(bp, tail))
}

func runShellKill(params shellExecParams) utils.MessageContent {
	if params.TaskID == "" {
		return utils.NewTextContent("Error: 'task_id' parameter is required for operation 'kill'.")
	}
	snap, ok := killBackground(params.TaskID, 2*time.Second)
	if !ok {
		return utils.NewTextContent(fmt.Sprintf("Error: no background task with task_id %q.", params.TaskID))
	}
	bp, _ := getBackground(params.TaskID)
	if bp == nil {
		// Should not happen — killBackground returned ok — but defensive.
		var result strings.Builder
		result.WriteString(fmt.Sprintf("task_id: %s\n", snap.TaskID))
		result.WriteString(fmt.Sprintf("state: %s\n", snap.State))
		return utils.NewTextContent(result.String())
	}
	return utils.NewTextContent("Kill requested.\n\n" + formatBackgroundStatus(bp, defaultStatusTailBytes))
}

func runShellList() utils.MessageContent {
	tasks := listBackground()
	if len(tasks) == 0 {
		return utils.NewTextContent("No background tasks. Use operation 'start' to launch one.")
	}
	var result strings.Builder
	result.WriteString(fmt.Sprintf("Known background tasks (%d):\n", len(tasks)))
	for _, t := range tasks {
		exitStr := ""
		if t.State != "running" {
			exitStr = fmt.Sprintf(" exit=%d", t.ExitCode)
		}
		result.WriteString(fmt.Sprintf("- %s [%s%s] (%s) pid=%d cmd=%q\n",
			t.TaskID, t.State, exitStr, t.Duration.Round(time.Millisecond), t.PID, truncate(t.Command, 120)))
	}
	result.WriteString("\nUse 'status' with a task_id to read output.")
	return utils.NewTextContent(result.String())
}

func formatBackgroundStatus(bp *bgProcess, tailBytes int) string {
	snap := bp.snapshot()
	stdoutTail := bp.stdout.Tail(tailBytes)
	stderrTail := bp.stderr.Tail(tailBytes)
	stdoutLen := bp.stdout.Len()
	stderrLen := bp.stderr.Len()

	var result strings.Builder
	result.WriteString(fmt.Sprintf("task_id: %s\n", snap.TaskID))
	result.WriteString(fmt.Sprintf("command: %s\n", snap.Command))
	if snap.Pwd != "" {
		result.WriteString(fmt.Sprintf("working directory: %s\n", snap.Pwd))
	}
	result.WriteString(fmt.Sprintf("pid: %d\n", snap.PID))
	result.WriteString(fmt.Sprintf("state: %s\n", snap.State))
	result.WriteString(fmt.Sprintf("duration: %s\n", snap.Duration.Round(time.Millisecond)))
	if snap.State != "running" {
		result.WriteString(fmt.Sprintf("exit code: %d\n", snap.ExitCode))
		if snap.ErrMsg != "" {
			result.WriteString(fmt.Sprintf("error: %s\n", snap.ErrMsg))
		}
		result.WriteString(fmt.Sprintf("ended_at: %s\n", snap.EndedAt.Format(time.RFC3339)))
	}

	result.WriteString(fmt.Sprintf("\n--- STDOUT (showing last %d of %d bytes) ---\n", len(stdoutTail), stdoutLen))
	if stdoutLen > 0 {
		out := stdoutTail
		if len(out) > 50000 {
			out = out[len(out)-50000:]
			result.WriteString("(truncated to last 50000 bytes for response size)\n")
		}
		result.WriteString(out)
	} else {
		result.WriteString("(empty)")
	}
	result.WriteString(fmt.Sprintf("\n\n--- STDERR (showing last %d of %d bytes) ---\n", len(stderrTail), stderrLen))
	if stderrLen > 0 {
		errOut := stderrTail
		if len(errOut) > 10000 {
			errOut = errOut[len(errOut)-10000:]
			result.WriteString("(truncated to last 10000 bytes for response size)\n")
		}
		result.WriteString(errOut)
	} else {
		result.WriteString("(empty)")
	}
	return result.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
