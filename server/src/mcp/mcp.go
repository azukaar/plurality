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

// globalUserID is the sentinel key for the shared/global layer loaded from
// data/mcp.json. Every user's effective view merges this layer with their own
// users-data/{userID}/mcp.json, with the global layer winning on name collision.
const globalUserID = ""

var (
	mu          sync.RWMutex
	initialized bool

	// All state is keyed by userID first; globalUserID ("") holds the shared layer.
	processes          = map[string]map[string]*ProcessManager{} // userID -> serverName -> shared (non-stateful) process
	tools              = map[string]map[string]ToolInfo{}        // userID -> namespaced toolName -> info
	statefulConfigs    = map[string]map[string]mcpServerConfig{} // userID -> serverName -> config (for lazy spawning)
	serverDescriptions = map[string]map[string]string{}          // userID -> serverName -> description from mcp.json

	// Stateful MCP: per-conversation process isolation. Keyed by conversationID
	// (globally unique). convOwner maps a conversation to its owning user so a
	// stateful call/cleanup can resolve the right user's server config.
	convProcesses = map[string]map[string]*convProcess{} // conversationID -> serverName -> process
	convOwner     = map[string]string{}                  // conversationID -> userID

	cleanupRunning bool // guards against spawning duplicate cleanupLoop goroutines
)

// startCleanupLoop launches the idle-process reaper at most once. Caller must
// hold mu.
func startCleanupLoop() {
	if cleanupRunning {
		return
	}
	cleanupRunning = true
	go cleanupLoop()
}

// resetState clears all in-memory maps. Caller must hold mu.
func resetState() {
	processes = map[string]map[string]*ProcessManager{}
	tools = map[string]map[string]ToolInfo{}
	statefulConfigs = map[string]map[string]mcpServerConfig{}
	serverDescriptions = map[string]map[string]string{}
}

// --- merge resolvers (caller holds mu) -------------------------------------
// Each checks the global layer first, then the user's own, so the global
// definition wins on name collision.

func resolveTool(userID, nsName string) (ToolInfo, bool) {
	if g, ok := tools[globalUserID][nsName]; ok {
		return g, true
	}
	t, ok := tools[userID][nsName]
	return t, ok
}

func resolveStatefulConfig(userID, serverName string) (mcpServerConfig, bool) {
	if g, ok := statefulConfigs[globalUserID][serverName]; ok {
		return g, true
	}
	c, ok := statefulConfigs[userID][serverName]
	return c, ok
}

func resolveSharedProcess(userID, serverName string) (*ProcessManager, bool) {
	if g, ok := processes[globalUserID][serverName]; ok {
		return g, true
	}
	p, ok := processes[userID][serverName]
	return p, ok
}

// dataDir returns the configured data dir (env DATA_DIR, default ./data
// next to the binary). Mirrors the convention in storage.Init.
func dataDir() string {
	if p := os.Getenv("DATA_DIR"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "data")
}

// MCPConfigPath returns the absolute path to the shared/global data/mcp.json.
func MCPConfigPath() string {
	return filepath.Join(dataDir(), "mcp.json")
}

// UserMCPConfigPath returns the absolute path to a user's
// users-data/{userID}/mcp.json.
func UserMCPConfigPath(userID string) string {
	return utils.UserFilePath(userID, "mcp.json")
}

// Init loads data/mcp.json, starts all configured servers, and discovers
// their tools. Safe to call when the file is missing (creates an empty one,
// mirroring the client behavior in MCP.dart:190-194).
func Init() {
	mu.Lock()
	defer mu.Unlock()

	initialized = true
	resetState()

	// 1. Load the shared/global layer from data/mcp.json (creating defaults).
	configPath := MCPConfigPath()
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		utils.Error("[MCP] Failed to create data dir", err)
	} else {
		data, err := os.ReadFile(configPath)
		if os.IsNotExist(err) {
			defaultCfg := defaultMCPConfig()
			if werr := os.WriteFile(configPath, []byte(defaultCfg), 0644); werr != nil {
				utils.Error("[MCP] Failed to write default config", werr)
			} else {
				utils.Log("[MCP] Created default config at %s", configPath)
			}
			data, err = os.ReadFile(configPath)
		}
		if err != nil {
			utils.Error("[MCP] Failed to read config", err)
		} else {
			var cfg mcpFile
			if jerr := json.Unmarshal(data, &cfg); jerr != nil {
				utils.Error("[MCP] Failed to parse mcp.json", jerr)
			} else {
				loadUserConfig(globalUserID, cfg)
			}
		}
	}

	// 2. Eagerly load every user's users-data/{userID}/mcp.json layer on top.
	for _, uid := range utils.ListUserIDsWith("mcp.json") {
		data, err := os.ReadFile(UserMCPConfigPath(uid))
		if err != nil {
			utils.Error("[MCP] Failed to read config for user "+uid, err)
			continue
		}
		var cfg mcpFile
		if jerr := json.Unmarshal(data, &cfg); jerr != nil {
			utils.Error("[MCP] Failed to parse mcp.json for user "+uid, jerr)
			continue
		}
		loadUserConfig(uid, cfg)
	}

	// Start cleanup goroutine if any user has stateful servers.
	for _, cfgs := range statefulConfigs {
		if len(cfgs) > 0 {
			startCleanupLoop()
			break
		}
	}
}

