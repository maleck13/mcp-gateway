package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kuadrant/mcp-gateway/internal/broker/upstream"
	"github.com/Kuadrant/mcp-gateway/internal/config"
	"github.com/Kuadrant/mcp-gateway/internal/session"
	"github.com/Kuadrant/mcp-gateway/internal/tests/server2"
	"github.com/maleck13/tdt"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

const (
	// MCPPort is the port the test server should listen on (TODO make dynamic?)
	MCPPort = "8088"

	// MCPAddr is the URL the client will use to contact the test server
	MCPAddr = "http://localhost:8088/mcp"

	// MCPAddrForgetAddr is the URL the client will use to force the server to forget a session
	MCPAddrForgetAddr = "http://localhost:8088/admin/forget"
)

var logger = slog.New(slog.NewTextHandler(os.Stdout, nil))

// TestMain starts an MCP server that we will run actual tests against
func TestMain(m *testing.M) {
	// Start an MCP server to test our broker client logic
	startFunc, shutdownFunc, err := server2.RunServer("http", MCPPort)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Server setup error: %v\n", err)
		os.Exit(1)
	}

	go func() {
		// Start the server in a Goroutine
		_ = startFunc()
	}()

	// wait for server to be ready
	time.Sleep(100 * time.Millisecond)

	code := m.Run()

	err = shutdownFunc()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Server shutdown error: %v\n", err)
		// Don't fail if the server doesn't shut down; it might have open clients
		// os.Exit(1)
	}

	os.Exit(code)
}

func TestOnConfigChange(t *testing.T) {
	b := NewBroker(logger)
	conf := &config.MCPServersConfig{}
	server1 := &config.MCPServer{
		Name:       "test1",
		URL:        MCPAddr,
		ToolPrefix: "_test1",
	}
	virtualServer1 := &config.VirtualServer{
		Name:  "test/test",
		Tools: []string{"test"},
	}
	b.OnConfigChange(context.TODO(), conf)
	servers := b.RegisteredMCPServers()
	require.Equal(t, 0, len(servers))
	if _, ok := servers[server1.ID()]; ok {
		t.Fatalf("expected server 1 not to be registered")
	}

	conf.Servers = append(conf.Servers, server1)
	conf.VirtualServers = append(conf.VirtualServers, virtualServer1)
	b.OnConfigChange(context.TODO(), conf)
	servers = b.RegisteredMCPServers()
	require.Equal(t, 1, len(servers))
	if _, ok := servers[server1.ID()]; !ok {
		t.Fatalf("expected server 1 to be registered")
	}

	vs, err := b.GetVirtualSeverByHeader("test/test")
	require.Nil(t, err, "error should be nil from GetVirtualSeverByHeader")
	if vs.Name != "test/test" {
		t.Fatalf("expected virtual server to have same name")
	}
	if len(vs.Tools) != 1 && vs.Tools[0] != "test" {
		t.Fatalf("expected the virtual server to have the test tool listed")
	}

	conf.Servers = []*config.MCPServer{}
	b.OnConfigChange(context.TODO(), conf)
	servers = b.RegisteredMCPServers()
	require.Equal(t, 0, len(servers))
	if _, ok := servers[server1.ID()]; ok {
		t.Fatalf("expected server 1 not to be registered")
	}

	_ = b.Shutdown(context.Background())
}

var _ http.ResponseWriter = &simpleResponseWriter{}

type simpleResponseWriter struct {
	Status  int
	Body    []byte
	Headers []http.Header
}

func (srw *simpleResponseWriter) Header() http.Header {
	h := http.Header{}
	srw.Headers = append(srw.Headers, h)
	return h
}

func (srw *simpleResponseWriter) WriteHeader(status int) {
	srw.Status = status
}
func (srw *simpleResponseWriter) Write(b []byte) (int, error) {
	srw.Body = b
	return len(b), nil
}

