package gateway

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/version"
)

// toolGroup says which agents get a meta-tool: an agent only sees the tools its role lets
// it use, which keeps its context small and never offers a tool it can't call.
type toolGroup int

const (
	groupConnectors toolGroup = iota // agents that can see at least one connector
	groupProfiles                    // agents that can see at least one profile
	groupApprovals                   // either of those, whose approvals can be answered outside the client
)

type metaTool struct {
	tool    *mcp.Tool
	handler mcp.ToolHandler
	group   toolGroup
}

// metaTools is every tool the broker offers, in one place.
func (g *Gateway) metaTools() []metaTool {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	approvalID := str("Approval id from a pending_approval result, to retry once approved")
	return []metaTool{
		{&mcp.Tool{
			Name:        "list_connectors",
			Description: "List the connectors (downstream MCP servers) you can use.",
			InputSchema: schema(nil),
			Annotations: ro,
		}, g.listConnectors, groupConnectors},
		{&mcp.Tool{
			Name:        "load_connector",
			Description: "List a connector's tools, one line each; describe_tool gives a tool's schema.",
			InputSchema: schema(map[string]any{
				"name":   str("Connector name from list_connectors"),
				"detail": map[string]any{"type": "string", "enum": []string{"summary", "full"}, "description": "summary (default), or full: every schema"},
			}, "name"),
			Annotations: ro,
		}, g.loadConnector, groupConnectors},
		{&mcp.Tool{
			Name:        "describe_tool",
			Description: "Get a connector tool's description and input schema.",
			InputSchema: schema(map[string]any{
				"connector": str("Connector name"),
				"tool":      str("Tool name from load_connector"),
			}, "connector", "tool"),
			Annotations: ro,
		}, g.describeTool, groupConnectors},
		{&mcp.Tool{
			Name:        "call_tool",
			Description: "Call a connector's tool. May wait for the user's approval.",
			InputSchema: schema(map[string]any{
				"connector":   str("Connector name"),
				"tool":        str("Tool name from load_connector"),
				"arguments":   map[string]any{"type": "object", "description": "Per the tool's schema"},
				"approval_id": approvalID,
			}, "connector", "tool"),
		}, g.callTool, groupConnectors},
		{&mcp.Tool{
			Name:        "unload_connector",
			Description: "Stop a connector.",
			InputSchema: schema(map[string]any{"name": str("Connector name")}, "name"),
		}, g.unloadConnector, groupConnectors},
		{&mcp.Tool{
			Name:        "approval_status",
			Description: "Check a pending approval.",
			InputSchema: schema(map[string]any{"approval_id": str("Approval id")}, "approval_id"),
			Annotations: ro,
		}, g.approvalStatus, groupApprovals},
		{&mcp.Tool{
			Name:        "list_profiles",
			Description: "List the app profiles (local apps such as ComfyUI) you can use.",
			InputSchema: schema(nil),
			Annotations: ro,
		}, g.listProfiles, groupProfiles},
		{&mcp.Tool{
			Name:        "profile",
			Description: "Act on an app profile. start, stop and update may need the user's approval.",
			InputSchema: schema(map[string]any{
				"name":         str("Profile name from list_profiles"),
				"action":       map[string]any{"type": "string", "enum": []string{"status", "logs", "plan", "start", "stop", "update"}, "description": "start returns at once; poll status until ready"},
				"tail":         map[string]any{"type": "integer", "description": "logs: lines (default 100, max 1000)"},
				"for":          map[string]any{"type": "string", "enum": []string{"start", "stop", "update"}, "description": "plan: which action (default start)"},
				"kill_foreign": map[string]any{"type": "boolean", "description": "stop: kill a process the broker didn't start that holds the profile's port (needs approval)"},
				"approval_id":  approvalID,
			}, "name", "action"),
		}, g.profileTool, groupProfiles},
	}
}

// instructions is the server instructions for an agent with the given tool groups.
func instructions(groups map[toolGroup]bool) string {
	var b strings.Builder
	b.WriteString("This is mcp-broker.")
	if groups[groupConnectors] {
		b.WriteString(" Start with list_connectors.")
	}
	if groups[groupProfiles] {
		b.WriteString(" list_profiles runs local apps.")
	}
	if groups[groupConnectors] || groups[groupProfiles] {
		b.WriteString(" Some calls need the user's approval.")
	}
	return b.String()
}

