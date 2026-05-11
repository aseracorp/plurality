package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// LongTaskMaxReminders caps how many times the end-of-turn reminder can fire
// per conversation. Without a cap a model that refuses to either complete or
// clear pending tasks could loop indefinitely.
const LongTaskMaxReminders = 5

// LongTaskItem is one entry in the checklist.
type LongTaskItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

// LongTaskState is the full persisted state, serialized as the tool's result
// body. Reading the latest long_task tool message and unmarshaling it yields
// the current state.
type LongTaskState struct {
	Tasks         []LongTaskItem `json:"tasks"`
	RemindersUsed int            `json:"reminders_used"`
	Nudge         string         `json:"nudge,omitempty"`
	// Paused suppresses the end-of-turn reminder loop. The task list itself
	// stays intact so the AI can resume it later. PauseReason is a short
	// free-text explanation surfaced to the user in the badge.
	Paused      bool   `json:"paused,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
}

var LongTaskTool = utils.AITool{
	Name:              "Long Task",
	Description:       "Maintain a checklist of subtasks for multi-step requests. Call 'set' once at the start with the full plan, then 'complete' tasks as you finish each one. If you stop while items are still open, the system will remind you to keep going — call 'clear' to drop them, or 'pause' (with optional 'reason') if you can't continue right now but want to come back later. Use 'resume' to re-enable a paused list.",
	ToolID:            "long_task",
	Cost:              0,
	PickerLabel:       "Long Task",
	PickerDescription: "Maintain a checklist of subtasks for multi-step requests",
	PickerDefault:     "on",
	PickerOrder:       60,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "long_task",
			Description: "Manage a persistent checklist of subtasks. Use this for any request that involves more than one step so progress stays visible. Operations: 'set' replaces the entire list, 'add' appends, 'complete' marks ids done, 'remove' deletes ids, 'clear' wipes everything, 'pause' suspends the reminder loop while keeping the list intact (use when you genuinely cannot continue right now — e.g. blocked on user input or an external event — and want to come back to it later; combine with manage_cron if you need an automatic wake-up), 'resume' re-enables a paused list. Whenever you finish a turn with unfinished, non-paused tasks the system will inject a reminder telling you to keep working.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"operation": {
						Type:        "string",
						Description: "Operation to perform on the checklist.",
						Enum:        []string{"set", "add", "complete", "remove", "clear", "pause", "resume"},
					},
					"titles": {
						Type:        "string",
						Description: "JSON array of task titles. Required for 'set' and 'add'. Example: [\"Read code\",\"Write plan\",\"Implement\"].",
					},
					"ids": {
						Type:        "string",
						Description: "JSON array of task ids. Required for 'complete' and 'remove'. Example: [\"t1\",\"t2\"]. Ids are returned by the tool when you 'set' or 'add' tasks.",
					},
					"reason": {
						Type:        "string",
						Description: "Optional short reason for 'pause' — e.g. \"waiting on user reply\" or \"blocked on deploy at 5pm\". Shown to the user.",
					},
				},
				Required: []string{"operation"},
			},
		},
	},
	LoadingString: "Updating task list ({{operation}})",
	Exec:          execLongTask,
}

// ReadLongTaskState scans the messages from newest to oldest and returns the
// most recent long_task state. If no prior call exists it returns a zero
// state. Used by both Exec (to apply ops on top of current state) and the
// end-of-turn reminder logic in tool_loop.
func ReadLongTaskState(messages []utils.Message) LongTaskState {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "tool" || m.Name != "long_task" {
			continue
		}
		var s LongTaskState
		if err := json.Unmarshal([]byte(m.TextContent()), &s); err != nil {
			continue
		}
		return s
	}
	return LongTaskState{Tasks: []LongTaskItem{}, RemindersUsed: 0}
}

// HasOutstanding returns true when at least one task is still open.
func (s LongTaskState) HasOutstanding() bool {
	for _, t := range s.Tasks {
		if !t.Done {
			return true
		}
	}
	return false
}

// FormatStateJSON serialises a state object back to canonical JSON text.
func FormatStateJSON(s LongTaskState) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `{"tasks":[],"reminders_used":0}`
	}
	return string(b)
}

func execLongTask(_ context.Context, input string, conv utils.Conversation) utils.MessageContent {
	var params struct {
		Operation string `json:"operation"`
		Titles    string `json:"titles"`
		IDs       string `json:"ids"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
	}

	state := ReadLongTaskState(conv.Messages)
	// The result of a normal user-driven operation always resets the transient
	// 'nudge' field — it's only meaningful inside reminder messages.
	state.Nudge = ""

	switch params.Operation {
	case "set":
		titles, err := decodeStringArray(params.Titles)
		if err != nil {
			return errResult(err.Error())
		}
		state.Tasks = state.Tasks[:0]
		nextID := 1
		for _, title := range titles {
			title = strings.TrimSpace(title)
			if title == "" {
				continue
			}
			state.Tasks = append(state.Tasks, LongTaskItem{
				ID:    "t" + strconv.Itoa(nextID),
				Title: title,
			})
			nextID++
		}
		// Reset reminders + pause flag when the list is reset — a fresh
		// 'set' is always an active plan.
		state.RemindersUsed = 0
		state.Paused = false
		state.PauseReason = ""

	case "add":
		titles, err := decodeStringArray(params.Titles)
		if err != nil {
			return errResult(err.Error())
		}
		nextID := nextTaskID(state.Tasks)
		for _, title := range titles {
			title = strings.TrimSpace(title)
			if title == "" {
				continue
			}
			state.Tasks = append(state.Tasks, LongTaskItem{
				ID:    "t" + strconv.Itoa(nextID),
				Title: title,
			})
			nextID++
		}

	case "complete":
		ids, err := decodeStringArray(params.IDs)
		if err != nil {
			return errResult(err.Error())
		}
		idSet := map[string]bool{}
		for _, id := range ids {
			idSet[strings.TrimSpace(id)] = true
		}
		var missing []string
		for id := range idSet {
			found := false
			for i := range state.Tasks {
				if state.Tasks[i].ID == id {
					state.Tasks[i].Done = true
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			return errResult("Unknown task id(s): " + strings.Join(missing, ", ") + ". Current state: " + FormatStateJSON(state))
		}

	case "remove":
		ids, err := decodeStringArray(params.IDs)
		if err != nil {
			return errResult(err.Error())
		}
		idSet := map[string]bool{}
		for _, id := range ids {
			idSet[strings.TrimSpace(id)] = true
		}
		filtered := state.Tasks[:0]
		for _, t := range state.Tasks {
			if !idSet[t.ID] {
				filtered = append(filtered, t)
			}
		}
		state.Tasks = filtered

	case "clear":
		state.Tasks = []LongTaskItem{}
		state.RemindersUsed = 0
		state.Paused = false
		state.PauseReason = ""

	case "pause":
		if len(state.Tasks) == 0 {
			return errResult("No task list to pause. Call 'set' first.")
		}
		if !state.HasOutstanding() {
			return errResult("All tasks are already done — nothing to pause.")
		}
		state.Paused = true
		state.PauseReason = strings.TrimSpace(params.Reason)
		// Reset the reminder count so that a later 'resume' starts with a
		// fresh budget of nudges.
		state.RemindersUsed = 0

	case "resume":
		state.Paused = false
		state.PauseReason = ""
		state.RemindersUsed = 0

	default:
		return errResult(fmt.Sprintf("Unknown operation %q. Use one of: set, add, complete, remove, clear, pause, resume.", params.Operation))
	}

	return utils.NewTextContent(FormatStateJSON(state))
}

func decodeStringArray(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return nil, fmt.Errorf("expected JSON string array, got %q: %w", raw, err)
	}
	return arr, nil
}

func nextTaskID(tasks []LongTaskItem) int {
	max := 0
	for _, t := range tasks {
		if !strings.HasPrefix(t.ID, "t") {
			continue
		}
		n, err := strconv.Atoi(t.ID[1:])
		if err == nil && n > max {
			max = n
		}
	}
	return max + 1
}

func errResult(msg string) utils.MessageContent {
	return utils.NewTextContent("Error: " + msg)
}