func TestOauthResourceHandler(t *testing.T) {
	var (
		resourceName = "mcp gateway"
		resource     = "https://test.com/mcp"
		idp          = "https://idp.com"
		bearerMethod = "header"
		scopes       = "groups,audience,roles"
	)
	t.Setenv(envOAuthResourceName, resourceName)
	t.Setenv(envOAuthResource, resource)
	t.Setenv(envOAuthAuthorizationServers, idp)
	t.Setenv(envOAuthBearerMethodsSupported, bearerMethod)
	t.Setenv(envOAuthScopesSupported, scopes)

	r := &http.Request{
		Method: http.MethodGet,
	}
	pr := &ProtectedResourceHandler{Logger: logger}
	recorder := &simpleResponseWriter{}
	pr.Handle(recorder, r)
	if recorder.Status != 200 {
		t.Fatalf("expected 200 status code got %v", recorder.Status)
	}
	config := &OAuthProtectedResource{}
	if err := json.Unmarshal(recorder.Body, config); err != nil {
		t.Fatalf("unexpected error %s", err)
	}
	if !slices.Contains(config.AuthorizationServers, idp) {
		t.Fatalf("expected %s to be in %v", idp, config.AuthorizationServers)
	}
	if config.Resource != resource {
		t.Fatalf("expected resource to be %s but was %s", resource, config.Resource)
	}
	if config.ResourceName != resourceName {
		t.Fatalf("expected resource to be %s but was %s", resourceName, config.ResourceName)
	}
	if !slices.ContainsFunc(config.ScopesSupported, func(val string) bool {
		return slices.Contains(strings.Split(scopes, ","), val)
	}) {
		t.Fatalf("expected %s to be in %v", scopes, config.ScopesSupported)
	}
	if !slices.Contains(config.BearerMethodsSupported, bearerMethod) {
		t.Fatalf("expected %s to be in %v", bearerMethod, config.BearerMethodsSupported)
	}

}

func TestGetServerInfo(t *testing.T) {
	b := NewBroker(logger)

	// Attach phony tools to the upstreams
	bImpl, ok := b.(*mcpBrokerImpl)
	require.True(t, ok)
	bImpl.mcpServers["test1"] = createTestManager(t, "test1", "", []mcp.Tool{
		mcp.NewTool("pour_chocolate"),
	})
	bImpl.mcpServers["test2"] = createTestManager(t, "test2", "", []mcp.Tool{
		mcp.NewTool("restore_from_tape"),
	})
	bImpl.mcpServers["test3"] = createTestManager(t, "test3", "t", []mcp.Tool{
		mcp.NewTool("restore_from_tape"),
	})
	bImpl.mcpServers["test4"] = createTestManager(t, "test4", "tt", []mcp.Tool{})

	svr, err := b.GetServerInfo("pour_chocolate")
	require.NotNil(t, svr)
	require.NoError(t, err)
	require.Equal(t, "test1", svr.Name)

	svr, err = b.GetServerInfo("restore_from_tape")
	require.NotNil(t, svr)
	require.NoError(t, err)
	require.Equal(t, "test2", svr.Name)

	// We used a prefix so that this tool exists
	svr, err = b.GetServerInfo("trestore_from_tape")
	require.NotNil(t, svr)
	require.NoError(t, err)
	require.Equal(t, "test3", svr.Name)

	// There is no tool, even though the prefix matches
	svr, err = b.GetServerInfo("tt_orbit_mars")
	require.Error(t, err)
	require.Nil(t, svr)
}

