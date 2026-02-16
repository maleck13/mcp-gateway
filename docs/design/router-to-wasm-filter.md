# MCP Router Migration to Wasm Filter

## Overview

The current MCP Gateway uses an Envoy External Processor (ext_proc) for routing MCP requests. This document proposes replacing ext_proc with a custom Wasm filter to reduce latency and simplify the architecture.

### Requirements

- **Go 1.24+**: Required for native WASI support (`GOOS=wasip1 GOARCH=wasm`)
- **Envoy 1.35.0+**: Required for `allow_on_headers_stop_iteration` support
- **Istio 1.24+**: Ships with Envoy 1.35

Using Go 1.24+ with `proxy-wasm-go-sdk` removes the need for TinyGo compilation, providing access to standard library features.

### Current Architecture

```
Client → Envoy → ext_proc (gRPC, port 50051) → Envoy → Upstream
```

The ext_proc handles:
- JSON-RPC parsing and validation
- Tool name extraction and prefix stripping
- Broker queries for tool→server resolution
- Session management (gateway↔upstream mapping)
- Header manipulation (`:authority`, `:path`, credentials)

**Why ext_proc was used**: MCP routing requires reading the request body (to extract the tool name) before setting routing headers (`:authority`, `:path`). The ext_proc protocol naturally supports this - it receives headers, then body, and can modify both before forwarding. In contrast, Wasm filters historically had to forward headers before the body was available, making body-based routing decisions impossible.

**What changed**: Envoy v1.35.0 introduced `allow_on_headers_stop_iteration` for Wasm filters, enabling them to buffer headers until body processing completes. This removes the technical barrier that required ext_proc.

**Problem**: Every request requires a gRPC round-trip to ext_proc, adding latency. Having a separate service also increases complexity around, managing, deploying, debugging and observability.

## Considered Alternatives

### Envoy MCP Filter (envoy.filters.http.mcp)

Envoy has a native MCP filter (`envoy.filters.http.mcp`) designed for Model Context Protocol traffic. This was evaluated as a potential replacement for ext_proc.

**Why it wasn't chosen**:

The MCP filter does not support **request body modification**. When routing tool calls to backend MCP servers, we need to strip the tool prefix from the request body. For example:

- Client sends: `{"method": "tools/call", "params": {"name": "weather_get_forecast", ...}}`
- Backend expects: `{"method": "tools/call", "params": {"name": "get_forecast", ...}}`

The prefix (`weather_`) must be stripped from `params.name` in the JSON body. The Envoy MCP filter only supports header manipulation, not body rewriting.

**MCP Filter capabilities**:
- Header-based routing decisions
- Protocol validation
- Metrics and observability

**MCP Filter limitations**:
- No request body modification
- No response body modification
- Cannot strip tool prefixes

**Conclusion**: A custom Wasm filter is required to support body rewriting for prefix stripping. The Wasm filter can perform all MCP filter functions plus body manipulation.

### Future Consideration: Envoy MCP Router Filter

Envoy has an [MCP Router filter](https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/mcp_router/v3/mcp_router.proto) (`envoy.filters.http.mcp_router`) that aggregates multiple backend MCP servers and presents Envoy as a single MCP server to clients.

**Capabilities**:
- Multiplexes multiple backend MCP servers into one unified interface
- Cluster-based routing to backends
- Custom path configuration per backend
- Session identity extraction (header or dynamic metadata)
- Host header rewriting

**Current limitations**:
- Marked as "work-in-progress" without substantial production testing
- Replaces standard HTTP router, ignoring route-level policies
- Requires trusted downstream and upstream components
- **Unknown**: Whether it supports tool prefix stripping in request bodies

**Recommendation**: Monitor this filter's development. If body modification support is added, it could replace the custom Wasm filter. Until then, the Wasm approach provides the flexibility needed for prefix stripping.

### Proposed Architecture

```
Client → Envoy → WASM Routing Filter → (optional Kuadrant → Guardrails) → Upstream
```

- **WASM Routing Filter**: In-process filter handles JSON-RPC parsing, routing decisions, body rewriting, header manipulation
- **No ext_proc**: Eliminates gRPC round-trip latency and external service complexity

## Implementation

### Filter Chain

1. **MCP Routing Wasm** - Parse JSON-RPC, route decisions, body rewrite (if prefix), set headers
2. **Kuadrant (optional)** - AuthPolicy and RateLimitPolicy on routed request
3. **Guardrails (optional)** - LLM safety checks
4. **Envoy Router** - Forward to upstream

### Data Flow

