// Package broker tracks upstream MCP servers and manages the relationship from clients to upstream
package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/Kuadrant/mcp-gateway/internal/broker/upstream"
	"github.com/Kuadrant/mcp-gateway/internal/config"
	"github.com/Kuadrant/mcp-gateway/internal/session"
	"github.com/maleck13/tdt"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// tdtToolSelectionKey is the reserved hash field in the session cache for storing
// per-session tool selections made via the discover_tools tool.
const tdtToolSelectionKey = "__tdt_tools"

var _ config.Observer = &mcpBrokerImpl{}

// MCPBroker manages a set of MCP servers and their sessions
type MCPBroker interface {

	// Returns tool annotations for a given tool name
	ToolAnnotations(serverID config.UpstreamMCPID, tool string) (mcp.ToolAnnotation, bool)

	// Returns server info for a given tool name
	GetServerInfo(tool string) (*config.MCPServer, error)

	// MCPServer gets an MCP server that federates the upstreams known to this MCPBroker
	MCPServer() *server.MCPServer

	//RegisteredServers returns the map of registered servers
	RegisteredMCPServers() map[config.UpstreamMCPID]*upstream.MCPManager

	// GetVirtualSeverByHeader returns a virtual server definition based on a header where the header is the namespaced/name of the virtual server resource
	GetVirtualSeverByHeader(namespaceName string) (config.VirtualServer, error)

	// ValidateAllServers performs comprehensive validation of all registered servers and returns status
	ValidateAllServers() StatusResponse

	// HandleStatusRequest handles HTTP status endpoint requests
	HandleStatusRequest(w http.ResponseWriter, r *http.Request)

	// RankedSearch performs relevance-ranked tool discovery
	RankedSearch(query tdt.Query, opts tdt.SearchOptions) []tdt.ScoredTool

	// IsBrokerTool returns true if the named tool is registered directly on
	// the broker's MCP server rather than on an upstream server.
	IsBrokerTool(name string) bool

	// Shutdown closes any resources associated with this Broker
	Shutdown(ctx context.Context) error

	config.Observer
}

// mcpBrokerImpl implements MCPBroker
type mcpBrokerImpl struct {
	virtualServers map[string]*config.VirtualServer
	vsLock         sync.RWMutex //vsLock is for managing access to the virtual servers

	// mcpServers tracks the known servers
	mcpServers map[config.UpstreamMCPID]*upstream.MCPManager
	// protects mcpServers
	mcpLock sync.RWMutex

	// listeningMCPServer returns an actual listening MCP server that federates registered MCP servers
	listeningMCPServer *server.MCPServer

	// brokerTools tracks tool names registered directly on the broker (not on upstream servers)
	brokerTools map[string]bool

	logger *slog.Logger

	// enforceToolFilter if set will ensure only a filtered list of tools is returned this list is based on the x-authorized-tools trusted header
	enforceToolFilter bool

	// trustedHeadersPublicKey this is the key to verify that a trusted header came from the trusted source (the owner of the private key)
	trustedHeadersPublicKey string

	// managerTickerInterval is the interval for MCP manager backend health checks
	managerTickerInterval time.Duration

	// enableToolDiscovery gates all tdt features: the tool index, discover_tools
	// registration, ranked search, and per-session tool selection filtering.
	enableToolDiscovery bool

	// sessionCache stores per-session tool selections alongside backend session mappings.
	// Optional — if nil, session tool selection is a no-op.
	sessionCache *session.Cache

	// toolIndex holds the tdt index for relevance-ranked tool discovery
	toolIndex *tdt.Index
}

// this ensures that mcpBrokerImpl implements the MCPBroker interface
var _ MCPBroker = &mcpBrokerImpl{}

// WithEnforceToolFilter defines enforceToolFilter setting and is intended for use with the NewBroker function
func WithEnforceToolFilter(enforce bool) func(mb *mcpBrokerImpl) {
	return func(mb *mcpBrokerImpl) {
		mb.enforceToolFilter = enforce
	}
}

