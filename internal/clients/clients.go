/*
Package clients provides a set of clients for use with the gateway code
*/
package clients

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Kuadrant/mcp-gateway/internal/config"
	"github.com/mark3labs/mcp-go/mcp"
)

const sessionHeader = "Mcp-Session-Id"

// HairpinClientPool manages *http.Client instances for hairpin requests,
// keyed by TLS ServerName (SNI). For HTTP or when all servers share the
// same listener, a single default client is used. When a server attaches
// to a different HTTPS listener, a client with that server's SNI is
// created on demand and cached.
type HairpinClientPool struct {
	defaultClient *http.Client
	baseTLSConfig *tls.Config // nil for HTTP
	mu            sync.RWMutex
	clients       map[string]*http.Client
}

// Get returns an *http.Client with the appropriate TLS ServerName.
// If sniOverride is empty, the default client (gateway hostname SNI) is returned.
func (p *HairpinClientPool) Get(sniOverride string) *http.Client {
	if sniOverride == "" || p.baseTLSConfig == nil {
		return p.defaultClient
	}

	sni := sniOverride
	if h, _, err := net.SplitHostPort(sniOverride); err == nil {
		sni = h
	}

	// double-checked locking: read lock for the fast path (concurrent readers),
	// then write lock with a re-check to avoid duplicate creation when multiple
	// goroutines race past the read lock simultaneously
	p.mu.RLock()
	c, ok := p.clients[sni]
	p.mu.RUnlock()
	if ok {
		return c
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok = p.clients[sni]; ok {
		return c
	}

	cfg := p.baseTLSConfig.Clone()
	cfg.ServerName = sni
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = cfg
	c = &http.Client{Transport: t}
	p.clients[sni] = c
	return c
}

// NewClient creates a fresh *http.Client with the appropriate TLS ServerName.
// Unlike Get, it never reuses a cached client — each call returns a new instance.
func (p *HairpinClientPool) NewClient(sniOverride string) *http.Client {
	if sniOverride == "" || p.baseTLSConfig == nil {
		return &http.Client{}
	}

	sni := sniOverride
	if h, _, err := net.SplitHostPort(sniOverride); err == nil {
		sni = h
	}

	cfg := p.baseTLSConfig.Clone()
	cfg.ServerName = sni
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = cfg
	return &http.Client{Transport: t}
}

// buildHairpinURL composes the hairpin URL the broker uses to send the internal
// initialize request back through the gateway. gatewayHost may be either a
// bare host[:port] (in which case http:// is assumed for backwards
// compatibility) or a full URL prefix that already carries an http:// or
// https:// scheme. This is what lets HTTPS-listener hairpins work without
// silently sending plain HTTP to a TLS-only port (issue #917).
func buildHairpinURL(gatewayHost, mcpPath string) string {
	lowerHost := strings.ToLower(gatewayHost)
	if strings.HasPrefix(lowerHost, "http://") || strings.HasPrefix(lowerHost, "https://") {
		return gatewayHost + mcpPath
	}
	return "http://" + gatewayHost + mcpPath
}

// HairpinSession holds the result of a hairpin initialize handshake.
// The caller uses SessionID for cache storage and Close to tear down
// the backend session when the gateway session expires.
type HairpinSession struct {
	sessionID  string
	serverURL  string
	httpClient *http.Client
	headers    map[string]string
}

// GetSessionID returns the backend session ID obtained during initialize.
func (h *HairpinSession) GetSessionID() string {
	return h.sessionID
}

// Close sends a DELETE to the backend to terminate the session.
func (h *HairpinSession) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.serverURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create close request: %w", err)
	}
	req.Header.Set(sessionHeader, h.sessionID)
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send close request: %w", err)
	}
	_ = resp.Body.Close()
	return nil
}