See [Wasm Router Request Flow](images/wasm-flow.md) for detailed flow diagrams.

```
tools/call "weather_get_forecast"
    │
    ├─ Wasm: parse JSON-RPC body using gjson
    ├─ Wasm: read JWT for tool→server mapping
    ├─ Wasm: read server config from plugin config (controller managed)
    ├─ Wasm: check shared data for backend session (or lazy init)
    ├─ Wasm: rewrite body if stripping prefix is needed
    └─ Wasm: set routing headers → Upstream
```

### Session Storage

**Default: Envoy Shared Data**

Sessions are stored in Envoy's shared data with no external dependencies:
- Key format: `mcp:session:{gatewaySessionID}:{serverID}`
- Value format: `{expiresAt}:{backendSessionID}`
- Expiration checked using `time.Now().Unix()` on each lookup
- Sessions deleted reactively on 404 response from backend

**Optional: Redis HTTP Wrapper**

For larger deployments where shared data limits are reached, an HTTP wrapper around Redis can be used. The Wasm filter accesses it via `DispatchHttpCall` (proxy-wasm-go-sdk does not support gRPC dispatch).

### Lazy Initialization (Hairpin Flow)

When a tool call arrives for a backend server without an established session the plugin will perform a lazy initialization flow and store a new backend MCP session keyed against the clients original "gateway mcp session id":

```
Client ──► Gateway/Wasm ──► DispatchHttpCall ──► Gateway ──► Backend MCP
                                                    │
                                                    ▼
Client ◄── Gateway/Wasm ◄── onInitCallback ◄────────┘
                │
                └──► ResumeHttpRequest ──► Backend MCP ──► Client
```

1. **triggerLazyInit**: Pauses the original request, stores pending state, dispatches async HTTP call
2. **Hairpin request**: Goes through gateway with `mcp-init-host` and `router-key` headers
3. **Backend MCP**: Responds with `mcp-session-id` header
4. **onInitCallback**: Stores session in shared data, resumes original request with backend session ID

The `routerAPIKey` configuration validates that hairpin initialization requests originate from the Wasm router itself, not external clients.

### Session Cleanup

Two mechanisms ensure sessions are cleaned up:

1. **Proactive**: Check `time.Now().Unix() > expiresAt` on each `getBackendSession()` call
2. **Reactive**: Delete session when backend returns 404 (session not found)

### Key Optimizations

1. **No broker lookup for tool calls**: Tool→serverID mapping stored in JWT, server config keyed against serverID is in EnvoyFilter configuration and managed externally by the controller component
2. **Conditional body rewrite**: Only rewrite body when prefix stripping is needed
3. **In-process execution**: Wasm runs inside Envoy, no gRPC round-trip
4. **Lazy session initialization**: Backend sessions created on-demand, not upfront

### What Remains External

| Component   | Purpose |
|-------------|---------|
| Broker      | Generic MCP Backend for non-tool-call requests |
| Redis HTTP  | Optional session state wrapper (REST API), accessible via `DispatchHttpCall` |

## Example Configuration

### EnvoyFilter Configuration

```yaml
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: mcp-wasm-router
  namespace: gateway-system
spec:
  targetRefs:
  - group: gateway.networking.k8s.io
    kind: Gateway
    name: mcp-gateway
  configPatches:
  - applyTo: HTTP_FILTER
    match:
      context: GATEWAY
      listener:
        portNumber: 8080
        filterChain:
          filter:
            name: envoy.filters.network.http_connection_manager
            subFilter:
              name: envoy.filters.http.router
    patch:
      operation: INSERT_BEFORE
      value:
        name: envoy.filters.http.wasm
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.http.wasm.v3.Wasm
          config:
            name: mcp-routing
            root_id: mcp_routing
            allow_on_headers_stop_iteration:
              value: true
            configuration:
              "@type": type.googleapis.com/google.protobuf.StringValue
              value: |
                {
                  "brokerHostname": "mcp.example.com",
                  "brokerPath": "/mcp",
                  "routerAPIKey": "secret-key-for-hairpin-validation",
                  "servers": {
                    "weather-server": {
                      "hostname": "weather.mcp.local",
                      "path": "/mcp",
                      "toolPrefix": "weather_",
                      "credentials": "Bearer xxx"
                    },
                    "github-server": {
                      "hostname": "api.githubcopilot.com",
                      "path": "/mcp",
                      "toolPrefix": "github_",
                      "credentials": "Bearer yyy"
                    }
                  }
                }
            vm_config:
              runtime: envoy.wasm.runtime.v8
              code:
                local:
                  filename: /etc/envoy/mcp-routing.wasm
```