// WithTrustedHeadersPublicKey defines the public key used to verify signed headers and is intended for use with the NewBroker function
func WithTrustedHeadersPublicKey(key string) func(mb *mcpBrokerImpl) {
	return func(mb *mcpBrokerImpl) {
		mb.trustedHeadersPublicKey = key
	}
}

// WithManagerTickerInterval sets the interval for MCP manager backend health checks
func WithManagerTickerInterval(interval time.Duration) func(mb *mcpBrokerImpl) {
	return func(mb *mcpBrokerImpl) {
		mb.managerTickerInterval = interval
	}
}

// WithEnableToolDiscovery enables tdt-based tool discovery features: the tool index,
// discover_tools MCP tool, ranked search, and per-session tool selection filtering.
func WithEnableToolDiscovery(enable bool) func(mb *mcpBrokerImpl) {
	return func(mb *mcpBrokerImpl) {
		mb.enableToolDiscovery = enable
	}
}

// WithSessionCache sets the session cache used for per-session tool selections.
// Only effective when tool discovery is enabled.
func WithSessionCache(cache *session.Cache) func(mb *mcpBrokerImpl) {
	return func(mb *mcpBrokerImpl) {
		mb.sessionCache = cache
	}
}

// NewBroker creates a new MCPBroker accepts optional config functions such as WithEnforceToolFilter
func NewBroker(logger *slog.Logger, opts ...func(*mcpBrokerImpl)) MCPBroker {
	mcpBkr := &mcpBrokerImpl{
		mcpServers:            map[config.UpstreamMCPID]*upstream.MCPManager{},
		logger:                logger,
		virtualServers:        map[string]*config.VirtualServer{},
		managerTickerInterval: time.Second * 60,
		brokerTools:           map[string]bool{},
	}

	for _, option := range opts {
		option(mcpBkr)
	}

	hooks := &server.Hooks{}

	// Enhanced session registration to log gateway session assignment
	hooks.AddOnRegisterSession(func(_ context.Context, session server.ClientSession) {
		// Note that AddOnRegisterSession is for GET, not POST, for a session.
		// https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#listening-for-messages-from-the-server
		slog.Info("Broker: Gateway client session connected with session", "gatewaySessionID", session.SessionID())
	})

	hooks.AddOnUnregisterSession(func(_ context.Context, session server.ClientSession) {
		slog.Info("Broker: Gateway client session unregister ", "gatewaySessionID", session.SessionID())
	})

	hooks.AddBeforeAny(func(_ context.Context, _ any, method mcp.MCPMethod, _ any) {
		slog.Info("Processing request", "method", method)
	})

	hooks.AddOnError(func(_ context.Context, _ any, method mcp.MCPMethod, _ any, err error) {
		slog.Info("MCP server error", "method", method, "error", err)
	})

	hooks.AddAfterListTools(func(ctx context.Context, id any, message *mcp.ListToolsRequest, result *mcp.ListToolsResult) {
		mcpBkr.FilterTools(ctx, id, message, result)
	})

	mcpBkr.listeningMCPServer = server.NewMCPServer(
		"Kagenti MCP Broker",
		"0.0.1",
		server.WithHooks(hooks),
		server.WithToolCapabilities(true),
	)

	// Initialize the tdt index for tool discovery when enabled
	if mcpBkr.enableToolDiscovery {
		mcpBkr.toolIndex = tdt.NewIndex()
		discoveryTool, discoveryHandler := mcpBkr.newDiscoverToolsHandler()
		mcpBkr.listeningMCPServer.AddTools(server.ServerTool{
			Tool:    discoveryTool,
			Handler: discoveryHandler,
		})
		mcpBkr.brokerTools[discoveryTool.Name] = true
	}

	return mcpBkr
}

