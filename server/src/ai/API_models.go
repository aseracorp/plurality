package ai

import (
	"encoding/json"
	"net/http"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/skills"
	"github.com/azukaar/plurality/src/utils"
)

// ModelInfo is an OpenAI-compatible model entry enriched with Plurality
// capability flags. Extra fields are silently ignored by generic clients
// that only read {id, object, owned_by}.
type ModelInfo struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	OwnedBy  string `json:"owned_by"`
	Text     bool   `json:"text,omitempty"`
	Vision   bool   `json:"vision,omitempty"`
	ImageGen bool   `json:"image_gen,omitempty"`
	Audio    bool   `json:"audio,omitempty"`
}

// PresetConfig holds a preset's model selection and display metadata.
type PresetConfig struct {
	Name    string              `json:"name"`
	Label   string              `json:"label"`
	Pricing string              `json:"pricing"`
	Color   string              `json:"color"`
	Order   int                 `json:"order"`
	Models  utils.ModelSelected `json:"models"`
}

// FunctionDef surfaces a single tool key in the modal.
type FunctionDef struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Default     string `json:"default"`
	Parent      string `json:"parent,omitempty"`
}

// FunctionBundle groups multiple tool keys behind one UI toggle.
type FunctionBundle struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// SkillDef surfaces a single server-side skill in the modal's Skills tab.
type SkillDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Default     string `json:"default"` // "on" / "off" / "ask"
}

// ModelsResponse extends the OpenAI list-models response with presets and
// function metadata at the root. Generic OpenAI clients ignore unknown
// root-level keys; Plurality clients use them to build the picker UI.
type ModelsResponse struct {
	Object          string                    `json:"object"`
	Data            []ModelInfo               `json:"data"`
	Presets         []PresetConfig            `json:"presets,omitempty"`
	Functions       []FunctionDef             `json:"functions,omitempty"`
	FunctionBundles map[string]FunctionBundle `json:"function_bundles,omitempty"`
	Skills          []SkillDef                `json:"skills,omitempty"`
}


// BuiltinFunctions are the server-provided tool toggles shown in the modal.
// Bundled tools use namespaced keys (bundle__tool) matching the names sent to
// the LLM. Standalone tools keep bare keys.
var BuiltinFunctions = []FunctionDef{
	{Key: "search_web", Label: "Search Web", Description: "Search sites via Google", Default: "on"},
	{Key: "place_search", Label: "Place Search", Description: "Search locations via Google Maps", Default: "on"},
	{Key: "visit_link", Label: "Visit Link", Description: "Visit websites shared in the chat", Default: "on"},
	{Key: "roll_dice", Label: "Roll Dice", Description: "Well... rolls a dice", Default: "on"},
	{Key: "generate_image", Label: "Image Generation", Description: "Generate images from text descriptions", Default: "on"},
	{Key: "conversations__search_conversations", Label: "Search Conversations", Description: "Search past conversations by topic", Default: "on", Parent: "conversations"},
	{Key: "conversations__retrieve_conversation", Label: "Retrieve Conversation", Description: "Retrieve messages from a past conversation", Default: "on", Parent: "conversations"},
	{Key: "mcp_capabilities__manage_mcp", Label: "Manage MCP", Description: "Read and edit MCP server configuration", Default: "ask", Parent: "mcp_capabilities"},
	{Key: "mcp_capabilities__debug_mcp", Label: "Debug MCP", Description: "View MCP server logs for debugging", Default: "ask", Parent: "mcp_capabilities"},
	{Key: "system_tools__shell_exec", Label: "Shell Execute", Description: "Execute shell commands on the server", Default: "ask", Parent: "system_tools"},
	{Key: "system_tools__apt_install", Label: "Apt Install", Description: "Install system packages via apt-get", Default: "ask", Parent: "system_tools"},
	{Key: "filesystem_server__fs_read", Label: "Read Files (Server)", Description: "List, find, and read files on the server", Default: "ask", Parent: "filesystem_server"},
	{Key: "filesystem_server__fs_write", Label: "Write Files (Server)", Description: "Edit, copy, move, delete files on the server", Default: "ask", Parent: "filesystem_server"},
}

var BuiltinFunctionBundles = map[string]FunctionBundle{
	"conversations":     {Key: "conversations", Label: "Search Conversations", Description: "Search and retrieve past conversations"},
	"mcp_capabilities":  {Key: "mcp_capabilities", Label: "MCP Capabilities", Description: "Manage and debug MCP server integrations"},
	"system_tools":      {Key: "system_tools", Label: "System Tools", Description: "Execute commands and install packages on the server"},
	"filesystem_server": {Key: "filesystem_server", Label: "Server Filesystem", Description: "Read and write files on the server"},
}

