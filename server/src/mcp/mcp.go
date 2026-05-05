package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// NamespaceSeparator is used to build namespaced tool names: "server__tool".
const NamespaceSeparator = "__"

// NamespacedToolName returns "serverName__toolName".
func NamespacedToolName(serverName, toolName string) string {
	return serverName + NamespaceSeparator + toolName
}

// ParseNamespacedTool splits "serverName__toolName" into (serverName, toolName, true).
// Returns ("", name, false) for bare names with no namespace.
func ParseNamespacedTool(name string) (serverName, toolName string, ok bool) {
	idx := strings.Index(name, NamespaceSeparator)
	if idx < 0 {
		return "", name, false
	}
	return name[:idx], name[idx+len(NamespaceSeparator):], true
}

// ToolInfo describes a single MCP tool, discovered via tools/list.
type ToolInfo struct {
	Name        string // bare tool name as advertised by the MCP server
	Description string
	ServerName  string          // which mcp.json server it lives on
	InputSchema json.RawMessage // original MCP inputSchema (JSON object)
}

// NamespacedName returns the "serverName__toolName" form.
func (t ToolInfo) NamespacedName() string {
	return NamespacedToolName(t.ServerName, t.Name)
}

type mcpServerConfig struct {
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	Stateful    bool     `json:"stateful"`
	Description string   `json:"description"`
}

type mcpFile struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

// convProcess tracks a per-conversation MCP process and its last activity.
type convProcess struct {
	pm       *ProcessManager
	lastUsed time.Time
}

var (
	mu          sync.RWMutex
	processes   = map[string]*ProcessManager{} // shared (non-stateful) servers
	tools       = map[string]ToolInfo{}        // toolName -> info
	initialized bool

	// Stateful MCP: per-conversation process isolation.
	statefulConfigs    = map[string]mcpServerConfig{}         // serverName -> config (for lazy spawning)
	convProcesses      = map[string]map[string]*convProcess{} // conversationID -> serverName -> process
	serverDescriptions = map[string]string{}                  // serverName -> description from mcp.json
)

// dataDir returns the configured data dir (env DATA_DIR, default ./data
// next to the binary). Mirrors the convention in storage.Init.
func dataDir() string {
	if p := os.Getenv("DATA_DIR"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "data")
}

// MCPConfigPath returns the absolute path to data/mcp.json.
func MCPConfigPath() string {
	return filepath.Join(dataDir(), "mcp.json")
}

