// Package main implements the MCP routing Wasm filter for Envoy
package main

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/proxy-wasm/proxy-wasm-go-sdk/proxywasm"
	"github.com/proxy-wasm/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/tidwall/gjson"
)

const (
	mcpSessionHeader    = "mcp-session-id"
	authorityHeader     = ":authority"
	pathHeader          = ":path"
	mcpServerNameHeader = "x-mcp-servername"
	mcpToolNameHeader   = "x-mcp-toolname"
	mcpMethodHeader     = "x-mcp-method"
	mcpAPIKeyHeader     = "x-mcp-api-key"
	contentLengthHeader = "content-length"

	// hairpin headers for broker -> backend MCP server initialization
	mcpInitHostHeader = "mcp-init-host"
	routerKeyHeader   = "router-key"

	methodToolsCall   = "tools/call"
	methodInitialize  = "initialize"
	methodInitialized = "notifications/initialized"

	// shared data key prefix for session storage
	sessionKeyPrefix = "mcp:session:"

	// HTTP call timeout for lazy init (5 seconds)
	initCallTimeout = 5000
)

var (
	errInvalidJSON    = errors.New("invalid JSON")
	errInvalidJSONRPC = errors.New("invalid jsonrpc version")
	errMissingMethod  = errors.New("missing method")
)

// session storage helpers using shared data
// key format: mcp:session:{gatewaySessionID}:{serverID}
// value format: {expiresAt}:{backendSessionID} where expiresAt is unix timestamp

func sessionKey(gatewaySessionID, serverID string) string {
	return sessionKeyPrefix + gatewaySessionID + ":" + serverID
}

func getBackendSession(gatewaySessionID, serverID string) (string, bool) {
	key := sessionKey(gatewaySessionID, serverID)
	data, cas, err := proxywasm.GetSharedData(key)
	if err != nil || cas == 0 || len(data) == 0 {
		return "", false
	}

	// parse stored value: {expiresAt}:{backendSessionID}
	value := string(data)
	idx := strings.Index(value, ":")
	if idx == -1 {
		// invalid format, delete and return not found
		deleteBackendSession(gatewaySessionID, serverID)
		return "", false
	}

	// check expiration
	expiresAt, err := strconv.ParseInt(value[:idx], 10, 64)
	if err != nil {
		deleteBackendSession(gatewaySessionID, serverID)
		return "", false
	}
	if time.Now().UTC().Unix() > expiresAt {
		proxywasm.LogDebugf("mcp-routing: session expired for %s:%s", gatewaySessionID, serverID)
		deleteBackendSession(gatewaySessionID, serverID)
		return "", false
	}

	backendSessionID := value[idx+1:]
	return backendSessionID, true
}

func setBackendSession(gatewaySessionID, serverID, backendSessionID string, expiresAt int64) error {
	key := sessionKey(gatewaySessionID, serverID)
	// store as {expiresAt}:{backendSessionID}
	value := strconv.FormatInt(expiresAt, 10) + ":" + backendSessionID
	return proxywasm.SetSharedData(key, []byte(value), 0)
}

func deleteBackendSession(gatewaySessionID, serverID string) {
	key := sessionKey(gatewaySessionID, serverID)
	// set empty value to delete
	if err := proxywasm.SetSharedData(key, nil, 0); err != nil {
		proxywasm.LogDebugf("mcp-routing: failed to delete session: %v", err)
	}
}

// getJWTExpiry extracts the exp claim from a JWT session ID
func getJWTExpiry(sessionID string) int64 {
	parts := strings.Split(sessionID, ".")
	if len(parts) != 3 {
		return 0
	}

	payload := decodeBase64URL(parts[1])
	if payload == "" {
		return 0
	}

	exp := gjson.Get(payload, "exp")
	if !exp.Exists() {
		return 0
	}

	return exp.Int()
}

// min returns the smaller of two integers
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {}

func init() {
	proxywasm.LogInfo("mcp-routing: init() called, setting VM context")
	proxywasm.SetVMContext(&vmContext{})
}

// vmContext implements types.VMContext
type vmContext struct {
	types.DefaultVMContext
}

// NewPluginContext implements types.VMContext
func (*vmContext) NewPluginContext(contextID uint32) types.PluginContext {
	proxywasm.LogInfof("mcp-routing: NewPluginContext called, contextID=%d", contextID)
	return &pluginContext{}
}

// OnVMStart implements types.VMContext
func (*vmContext) OnVMStart(vmConfigurationSize int) types.OnVMStartStatus {
	proxywasm.LogInfo("mcp-routing: VM started")
	return types.OnVMStartStatusOK
}