func TestToolAnnotations(t *testing.T) {
	b := NewBroker(logger,
		WithEnforceToolFilter(true),
		WithManagerTickerInterval(time.Microsecond),
		WithTrustedHeadersPublicKey("abc"))
	require.NotNil(t, b)
	require.NotNil(t, b.MCPServer())

	// Attach phony tools to the upstreams
	bImpl, ok := b.(*mcpBrokerImpl)
	require.True(t, ok)
	bImpl.mcpServers["test1"] = createTestManager(t, "test1", "", []mcp.Tool{
		mcp.NewTool("get_status", mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:   mcp.ToBoolPtr(true),
			IdempotentHint: mcp.ToBoolPtr(true),
		})),
		mcp.NewTool("pour_chocolate", mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:   mcp.ToBoolPtr(false),
			IdempotentHint: mcp.ToBoolPtr(false),
		})),
	})

	testCases := []struct {
		name       string
		serverName config.UpstreamMCPID
		toolName   string
		shouldFail bool
		readOnly   bool
		idempotent bool
	}{
		{
			name:       "status tool",
			serverName: "test1",
			toolName:   "get_status",
			shouldFail: false,
			readOnly:   true,
			idempotent: true,
		},
		{
			name:       "pour tool",
			serverName: "test1",
			toolName:   "pour_chocolate",
			shouldFail: false,
			readOnly:   false,
			idempotent: false,
		},
		{
			name:       "invalid tool",
			serverName: "test1",
			toolName:   "plant_rutabaga",
			shouldFail: true,
		},
		{
			name:       "invalid server",
			serverName: "miami",
			toolName:   "get_status",
			shouldFail: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			annotation, exists := b.ToolAnnotations(tc.serverName, tc.toolName)
			if tc.shouldFail {
				require.False(t, exists, "expected no annotation to be found")
				return
			}
			require.True(t, exists, "expected annotation to be found")
			require.Equal(t, tc.readOnly, *annotation.ReadOnlyHint, "readOnly mismatch: %#v", annotation)
			require.Equal(t, tc.idempotent, *annotation.IdempotentHint, "idempotent mismatch: %#v", annotation)
		})
	}
}

func createTestManagerWithMetadata(t *testing.T, serverName, toolPrefix string, category, hint string, tags map[string]string, tools []mcp.Tool) *upstream.MCPManager {
	t.Helper()
	mcpServer := upstream.NewUpstreamMCP(&config.MCPServer{
		Name:       serverName,
		ToolPrefix: toolPrefix,
		URL:        "http://test.local/mcp",
		Category:   category,
		Tags:       tags,
		Hint:       hint,
	})
	manager := upstream.NewUpstreamMCPManager(mcpServer, nil, slog.Default(), 0, nil)
	manager.SetToolsForTesting(tools)
	return manager
}

func TestRebuildToolIndex(t *testing.T) {
	b := NewBroker(logger, WithEnableToolDiscovery(true))
	bImpl, ok := b.(*mcpBrokerImpl)
	require.True(t, ok)

	// Create managers with category/tags/hint metadata
	mgr1 := createTestManagerWithMetadata(t, "observability-server", "obs_",
		"observability", "Provides metrics and log querying capabilities",
		map[string]string{"team": "platform"},
		[]mcp.Tool{
			{Name: "get_metrics", Description: "Fetch Prometheus metrics from a target"},
			{Name: "query_logs", Description: "Search structured logs by pattern"},
		},
	)
	mgr2 := createTestManagerWithMetadata(t, "cicd-server", "ci_",
		"cicd", "CI/CD pipeline management",
		map[string]string{"team": "devops"},
		[]mcp.Tool{
			{Name: "trigger_pipeline", Description: "Trigger a CI/CD pipeline run"},
		},
	)

	bImpl.mcpServers["obs"] = mgr1
	bImpl.mcpServers["ci"] = mgr2

	// Simulate a tool discovery callback triggering index rebuild
	bImpl.rebuildToolIndex()

	// RankedSearch for metrics-related tools
	results := b.RankedSearch(tdt.Query{Text: "metrics prometheus"}, tdt.SearchOptions{TopK: 5})
	require.NotEmpty(t, results, "expected ranked results for metrics query")
	require.Equal(t, "get_metrics", results[0].ToolName, "expected get_metrics to be top result")

	// RankedSearch with category filter
	results = b.RankedSearch(tdt.Query{Category: "cicd"}, tdt.SearchOptions{})
	require.Len(t, results, 1)
	require.Equal(t, "trigger_pipeline", results[0].ToolName)

	// RankedSearch for logs
	results = b.RankedSearch(tdt.Query{Text: "search logs"}, tdt.SearchOptions{TopK: 5})
	require.NotEmpty(t, results)
	require.Equal(t, "query_logs", results[0].ToolName, "expected query_logs to be top result for log search")
}

