package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

const WaitToolID = "wait"

// MaxWaitSeconds caps the agent-side pause at one hour.
const MaxWaitSeconds = 3600

// WaitTool pauses the agent for N seconds. Exec returns immediately with the
// wake-up timestamp so the chip can render a live countdown; the LLM loop
// detects the call by name and parks itself in place (see pauseForWait in
// the ai package) until the timer fires, then injects a "Timer is done"
// signal and continues — all without dropping the SSE clients attached to
// the conversation.
//
// PickerDefault is "" so it's hidden from the picker. GetRequests force-
// includes it always — it cannot be disabled by the user.
var WaitTool = utils.AITool{
	Name:        "Wait",
	Description: "Pause the agent for a given number of seconds and resume automatically",
	ToolID:      WaitToolID,
	Cost:        0,
	// Empty PickerDefault hides it from the toggle UI. The registry force-
	// includes the tool unconditionally.
	PickerDefault: "",
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "wait",
			Description: "Pause the agent for a given number of seconds, then resume the conversation autonomously. Use when you need to wait before acting again (e.g. polling, periodic checks, deferred follow-ups). The current turn ends after this call; the agent is restarted automatically when the wait elapses and the next turn should react to whatever has changed in the meantime.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"seconds": {
						Type:        "integer",
						Description: fmt.Sprintf("Number of seconds to wait before resuming. Must be a positive integer; capped at %d.", MaxWaitSeconds),
					},
				},
				Required: []string{"seconds"},
			},
		},
	},
	LoadingString: "Waiting {{seconds}}s",
	Exec: func(_ context.Context, args string, _ utils.Conversation) utils.MessageContent {
		var params struct {
			Seconds int `json:"seconds"`
		}
		if err := json.Unmarshal([]byte(args), &params); err != nil {
			return utils.NewTextContent("Error parsing parameters: " + err.Error())
		}
		if params.Seconds <= 0 {
			return utils.NewTextContent("Error: 'seconds' must be a positive integer")
		}
		if params.Seconds > MaxWaitSeconds {
			params.Seconds = MaxWaitSeconds
		}
		wakeAt := time.Now().Add(time.Duration(params.Seconds) * time.Second).UTC()
		out := map[string]interface{}{
			"wait_seconds": params.Seconds,
			"wake_at":      wakeAt.Format(time.RFC3339),
			"status":       fmt.Sprintf("Waiting %d second(s). The agent will resume automatically.", params.Seconds),
		}
		b, _ := json.Marshal(out)
		return utils.NewTextContent(string(b))
	},
}

// ParseWaitSeconds extracts the 'seconds' field from a wait tool call's
// arguments JSON, returning the (clamped) duration in seconds, or 0 if the
// arguments don't parse or specify a non-positive value.
func ParseWaitSeconds(argsJSON string) int {
	var p struct {
		Seconds int `json:"seconds"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &p); err != nil {
		return 0
	}
	if p.Seconds <= 0 {
		return 0
	}
	if p.Seconds > MaxWaitSeconds {
		return MaxWaitSeconds
	}
	return p.Seconds
}