// pluginContext holds the server configuration loaded from EnvoyFilter
type pluginContext struct {
	types.DefaultPluginContext
	config *pluginConfig
}

// pluginConfig represents the configuration from EnvoyFilter
type pluginConfig struct {
	Servers            map[string]*serverConfig `json:"servers"`
	BrokerHostname     string                   `json:"brokerHostname"`     // external hostname for :authority header
	BrokerInternalHost string                   `json:"brokerInternalHost"` // internal cluster name for DispatchHttpCall
	BrokerPath         string                   `json:"brokerPath"`
	RouterAPIKey       string                   `json:"routerAPIKey"` // for hairpin validation
}

// serverConfig represents an upstream MCP server configuration
type serverConfig struct {
	Hostname    string `json:"hostname"`
	Path        string `json:"path"`
	Credentials string `json:"credentials"`
	ToolPrefix  string `json:"toolPrefix"`
}

// OnPluginStart loads the plugin configuration
func (ctx *pluginContext) OnPluginStart(pluginConfigurationSize int) types.OnPluginStartStatus {
	proxywasm.LogInfof("mcp-routing: OnPluginStart called, configSize=%d", pluginConfigurationSize)
	data, err := proxywasm.GetPluginConfiguration()
	if err != nil {
		proxywasm.LogCriticalf("mcp-routing: failed to get plugin configuration: %v", err)
		return types.OnPluginStartStatusFailed
	}

	if len(data) == 0 {
		proxywasm.LogWarn("mcp-routing: empty plugin configuration, using defaults")
		ctx.config = &pluginConfig{
			Servers:            make(map[string]*serverConfig),
			BrokerHostname:     "mcp-broker-router.mcp-system.svc.cluster.local",
			BrokerInternalHost: "mcp-broker-router.mcp-system.svc.cluster.local",
			BrokerPath:         "/mcp",
		}
		return types.OnPluginStartStatusOK
	}

	cfg, err := parsePluginConfig(data)
	if err != nil {
		proxywasm.LogCriticalf("mcp-routing: failed to parse plugin configuration: %v", err)
		return types.OnPluginStartStatusFailed
	}

	ctx.config = cfg
	proxywasm.LogInfof("mcp-routing: loaded %d server configurations", len(cfg.Servers))
	return types.OnPluginStartStatusOK
}

// parsePluginConfig parses the JSON configuration using gjson
func parsePluginConfig(data []byte) (*pluginConfig, error) {
	proxywasm.LogDebugf("mcp-routing: parsePluginConfig called, dataLen=%d", len(data))
	proxywasm.LogDebugf("mcp-routing: config data: %s", string(data))

	cfg := &pluginConfig{
		Servers:            make(map[string]*serverConfig),
		BrokerHostname:     "mcp-broker-router.mcp-system.svc.cluster.local",
		BrokerInternalHost: "mcp-broker-router.mcp-system.svc.cluster.local",
		BrokerPath:         "/mcp",
	}

	json := string(data)

	if gjson.Get(json, "brokerHostname").Exists() {
		cfg.BrokerHostname = gjson.Get(json, "brokerHostname").String()
	}
	if gjson.Get(json, "brokerInternalHost").Exists() {
		cfg.BrokerInternalHost = gjson.Get(json, "brokerInternalHost").String()
	}
	if gjson.Get(json, "brokerPath").Exists() {
		cfg.BrokerPath = gjson.Get(json, "brokerPath").String()
	}
	if gjson.Get(json, "routerAPIKey").Exists() {
		cfg.RouterAPIKey = gjson.Get(json, "routerAPIKey").String()
	}

	servers := gjson.Get(json, "servers")
	if servers.Exists() && servers.IsObject() {
		servers.ForEach(func(key, value gjson.Result) bool {
			serverID := key.String()
			cfg.Servers[serverID] = &serverConfig{
				Hostname:    value.Get("hostname").String(),
				Path:        value.Get("path").String(),
				Credentials: value.Get("credentials").String(),
				ToolPrefix:  value.Get("toolPrefix").String(),
			}
			if cfg.Servers[serverID].Path == "" {
				cfg.Servers[serverID].Path = "/mcp"
			}
			return true
		})
	}

	return cfg, nil
}

// NewHttpContext implements types.PluginContext
func (ctx *pluginContext) NewHttpContext(contextID uint32) types.HttpContext {
	proxywasm.LogInfof("mcp-routing: NewHttpContext called, contextID=%d", contextID)
	return &httpContext{
		contextID: contextID,
		config:    ctx.config,
	}
}