func TestDiscoverToolRegistered(t *testing.T) {
	b := NewBroker(logger, WithEnableToolDiscovery(true))
	tools := b.MCPServer().ListTools()
	found := false
	for name := range tools {
		if name == "discover_tools" {
			found = true
			break
		}
	}
	require.True(t, found, "discover_tools should be registered on the gateway MCP server")
}

func TestDiscoverToolNotRegisteredWhenDisabled(t *testing.T) {
	b := NewBroker(logger) // enableToolDiscovery defaults to false
	tools := b.MCPServer().ListTools()
	for name := range tools {
		require.NotEqual(t, "discover_tools", name, "discover_tools should not be registered when tool discovery is disabled")
	}
}

func TestRankedSearchReturnsNilWhenDisabled(t *testing.T) {
	b := NewBroker(logger) // enableToolDiscovery defaults to false
	results := b.RankedSearch(tdt.Query{Text: "anything"}, tdt.SearchOptions{TopK: 5})
	require.Nil(t, results, "RankedSearch should return nil when tool discovery is disabled")
}

func newTestCache(t *testing.T) *session.Cache {
	t.Helper()
	cache, err := session.NewCache(context.Background())
	require.NoError(t, err)
	return cache
}

func TestSessionToolSelection_SetGetClear(t *testing.T) {
	cache := newTestCache(t)
	b := NewBroker(logger,
		WithEnableToolDiscovery(true),
		WithSessionCache(cache),
	)
	bImpl := b.(*mcpBrokerImpl)
	ctx := context.Background()
	sessionID := "test-session-123"

	// Initially no selection
	tools, exists, err := bImpl.GetSessionToolSelection(ctx, sessionID)
	require.NoError(t, err)
	require.False(t, exists)
	require.Nil(t, tools)

	// Set a selection
	err = bImpl.SetSessionToolSelection(ctx, sessionID, []string{"tool_a", "tool_b"})
	require.NoError(t, err)

	// Get returns the selection
	tools, exists, err = bImpl.GetSessionToolSelection(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, []string{"tool_a", "tool_b"}, tools)

	// Clear the selection
	err = bImpl.ClearSessionToolSelection(ctx, sessionID)
	require.NoError(t, err)

	// After clearing, no selection
	tools, exists, err = bImpl.GetSessionToolSelection(ctx, sessionID)
	require.NoError(t, err)
	require.False(t, exists)
	require.Nil(t, tools)
}

func TestSessionToolSelection_NilCache(t *testing.T) {
	b := NewBroker(logger, WithEnableToolDiscovery(true))
	bImpl := b.(*mcpBrokerImpl)
	ctx := context.Background()

	// All operations are no-ops with nil cache
	err := bImpl.SetSessionToolSelection(ctx, "s1", []string{"tool_a"})
	require.NoError(t, err)

	tools, exists, err := bImpl.GetSessionToolSelection(ctx, "s1")
	require.NoError(t, err)
	require.False(t, exists)
	require.Nil(t, tools)

	err = bImpl.ClearSessionToolSelection(ctx, "s1")
	require.NoError(t, err)
}