// Init loads data/mcp.json, starts all configured servers, and discovers
// their tools. Safe to call when the file is missing (creates an empty one,
// mirroring the client behavior in MCP.dart:190-194).
func Init() {
	mu.Lock()
	defer mu.Unlock()

	initialized = true
	tools = map[string]ToolInfo{}
	processes = map[string]*ProcessManager{}
	statefulConfigs = map[string]mcpServerConfig{}
	serverDescriptions = map[string]string{}

	configPath := MCPConfigPath()
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		utils.Error("[MCP] Failed to create data dir", err)
		return
	}

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		defaultCfg := defaultMCPConfig()
		if werr := os.WriteFile(configPath, []byte(defaultCfg), 0644); werr != nil {
			utils.Error("[MCP] Failed to write default config", werr)
		} else {
			utils.Log("[MCP] Created default config at %s", configPath)
		}
		// Re-read so we continue to start the default servers.
		data, err = os.ReadFile(configPath)
		if err != nil {
			utils.Error("[MCP] Failed to re-read config", err)
			return
		}
	}
	if err != nil {
		utils.Error("[MCP] Failed to read config", err)
		return
	}

	var cfg mcpFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		utils.Error("[MCP] Failed to parse mcp.json", err)
		return
	}
	if len(cfg.MCPServers) == 0 {
		utils.Log("[MCP] No servers configured")
		return
	}

	for name, server := range cfg.MCPServers {
		if name == "" || server.Command == "" {
			utils.Log("[MCP] Skipping invalid entry: name=%q command=%q", name, server.Command)
			continue
		}

		// Resolve command to absolute path for reliability.
		cmdPath := server.Command
		if resolved, lookErr := exec.LookPath(server.Command); lookErr == nil {
			cmdPath = resolved
		}

		utils.Log("[MCP] Starting %s (%s)...", name, cmdPath)
		pm := NewProcessManager(name, cmdPath, server.Args)
		if err := pm.Start(); err != nil {
			utils.Error("[MCP] Failed to start "+name, err)
			continue
		}

		// Initialize then list tools. Many MCP servers require "initialize"
		// before they will accept other calls.
		utils.Log("[MCP] %s: sending initialize...", name)
		if _, err := pm.SendRequest("initialize", map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo":      map[string]interface{}{"name": "plurality", "version": "1.0"},
		}); err != nil {
			utils.Log("[MCP] %s: initialize warning: %v (continuing)", name, err)
		}
		_, _ = pm.SendRequest("notifications/initialized", nil)

		utils.Log("[MCP] %s: fetching tools...", name)
		raw, err := pm.SendRequest("tools/list", map[string]interface{}{})
		if err != nil {
			utils.Error("[MCP] "+name+": tools/list failed", err)
			pm.Stop()
			continue
		}

		var list struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			utils.Error("[MCP] "+name+": tools/list parse", err)
			pm.Stop()
			continue
		}

		for _, t := range list.Tools {
			if t.Name == "" {
				continue
			}
			nsName := NamespacedToolName(name, t.Name)
			tools[nsName] = ToolInfo{
				Name:        t.Name,
				Description: t.Description,
				ServerName:  name,
				InputSchema: t.InputSchema,
			}
		}
		utils.Log("[MCP] %s: %d tools loaded (stateful=%v)", name, len(list.Tools), server.Stateful)

		if server.Description != "" {
			serverDescriptions[name] = server.Description
		}

		if server.Stateful {
			// Stateful: stop the discovery process and save config for
			// lazy per-conversation spawning.
			pm.Stop()
			server.Command = cmdPath // store resolved path
			statefulConfigs[name] = server
		} else {
			// Shared: keep the process running for all conversations.
			processes[name] = pm
		}
	}

	// Start cleanup goroutine for idle stateful processes.
	if len(statefulConfigs) > 0 {
		go cleanupLoop()
	}
}

// Shutdown stops all running MCP servers (shared and per-conversation).
func Shutdown() {
	mu.Lock()
	procs := processes
	processes = map[string]*ProcessManager{}
	tools = map[string]ToolInfo{}
	statefulConfigs = map[string]mcpServerConfig{}

	convProcs := convProcesses
	convProcesses = map[string]map[string]*convProcess{}
	mu.Unlock()

	for _, p := range procs {
		p.Stop()
	}
	for _, servers := range convProcs {
		for _, cp := range servers {
			cp.pm.Stop()
		}
	}
}

// ListTools returns all discovered MCP tools.
func ListTools() []ToolInfo {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]ToolInfo, 0, len(tools))
	for _, t := range tools {
		out = append(out, t)
	}
	return out
}

// ServerNames returns the names of all currently running MCP servers,
// whether they exposed tools or not.
func ServerNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(processes))
	for name := range processes {
		out = append(out, name)
	}
	return out
}

// ToolsByServer groups discovered tools by their server name.
func ToolsByServer() map[string][]ToolInfo {
	mu.RLock()
	defer mu.RUnlock()
	out := map[string][]ToolInfo{}
	for _, t := range tools {
		out[t.ServerName] = append(out[t.ServerName], t)
	}
	return out
}

// ServerDescription returns the user-configured description for a server,
// or empty string if none was set.
func ServerDescription(serverName string) string {
	mu.RLock()
	defer mu.RUnlock()
	return serverDescriptions[serverName]
}

// ServerDescriptions returns all server descriptions keyed by server name.
func ServerDescriptions() map[string]string {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]string, len(serverDescriptions))
	for k, v := range serverDescriptions {
		out[k] = v
	}
	return out
}

// IsMCPTool reports whether the given (namespaced) tool name belongs to a
// configured MCP server.
func IsMCPTool(toolName string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := tools[toolName]
	return ok
}

// ToolsRequests returns OpenAI-format tool definitions for all MCP tools,
// using namespaced names (serverName__toolName) and enriched descriptions.
func ToolsRequests() []utils.ToolsRequest {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]utils.ToolsRequest, 0, len(tools))
	for nsName, t := range tools {
		desc := fmt.Sprintf("[MCP server: %s] %s", t.ServerName, t.Description)
		out = append(out, utils.ToolsRequest{
			Type: "function",
			Function: utils.FunctionToolsRequest{
				Name:        nsName,
				Description: desc,
				Parameters:  schemaToParameters(t.InputSchema),
			},
		})
	}
	return out
}