// httpContext handles individual HTTP requests
type httpContext struct {
	types.DefaultHttpContext
	contextID           uint32
	config              *pluginConfig
	mcpSessionID        string
	toolMappings        map[string]string // tool name -> server ID from JWT
	targetServerID      string            // server we routed to (for response handling)
	upstreamSessionID   string            // session ID from upstream response
	mcpMethod           string            // MCP method being processed
	isInitializeRequest bool              // true if this is an initialize request

	// lazy init state
	pendingRequest      *mcpRequest   // stored while waiting for backend init
	pendingBody         []byte        // original request body
	pendingServerID     string        // server being initialized
	pendingServerCfg    *serverConfig // config of server being initialized
	pendingUpstreamTool string        // tool name with prefix stripped
	waitingForInit      bool          // true while waiting for init callback
	initCallID          uint32        // token ID from DispatchHttpCall
}

// OnHttpRequestHeaders buffers headers until body is available
func (ctx *httpContext) OnHttpRequestHeaders(numHeaders int, endOfStream bool) types.Action {
	proxywasm.LogInfof("mcp-routing: OnHttpRequestHeaders called, numHeaders=%d, endOfStream=%v", numHeaders, endOfStream)

	// test mode: if path contains ?test=true, return a simple response to verify wasm is working
	path, _ := proxywasm.GetHttpRequestHeader(":path")
	proxywasm.LogDebugf("mcp-routing: request path=%s", path)
	if strings.Contains(path, "test=true") {
		proxywasm.LogInfo("mcp-routing: test mode triggered")
		if err := proxywasm.SendHttpResponse(200, [][2]string{
			{"content-type", "application/json"},
			{"x-mcp-wasm-test", "ok"},
		}, []byte(`{"status":"wasm-ok","message":"mcp-routing wasm filter is working"}`), -1); err != nil {
			proxywasm.LogErrorf("mcp-routing: failed to send test response: %v", err)
		}
		return types.ActionPause
	}

	// extract session ID for later use
	sessionID, err := proxywasm.GetHttpRequestHeader(mcpSessionHeader)
	if err == nil && sessionID != "" {
		ctx.mcpSessionID = sessionID
		// parse tool mappings from session JWT
		ctx.toolMappings = parseToolMappingsFromJWT(sessionID)
	}
	if err != nil && errors.Is(err, types.ErrorStatusNotFound) {
		proxywasm.LogErrorf("mcp-routing: failed to get GetHttpRequestHeader: %v", err)
	}

	// buffer headers if body follows

	if !endOfStream {
		proxywasm.LogDebugf("mcp-routing: pausing on headers path=%s", path)
		return types.ActionPause
	}

	// no body (e.g., GET request), continue without modification
	proxywasm.LogDebugf("mcp-routing: Not pausing on headers path=%s", path)
	return types.ActionContinue
}

// OnHttpRequestBody handles the request body, parses MCP request, and sets routing headers
func (ctx *httpContext) OnHttpRequestBody(bodySize int, endOfStream bool) types.Action {
	proxywasm.LogInfof("mcp-routing: OnHttpRequestBody called, bodySize=%d, endOfStream=%v", bodySize, endOfStream)

	if !endOfStream {
		proxywasm.LogDebugf("mcp-routing: body not complete, pausing")
		return types.ActionPause
	}

	body, err := proxywasm.GetHttpRequestBody(0, bodySize)
	if err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to get request body: %v", err)
		return types.ActionContinue
	}

	mcpReq, err := parseMCPRequest(body)
	if err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to parse MCP request: %v", err)
		ctx.sendErrorResponse(400, "invalid MCP request")
		return types.ActionPause
	}

	// store method for response handling
	ctx.mcpMethod = mcpReq.Method

	switch mcpReq.Method {
	case methodToolsCall:
		return ctx.handleToolCall(mcpReq, body)
	case methodInitialize, methodInitialized:
		ctx.isInitializeRequest = true
		return ctx.handleBrokerRequest(mcpReq)
	default:
		return ctx.handleBrokerRequest(mcpReq)
	}
}

// mcpRequest represents a parsed MCP JSON-RPC request
type mcpRequest struct {
	ID       int64
	JSONRPC  string
	Method   string
	ToolName string
	rawBody  []byte
}