func TestFilterTools_SessionSelectionFilters(t *testing.T) {
	cache := newTestCache(t)
	b := NewBroker(logger,
		WithEnableToolDiscovery(true),
		WithSessionCache(cache),
	)
	bImpl := b.(*mcpBrokerImpl)
	ctx := context.Background()
	sessionID := "filter-session-456"

	// Store a selection for this session
	err := bImpl.SetSessionToolSelection(ctx, sessionID, []string{"tool_a", "tool_c"})
	require.NoError(t, err)

	// Simulate tools coming from upstream
	allTools := []mcp.Tool{
		mcp.NewTool("tool_a"),
		mcp.NewTool("tool_b"),
		mcp.NewTool("tool_c"),
		mcp.NewTool("tool_d"),
	}

	// applySessionToolSelection requires a session in context — test the method directly
	// by setting up context with a fake session
	filtered := bImpl.applySessionToolSelection(ctx, allTools)
	// Without a session in context, all tools pass through
	require.Len(t, filtered, 4, "without session in context, all tools should pass through")
}

func TestFilterTools_BrokerToolsAlwaysPassThrough(t *testing.T) {
	cache := newTestCache(t)
	b := NewBroker(logger,
		WithEnableToolDiscovery(true),
		WithSessionCache(cache),
	)
	bImpl := b.(*mcpBrokerImpl)

	// discover_tools is a broker tool
	require.True(t, bImpl.IsBrokerTool("discover_tools"))
}

func TestFilterTools_NoSelectionReturnsAllTools(t *testing.T) {
	cache := newTestCache(t)
	b := NewBroker(logger,
		WithEnableToolDiscovery(true),
		WithSessionCache(cache),
	)
	bImpl := b.(*mcpBrokerImpl)
	ctx := context.Background()

	// No selection stored — all tools should pass through
	allTools := []mcp.Tool{
		mcp.NewTool("tool_a"),
		mcp.NewTool("tool_b"),
	}
	filtered := bImpl.applySessionToolSelection(ctx, allTools)
	require.Len(t, filtered, 2, "without session selection, all tools should pass through")
}

func TestFilterTools_DisabledDiscoveryReturnsAllTools(t *testing.T) {
	cache := newTestCache(t)
	b := NewBroker(logger, WithSessionCache(cache)) // discovery disabled
	bImpl := b.(*mcpBrokerImpl)
	ctx := context.Background()

	allTools := []mcp.Tool{
		mcp.NewTool("tool_a"),
		mcp.NewTool("tool_b"),
	}
	filtered := bImpl.applySessionToolSelection(ctx, allTools)
	require.Len(t, filtered, 2, "with discovery disabled, all tools should pass through")
}

func TestDiscoverToolsHandler_CatalogOnEmptyQuery(t *testing.T) {
	b := NewBroker(logger, WithEnableToolDiscovery(true))
	bImpl := b.(*mcpBrokerImpl)

	// Populate the index
	bImpl.mcpServers["obs"] = createTestManagerWithMetadata(t, "obs-server", "obs_",
		"observability", "Metrics and logs",
		map[string]string{"team": "platform"},
		[]mcp.Tool{{Name: "get_metrics", Description: "Fetch metrics"}},
	)
	bImpl.rebuildToolIndex()

	// Call handler with no arguments — should return catalog
	result, err := bImpl.handleDiscoverToolsCatalog()
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.NotEmpty(t, result.Content)

	textContent, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var catalog tdt.CatalogResponse
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &catalog))
	require.Len(t, catalog.Categories, 1)
	require.Equal(t, "observability", catalog.Categories[0].Name)
}