func (m *mcpBrokerImpl) OnConfigChange(ctx context.Context, conf *config.MCPServersConfig) {
	m.logger.Debug("Broker OnConfigChange start", "Total managers for upstream mcp servers", len(m.mcpServers), "total servers", len(conf.Servers))
	// unregister decommissioned servers
	m.mcpLock.Lock()
	defer m.mcpLock.Unlock()

	for serverID := range m.mcpServers {
		if !slices.ContainsFunc(conf.Servers, func(s *config.MCPServer) bool {
			return serverID == s.ID()
		}) {
			m.logger.Info("un-register upstream server", "server id", serverID)
			if man, ok := m.mcpServers[serverID]; ok {
				m.logger.Info("stopping manager for unregistered server", "server id", serverID)
				man.Stop()
				delete(m.mcpServers, serverID)
			}
		}
	}
	// ensure new servers registered

	for _, mcpServer := range conf.Servers {
		man, ok := m.mcpServers[mcpServer.ID()]
		if ok {
			m.logger.Info("Server is registered", "mcpID", mcpServer.ID())
			// already have a manger
			if mcpServer.ConfigChanged(man.MCP.GetConfig()) {
				// todo prob could look at just updating the config
				m.logger.Info("Server Config Changed removing manager", "mcpID", mcpServer.ID())
				man.Stop()
				delete(m.mcpServers, mcpServer.ID())
			}
		}
		// check if we need to setup a new manager
		if _, ok := m.mcpServers[mcpServer.ID()]; !ok {
			m.logger.Info("starting new manager", "server id", mcpServer.ID())
			var onToolsChanged upstream.OnToolsChanged
			if m.enableToolDiscovery {
				onToolsChanged = m.rebuildToolIndex
			}
			manager := upstream.NewUpstreamMCPManager(upstream.NewUpstreamMCP(mcpServer), m.listeningMCPServer, m.logger.With("sub-component", "mcp-manager"), m.managerTickerInterval, onToolsChanged)
			m.mcpServers[mcpServer.ID()] = manager
			go func() {
				m.logger.Info("Starting manager for", "mcpID", mcpServer.ID())
				manager.Start(ctx)
			}()
		}
	}
	// register virtual servers
	m.vsLock.Lock()
	for _, vs := range conf.VirtualServers {
		m.virtualServers[vs.Name] = vs
	}
	m.vsLock.Unlock()
	m.logger.Debug("Broker OnConfigChange done", "Total managers for upstream mcp servers", len(m.mcpServers), "total servers", len(conf.Servers))
}

// rebuildToolIndex collects tools from all managers and rebuilds the tdt index.
// Called by MCPManager callbacks after tool discovery completes.
func (m *mcpBrokerImpl) rebuildToolIndex() {
	m.mcpLock.RLock()
	defer m.mcpLock.RUnlock()

	var servers []tdt.ServerMetadata
	for _, manager := range m.mcpServers {
		cfg := manager.MCP.GetConfig()
		tools := manager.GetManagedTools()
		toolInfos := make([]tdt.ToolInfo, len(tools))
		for i, t := range tools {
			toolInfos[i] = tdt.ToolInfo{
				Name:        t.Name,
				Description: t.Description,
			}
		}
		servers = append(servers, tdt.ServerMetadata{
			ServerName: cfg.Name,
			ToolPrefix: cfg.ToolPrefix,
			Category:   cfg.Category,
			Tags:       cfg.Tags,
			Hint:       cfg.Hint,
			Tools:      toolInfos,
		})
	}
	m.toolIndex.Update(servers)
	m.logger.Debug("tdt index rebuilt", "servers", len(servers))
}

// RankedSearch performs relevance-ranked tool discovery using the tdt index.
// Returns nil when tool discovery is disabled.
func (m *mcpBrokerImpl) RankedSearch(query tdt.Query, opts tdt.SearchOptions) []tdt.ScoredTool {
	if !m.enableToolDiscovery || m.toolIndex == nil {
		return nil
	}
	return m.toolIndex.RankedSearch(query, opts)
}