// buildModelInfoList projects the litellm-driven registry into the client-facing
// ModelInfo shape. Capability flags come from the model_info blocks in
// litellm_config.yaml; the embedding model is hidden from the picker.
func buildModelInfoList() []ModelInfo {
	entries := Models.All()
	out := make([]ModelInfo, 0, len(entries))
	for _, e := range entries {
		if e.Mode == "embedding" {
			continue
		}
		info := ModelInfo{ID: e.Name, Object: "model", OwnedBy: "plurality"}
		switch e.Mode {
		case "image_generation":
			info.ImageGen = true
		case "audio_speech", "audio_transcription":
			info.Audio = true
		default:
			info.Text = true
			info.Vision = e.SupportsVision
		}
		out = append(out, info)
	}
	return out
}

// orderedPresets projects the picker-visible shortcuts from auth.Config into
// the client-facing PresetConfig shape. Only entries whose Name matches one of
// auth.ReservedShortcutNames ("fast", "medium", "smart") are surfaced — extras
// in the config file are kept on disk but invisible to the UI. Order matches
// auth.ReservedShortcutNames; the validator already enforces presence.
func orderedPresets() []PresetConfig {
	shortcuts := auth.GetConfig().Shortcuts
	byName := make(map[string]auth.Shortcut, len(shortcuts))
	for _, s := range shortcuts {
		byName[s.Name] = s
	}

	out := make([]PresetConfig, 0, len(auth.ReservedShortcutNames))
	for i, name := range auth.ReservedShortcutNames {
		s, ok := byName[name]
		if !ok {
			continue
		}
		out = append(out, PresetConfig{
			Name:    s.Name,
			Label:   s.Label,
			Pricing: s.Pricing,
			Color:   s.Color,
			Order:   i,
			Models: utils.ModelSelected{
				Text:     toUtilsModel(s.Models.Text),
				Vision:   toUtilsModel(s.Models.Vision),
				ImageGen: toUtilsModel(s.Models.ImageGen),
			},
		})
	}
	return out
}

// toUtilsModel converts an auth.ShortcutModel into utils.Model. Returns nil
// when the source is nil so optional fields stay optional in the JSON.
func toUtilsModel(m *auth.ShortcutModel) *utils.Model {
	if m == nil {
		return nil
	}
	return &utils.Model{Name: m.Name, Tools: m.Tools}
}

// HandleListModels is OpenAI-list-compatible with added {presets, functions,
// function_bundles, skills} root-level fields for rich clients.
func HandleListModels(w http.ResponseWriter, r *http.Request) {
	functions := make([]FunctionDef, 0, len(BuiltinFunctions)+8)
	functions = append(functions, BuiltinFunctions...)

	bundles := map[string]FunctionBundle{}
	for k, v := range BuiltinFunctionBundles {
		bundles[k] = v
	}

	// Server-side MCP servers become one bundle each, grouping their tools.
	for serverName, mcpTools := range mcp.ToolsByServer() {
		if serverName == "" {
			continue
		}
		bundleKey := "mcp:" + serverName
		// Prefer user-configured description from mcp.json; fall back to first tool's.
		firstDesc := mcp.ServerDescription(serverName)
		if firstDesc == "" {
			for _, t := range mcpTools {
				if t.Description != "" {
					firstDesc = truncate(firstLine(t.Description), 100)
					break
				}
			}
		}
		bundles[bundleKey] = FunctionBundle{
			Key:         bundleKey,
			Label:       serverName,
			Description: firstDesc,
		}
		for _, t := range mcpTools {
			functions = append(functions, FunctionDef{
				Key:         t.NamespacedName(),
				Label:       t.Name,
				Description: truncate(firstLine(t.Description), 100),
				Default:     "on",
				Parent:      bundleKey,
			})
		}
	}

	// Expose server skills to the picker. retrieve_server_skill itself is
	// force-included in GetRequests — we don't list it as a toggle.
	var skillDefs []SkillDef
	for _, s := range skills.List() {
		skillDefs = append(skillDefs, SkillDef{
			Name:        s.Name,
			Description: s.Description,
			Default:     "on",
		})
	}

	presets := orderedPresets()

	// Inject server-side MCP tools into presets so they're enabled out of the box.
	mcpTools := mcp.ListTools()
	if len(mcpTools) > 0 {
		for i := range presets {
			for _, modelPtr := range []*utils.Model{presets[i].Models.Text, presets[i].Models.Vision} {
				if modelPtr != nil && modelPtr.Tools != nil {
					for _, t := range mcpTools {
						modelPtr.Tools[t.NamespacedName()] = "true"
					}
				}
			}
		}
	}

	resp := ModelsResponse{
		Object:          "list",
		Data:            buildModelInfoList(),
		Presets:         presets,
		Functions:       functions,
		FunctionBundles: bundles,
		Skills:          skillDefs,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