// schemaToParameters converts an MCP inputSchema (arbitrary JSON schema)
// into the Plurality-constrained ParameterToolsRequest shape.
// Complex schemas (nested objects, arrays of objects) lose fidelity —
// same limitation as the existing client-side MCP flow (see todo.txt:41).
func schemaToParameters(raw json.RawMessage) *utils.ParameterToolsRequest {
	if len(raw) == 0 {
		return &utils.ParameterToolsRequest{Type: "object", Properties: map[string]utils.PropertyParameterToolsRequest{}}
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return &utils.ParameterToolsRequest{Type: "object", Properties: map[string]utils.PropertyParameterToolsRequest{}}
	}
	props := map[string]utils.PropertyParameterToolsRequest{}
	for name, propRaw := range schema.Properties {
		var prop struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		}
		_ = json.Unmarshal(propRaw, &prop)
		if prop.Type == "" {
			prop.Type = "string"
		}
		props[name] = utils.PropertyParameterToolsRequest{
			Type:        prop.Type,
			Description: prop.Description,
			Enum:        prop.Enum,
		}
	}
	typ := schema.Type
	if typ == "" {
		typ = "object"
	}
	return &utils.ParameterToolsRequest{
		Type:       typ,
		Properties: props,
		Required:   schema.Required,
	}
}

// CallTool dispatches a tools/call request to the MCP server that owns
// toolName (namespaced) and returns the flattened text content. For stateful
// servers the conversationID is used to route to a per-conversation process.
func CallTool(ctx context.Context, toolName, argsJSON, conversationID string) utils.MessageContent {
	mu.RLock()
	info, ok := tools[toolName]
	mu.RUnlock()

	if !ok {
		return utils.NewTextContent(fmt.Sprintf("Error: MCP tool %q not found", toolName))
	}

	// Resolve the process manager: per-conversation for stateful, shared otherwise.
	pm, err := getProcessForCall(info.ServerName, conversationID)
	if err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error: %v", err))
	}

	// Parse args JSON; treat empty as empty object.
	var args map[string]interface{}
	if strings.TrimSpace(argsJSON) == "" {
		args = map[string]interface{}{}
	} else if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return utils.NewTextContent(fmt.Sprintf("Error: invalid arguments for %s: %v", toolName, err))
	}

	// Send the BARE tool name to the MCP process (it doesn't know about namespacing).
	raw, callErr := pm.SendRequest("tools/call", map[string]interface{}{
		"name":      info.Name,
		"arguments": args,
	})
	if callErr != nil {
		return utils.NewTextContent(fmt.Sprintf("Error: MCP call failed: %v", callErr))
	}

	// MCP tools/call returns { content: [{ type:"text", text:"..." }, ...] }
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return utils.NewTextContent(fmt.Sprintf("MCP result (raw): %s", string(raw)))
	}

	var out strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" && c.Text != "" {
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString(c.Text)
		}
	}
	if out.Len() == 0 {
		return utils.NewTextContent("(no text content)")
	}
	return utils.NewTextContent(out.String())
}

// getProcessForCall returns the correct ProcessManager for a tool call.
// Stateful servers get a per-conversation process; shared servers use the global one.
func getProcessForCall(serverName, conversationID string) (*ProcessManager, error) {
	mu.RLock()
	cfg, isStateful := statefulConfigs[serverName]
	if !isStateful {
		pm := processes[serverName]
		mu.RUnlock()
		if pm == nil {
			return nil, fmt.Errorf("MCP server %q not running", serverName)
		}
		return pm, nil
	}
	mu.RUnlock()

	return getOrCreateConvProcess(conversationID, serverName, cfg)
}

