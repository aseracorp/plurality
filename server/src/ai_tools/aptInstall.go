package ai_tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// AptInstallTool allows the AI to install packages via apt.
var AptInstallTool = utils.AITool{
	Name:              "Apt Install",
	Description:       "Install packages via apt",
	ToolID:            "apt_install",
	BundleName:        "system_tools",
	PickerLabel:       "Apt Install",
	PickerDescription: "Install system packages via apt-get",
	PickerDefault:     "ask",
	PickerOrder:       120,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "apt_install",
			Description: "Install system packages using apt-get. Note: May require root privileges depending on server configuration.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"packages": {
						Type:        "string",
						Description: "Space-separated list of packages to install (e.g., 'curl wget htop')",
					},
				},
				Required: []string{"packages"},
			},
		},
	},
	LoadingString: "Installing packages...",
	IconURL:       "",
	Exec:          execAptInstall,
}

func execAptInstall(ctx context.Context, input string, conv utils.Conversation) utils.MessageContent {
	var params struct {
		Packages string `json:"packages"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	if params.Packages == "" {
		return utils.NewTextContent("Error: 'packages' parameter is required.")
	}

	// Sanitize package names - only allow alphanumeric, dash, underscore, dot, plus
	packages := strings.Fields(params.Packages)
	for _, pkg := range packages {
		for _, c := range pkg {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '+') {
				return utils.NewTextContent(fmt.Sprintf("Error: invalid character in package name '%s'", pkg))
			}
		}
	}

	// Build command - try without sudo first, fall back to sudo if available
	args := append([]string{"apt-get", "install", "-y"}, packages...)

	var stdout, stderr bytes.Buffer

	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, args[0], args[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(cmd.Environ(), "DEBIAN_FRONTEND=noninteractive")

	err := cmd.Run()

	// Build response
	var result strings.Builder
	result.WriteString(fmt.Sprintf("Packages: %s\n\n", params.Packages))

	if stdout.Len() > 0 {
		out := stdout.String()
		if len(out) > 20000 {
			out = out[:20000] + "\n... (truncated)"
		}
		result.WriteString("--- Output ---\n")
		result.WriteString(out)
	}

	if stderr.Len() > 0 {
		errOut := stderr.String()
		if len(errOut) > 5000 {
			errOut = errOut[:5000] + "\n... (truncated)"
		}
		result.WriteString("\n--- Errors ---\n")
		result.WriteString(errOut)
	}

	if err != nil {
		result.WriteString(fmt.Sprintf("\n\nInstallation failed: %s", err.Error()))
		result.WriteString("\nNote: The server runs as non-root user. Package installation requires rebuilding the container image.")
	} else {
		result.WriteString("\n\nInstallation completed successfully.")
	}

	return utils.NewTextContent(result.String())
}