// Initialize performs a hairpin initialize handshake through the gateway.
// It sends the JSON-RPC initialize request, extracts the backend session
// ID from the response, and sends the notifications/initialized follow-up.
// Returns a HairpinSession that the caller uses for session tracking and cleanup.
//
// The caller must set the routing key and mcp-init-host headers in passThroughHeaders.
// These one-shot headers are only sent on the initialize request, not on
// notifications/initialized, avoiding the header-leaking bug where the
// mcp-go transport applied them to every request.
func Initialize(ctx context.Context, gatewayHost string, conf *config.MCPServer, passThroughHeaders map[string]string, clientElicitation bool, hairpinClientPool *HairpinClientPool) (*HairpinSession, error) {
	mcpPath, err := conf.Path()
	if err != nil {
		return nil, err
	}

	url := buildHairpinURL(gatewayHost, mcpPath)
	hairpinHTTPClient := hairpinClientPool.Get(conf.Hostname)
	passThroughHeaders["x-client-id"] = "lazyinit"

	caps := mcp.ClientCapabilities{}
	if clientElicitation {
		caps.Elicitation = &mcp.ElicitationCapability{}
	}

	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
			"capabilities":    caps,
			"clientInfo": map[string]string{
				"name":    "mcp-gateway",
				"version": "0.0.1",
			},
		},
	}
	body, err := json.Marshal(initReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal initialize request: %w", err)
	}

	passThroughHeaders["x-client-id"] = "lazyinit"

	// send initialize — includes all passThroughHeaders (router-key, mcp-init-host, etc.)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create initialize request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range passThroughHeaders {
		req.Header.Set(k, v)
	}

	resp, err := hairpinHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send initialize request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("initialize returned %d: %s", resp.StatusCode, string(respBody))
	}

	backendSessionID := resp.Header.Get(sessionHeader)
	if backendSessionID == "" {
		return nil, fmt.Errorf("backend did not return a session ID")
	}

	// build the set of headers for the notifications/initialized request:
	// pass through everything except the one-shot routing headers
	notifyHeaders := make(map[string]string, len(passThroughHeaders))
	for k, v := range passThroughHeaders {
		lk := strings.ToLower(k)
		if lk == "router-key" || lk == "mcp-init-host" {
			continue
		}
		notifyHeaders[k] = v
	}

	// send notifications/initialized — only with the backend session, no routing headers
	notifyReq := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	notifyBody, err := json.Marshal(notifyReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal initialized notification: %w", err)
	}

	nReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(notifyBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create initialized notification request: %w", err)
	}
	nReq.Header.Set("Content-Type", "application/json")
	nReq.Header.Set(sessionHeader, backendSessionID)
	for k, v := range notifyHeaders {
		nReq.Header.Set(k, v)
	}

	nResp, err := hairpinHTTPClient.Do(nReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send initialized notification: %w", err)
	}
	_ = nResp.Body.Close()

	return &HairpinSession{
		sessionID:  backendSessionID,
		serverURL:  url,
		httpClient: hairpinHTTPClient,
		headers:    notifyHeaders,
	}, nil
}

// BuildHairpinHTTPClientPool returns a HairpinClientPool for hairpin requests.
// For HTTPS private hosts it configures TLS with the publicHost as the default
// ServerName (SNI). Servers on a different HTTPS listener can obtain a client
// with a different SNI via pool.Get(serverHostname).
// For plain HTTP it returns a pool whose default client has no TLS.
func BuildHairpinHTTPClientPool(privateHost, publicHost, caCertPath string) (*HairpinClientPool, error) {
	if !strings.HasPrefix(strings.ToLower(privateHost), "https://") {
		return &HairpinClientPool{
			defaultClient: &http.Client{},
			clients:       make(map[string]*http.Client),
		}, nil
	}

	certPool, err := x509.SystemCertPool()
	if err != nil {
		certPool = x509.NewCertPool()
	}

	if caCertPath != "" {
		pem, err := os.ReadFile(caCertPath) //nolint:gosec // path comes from a CLI flag, not user input
		if err != nil {
			return nil, fmt.Errorf("failed to read gateway CA cert: %w", err)
		}
		if !certPool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("failed to parse gateway CA cert PEM")
		}
	}

	defaultSNI := publicHost
	if h, _, err := net.SplitHostPort(publicHost); err == nil {
		defaultSNI = h
	}

	baseCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    certPool,
		ServerName: defaultSNI,
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = baseCfg
	return &HairpinClientPool{
		defaultClient: &http.Client{Transport: t},
		baseTLSConfig: baseCfg,
		clients:       make(map[string]*http.Client),
	}, nil
}