// parseMCPRequest parses the JSON-RPC request body using gjson
func parseMCPRequest(body []byte) (*mcpRequest, error) {
	proxywasm.LogDebugf("mcp-routing: parseMCPRequest called, bodyLen=%d", len(body))
	json := string(body)
	proxywasm.LogDebugf("mcp-routing: request body: %s", json)

	if !gjson.Valid(json) {
		proxywasm.LogError("mcp-routing: invalid JSON")
		return nil, errInvalidJSON
	}

	req := &mcpRequest{
		rawBody: body,
	}

	if gjson.Get(json, "id").Exists() {
		req.ID = gjson.Get(json, "id").Int()
	}
	req.JSONRPC = gjson.Get(json, "jsonrpc").String()
	req.Method = gjson.Get(json, "method").String()

	if req.Method == methodToolsCall {
		req.ToolName = gjson.Get(json, "params.name").String()
	}

	if req.JSONRPC != "2.0" {
		proxywasm.LogErrorf("mcp-routing: invalid jsonrpc version: %s", req.JSONRPC)
		return nil, errInvalidJSONRPC
	}

	if req.Method == "" {
		proxywasm.LogError("mcp-routing: missing method in request")
		return nil, errMissingMethod
	}

	proxywasm.LogDebugf("mcp-routing: parsed MCP request - method=%s, id=%d, tool=%s", req.Method, req.ID, req.ToolName)
	return req, nil
}

// handleToolCall routes tool calls to the appropriate upstream server
func (ctx *httpContext) handleToolCall(req *mcpRequest, body []byte) types.Action {
	proxywasm.LogInfof("mcp-routing: handleToolCall called, tool=%s, sessionID=%s", req.ToolName, ctx.mcpSessionID)

	if req.ToolName == "" {
		proxywasm.LogError("mcp-routing: no tool name in tools/call request")
		ctx.sendErrorResponse(400, "no tool name in request")
		return types.ActionPause
	}

	if ctx.mcpSessionID == "" {
		proxywasm.LogError("mcp-routing: no session ID for tool call")
		ctx.sendErrorResponse(400, "no session ID")
		return types.ActionPause
	}

	// find server for this tool
	serverID, serverCfg, upstreamToolName := ctx.resolveToolToServer(req.ToolName)
	if serverCfg == nil {
		proxywasm.LogErrorf("mcp-routing: no server found for tool: %s", req.ToolName)
		ctx.sendJSONRPCError(req.ID, -32602, "Tool not found")
		return types.ActionPause
	}

	// store for response handling
	ctx.targetServerID = serverID

	// check for existing backend session
	backendSessionID, hasSession := getBackendSession(ctx.mcpSessionID, serverID)
	if !hasSession {
		// no backend session, trigger lazy init
		proxywasm.LogInfof("mcp-routing: no backend session for %s, initiating lazy init", serverID)
		return ctx.triggerLazyInit(req, body, serverID, serverCfg, upstreamToolName)
	}

	proxywasm.LogInfof("mcp-routing: routing tool with backend session %s to server %s (upstream: %s, session: %s)",
		req.ToolName, serverID, upstreamToolName, backendSessionID[:minInt(20, len(backendSessionID))])

	return ctx.routeToolCallToBackend(req, body, serverCfg, serverID, upstreamToolName, backendSessionID)
}

// triggerLazyInit initiates an async HTTP call to initialize the backend MCP server
func (ctx *httpContext) triggerLazyInit(req *mcpRequest, body []byte, serverID string, serverCfg *serverConfig, upstreamToolName string) types.Action {
	proxywasm.LogInfof("mcp-routing: triggerLazyInit called, serverID=%s, tool=%s", serverID, upstreamToolName)

	// store pending request state
	ctx.pendingRequest = req
	ctx.pendingBody = body
	ctx.pendingServerID = serverID
	ctx.pendingServerCfg = serverCfg
	ctx.pendingUpstreamTool = upstreamToolName
	ctx.waitingForInit = true

	// build initialize request body
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcp-gateway","version":"1.0"}}}`

	// build headers for hairpin request
	headers := [][2]string{
		{":method", "POST"},
		{":path", serverCfg.Path},
		{":authority", ctx.config.BrokerHostname},
		{mcpInitHostHeader, serverCfg.Hostname},
		{routerKeyHeader, ctx.config.RouterAPIKey},
		{"content-type", "application/json"},
		{"accept", "text/event-stream"},
		{mcpMethodHeader, methodInitialize},
		{mcpServerNameHeader, serverID},
	}

	// add credentials if configured
	if serverCfg.Credentials != "" {
		headers = append(headers, [2]string{mcpAPIKeyHeader, serverCfg.Credentials})
	}

	// dispatch async HTTP call through the gateway (hairpin)
	clusterName := "outbound|8080||" + ctx.config.BrokerInternalHost
	proxywasm.LogDebugf("mcp-routing: dispatching to cluster: %s", clusterName)
	callID, err := proxywasm.DispatchHttpCall(
		clusterName,
		headers,
		[]byte(initBody),
		nil, // no trailers
		initCallTimeout,
		ctx.onInitCallback,
	)
	if err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to dispatch init call: %v", err)
		ctx.sendJSONRPCError(req.ID, -32603, "Failed to initialize backend")
		return types.ActionPause
	}

	ctx.initCallID = callID
	proxywasm.LogInfof("mcp-routing: dispatched lazy init for %s (call ID: %d)", serverID, callID)

	// pause request while waiting for init callback
	return types.ActionPause
}