var allGroups = map[toolGroup]bool{groupConnectors: true, groupProfiles: true, groupApprovals: true}

func newServer(groups map[toolGroup]bool) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "mcp-broker", Version: version.Version}, &mcp.ServerOptions{
		Instructions: instructions(groups),
		// Declared even when an agent has no tools yet: a reload may give it some, and the
		// client then hears tools/list_changed.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
		// Sessions are required (server-initiated elicitation for approvals and passthrough),
		// and the SDK serves the 2026-07-28 protocol only statelessly, so pin the
		// session-based versions explicitly rather than rely on the HTTP layer to refuse.
		SupportedProtocolVersions: []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
	})
}

// Server builds an MCP server with every meta-tool, whoever calls. Agents get ServerFor.
func (g *Gateway) Server() *mcp.Server {
	s := newServer(allGroups)
	for _, t := range g.metaTools() {
		s.AddTool(t.tool, t.handler)
	}
	return s
}

// groupsFor works out which tool groups agent gets under the current config.
func (g *Gateway) groupsFor(agent string) map[toolGroup]bool {
	st := g.st()
	out := map[toolGroup]bool{}
	if len(st.pol.VisibleConnectors(agent)) > 0 {
		out[groupConnectors] = true
	}
	for name := range st.cfg.Profiles {
		if g.profileVisible(agent, name) {
			out[groupProfiles] = true
			break
		}
	}
	if out[groupConnectors] || out[groupProfiles] {
		for _, c := range st.pol.Channels(agent) {
			if c != config.ChanElicitation {
				out[groupApprovals] = true
			}
		}
	}
	return out
}

// agentServer is one agent's MCP server and the meta-tools it has registered.
type agentServer struct {
	srv   *mcp.Server
	tools map[string]bool
}

type servers struct {
	mu sync.Mutex
	m  map[string]*agentServer
}

// ServerFor is agent's MCP server: only the meta-tools its role lets it use. It is built
// once per agent and kept up to date on reload (tools added or removed there reach
// connected clients as tools/list_changed).
func (g *Gateway) ServerFor(agent string) *mcp.Server {
	g.srvs.mu.Lock()
	defer g.srvs.mu.Unlock()
	if as, ok := g.srvs.m[agent]; ok {
		return as.srv
	}
	if g.srvs.m == nil {
		g.srvs.m = map[string]*agentServer{}
	}
	as := &agentServer{srv: newServer(g.groupsFor(agent)), tools: map[string]bool{}}
	g.syncServer(agent, as)
	g.srvs.m[agent] = as
	return as.srv
}

// syncServer registers the meta-tools agent may now use on its server and removes the
// rest. The caller holds g.srvs.mu.
func (g *Gateway) syncServer(agent string, as *agentServer) {
	groups := g.groupsFor(agent)
	var remove []string
	for _, t := range g.metaTools() {
		name := t.tool.Name
		switch want := groups[t.group]; {
		case want && !as.tools[name]:
			as.srv.AddTool(t.tool, t.handler)
			as.tools[name] = true
		case !want && as.tools[name]:
			remove = append(remove, name)
			delete(as.tools, name)
		}
	}
	if len(remove) > 0 {
		as.srv.RemoveTools(remove...)
	}
}

// syncServers brings every agent's server in line with the current config.
func (g *Gateway) syncServers() {
	g.srvs.mu.Lock()
	defer g.srvs.mu.Unlock()
	for agent, as := range g.srvs.m {
		g.syncServer(agent, as)
	}
}

// BrokerContextBytes is what agent's context holds before it loads any connector: the
// meta-tools it is offered and the server instructions. An empty agent means every tool.
func (g *Gateway) BrokerContextBytes(agent string) int {
	groups := allGroups
	if agent != "" {
		groups = g.groupsFor(agent)
	}
	var tools []*mcp.Tool
	for _, t := range g.metaTools() {
		if groups[t.group] {
			tools = append(tools, t.tool)
		}
	}
	b, _ := json.Marshal(tools)
	if len(tools) == 0 {
		b = nil
	}
	return len(b) + len(instructions(groups))
}