// notifyToolsListChanged sends a notifications/tools/list_changed notification
// to the client so it re-fetches tools/list after a session scope change.
func (m *mcpBrokerImpl) notifyToolsListChanged(ctx context.Context) {
	mcpServer := server.ServerFromContext(ctx)
	if mcpServer == nil {
		return
	}
	if err := mcpServer.SendNotificationToClient(ctx, "notifications/tools/list_changed", nil); err != nil {
		m.logger.Error("failed to send tools/list_changed notification", "error", err)
	}
}

// discoverToolsResponse is the JSON response for the discover_tools tool.
type discoverToolsResponse struct {
	Action       string `json:"action"`
	ToolsMatched int    `json:"tools_matched,omitempty"`
	Message      string `json:"message"`
}

// newDiscoverToolsHandler returns the MCP tool definition and handler for the broker's
// discover_tools tool. It supports three modes:
//   - reset=true: clears the session tool selection
//   - query provided: runs RankedSearch, stores results as session selection
//   - empty/no params: returns the catalog
func (m *mcpBrokerImpl) newDiscoverToolsHandler() (mcp.Tool, server.ToolHandlerFunc) {
	tool := mcp.Tool{
		Name: "discover_tools",
		Description: "Search for relevant tools and scope your session to only use the matched tools. " +
			"Call with a query to find and select tools. Call with reset=true to clear the selection and see all tools again. " +
			"Call with both reset=true and a query to clear the current selection and search in one step. " +
			"Call with no parameters to see the full catalog of available tool categories. " +
			"IMPORTANT: After a search or reset, your tool list will be updated on the NEXT turn. " +
			"You MUST end your current turn after calling this tool. Do NOT attempt to call any " +
			"discovered tools in the same turn — they will only appear in your tool list on the next turn.",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Natural language description of what tools you need (e.g. 'weather data', 'CI/CD pipelines')",
				},
				"reset": map[string]any{
					"type":        "boolean",
					"description": "Set to true to clear the current tool selection and restore access to all tools",
				},
			},
		},
		Annotations: mcp.ToolAnnotation{
			ReadOnlyHint: mcp.ToBoolPtr(true),
		},
	}

	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		// Check for reset flag — clear the selection first
		wantsReset := false
		if resetVal, ok := args["reset"]; ok {
			if reset, isBool := resetVal.(bool); isBool && reset {
				wantsReset = true
				session := server.ClientSessionFromContext(ctx)
				if session != nil {
					if err := m.ClearSessionToolSelection(ctx, session.SessionID()); err != nil {
						m.logger.Error("failed to clear session tool selection", "error", err)
					}
					// Don't notify here — if a query follows, handleDiscoverToolsSearch
					// will store a new selection and notify after the final state is set.
					// Notifying now would cause the client to re-fetch tools/list before
					// the new selection is in place, leading to a stale tool set.
				}
			}
		}

		// Check for query — search and scope (works after a reset too)
		if queryVal, ok := args["query"]; ok {
			if queryStr, isStr := queryVal.(string); isStr && queryStr != "" {
				return m.handleDiscoverToolsSearch(ctx, queryStr)
			}
		}

		// Reset-only (no query): notify now that the final state is established
		if wantsReset {
			m.notifyToolsListChanged(ctx)
			resp := discoverToolsResponse{
				Action:  "reset",
				Message: "Tool selection cleared. All available tools will now be returned by tools/list.",
			}
			data, _ := json.Marshal(resp)
			return mcp.NewToolResultText(string(data)), nil
		}

		// Default: return catalog
		return m.handleDiscoverToolsCatalog()
	}

	return tool, handler
}

