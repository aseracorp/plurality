package ai_tools

import (
	"context"
	"fmt"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// CavemanRetrieveTool is the recovery half of "read less".
//
// Caveman compression is non-destructive: a tool result keeps its full body in
// messages.content and carries a deterministic projection in
// messages.compressed_content, which is what the LLM sees. When the projection
// is not enough — the model needs an exact path, a full diff, a line it can
// only see as "..." — this tool returns the untouched original from the DB.
//
// This is why compression is safe to enable at all: the model is never
// permanently deprived of information, it is only given a cheaper default view.
var CavemanRetrieveTool = utils.AITool{
	Name:              "Caveman Retrieve",
	Description:       "Retrieve the full, uncompressed body of a compressed tool result",
	ToolID:            "caveman_retrieve",
	PickerLabel:       "Caveman Retrieve",
	PickerDescription: "Read the full original of a compressed tool result",
	// Hidden from the picker: it is force-included exactly when the model has
	// the read_less stone on, so it must never appear as a user toggle for a
	// model that has nothing to recover.
	PickerDefault: "",
	PickerOrder:   100,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name: "caveman_retrieve",
			Description: "Return the FULL original of a tool result that was compressed for the model " +
				"(banners read: \"[caveman: compressed tool result ... call caveman_retrieve with tool_call_id=...]\"). " +
				"Use it whenever the compressed view is missing detail you need: exact paths, complete " +
				"diffs, specific lines, full JSON. The result is returned in pages.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"tool_call_id": {
						Type:        "string",
						Description: "The tool_call_id named in the caveman banner of the compressed tool result",
					},
					"offset": {
						Type:        "integer",
						Description: "Character offset into the original (default 0)",
					},
					"limit": {
						Type:        "integer",
						Description: "Max characters to return (default 20000, max 100000)",
					},
				},
				Required: []string{"tool_call_id"},
			},
		},
	},
	LoadingString: "Retrieving full tool result",
	IconURL:       "",
	Exec: func(ctx context.Context, args string, conv utils.Conversation) utils.MessageContent {
		parsed := utils.ParseJson(args)
		callID, _ := parsed["tool_call_id"].(string)
		if callID == "" {
			return utils.NewTextContent("Error: tool_call_id is required (the id named in the caveman banner)")
		}

		// Paging is mandatory: originals can be multi-megabyte (observed: a
		// 4.2MB config dump). Handing one back whole would re-create the exact
		// context blow-up compression exists to prevent.
		limit := 20_000
		if l, ok := parsed["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}
		if limit > 100_000 {
			limit = 100_000
		}
		offset := 0
		if o, ok := parsed["offset"].(float64); ok && o > 0 {
			offset = int(o)
		}

		body, name, found, err := db.ToolResultByCallID(ctx, conv.ID, callID)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error: %v", err))
		}
		if !found {
			return utils.NewTextContent(fmt.Sprintf(
				"No tool result with tool_call_id=%q in this conversation. Use the id shown in the caveman banner.", callID))
		}
		if offset > len(body) {
			offset = len(body)
		}
		end := offset + limit
		if end > len(body) {
			end = len(body)
		}
		chunk := body[offset:end]

		header := fmt.Sprintf(
			"[caveman_retrieve: full original of tool result tool_call_id=%q (tool=%s), chars %d-%d of %d. "+
				"Next page: offset=%d.]\n",
			callID, name, offset, end, len(body), end,
		)
		if end >= len(body) {
			header = fmt.Sprintf(
				"[caveman_retrieve: full original of tool result tool_call_id=%q (tool=%s), chars %d-%d of %d — end of text.]\n",
				callID, name, offset, end, len(body),
			)
		}
		return utils.NewTextContent(header + chunk)
	},
}