// loadUserConfig starts and discovers all servers in cfg for a single user,
// writing the results into that user's slot of every state map. Caller must
// hold mu. For non-global users, a server name already present in the global
// layer is skipped (global wins on collision).
func loadUserConfig(userID string, cfg mcpFile) {
	if len(cfg.MCPServers) == 0 {
		if userID == globalUserID {
			utils.Log("[MCP] No global servers configured")
		}
		return
	}

	tools[userID] = map[string]ToolInfo{}
	processes[userID] = map[string]*ProcessManager{}
	statefulConfigs[userID] = map[string]mcpServerConfig{}
	serverDescriptions[userID] = map[string]string{}

	logPrefix := "global"
	if userID != globalUserID {
		logPrefix = "user " + userID
	}

	for name, server := range cfg.MCPServers {
		if name == "" || server.Command == "" {
			utils.Log("[MCP] (%s) Skipping invalid entry: name=%q command=%q", logPrefix, name, server.Command)
			continue
		}

		// A user may not shadow an admin-provisioned global server (it runs an
		// arbitrary command under the admin's intent).
		if userID != globalUserID {
			if _, clash := statefulConfigs[globalUserID][name]; clash {
				utils.Log("[MCP] (%s) Skipping %q: shadows a global server", logPrefix, name)
				continue
			}
			if _, clash := processes[globalUserID][name]; clash {
				utils.Log("[MCP] (%s) Skipping %q: shadows a global server", logPrefix, name)
				continue
			}
		}

		// Resolve command to absolute path for reliability.
		cmdPath := server.Command
		if resolved, lookErr := exec.LookPath(server.Command); lookErr == nil {
			cmdPath = resolved
		}

		utils.Log("[MCP] (%s) Starting %s (%s)...", logPrefix, name, cmdPath)
		pm := NewProcessManager(name, cmdPath, server.Args)
		if err := pm.Start(); err != nil {
			utils.Error("[MCP] ("+logPrefix+") Failed to start "+name, err)
			continue
		}

		// Initialize then list tools. Many MCP servers require "initialize"
		// before they will accept other calls.
		if _, err := pm.SendRequest("initialize", map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo":      map[string]interface{}{"name": "plurality", "version": "1.0"},
		}); err != nil {
			utils.Log("[MCP] (%s) %s: initialize warning: %v (continuing)", logPrefix, name, err)
		}
		_, _ = pm.SendRequest("notifications/initialized", nil)

		raw, err := pm.SendRequest("tools/list", map[string]interface{}{})
		if err != nil {
			utils.Error("[MCP] ("+logPrefix+") "+name+": tools/list failed", err)
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
			utils.Error("[MCP] ("+logPrefix+") "+name+": tools/list parse", err)
			pm.Stop()
			continue
		}

		for _, t := range list.Tools {
			if t.Name == "" {
				continue
			}
			nsName := NamespacedToolName(name, t.Name)
			tools[userID][nsName] = ToolInfo{
				Name:        t.Name,
				Description: t.Description,
				ServerName:  name,
				InputSchema: t.InputSchema,
			}
		}
		utils.Log("[MCP] (%s) %s: %d tools loaded (stateful=%v)", logPrefix, name, len(list.Tools), server.Stateful)

		if server.Description != "" {
			serverDescriptions[userID][name] = server.Description
		}

		if server.Stateful {
			// Stateful: stop the discovery process and save config for
			// lazy per-conversation spawning.
			pm.Stop()
			server.Command = cmdPath // store resolved path
			statefulConfigs[userID][name] = server
		} else {
			// Shared: keep the process running for all conversations.
			processes[userID][name] = pm
		}
	}
}