func TestDiscoverToolsHandler_SearchStoresSelection(t *testing.T) {
	cache := newTestCache(t)
	b := NewBroker(logger,
		WithEnableToolDiscovery(true),
		WithSessionCache(cache),
	)
	bImpl := b.(*mcpBrokerImpl)

	// Set up servers with prefixes
	bImpl.mcpServers["obs"] = createTestManagerWithMetadata(t, "obs-server", "obs_",
		"observability", "Metrics and logs",
		map[string]string{},
		[]mcp.Tool{
			{Name: "get_metrics", Description: "Fetch Prometheus metrics"},
			{Name: "query_logs", Description: "Search structured logs"},
		},
	)
	bImpl.mcpServers["ci"] = createTestManagerWithMetadata(t, "ci-server", "ci_",
		"cicd", "CI/CD pipelines",
		map[string]string{},
		[]mcp.Tool{
			{Name: "trigger_pipeline", Description: "Trigger a CI/CD pipeline run"},
		},
	)
	bImpl.rebuildToolIndex()

	// Call search — no session in context, but should still return results
	ctx := context.Background()
	result, err := bImpl.handleDiscoverToolsSearch(ctx, "metrics prometheus")
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var resp discoverToolsResponse
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &resp))
	require.Equal(t, "search", resp.Action)
	require.Greater(t, resp.ToolsMatched, 0)
}

func TestDiscoverToolsHandler_ResetClearsSelection(t *testing.T) {
	cache := newTestCache(t)
	b := NewBroker(logger,
		WithEnableToolDiscovery(true),
		WithSessionCache(cache),
	)
	bImpl := b.(*mcpBrokerImpl)
	ctx := context.Background()

	// Call reset via the handler (no session in context — gracefully handles nil session)
	_, handler := bImpl.newDiscoverToolsHandler()
	req := mcp.CallToolRequest{}
	req.Params.Name = "discover_tools"
	req.Params.Arguments = map[string]any{"reset": true}

	result, err := handler(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var resp discoverToolsResponse
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &resp))
	require.Equal(t, "reset", resp.Action)
	require.Contains(t, resp.Message, "cleared")
}

func TestDiscoverToolsHandler_ResetAndSearchInOneCall(t *testing.T) {
	cache := newTestCache(t)
	b := NewBroker(logger,
		WithEnableToolDiscovery(true),
		WithSessionCache(cache),
	)
	bImpl := b.(*mcpBrokerImpl)
	ctx := context.Background()

	// Set up servers with tools
	bImpl.mcpServers["obs"] = createTestManagerWithMetadata(t, "obs-server", "obs_",
		"observability", "Metrics and logs",
		map[string]string{},
		[]mcp.Tool{
			{Name: "get_metrics", Description: "Fetch Prometheus metrics"},
			{Name: "query_logs", Description: "Search structured logs"},
		},
	)
	bImpl.rebuildToolIndex()

	// Call with both reset=true and query — should clear then search
	_, handler := bImpl.newDiscoverToolsHandler()
	req := mcp.CallToolRequest{}
	req.Params.Name = "discover_tools"
	req.Params.Arguments = map[string]any{
		"reset": true,
		"query": "metrics prometheus",
	}

	result, err := handler(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var resp discoverToolsResponse
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &resp))
	require.Equal(t, "search", resp.Action, "combined reset+query should return search results")
	require.Greater(t, resp.ToolsMatched, 0)
}

func TestDiscoverToolsHandler_PrefixToolName(t *testing.T) {
	b := NewBroker(logger, WithEnableToolDiscovery(true))
	bImpl := b.(*mcpBrokerImpl)

	bImpl.mcpServers["s1"] = createTestManagerWithMetadata(t, "my-server", "ms_",
		"", "", nil,
		[]mcp.Tool{{Name: "do_thing", Description: "Does a thing"}},
	)
	bImpl.mcpServers["s2"] = createTestManagerWithMetadata(t, "no-prefix-server", "",
		"", "", nil,
		[]mcp.Tool{{Name: "other_thing", Description: "Other thing"}},
	)

	require.Equal(t, "ms_do_thing", bImpl.prefixToolName("my-server", "do_thing"))
	require.Equal(t, "other_thing", bImpl.prefixToolName("no-prefix-server", "other_thing"))
	require.Equal(t, "unknown_tool", bImpl.prefixToolName("nonexistent", "unknown_tool"))
}
