package broker

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kuadrant/mcp-gateway/internal/broker/upstream"
	"github.com/Kuadrant/mcp-gateway/internal/config"
	"github.com/Kuadrant/mcp-gateway/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProtocolHandler2025_ShouldFetchFresh_UsesUserSpecificListOnly(t *testing.T) {
	mockServer := newMockActiveMCPServer(config.MCPServer{
		Name:             "srv",
		URL:              "http://localhost/mcp",
		Prefix:           "s_",
		UserSpecificList: true,
	})
	b := &mcpBrokerImpl{
		mcpServers: map[config.UpstreamMCPID]upstream.ActiveMCPServer{
			mockServer.cfg.ID(): mockServer,
		},
		logger: slog.Default(),
	}
	h := NewProtocolHandler2025(b, slog.Default())
	srv := toUserSpecificServer(mockServer.cfg)

	// regardless of cache metadata, ShouldFetchFresh should follow CRD field
	require.True(t, h.ShouldFetchFresh(srv, nil))
	require.True(t, h.ShouldFetchFresh(srv, &upstream.CacheMetadata{CacheScope: "private", TTLMs: 0}))
	require.True(t, h.ShouldFetchFresh(srv, &upstream.CacheMetadata{CacheScope: "public", TTLMs: 5000}))
}

func TestProtocolHandler2025_ShouldFetchFresh_FalseWhenNotUserSpecific(t *testing.T) {
	mockServer := newMockActiveMCPServer(config.MCPServer{
		Name:             "srv",
		URL:              "http://localhost/mcp",
		Prefix:           "s_",
		UserSpecificList: false,
	})
	b := &mcpBrokerImpl{
		mcpServers: map[config.UpstreamMCPID]upstream.ActiveMCPServer{
			mockServer.cfg.ID(): mockServer,
		},
		logger: slog.Default(),
	}
	h := NewProtocolHandler2025(b, slog.Default())
	srv := toUserSpecificServer(mockServer.cfg)

	require.False(t, h.ShouldFetchFresh(srv, nil))
}

func TestProtocolHandler2025_AggregateCache_ReturnsZero(t *testing.T) {
	h := NewProtocolHandler2025(nil, slog.Default())

	ttl, scope := h.AggregateCache([]upstream.CacheMetadata{
		{TTLMs: 5000, CacheScope: "public"},
		{TTLMs: 10000, CacheScope: "private"},
	})
	assert.Equal(t, 0, ttl)
	assert.Equal(t, "", scope)

	ttl, scope = h.AggregateCache(nil)
	assert.Equal(t, 0, ttl)
	assert.Equal(t, "", scope)
}

func TestProtocolHandler2025_FetchUserSpecificTools(t *testing.T) {
	var initCount atomic.Int32
	ts := newTestMCPServer(&initCount, "upstream-session-1")
	defer ts.Close()

	cfg := config.MCPServer{
		Name:             "user-server",
		URL:              ts.URL,
		Prefix:           "us_",
		State:            "Enabled",
		UserSpecificList: true,
	}
	cache, _ := session.NewCache()
	srv := toUserSpecificServer(cfg)
	b := &mcpBrokerImpl{
		logger:                   slog.Default(),
		sessionCache:             cache,
		userSpecificFetchTimeout: 10 * time.Second,
	}
	h := NewProtocolHandler2025(b, slog.Default())

	result := &mcp.ListToolsResult{
		Tools: []*mcp.Tool{{Name: "cached-tool"}},
	}
	headers := http.Header{
		"Mcp-Session-Id": []string{"gw-session-abc"},
		"Authorization":  []string{"Bearer user-token"},
	}

	h.FetchUserSpecificTools(context.Background(), []userSpecificServer{srv}, headers, result)

	require.Len(t, result.Tools, 2)
	assert.Equal(t, "cached-tool", result.Tools[0].Name)
	assert.Equal(t, "us_user_tool", result.Tools[1].Name)
}

func TestProtocolHandler2025_FetchUserSpecificTools_EmptyServers(t *testing.T) {
	h := NewProtocolHandler2025(nil, slog.Default())

	result := &mcp.ListToolsResult{
		Tools: []*mcp.Tool{{Name: "existing"}},
	}

	h.FetchUserSpecificTools(context.Background(), nil, http.Header{}, result)
	assert.Len(t, result.Tools, 1)
}