// onInitCallback handles the response from the lazy init HTTP call
func (ctx *httpContext) onInitCallback(numHeaders, bodySize, numTrailers int) {
	proxywasm.LogInfof("mcp-routing: onInitCallback called, numHeaders=%d, bodySize=%d", numHeaders, bodySize)

	// get all response headers
	headers, err := proxywasm.GetHttpCallResponseHeaders()
	if err != nil {
		proxywasm.LogErrorf("mcp-routing: init callback failed to get headers: %v", err)
		ctx.sendJSONRPCError(ctx.pendingRequest.ID, -32603, "Init failed")
		return
	}

	// find status and session ID from headers
	var status, backendSessionID string
	for _, h := range headers {
		switch h[0] {
		case ":status":
			status = h[1]
		case mcpSessionHeader:
			backendSessionID = h[1]
		}
	}

	if status != "200" {
		proxywasm.LogErrorf("mcp-routing: init call failed with status: %s", status)
		ctx.sendJSONRPCError(ctx.pendingRequest.ID, -32603, "Backend init failed")
		return
	}

	if backendSessionID == "" {
		proxywasm.LogErrorf("mcp-routing: no session ID in init response")
		ctx.sendJSONRPCError(ctx.pendingRequest.ID, -32603, "No session from backend")
		return
	}

	proxywasm.LogInfof("mcp-routing: lazy init complete for %s, session: %s",
		ctx.pendingServerID, backendSessionID[:minInt(20, len(backendSessionID))])

	// get expiration from gateway session JWT
	expiresAt := getJWTExpiry(ctx.mcpSessionID)
	if expiresAt == 0 {
		// no exp claim, default to 1 hour from now
		expiresAt = time.Now().UTC().Unix() + 3600
	}

	// store the backend session with expiration
	if err := setBackendSession(ctx.mcpSessionID, ctx.pendingServerID, backendSessionID, expiresAt); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to store backend session: %v", err)
		// continue anyway, next request will re-init
	}

	ctx.waitingForInit = false

	// resume the original request
	ctx.resumePendingToolCall(backendSessionID)
}

// resumePendingToolCall continues processing the original tool call after init
func (ctx *httpContext) resumePendingToolCall(backendSessionID string) {
	proxywasm.LogInfof("mcp-routing: resumePendingToolCall called, serverID=%s, tool=%s, backendSessionID=%s",
		ctx.pendingServerID, ctx.pendingUpstreamTool, backendSessionID)

	// route the pending request to the backend
	action := ctx.routeToolCallToBackend(
		ctx.pendingRequest,
		ctx.pendingBody,
		ctx.pendingServerCfg,
		ctx.pendingServerID,
		ctx.pendingUpstreamTool,
		backendSessionID,
	)

	proxywasm.LogDebugf("mcp-routing: routeToolCallToBackend returned action=%v", action)

	if action == types.ActionContinue {
		proxywasm.LogInfof("mcp-routing: resuming paused HTTP request")
		// resume the paused request
		if err := proxywasm.ResumeHttpRequest(); err != nil {
			proxywasm.LogErrorf("mcp-routing: failed to resume request: %v", err)
		}
	} else {
		proxywasm.LogWarnf("mcp-routing: not resuming request, action=%v", action)
	}
}

