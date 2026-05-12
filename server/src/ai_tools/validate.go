package ai_tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ValidateToolCallArgs enforces strict schema conformance on the JSON args
// the LLM produced for a tool call. It rejects:
//   - args that don't parse as a JSON object
//   - keys not declared in schema.properties
//   - keys listed in schema.required that are absent from args
//
// schemaJSON is the raw JSON Schema (the same shape MCP exposes via
// inputSchema, and the same shape we marshal builtin ParameterToolsRequests
// into). Pass an empty schema (nil/"") to skip validation — used for tools
// with no declared parameters.
//
// Returns (true, "") on success. On failure, returns (false, errMsg) with
// a deterministic, sorted message suitable for piping straight back to the
// LLM as a tool result so it can self-correct.
func ValidateToolCallArgs(toolName string, schemaJSON json.RawMessage, argsJSON string) (bool, string) {
	trimmedSchema := strings.TrimSpace(string(schemaJSON))
	if trimmedSchema == "" || trimmedSchema == "null" {
		return true, ""
	}

	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return true, ""
	}

	trimmedArgs := strings.TrimSpace(argsJSON)
	if trimmedArgs == "" {
		trimmedArgs = "{}"
	}

	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmedArgs), &args); err != nil {
		return false, fmt.Sprintf("Error: invalid arguments for %s: arguments must be a JSON object (%s)", toolName, err.Error())
	}

	allowed := schema.Properties
	if allowed == nil {
		allowed = map[string]json.RawMessage{}
	}

	var unknown []string
	for key := range args {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}

	var missing []string
	for _, req := range schema.Required {
		if _, ok := args[req]; !ok {
			missing = append(missing, req)
		}
	}

	if len(unknown) == 0 && len(missing) == 0 {
		return true, ""
	}

	sort.Strings(unknown)
	sort.Strings(missing)
	allowedNames := make([]string, 0, len(allowed))
	for k := range allowed {
		allowedNames = append(allowedNames, k)
	}
	sort.Strings(allowedNames)

	var parts []string
	if len(unknown) > 0 {
		parts = append(parts, fmt.Sprintf("unknown parameter(s) [%s]", strings.Join(unknown, ", ")))
	}
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("missing required parameter(s) [%s]", strings.Join(missing, ", ")))
	}
	return false, fmt.Sprintf("Error: invalid arguments for %s: %s; allowed parameters: [%s]", toolName, strings.Join(parts, "; "), strings.Join(allowedNames, ", "))
}

// SchemaForBuiltinTool marshals a builtin tool's ParameterToolsRequest into
// the raw JSON Schema form that ValidateToolCallArgs expects. Returns an
// empty RawMessage when the tool declares no parameters (validation skipped).
func SchemaForBuiltinTool(toolName string) json.RawMessage {
	tool, ok := GetTool(toolName)
	if !ok {
		return nil
	}
	params := tool.ToolRequest.Function.Parameters
	if params == nil {
		params = tool.ToolRequest.Function.InputSchema
	}
	if params == nil {
		return nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	return raw
}