// handleDiscoverToolsSearch runs a ranked search and stores the results as the session selection.
func (m *mcpBrokerImpl) handleDiscoverToolsSearch(ctx context.Context, queryStr string) (*mcp.CallToolResult, error) {
	allResults := m.toolIndex.RankedSearch(tdt.Query{Text: queryStr}, tdt.SearchOptions{})

	// Keep only tools with meaningful relevance (score > 0.60)
	results := make([]tdt.ScoredTool, 0, len(allResults))
	for _, r := range allResults {
		if r.Score > 0.60 {
			results = append(results, r)
		}
	}

	// Build prefixed tool names for the session selection
	prefixedNames := make([]string, 0, len(results))
	for _, r := range results {
		prefixed := m.prefixToolName(r.ServerName, r.ToolName)
		prefixedNames = append(prefixedNames, prefixed)
	}

	// Store the selection in the session cache
	session := server.ClientSessionFromContext(ctx)
	if session != nil && m.sessionCache != nil {
		if err := m.SetSessionToolSelection(ctx, session.SessionID(), prefixedNames); err != nil {
			m.logger.Error("failed to store session tool selection", "error", err)
			// Don't fail the tool call — return results anyway
		}
	}
	m.notifyToolsListChanged(ctx)

	resp := discoverToolsResponse{
		Action:       "search",
		ToolsMatched: len(results),
		Message:      fmt.Sprintf("Found %d tools matching your query. Your tool list will update on the next turn. End your current turn now — do not attempt to call any tools until the next turn when your updated tool definitions are available. Call with reset=true to restore all tools.", len(results)),
	}
	data, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(data)), nil
}

