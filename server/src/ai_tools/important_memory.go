package ai_tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/azukaar/plurality/src/memory"
	"github.com/azukaar/plurality/src/utils"
)

const UpdateImportantMemoryToolID = "update_important_memory"

// ImportantMemoryMaxChars caps the size of the GLOBAL important_memory
// snippet. The snippet is injected verbatim into the system prompt of EVERY
// conversation (byte-stable prefix for KV prompt caching), so it must stay
// compact. ~2.5k chars is roughly 600-700 tokens — several orders of
// magnitude below the context window, while still plenty for durable user
// facts. This is an ENFORCED contract, not a suggestion: the tool rejects
// oversized writes at the server instead of silently bloating every prompt.
const ImportantMemoryMaxChars = 2500

// importantMemoryScratchPatterns detect per-conversation task state that
// must never live in the global important_memory. Patterns are deliberately
// NARROW and line-anchored to keep false positives near zero: we only reject
// content that is unambiguously a task-log / clock (the observed failure mode
// where agents used the global memory as a progress scratch-pad: DONE/NEXT/
// TODO/CUTOVER lines, "- [ ]" checkboxes, self-maintained clocks). Borderline
// prose that happens to contain "progress" or "update" as words is allowed.
var importantMemoryScratchPatterns = []*regexp.Regexp{
	// Checklist-style task-log line: "DONE: ...", "NEXT: ...", "TODO: ...",
	// "CUTOVER: ...", "Step 2: ..." (with any of :, ., -, or whitespace after)
	regexp.MustCompile(`(?im)^\s*(?:done|next|todo|todos|cutover|in.?progress|wip|step\s*\d+)\s*[:.\-]`),
	// Markdown checkbox line: "- [ ] ..." / "- [x] ..."
	regexp.MustCompile(`(?im)^\s*[-*]\s*\[[ xX]\]`),
	// Line that is just a self-maintained clock, e.g. "14:32" or "14:32 UTC"
	regexp.MustCompile(`(?im)^\s*\d{1,2}:\d{2}\b`),
	// "current time: 14:32" / "as of 14:32" / "last updated: Nov 3"
	regexp.MustCompile(`(?i)\b(?:current time|as of|last updated|updated at)\s*[:=]?\s*\d{1,2}:\d{2}\b`),
	// "today is Nov 3" / "today's date: 2026-11-03" / "date: 2026-11-03"
	regexp.MustCompile(`(?i)\b(?:today\s+is\s+\w{3,9}\s+\d{1,2}|(?:today(?:'s date)?|date)\s*[:=]?\s*(?:\w{3,9}\s+\d{1,2}|\d{4}-\d{2}-\d{2}))`),
}

func memoryToolError(msg string) utils.MessageContent {
	b, _ := json.Marshal(map[string]string{"status": "error", "error": msg})
	return utils.NewTextContent(string(b))
}

// sanitizeImportantMemory enforces the "global durable facts only" contract:
//  1. Blank/whitespace-only content is rejected (the snippet is injected into
//     every system prompt; storing empty lines is pointless and pads the
//     stable prefix).
//  2. Oversized content is rejected (see ImportantMemoryMaxChars).
//  3. Content that looks like per-conversation task state (scratch progress
//     logs, clocks, step-by-step commands) is rejected and explained — the
//     model is told to keep that state in the conversation / long_task
//     tool instead.
//
// The returned string is the (unchanged) content when it is acceptable, or
// an empty string plus a descriptive error via *blockReason when the write
// must be refused. Nothing here modifies the caller's data; the tool's Exec
// decides whether to persist.
func sanitizeImportantMemory(content string) (string, string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", "important_memory cannot be empty/blank. Keep durable user facts only (name, contacts, preferences, standing rules); task state belongs in the conversation (long_task checklist)."
	}
	if len([]rune(content)) > ImportantMemoryMaxChars {
		return "", "important_memory is limited to " + strconv.Itoa(ImportantMemoryMaxChars) + " characters (~600-700 tokens) because it is injected into EVERY conversation's system prompt. Please keep only durable facts about the user and move task details into the conversation (long_task checklist / files)."
	}
	for _, re := range importantMemoryScratchPatterns {
		if re.MatchString(content) {
			return "", "important_memory must contain only durable global facts about the user (name, contacts, preferences, standing rules). The proposed content looks like per-conversation task state (progress logs, timestamps/clocks, step-by-step commands) which must NOT go into the global memory — keep it in this conversation (long_task checklist) instead. Rewrite the memory to durable facts only."
		}
	}
	return content, ""
}