// routeToolCallToBackend sets up routing headers and body for a tool call
func (ctx *httpContext) routeToolCallToBackend(req *mcpRequest, body []byte, serverCfg *serverConfig, serverID, upstreamToolName, backendSessionID string) types.Action {
	proxywasm.LogInfof("mcp-routing: routeToolCallToBackend called, serverID=%s, host=%s, tool=%s , backend session id %s", serverID, serverCfg.Hostname, upstreamToolName, backendSessionID)

	// set routing headers
	if err := proxywasm.ReplaceHttpRequestHeader(authorityHeader, serverCfg.Hostname); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set authority: %v", err)
	}
	if err := proxywasm.ReplaceHttpRequestHeader(pathHeader, serverCfg.Path); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set path: %v", err)
	}

	// clear route cache so Envoy re-evaluates route with new headers
	// uses foreign function available in Envoy 1.33+
	if _, err := proxywasm.CallForeignFunction("clear_route_cache", nil); err != nil {
		proxywasm.LogDebugf("mcp-routing: failed to clear route cache: %v", err)
	}

	// set backend session ID
	if err := proxywasm.ReplaceHttpRequestHeader(mcpSessionHeader, backendSessionID); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set backend session: %v", err)
	}

	// set MCP-specific headers
	if err := proxywasm.AddHttpRequestHeader(mcpMethodHeader, req.Method); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set method header: %v", err)
	}
	if err := proxywasm.AddHttpRequestHeader(mcpServerNameHeader, serverID); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set server name header: %v", err)
	}
	if err := proxywasm.AddHttpRequestHeader(mcpToolNameHeader, upstreamToolName); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set tool name header: %v", err)
	}

	// set credentials if configured
	if serverCfg.Credentials != "" {
		if err := proxywasm.AddHttpRequestHeader(mcpAPIKeyHeader, serverCfg.Credentials); err != nil {
			proxywasm.LogErrorf("mcp-routing: failed to set api key header: %v", err)
		}
	}

	// rewrite body if prefix stripping is needed
	if upstreamToolName != req.ToolName {
		proxywasm.LogInfof("mcp-routing: stripping prefix, original=%s, upstream=%s", req.ToolName, upstreamToolName)
		newBody := rewriteToolName(body, upstreamToolName)
		proxywasm.LogDebugf("mcp-routing: rewritten body: %s", string(newBody))
		if err := proxywasm.ReplaceHttpRequestBody(newBody); err != nil {
			proxywasm.LogErrorf("mcp-routing: failed to replace body: %v", err)
		}
		// remove content-length so envoy recalculates
		if err := proxywasm.RemoveHttpRequestHeader(contentLengthHeader); err != nil {
			proxywasm.LogErrorf("mcp-routing: failed to remove content-length: %v", err)
		}
	} else {
		proxywasm.LogDebugf("mcp-routing: no prefix stripping needed, tool=%s", req.ToolName)
	}

	return types.ActionContinue
}

// handleBrokerRequest routes non-tool-call requests to the broker
// this includes initialize, notifications, and other MCP methods
// also handles hairpin requests from broker to backend MCP servers
func (ctx *httpContext) handleBrokerRequest(req *mcpRequest) types.Action {
	proxywasm.LogInfof("mcp-routing: handleBrokerRequest called, method=%s, isInit=%v", req.Method, ctx.isInitializeRequest)

	// check for hairpin request (broker initializing connection to backend MCP server)
	if ctx.isInitializeRequest {
		proxywasm.LogInfof("mcp-routing: initialize looking for init host %s", req.Method)
		initHost, err := proxywasm.GetHttpRequestHeader(mcpInitHostHeader)
		if err == nil && initHost != "" {
			return ctx.handleHairpinRequest(req, initHost)
		}
	}

	proxywasm.LogInfof("mcp-routing: routing %s to broker", req.Method)

	ctx.targetServerID = "mcpBroker"

	if err := proxywasm.ReplaceHttpRequestHeader(authorityHeader, ctx.config.BrokerHostname); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set broker authority: %v", err)
	}
	if err := proxywasm.ReplaceHttpRequestHeader(pathHeader, ctx.config.BrokerPath); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set broker path: %v", err)
	}

	// clear route cache so Envoy re-evaluates route with new headers
	if _, err := proxywasm.CallForeignFunction("clear_route_cache", nil); err != nil {
		proxywasm.LogDebugf("mcp-routing: failed to clear route cache: %v", err)
	}

	if err := proxywasm.AddHttpRequestHeader(mcpMethodHeader, req.Method); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set method header: %v", err)
	}
	if err := proxywasm.AddHttpRequestHeader(mcpServerNameHeader, "mcpBroker"); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set server name header: %v", err)
	}

	return types.ActionContinue
}

// handleHairpinRequest routes broker's initialize request to a backend MCP server
func (ctx *httpContext) handleHairpinRequest(req *mcpRequest, targetHost string) types.Action {
	proxywasm.LogInfof("mcp-routing: handleHairpinRequest called, method=%s, targetHost=%s", req.Method, targetHost)

	// validate router key
	routerKey, err := proxywasm.GetHttpRequestHeader(routerKeyHeader)
	if err != nil || routerKey != ctx.config.RouterAPIKey {
		proxywasm.LogWarn("mcp-routing: hairpin request rejected - invalid router key")
		ctx.sendErrorResponse(400, "bad request")
		return types.ActionPause
	}

	proxywasm.LogInfof("mcp-routing: hairpin %s to %s", req.Method, targetHost)

	ctx.targetServerID = targetHost

	// route to the target MCP server
	if err := proxywasm.ReplaceHttpRequestHeader(authorityHeader, targetHost); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set hairpin authority: %v", err)
	}

	// clear route cache so Envoy re-evaluates route with new headers
	if _, err := proxywasm.CallForeignFunction("clear_route_cache", nil); err != nil {
		proxywasm.LogDebugf("mcp-routing: failed to clear route cache: %v", err)
	}

	if err := proxywasm.AddHttpRequestHeader(mcpMethodHeader, req.Method); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to set method header: %v", err)
	}

	// remove hairpin-specific headers before forwarding to backend
	if err := proxywasm.RemoveHttpRequestHeader(mcpInitHostHeader); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to remove init host header: %v", err)
	}
	if err := proxywasm.RemoveHttpRequestHeader(routerKeyHeader); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to remove router key header: %v", err)
	}

	return types.ActionContinue
}