// getOrCreateConvProcess lazily spawns and initializes a stateful MCP
// process for a specific conversation.
func getOrCreateConvProcess(conversationID, serverName string, cfg mcpServerConfig) (*ProcessManager, error) {
	mu.Lock()
	defer mu.Unlock()

	if servers, ok := convProcesses[conversationID]; ok {
		if cp, ok := servers[serverName]; ok {
			// Check if process is still running; if crashed, respawn
			if cp.pm.IsRunning() {
				cp.lastUsed = time.Now()
				return cp.pm, nil
			}
			utils.Log("[MCP] Stateful process %s/%s crashed, respawning", serverName, conversationID)
			delete(servers, serverName)
		}
	}

	// Spawn a new process for this conversation.
	pm := NewProcessManager(serverName+"/"+conversationID, cfg.Command, cfg.Args)
	if err := pm.Start(); err != nil {
		return nil, fmt.Errorf("failed to start stateful MCP %s for conversation: %w", serverName, err)
	}

	if _, err := pm.SendRequest("initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "plurality", "version": "1.0"},
	}); err != nil {
		utils.Log("[MCP] %s/%s: initialize warning: %v", serverName, conversationID, err)
	}
	_, _ = pm.SendRequest("notifications/initialized", nil)

	if convProcesses[conversationID] == nil {
		convProcesses[conversationID] = map[string]*convProcess{}
	}
	convProcesses[conversationID][serverName] = &convProcess{
		pm:       pm,
		lastUsed: time.Now(),
	}
	utils.Log("[MCP] Started stateful process %s for conversation %s", serverName, conversationID)
	return pm, nil
}

// CleanupConversation stops all stateful MCP processes for a conversation.
func CleanupConversation(conversationID string) {
	mu.Lock()
	servers := convProcesses[conversationID]
	delete(convProcesses, conversationID)
	mu.Unlock()

	for name, cp := range servers {
		utils.Log("[MCP] Stopping stateful process %s for conversation %s", name, conversationID)
		cp.pm.Stop()
	}
}

// cleanupLoop periodically reaps idle stateful processes.
func cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		mu.Lock()
		if !initialized {
			mu.Unlock()
			return
		}
		now := time.Now()
		for convID, servers := range convProcesses {
			for name, cp := range servers {
				if now.Sub(cp.lastUsed) > 5*time.Minute {
					utils.Log("[MCP] Reaping idle stateful process %s/%s", name, convID)
					cp.pm.Stop()
					delete(servers, name)
				}
			}
			if len(servers) == 0 {
				delete(convProcesses, convID)
			}
		}
		mu.Unlock()
	}
}

// GetMCPLogs returns the recent stderr logs for an MCP process.
// For stateful servers, uses the conversation-specific process.
// For shared servers, uses the global process.
func GetMCPLogs(serverName, conversationID string) string {
	mu.RLock()
	defer mu.RUnlock()

	// Try conversation-specific process first (stateful)
	if servers, ok := convProcesses[conversationID]; ok {
		if cp, ok := servers[serverName]; ok {
			logs := cp.pm.GetLogs()
			if len(logs) == 0 {
				return "(no logs captured yet)"
			}
			return strings.Join(logs, "\n")
		}
	}

	// Try shared process
	if pm, ok := processes[serverName]; ok {
		logs := pm.GetLogs()
		if len(logs) == 0 {
			return "(no logs captured yet)"
		}
		return strings.Join(logs, "\n")
	}

	return fmt.Sprintf("(no process found for %s)", serverName)
}

// ListAllMCPServers returns names of all configured MCP servers.
func ListAllMCPServers() []string {
	mu.RLock()
	defer mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for name := range statefulConfigs {
		if !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	for name := range processes {
		if !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	return out
}

// HasAnyServers returns true if any MCP servers are configured.
func HasAnyServers() bool {
	mu.RLock()
	defer mu.RUnlock()
	return len(statefulConfigs) > 0 || len(processes) > 0
}

// defaultMCPConfig returns the default mcp.json content, including
// Lightpanda if the binary is found on the system.
func defaultMCPConfig() string {
	cfg := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			// TODO: replace with Playwright
			// "lightpanda": map[string]interface{}{
			// 	"command":     "/app/lightpanda",
			// 	"args":        []string{"mcp"},
			// 	"stateful":    true,
			// 	"description": "Lightpanda headless browser for interactive web browsing with JavaScript support. Only use for heavier web tasks requiring JS rendering, form interaction, or dynamic content. For simple page fetching, prefer the visit_link tool instead.",
			// },
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b) + "\n"
}