// UpdateImportantMemoryTool lets the assistant overwrite the user's GLOBAL
// durable memory snippet (name, contacts, preferences, standing rules).
// It is injected into the system prompt of EVERY conversation, so it must
// stay compact and durable — never per-conversation task state.
var UpdateImportantMemoryTool = utils.AITool{
	Name:              "Update Important Memory",
	Description:       "Overwrite the user's GLOBAL important_memory snippet - the only memory that is visible in EVERY conversation. It must contain only durable facts about the user that apply to ALL conversations (name, email, phone, preferences, standing constraints, high-level stable projects).",
	ToolID:            UpdateImportantMemoryToolID,
	PickerLabel:       "Important Memory",
	PickerDescription: "Maintain the user's GLOBAL memory: durable facts (name, contacts, preferences, standing rules) shown in every conversation. NOT for task progress - use the task list / long_task tool inside the conversation for that.",
	PickerDefault:     "on",
	PickerOrder:       45,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name: UpdateImportantMemoryToolID,
			Description: `Overwrite the GLOBAL important_memory snippet that is injected verbatim into the system prompt of EVERY conversation. The tool is write-only: you cannot read the value back, but the current contents are always visible in the system prompt.

Store ONLY durable facts about the user that apply to ALL conversations: name, email, phone, address, preferences, standing constraints/rules, stable high-level projects (e.g. 'homelab with pve01/pve02'). Keep it to a few short lines.

NEVER store in here: task progress (DONE / NEXT / TODO / CUTOVER steps), current working state, timestamps or clocks ('current time...'), step-by-step commands, file/infra details of one specific job, or anything that only matters to the conversation you are in right now. That kind of state belongs in the conversation itself - use the long_task checklist for todo steps and leave the details in the chat history. If what you are about to write is mostly task state, DO NOT update important_memory; keep it in the conversation instead.

The server ENFORCES this contract: writes that are empty, longer than ` + "2500" + ` characters, or that look like task-scratch (DONE/NEXT/TODO timestamps, clocks, step numbers) are rejected with an error. Aim for a few short lines of durable facts.

Before overwriting, preserve anything still true from the previous content that you want to keep (durable facts only); drop task-specific and time-specific lines.`,
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"content": {
						Type:        "string",
						Description: "The new full content of the GLOBAL memory. This REPLACES the previous content entirely. Durable user facts only - no task progress, no timestamps, no per-conversation state. Maximum " + "2500" + " characters.",
					},
				},
				Required: []string{"content"},
			},
		},
	},
	LoadingString: "Updating important memory",
	Exec: func(ctx context.Context, args string, _ utils.Conversation) utils.MessageContent {
		var p struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return memoryToolError("invalid arguments: " + err.Error())
		}
		userID, _ := ctx.Value("userID").(string)
		if userID == "" {
			return memoryToolError("no user in context")
		}
		clean, blockReason := sanitizeImportantMemory(p.Content)
		if blockReason != "" {
			// The model wrote task state into the global memory — refuse and
			// explain. The user's previous memory is left untouched.
			return memoryToolError(blockReason)
		}
		if err := memory.Set(userID, clean); err != nil {
			return memoryToolError(err.Error())
		}
		out, _ := json.Marshal(map[string]string{"status": "updated"})
		return utils.NewTextContent(string(out))
	},
}