// resolveToolToServer finds the server for a given tool name
// returns serverID, server config, and the upstream tool name (with prefix stripped)
func (ctx *httpContext) resolveToolToServer(toolName string) (string, *serverConfig, string) {
	proxywasm.LogDebugf("mcp-routing: resolveToolToServer called, tool=%s, numServers=%d, numMappings=%d",
		toolName, len(ctx.config.Servers), len(ctx.toolMappings))

	// first check JWT-based tool mappings
	if serverID, ok := ctx.toolMappings[toolName]; ok {
		if cfg, ok := ctx.config.Servers[serverID]; ok {
			upstreamName := toolName
			if cfg.ToolPrefix != "" {
				upstreamName = strings.TrimPrefix(toolName, cfg.ToolPrefix)
			}
			return serverID, cfg, upstreamName
		}
	}

	// fallback: check if tool name matches any server prefix
	for serverID, cfg := range ctx.config.Servers {
		if cfg.ToolPrefix != "" && strings.HasPrefix(toolName, cfg.ToolPrefix) {
			upstreamName := strings.TrimPrefix(toolName, cfg.ToolPrefix)
			return serverID, cfg, upstreamName
		}
	}

	return "", nil, ""
}

// parseToolMappingsFromJWT extracts tool->server mappings from the session JWT
// JWT format: {"tools": {"weather_get_forecast": "weather-server", ...}}
func parseToolMappingsFromJWT(sessionID string) map[string]string {
	mappings := make(map[string]string)

	// JWT has 3 parts separated by dots: header.payload.signature
	parts := strings.Split(sessionID, ".")
	if len(parts) != 3 {
		return mappings
	}

	// decode base64url payload (middle part)
	payload := decodeBase64URL(parts[1])
	if payload == "" {
		return mappings
	}

	// parse tools claim
	tools := gjson.Get(payload, "tools")
	if !tools.Exists() || !tools.IsObject() {
		return mappings
	}

	tools.ForEach(func(key, value gjson.Result) bool {
		mappings[key.String()] = value.String()
		return true
	})

	proxywasm.LogDebugf("mcp-routing: parsed %d tool mappings from JWT", len(mappings))
	return mappings
}

// decodeBase64URL decodes a base64url string (no padding)
func decodeBase64URL(s string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		proxywasm.LogDebugf("mcp-routing: failed to decode base64url: %v", err)
		return ""
	}
	return string(decoded)
}

// rewriteToolName replaces the tool name in the JSON body
func rewriteToolName(body []byte, newToolName string) []byte {
	json := string(body)

	// find params.name and replace it
	nameResult := gjson.Get(json, "params.name")
	if !nameResult.Exists() {
		return body
	}

	// simple string replacement at the exact location
	start := nameResult.Index
	end := start + len(nameResult.Raw)

	// construct new JSON with replaced tool name
	newJSON := json[:start] + `"` + newToolName + `"` + json[end:]

	return []byte(newJSON)
}

// sendErrorResponse sends an HTTP error response
func (ctx *httpContext) sendErrorResponse(status uint32, message string) {
	body := `{"error": "` + message + `"}`
	if err := proxywasm.SendHttpResponse(status, [][2]string{
		{"content-type", "application/json"},
	}, []byte(body), -1); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to send error response: %v", err)
	}
}

// sendJSONRPCError sends a JSON-RPC error response
func (ctx *httpContext) sendJSONRPCError(id int64, code int, message string) {
	// format as SSE event like the broker does
	body := "event: message\n"
	body += `data: {"jsonrpc":"2.0","id":` + strconv.FormatInt(id, 10) + `,"error":{"code":` + strconv.Itoa(code) + `,"message":"` + message + `"}}` + "\n\n"

	headers := [][2]string{
		{"content-type", "text/event-stream"},
		{mcpSessionHeader, ctx.mcpSessionID},
	}

	if err := proxywasm.SendHttpResponse(200, headers, []byte(body), -1); err != nil {
		proxywasm.LogErrorf("mcp-routing: failed to send JSON-RPC error: %v", err)
	}
}