// handleDiscoverToolsCatalog returns the full catalog.
func (m *mcpBrokerImpl) handleDiscoverToolsCatalog() (*mcp.CallToolResult, error) {
	catalog := m.toolIndex.Catalog()
	data, err := json.Marshal(catalog)
	if err != nil {
		return mcp.NewToolResultError("failed to marshal catalog: " + err.Error()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// prefixToolName returns the prefixed tool name by looking up the server's prefix.
func (m *mcpBrokerImpl) prefixToolName(serverName, toolName string) string {
	m.mcpLock.RLock()
	defer m.mcpLock.RUnlock()
	for _, manager := range m.mcpServers {
		if manager.MCPName() == serverName {
			prefix := manager.MCP.GetPrefix()
			if prefix == "" {
				return toolName
			}
			return prefix + toolName
		}
	}
	return toolName
}

func (m *mcpBrokerImpl) RegisteredMCPServers() map[config.UpstreamMCPID]*upstream.MCPManager {
	m.mcpLock.RLock()
	defer m.mcpLock.RUnlock()
	return m.mcpServers
}

func (m *mcpBrokerImpl) GetVirtualSeverByHeader(namespaceName string) (config.VirtualServer, error) {
	m.vsLock.RLock()
	defer m.vsLock.RUnlock()
	for _, vs := range m.virtualServers {
		if vs.Name == namespaceName {
			return *vs, nil
		}
	}
	return config.VirtualServer{}, fmt.Errorf("virtual server %s not found", namespaceName)
}

func (m *mcpBrokerImpl) ToolAnnotations(serverID config.UpstreamMCPID, tool string) (mcp.ToolAnnotation, bool) {
	// Avoid race with OnConfigChange()
	m.mcpLock.RLock()
	defer m.mcpLock.RUnlock()

	upstream, ok := m.mcpServers[serverID]
	if !ok {
		return mcp.ToolAnnotation{}, false
	}
	t := upstream.GetServedManagedTool(tool)
	if t != nil {
		return t.Annotations, true
	}
	return mcp.ToolAnnotation{}, false
}

// GetServerInfo implements MCPBroker by providing a lookup of the server that implements a tool.
func (m *mcpBrokerImpl) GetServerInfo(tool string) (*config.MCPServer, error) {
	// Avoid race with OnConfigChange()
	m.mcpLock.RLock()
	defer m.mcpLock.RUnlock()

	for _, upstream := range m.mcpServers {
		t := upstream.GetServedManagedTool(tool)
		if t != nil {
			slog.Info("[EXT-PROC] Found matching server",
				"toolName", tool,
				"serverPrefix", upstream.MCP.GetPrefix(),
				"serverName", upstream.MCP.GetName())
			retval := upstream.MCP.GetConfig()
			return &retval, nil
		}
	}

	return nil, fmt.Errorf("tool name %q doesn't match any configured server", tool)
}

func (m *mcpBrokerImpl) IsBrokerTool(name string) bool {
	return m.brokerTools[name]
}

func (m *mcpBrokerImpl) Shutdown(_ context.Context) error {
	// Avoid race with OnConfigChange()
	m.mcpLock.RLock()
	defer m.mcpLock.RUnlock()

	// Close the long-running notification channel
	for _, mcpServer := range m.mcpServers {
		if mcpServer != nil {
			mcpServer.Stop()
		}
	}
	return nil
}

// MCPServer is a listening MCP server that federates the endpoints
func (m *mcpBrokerImpl) MCPServer() *server.MCPServer {
	return m.listeningMCPServer
}

// HandleStatusRequest handles HTTP status endpoint requests
func (m *mcpBrokerImpl) HandleStatusRequest(w http.ResponseWriter, r *http.Request) {
	handler := NewStatusHandler(m, *m.logger)
	handler.ServeHTTP(w, r)
}

// ValidateAllServers performs comprehensive validation of all registered servers and returns status
func (m *mcpBrokerImpl) ValidateAllServers() StatusResponse {
	// The race is with len(m.mcpServers), which is not thread-safe in Go
	m.mcpLock.RLock()
	defer m.mcpLock.RUnlock()

	response := StatusResponse{
		Servers:          make([]upstream.ServerValidationStatus, 0),
		OverallValid:     true,
		TotalServers:     len(m.mcpServers),
		HealthyServers:   0,
		UnHealthyServers: 0,
		ToolConflicts:    0,
		Timestamp:        time.Now(),
	}

	m.logger.Debug("ValidateAllServers: checking servers", "# servers", len(m.mcpServers))

	for _, upstream := range m.RegisteredMCPServers() {
		status := upstream.GetStatus()
		response.Servers = append(response.Servers, status)

		if !status.Ready {
			response.UnHealthyServers++
			response.OverallValid = false
		} else {
			response.HealthyServers++
		}
	}

	m.logger.Info("Server validation completed",
		"totalServers", response.TotalServers,
		"healthyServers", response.HealthyServers,
		"unhealthyServers", response.UnHealthyServers,
		"overallValid", response.OverallValid)

	return response
}

// SetSessionToolSelection stores a list of selected tool names for the given session.
func (m *mcpBrokerImpl) SetSessionToolSelection(ctx context.Context, sessionID string, toolNames []string) error {
	if m.sessionCache == nil {
		return nil
	}
	data, err := json.Marshal(toolNames)
	if err != nil {
		return fmt.Errorf("failed to marshal tool selection: %w", err)
	}
	_, err = m.sessionCache.AddSession(ctx, sessionID, tdtToolSelectionKey, string(data))
	return err
}

// GetSessionToolSelection retrieves the per-session tool selection.
// Returns (nil, false, nil) if no selection exists.
func (m *mcpBrokerImpl) GetSessionToolSelection(ctx context.Context, sessionID string) ([]string, bool, error) {
	if m.sessionCache == nil {
		return nil, false, nil
	}
	sessions, err := m.sessionCache.GetSession(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	raw, ok := sessions[tdtToolSelectionKey]
	if !ok || raw == "" {
		return nil, false, nil
	}
	var toolNames []string
	if err := json.Unmarshal([]byte(raw), &toolNames); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal tool selection: %w", err)
	}
	return toolNames, true, nil
}

// ClearSessionToolSelection removes the per-session tool selection.
func (m *mcpBrokerImpl) ClearSessionToolSelection(ctx context.Context, sessionID string) error {
	if m.sessionCache == nil {
		return nil
	}
	return m.sessionCache.RemoveServerSession(ctx, sessionID, tdtToolSelectionKey)
}
