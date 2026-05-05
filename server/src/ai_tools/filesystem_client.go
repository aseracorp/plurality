package ai_tools

import (
	"github.com/azukaar/plurality/src/utils"
)

// FsClientReadToolRequest and FsClientWriteToolRequest are schema-only tool
// definitions for the device-side filesystem tools. The server never executes
// them — when the LLM calls these names, categorizeToolCalls in the tool loop
// finds no Registry entry and routes the call to the client.
//
// They are force-included by GetRequests when payload.HasAttachedFolder is true,
// so the LLM only sees them while the user has a folder attached to the
// conversation. Paths are interpreted by the client *relative to the attached
// folder*; the absolute sandbox root never leaves the device.

var FsClientReadToolRequest = utils.ToolsRequest{
	Type: "function",
	Function: utils.FunctionToolsRequest{
		Name:        "filesystem_client__fs_read",
		Description: "[device] Read files in the user's attached folder. Set 'op' to one of: list (directory entries), find (recursive name pattern match), read (whole file as text), read_segment (line range), stat (metadata). Paths are RELATIVE to the attached folder root; '..' is rejected.",
		Parameters: &utils.ParameterToolsRequest{
			Type: "object",
			Properties: map[string]utils.PropertyParameterToolsRequest{
				"op": {
					Type:        "string",
					Description: "Operation: list | find | read | read_segment | stat",
					Enum:        []string{"list", "find", "read", "read_segment", "stat"},
				},
				"path": {
					Type:        "string",
					Description: "Path RELATIVE to the attached folder. Use '.' for the root.",
				},
				"pattern": {
					Type:        "string",
					Description: "For 'find': glob pattern matched against entry names (e.g. '*.dart').",
				},
				"recursive": {
					Type:        "string",
					Description: "For 'list': 'true' to recurse, 'false' for shallow listing (default: false).",
				},
				"start_line": {
					Type:        "integer",
					Description: "For 'read_segment': 1-based starting line (inclusive).",
				},
				"end_line": {
					Type:        "integer",
					Description: "For 'read_segment': 1-based ending line (inclusive). 0 or omitted means to end of file.",
				},
			},
			Required: []string{"op", "path"},
		},
	},
}

var FsClientWriteToolRequest = utils.ToolsRequest{
	Type: "function",
	Function: utils.FunctionToolsRequest{
		Name:        "filesystem_client__fs_write",
		Description: "[device] Modify files in the user's attached folder. Set 'op' to one of: create (write a new file, fails if exists), edit (single occurrence search-and-replace), copy, move, delete, mkdir. Paths are RELATIVE to the attached folder root; '..' is rejected.",
		Parameters: &utils.ParameterToolsRequest{
			Type: "object",
			Properties: map[string]utils.PropertyParameterToolsRequest{
				"op": {
					Type:        "string",
					Description: "Operation: create | edit | copy | move | delete | mkdir",
					Enum:        []string{"create", "edit", "copy", "move", "delete", "mkdir"},
				},
				"path": {
					Type:        "string",
					Description: "Target path RELATIVE to the attached folder.",
				},
				"dest_path": {
					Type:        "string",
					Description: "For 'copy' and 'move': destination path RELATIVE to the attached folder.",
				},
				"content": {
					Type:        "string",
					Description: "For 'create': the file's text content.",
				},
				"old_text": {
					Type:        "string",
					Description: "For 'edit': literal substring to find. Must occur exactly once in the file.",
				},
				"new_text": {
					Type:        "string",
					Description: "For 'edit': replacement text.",
				},
			},
			Required: []string{"op", "path"},
		},
	},
}