/*
parsed MCP request - method=initialize, id=1, tool=	thread=39
mcp-gateway-istio-6c75b8d46c-ng2kj istio-proxy 2026-02-13T17:39:38.074435Z	info	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1149	wasm log mcp-routing mcp_routing: mcp-routing: handleBrokerRequest called, method=initialize, isInit=true	thread=39
mcp-gateway-istio-6c75b8d46c-ng2kj istio-proxy 2026-02-13T17:39:38.074459Z	info	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1149	wasm log mcp-routing mcp_routing: mcp-routing: handleHairpinRequest called, method=initialize, targetHost=server1.mcp.local	thread=39
mcp-gateway-istio-6c75b8d46c-ng2kj istio-proxy 2026-02-13T17:39:38.074518Z	info	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1149	wasm log mcp-routing mcp_routing: mcp-routing: hairpin initialize to server1.mcp.local	thread=39
mcp-gateway-istio-6c75b8d46c-ng2kj istio-proxy 2026-02-13T17:39:38.075207Z	info	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1149	wasm log mcp-routing mcp_routing: mcp-routing: OnHttpResponseHeaders called, numHeaders=6, endOfStream=false, target=server1.mcp.local	thread=39
mcp-gateway-istio-6c75b8d46c-ng2kj istio-proxy 2026-02-13T17:39:38.075258Z	debug	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1146	wasm log mcp-routing mcp_routing: mcp-routing: response status: 400 for method: initialize	thread=39
mcp-gateway-istio-6c75b8d46c-ng2kj istio-proxy 2026-02-13T17:39:38.075265Z	info	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1149	wasm log mcp-routing mcp_routing: mcp-routing: initialize response received, status: 400	thread=39
*/

// OnHttpResponseHeaders handles response headers from upstream
func (ctx *httpContext) OnHttpResponseHeaders(numHeaders int, endOfStream bool) types.Action {
	proxywasm.LogInfof("mcp-routing: OnHttpResponseHeaders called, numHeaders=%d, endOfStream=%v, target=%s",
		numHeaders, endOfStream, ctx.targetServerID)

	// capture upstream session ID for session management
	upstreamSession, err := proxywasm.GetHttpResponseHeader(mcpSessionHeader)
	if err == nil && upstreamSession != "" {
		ctx.upstreamSessionID = upstreamSession
		proxywasm.LogDebugf("mcp-routing: captured upstream session: %s for server: %s",
			upstreamSession, ctx.targetServerID)
	}

	// get response status
	status, err := proxywasm.GetHttpResponseHeader(":status")
	if err == nil {
		proxywasm.LogDebugf("mcp-routing: response status: %s for method: %s", status, ctx.mcpMethod)
	}

	// handle 404 from backend - session may have expired
	// delete cached session so next request will re-init
	if status == "404" && ctx.mcpMethod == methodToolsCall && ctx.targetServerID != "" && ctx.targetServerID != "mcpBroker" {
		proxywasm.LogInfof("mcp-routing: 404 from %s, clearing cached session", ctx.targetServerID)
		deleteBackendSession(ctx.mcpSessionID, ctx.targetServerID)
	}

	// for initialize responses, we might want to capture and transform the response
	if ctx.isInitializeRequest {
		// the broker handles initialize responses, but we log for debugging
		proxywasm.LogInfof("mcp-routing: initialize response received, status: %s", status)
	}

	return types.ActionContinue
}

// OnHttpResponseBody handles response body from upstream
func (ctx *httpContext) OnHttpResponseBody(bodySize int, endOfStream bool) types.Action {
	proxywasm.LogDebugf("mcp-routing: OnHttpResponseBody called, bodySize=%d, endOfStream=%v", bodySize, endOfStream)

	if !endOfStream {
		// continue buffering if needed for response transformation
		return types.ActionContinue
	}

	// log response body for debugging
	if bodySize > 0 {
		body, err := proxywasm.GetHttpResponseBody(0, bodySize)
		if err != nil {
			proxywasm.LogErrorf("mcp-routing: failed to get response body: %v", err)
		} else {
			// truncate if too long
			bodyStr := string(body)
			if len(bodyStr) > 1000 {
				bodyStr = bodyStr[:1000] + "...(truncated)"
			}
			proxywasm.LogDebugf("mcp-routing: response body for method=%s, target=%s: %s", ctx.mcpMethod, ctx.targetServerID, bodyStr)
		}
	}

	return types.ActionContinue
}

// OnHttpStreamDone is called when the HTTP stream is complete
func (ctx *httpContext) OnHttpStreamDone() {
	proxywasm.LogInfof("mcp-routing: OnHttpStreamDone called, method=%s, server=%s, contextID=%d",
		ctx.mcpMethod, ctx.targetServerID, ctx.contextID)
}