### Configuration Fields

| Field | Description |
|-------|-------------|
| `brokerHostname` | Public gateway hostname (matches `--mcp-gateway-public-host` flag) |
| `brokerPath` | Path for broker requests (default: `/mcp`) |
| `routerAPIKey` | Secret key for validating hairpin requests from broker |
| `servers` | Map of server ID to server configuration |
| `servers.<id>.hostname` | Upstream server hostname |
| `servers.<id>.path` | Upstream server path (default: `/mcp`) |
| `servers.<id>.toolPrefix` | Prefix to strip from tool names |
| `servers.<id>.credentials` | Credentials header value (see note below) |

**Note on Credentials**: The `credentials` field is currently stored in plain text within the EnvoyFilter configuration. This is a security concern as credentials are visible to anyone with read access to the EnvoyFilter resource. Options:
- Storing credentials in an external secrets manager (vault) accessed via `DispatchHttpCall`
- Using Kubernetes RBAC to restrict EnvoyFilter read access


### Tool→Server Mapping in JWT

The broker sets tool mappings in the session JWT during `initialize`:

```json
{
  "exp": 1234567890,
  "tools": {
    "weather_get_forecast": "weather-server",
    "github_search": "github-server"
  }
}
```

This conservatively supports up to ~200 tool mappings while staying within common HTTP header size limits.

## Appendix

### Wasm vs ext_proc Comparison

| Feature | ext_proc | Wasm |
|---------|----------|------|
| Body modification  | Yes | Yes |
| JSON parsing       | Yes | Yes (gjson) |
| Ext HTTP calls     | Yes | Yes |
| Ext gRPC calls     | Yes | No (Go SDK limitation) |
| In-process         | No  | Yes |
| Time API           | Yes | Yes (use `.UTC()`) |
| Crypto (RSA, etc)  | Yes | No (WASI limitation) |

### Header/Body Buffering

**Solution**: Use `allow_on_headers_stop_iteration` in Envoy's Wasm PluginConfig to buffer headers until body processing completes. Requires Envoy v1.35.0 or later.

Since Istio's `WasmPlugin` resource does not expose `allow_on_headers_stop_iteration`, we use `EnvoyFilter` for all Wasm configuration.

### Redis HTTP Wrapper API (Optional)

If shared data limits are reached, a simple HTTP wrapper around Redis will allow external session storage:

```
GET    /session/{gatewaySessionID}/{serverID}  → {"backendSessionID": "...", "expiresAt": 123}
PUT    /session/{gatewaySessionID}/{serverID}  → store session (body: {"backendSessionID": "...", "expiresAt": 123})
DELETE /session/{gatewaySessionID}/{serverID}  → delete session
```

### WASI Limitations

Envoy's WASI runtime does not support all WASI functions. Key limitations:

**Time Package**: Use `time.Now().UTC().Unix()` to avoid timezone file lookups. Do not use `time.Now().Unix()` directly.

### Build Requirements

```bash
# Build with Go 1.24+ (native WASI support, no TinyGo needed)
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o mcp-routing.wasm .
```

### Clearing Route Cache

When a Wasm filter modifies routing headers (`:authority`, `:path`), Envoy may still use the cached route from the original headers. To force Envoy to re-evaluate the route based on modified headers, call the `clear_route_cache` foreign function.

**Availability**: Envoy 1.33.0+

**Usage in proxy-wasm-go-sdk**:

```go
// After modifying :authority or :path headers
if _, err := proxywasm.CallForeignFunction("clear_route_cache", nil); err != nil {
    proxywasm.LogDebugf("failed to clear route cache: %v", err)
}
```

**When to use**:
- After `ReplaceHttpRequestHeader(":authority", newHost)`
- After `ReplaceHttpRequestHeader(":path", newPath)`
- Before returning `types.ActionContinue` to forward the request

**Note**: As of Envoy 1.33.0, the route cache is not automatically cleared when Wasm extensions modify request headers (for ABI versions > 0.2.1). You must explicitly call this function.

## References

- [proxy-wasm-go-sdk](https://github.com/proxy-wasm/proxy-wasm-go-sdk)
- [proxy-wasm spec v0.2.1](https://github.com/proxy-wasm/spec/blob/main/abi-versions/v0.2.1/README.md)
- [Envoy Wasm proto (v1.35.0)](https://www.envoyproxy.io/docs/envoy/v1.35.0/api-v3/extensions/wasm/v3/wasm.proto)
- [proxy-wasm StopIteration issue](https://github.com/proxy-wasm/spec/issues/63)