// Shutdown stops all running MCP servers (shared and per-conversation).
func Shutdown() {
	mu.Lock()
	procs := processes
	convProcs := convProcesses
	resetState()
	convProcesses = map[string]map[string]*convProcess{}
	convOwner = map[string]string{}
	mu.Unlock()

	for _, servers := range procs {
		for _, p := range servers {
			p.Stop()
		}
	}
	for _, servers := range convProcs {
		for _, cp := range servers {
			cp.pm.Stop()
		}
	}
}

// ReinitUser stops and reloads only one user's MCP servers and tools, leaving
// the global layer and all other users untouched. It also tears down that
// user's stateful per-conversation processes so they respawn under the new
// config. Used by manage_mcp after a config edit.
func ReinitUser(userID string) {
	if userID == globalUserID {
		return // global layer is reloaded only via full Init()
	}

	// Collect this user's processes (and their stateful conv processes) to stop,
	// then clear their slots — all under the lock.
	mu.Lock()
	var toStop []*ProcessManager
	for _, p := range processes[userID] {
		toStop = append(toStop, p)
	}
	for convID, owner := range convOwner {
		if owner != userID {
			continue
		}
		for _, cp := range convProcesses[convID] {
			toStop = append(toStop, cp.pm)
		}
		delete(convProcesses, convID)
		delete(convOwner, convID)
	}
	delete(processes, userID)
	delete(tools, userID)
	delete(statefulConfigs, userID)
	delete(serverDescriptions, userID)
	mu.Unlock()

	// Stop the old processes outside the lock (Stop can block).
	for _, p := range toStop {
		p.Stop()
	}

	// Reload from the user's config file.
	data, err := os.ReadFile(UserMCPConfigPath(userID))
	if os.IsNotExist(err) {
		return // user removed all their servers; global-only view remains
	}
	if err != nil {
		utils.Error("[MCP] ReinitUser: failed to read config for "+userID, err)
		return
	}
	var cfg mcpFile
	if jerr := json.Unmarshal(data, &cfg); jerr != nil {
		utils.Error("[MCP] ReinitUser: failed to parse mcp.json for "+userID, jerr)
		return
	}

	mu.Lock()
	loadUserConfig(userID, cfg)
	if len(statefulConfigs[userID]) > 0 {
		startCleanupLoop()
	}
	mu.Unlock()
}

// mergedTools returns a userID's effective namespaced tool map (global layer
// plus the user's own, global winning on collision). Caller must hold mu.
func mergedTools(userID string) map[string]ToolInfo {
	out := make(map[string]ToolInfo, len(tools[globalUserID])+len(tools[userID]))
	for k, v := range tools[userID] {
		out[k] = v
	}
	for k, v := range tools[globalUserID] { // global wins
		out[k] = v
	}
	return out
}

// ListTools returns all MCP tools visible to a user (global + their own).
func ListTools(userID string) []ToolInfo {
	mu.RLock()
	defer mu.RUnlock()
	merged := mergedTools(userID)
	out := make([]ToolInfo, 0, len(merged))
	for _, t := range merged {
		out = append(out, t)
	}
	return out
}

// GetToolInputSchema returns the raw MCP inputSchema for a namespaced tool
// name in a user's view. Used by the strict-arg validator at dispatch time.
func GetToolInputSchema(userID, toolName string) (json.RawMessage, bool) {
	mu.RLock()
	defer mu.RUnlock()
	t, ok := resolveTool(userID, toolName)
	if !ok {
		return nil, false
	}
	return t.InputSchema, true
}

// ToolsByServer groups a user's visible tools by their server name.
func ToolsByServer(userID string) map[string][]ToolInfo {
	mu.RLock()
	defer mu.RUnlock()
	out := map[string][]ToolInfo{}
	for _, t := range mergedTools(userID) {
		out[t.ServerName] = append(out[t.ServerName], t)
	}
	return out
}

// ServerDescription returns the configured description for a server in a
// user's view, or empty string if none was set.
func ServerDescription(userID, serverName string) string {
	mu.RLock()
	defer mu.RUnlock()
	if d, ok := serverDescriptions[globalUserID][serverName]; ok {
		return d
	}
	return serverDescriptions[userID][serverName]
}

// ServerDescriptions returns all server descriptions visible to a user.
func ServerDescriptions(userID string) map[string]string {
	mu.RLock()
	defer mu.RUnlock()
	out := map[string]string{}
	for k, v := range serverDescriptions[userID] {
		out[k] = v
	}
	for k, v := range serverDescriptions[globalUserID] { // global wins
		out[k] = v
	}
	return out
}

// IsMCPTool reports whether the given (namespaced) tool name belongs to a
// configured MCP server in a user's view.
func IsMCPTool(userID, toolName string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := resolveTool(userID, toolName)
	return ok
}

