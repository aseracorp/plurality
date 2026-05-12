package ai_tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateToolCallArgs(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"prompt": {"type": "string"},
			"model":  {"type": "string"}
		},
		"required": ["prompt"]
	}`)

	cases := []struct {
		name        string
		schema      json.RawMessage
		args        string
		wantOK      bool
		wantSubstrs []string
	}{
		{name: "valid args", schema: schema, args: `{"prompt":"hi"}`, wantOK: true},
		{name: "valid with optional", schema: schema, args: `{"prompt":"hi","model":"flux"}`, wantOK: true},
		{name: "empty args treated as {}", schema: schema, args: "", wantOK: false, wantSubstrs: []string{"missing required parameter(s) [prompt]"}},
		{name: "empty schema skips validation", schema: nil, args: `{"anything":1}`, wantOK: true},
		{name: "null schema skips validation", schema: json.RawMessage("null"), args: `{"anything":1}`, wantOK: true},
		{name: "unknown param", schema: schema, args: `{"prompt":"hi","quality":"hd"}`, wantOK: false, wantSubstrs: []string{"unknown parameter(s) [quality]", "allowed parameters: [model, prompt]"}},
		{name: "multiple unknown sorted", schema: schema, args: `{"prompt":"hi","zeta":1,"alpha":2}`, wantOK: false, wantSubstrs: []string{"unknown parameter(s) [alpha, zeta]"}},
		{name: "missing required", schema: schema, args: `{"model":"flux"}`, wantOK: false, wantSubstrs: []string{"missing required parameter(s) [prompt]"}},
		{name: "missing AND unknown reported together", schema: schema, args: `{"model":"flux","quality":"hd"}`, wantOK: false, wantSubstrs: []string{"unknown parameter(s) [quality]", "missing required parameter(s) [prompt]"}},
		{name: "non-object args", schema: schema, args: `"just a string"`, wantOK: false, wantSubstrs: []string{"must be a JSON object"}},
		{name: "schema with no properties accepts empty", schema: json.RawMessage(`{"type":"object"}`), args: `{}`, wantOK: true},
		{name: "schema with no properties rejects keys", schema: json.RawMessage(`{"type":"object"}`), args: `{"x":1}`, wantOK: false, wantSubstrs: []string{"unknown parameter(s) [x]"}},
		{name: "malformed schema falls open", schema: json.RawMessage(`not-json`), args: `{"x":1}`, wantOK: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, msg := ValidateToolCallArgs("toolX", tc.schema, tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (msg=%q)", ok, tc.wantOK, msg)
			}
			for _, sub := range tc.wantSubstrs {
				if !strings.Contains(msg, sub) {
					t.Errorf("expected error message to contain %q, got %q", sub, msg)
				}
			}
			if !tc.wantOK && !strings.HasPrefix(msg, "Error: invalid arguments for toolX:") {
				t.Errorf("error prefix wrong: %q", msg)
			}
		})
	}
}
