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

// ShellExecTool allows the AI to execute shell commands.
var ShellExecTool = utils.AITool{
	Name:        "Shell Execute",
	Description: "Execute shell commands",
	ToolID:      "shell_exec",
	Cost:        0,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "shell_exec",
			Description: "Execute a shell command and return its output. Use with caution - commands run with server permissions.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"command": {
						Type:        "string",
						Description: "The shell command to execute",
					},
					"pwd": {
						Type:        "string",
						Description: "Working directory for the command (optional, defaults to server directory)",
					},
				},
				Required: []string{"command"},
			},
		},
	},
	LoadingString: "Executing command...",
	IconURL:       "",
	Exec:          execShellExec,
}

func execShellExec(ctx context.Context, input string, conv utils.Conversation) utils.MessageContent {
	var params struct {
		Command string `json:"command"`
		Pwd     string `json:"pwd"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	if params.Command == "" {
		return utils.NewTextContent("Error: 'command' parameter is required.")
	}

	// Create command based on OS
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", params.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", params.Command)
	}

	// Set working directory if specified
	if params.Pwd != "" {
		cmd.Dir = params.Pwd
	}

	// Capture stdout and stderr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Set timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd.WaitDelay = 60 * time.Second

	// Run command
	startTime := time.Now()
	err := cmd.Start()
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error starting command: %s", err.Error()))
	}

	// Wait with timeout
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-timeoutCtx.Done():
		cmd.Process.Kill()
		return utils.NewTextContent("Error: command timed out after 60 seconds")
	case err = <-done:
	}

	duration := time.Since(startTime)

	// Build response
	var result strings.Builder
	result.WriteString(fmt.Sprintf("Command: %s\n", params.Command))
	if params.Pwd != "" {
		result.WriteString(fmt.Sprintf("Working directory: %s\n", params.Pwd))
	}
	result.WriteString(fmt.Sprintf("Duration: %s\n", duration.Round(time.Millisecond)))
	result.WriteString(fmt.Sprintf("Exit code: %d\n", cmd.ProcessState.ExitCode()))
	result.WriteString("\n--- STDOUT ---\n")
	if stdout.Len() > 0 {
		// Limit output to prevent huge responses
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

	if err != nil {
		result.WriteString(fmt.Sprintf("\n\nError: %s", err.Error()))
	}

	return utils.NewTextContent(result.String())
}