// ToolsRequests returns OpenAI-format tool definitions for a user's visible
// MCP tools, using namespaced names (serverName__toolName) and enriched descriptions.
func ToolsRequests(userID string) []utils.ToolsRequest {
	mu.RLock()
	defer mu.RUnlock()
	merged := mergedTools(userID)
	out := make([]utils.ToolsRequest, 0, len(merged))
	for nsName, t := range merged {
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
			Items       struct {
				Type        string   `json:"type"`
				Description string   `json:"description"`
				Enum        []string `json:"enum"`
			} `json:"items"`
		}
		_ = json.Unmarshal(propRaw, &prop)
		if prop.Type == "" {
			prop.Type = "string"
		}
		newProp := utils.PropertyParameterToolsRequest{
			Type:        prop.Type,
			Description: prop.Description,
			Enum:        prop.Enum,
		}
		// Array properties MUST carry an "items" schema — Google's Gemini
		// endpoint rejects tool declarations whose array params lack it
		// ("items: missing field" 400s on vision turns). Preserve the item
		// schema when the MCP tool defined one; otherwise default to string.
		if prop.Type == "array" {
			items := utils.PropertyParameterToolsRequest{
				Type:        "string",
				Description: "Element of the array.",
			}
			if prop.Items.Type != "" {
				items = utils.PropertyParameterToolsRequest{
					Type:        prop.Items.Type,
					Description: prop.Items.Description,
					Enum:        prop.Items.Enum,
				}
			}
			newProp.Items = &items
		}
		props[name] = newProp
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
func CallTool(ctx context.Context, userID, toolName, argsJSON, conversationID string) utils.MessageContent {
	mu.RLock()
	info, ok := resolveTool(userID, toolName)
	mu.RUnlock()

	if !ok {
		return utils.NewTextContent(fmt.Sprintf("Error: MCP tool %q not found", toolName))
	}

	// Remember which user owns this conversation so stateful spawn/cleanup can
	// resolve the right config later (CleanupConversation only has the convID).
	mu.Lock()
	convOwner[conversationID] = userID
	mu.Unlock()

	// Resolve the process manager: per-conversation for stateful, shared otherwise.
	pm, err := getProcessForCall(userID, info.ServerName, conversationID)
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

// getProcessForCall returns the correct ProcessManager for a tool call in a
// user's view. Stateful servers get a per-conversation process; shared servers
// use the running one.
func getProcessForCall(userID, serverName, conversationID string) (*ProcessManager, error) {
	mu.RLock()
	cfg, isStateful := resolveStatefulConfig(userID, serverName)
	if !isStateful {
		pm, ok := resolveSharedProcess(userID, serverName)
		mu.RUnlock()
		if !ok {
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
	delete(convOwner, conversationID)
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
			cleanupRunning = false
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
				delete(convOwner, convID)
			}
		}
		mu.Unlock()
	}
}

// GetMCPLogs returns the recent stderr logs for an MCP process.
// For stateful servers, uses the conversation-specific process.
// For shared servers, uses the global process.
func GetMCPLogs(userID, serverName, conversationID string) string {
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

	// Try shared process (global, then user's own)
	if pm, ok := resolveSharedProcess(userID, serverName); ok {
		logs := pm.GetLogs()
		if len(logs) == 0 {
			return "(no logs captured yet)"
		}
		return strings.Join(logs, "\n")
	}

	return fmt.Sprintf("(no process found for %s)", serverName)
}

// ListAllMCPServers returns names of all MCP servers configured in a user's
// view (global + their own).
func ListAllMCPServers(userID string) []string {
	mu.RLock()
	defer mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	for _, uid := range []string{globalUserID, userID} {
		for name := range statefulConfigs[uid] {
			add(name)
		}
		for name := range processes[uid] {
			add(name)
		}
	}
	return out
}

// HasAnyServers returns true if any MCP servers are configured in a user's view.
func HasAnyServers(userID string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return len(statefulConfigs[globalUserID]) > 0 || len(processes[globalUserID]) > 0 ||
		len(statefulConfigs[userID]) > 0 || len(processes[userID]) > 0
}

// defaultMCPConfig returns the default mcp.json content, including
// Playwright for headless browser automation.
func defaultMCPConfig() string {
	cfg := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"playwright": map[string]interface{}{
				"command":     "npx",
				"args":        []string{"@playwright/mcp", "--headless"},
				"stateful":    true,
				"description": "Playwright headless browser for interactive web browsing with JavaScript support. Only use for heavier web tasks requiring JS rendering, form interaction, or dynamic content. For simple page fetching, prefer the visit_link tool instead.",
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b) + "\n"
}
