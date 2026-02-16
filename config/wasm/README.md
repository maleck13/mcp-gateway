# MCP Wasm Router

This directory contains EnvoyFilter configurations for the Wasm-based MCP router.

## Overview

The Wasm router replaces the ext_proc-based router with an in-process Wasm filter, reducing latency by eliminating gRPC round-trips.

## Requirements

- Envoy 1.35.0+ (for `allow_on_headers_stop_iteration` support)
- Istio 1.24+ (ships with Envoy 1.35)

## Files

| File | Description |
|------|-------------|
| `envoyfilter.yaml` | EnvoyFilter loading Wasm from local file |
| `envoyfilter-remote.yaml` | EnvoyFilter loading Wasm from remote URI |
| `gateway-patch.yaml` | Deployment patch to mount Wasm via init container |
| `kustomization.yaml` | Kustomize configuration |

## Quick Start (Kind)

```bash
# Build, package, load, and deploy in one command
make wasm-router-deploy
```

This will:
1. Build the Wasm binary
2. Package it as an OCI image
3. Load the image into Kind
4. Patch the gateway deployment with an init container
5. Apply the EnvoyFilter

## Manual Deployment

### Step 1: Build and Load Image

```bash
# Build Wasm binary
make wasm-router

# Build OCI image
make wasm-router-image

# Load into Kind cluster
make wasm-router-load
```

### Step 2: Patch Gateway

The gateway needs the Wasm binary mounted. The init container approach copies it from the OCI image:

```bash
# Find the gateway deployment name
GATEWAY_DEPLOY=$(kubectl get deploy -n gateway-system \
  -l gateway.io/name=mcp-gateway -o jsonpath='{.items[0].metadata.name}')

# Apply the patch
kubectl patch deployment $GATEWAY_DEPLOY -n gateway-system \
  --patch-file config/wasm/gateway-patch.yaml

# Wait for rollout
kubectl rollout status deployment/$GATEWAY_DEPLOY -n gateway-system
```

### Step 3: Apply EnvoyFilter

```bash
kubectl apply -k config/wasm/
```

## Configuration

The Wasm filter accepts configuration via the `configuration` field:

```json
{
  "brokerHostname": "mcp-broker-router.mcp-system.svc.cluster.local",
  "brokerPath": "/mcp",
  "servers": {
    "weather-server": {
      "hostname": "weather.mcp.local",
      "path": "/mcp",
      "toolPrefix": "weather_",
      "credentials": "Bearer xxx"
    }
  }
}
```

### Configuration Fields

| Field | Description |
|-------|-------------|
| `brokerHostname` | Hostname for routing non-tool-call requests |
| `brokerPath` | Path for broker requests (default: `/mcp`) |
| `servers` | Map of server ID to server configuration |
| `servers.<id>.hostname` | Upstream server hostname |
| `servers.<id>.path` | Upstream server path (default: `/mcp`) |
| `servers.<id>.toolPrefix` | Prefix to strip from tool names |
| `servers.<id>.credentials` | Credentials header value |

## Why Not WasmPlugin?

Istio's `WasmPlugin` CRD doesn't expose `allow_on_headers_stop_iteration`, which is required for MCP routing. This setting allows the Wasm filter to buffer headers until the request body is processed, enabling routing decisions based on the JSON-RPC method and tool name.

We use `EnvoyFilter` directly to access this configuration option.

## Tool Routing

The Wasm filter routes tool calls using:

1. **JWT claims**: Tool mappings from `tools` claim in session JWT
2. **Prefix matching**: Fallback to matching tool prefix against server config

### JWT Tool Mappings

The broker sets tool mappings in the session JWT:

```json
{
  "tools": {
    "weather_get_forecast": "weather-server",
    "github_search": "github-server"
  }
}
```

## Switching from ext_proc

To switch from ext_proc to Wasm:

```bash
# Remove the ext_proc EnvoyFilter
kubectl delete envoyfilter mcp-ext-proc -n istio-system

# Deploy Wasm router
make wasm-router-deploy
```

To switch back:

```bash
# Remove Wasm router
make wasm-router-undeploy

# Re-apply ext_proc
kubectl apply -f config/istio/envoyfilter.yaml
```

## Troubleshooting

### Check if Wasm is loaded

```bash
# Check gateway logs for Wasm loading
kubectl logs -n gateway-system -l gateway.io/name=mcp-gateway -c istio-proxy | grep -i wasm

# Check if init container ran
kubectl get pods -n gateway-system -l gateway.io/name=mcp-gateway -o jsonpath='{.items[0].status.initContainerStatuses}'
```

### Verify EnvoyFilter is applied

```bash
kubectl get envoyfilter -n istio-system
istioctl proxy-config listener <gateway-pod> -n gateway-system -o json | grep mcp-routing
```

## Architecture

```
Client → Envoy → Wasm Filter → (optional Kuadrant) → Upstream
              ↓
         MCP Filter (parses JSON-RPC)
              ↓
         Wasm (routes based on tool name)
```

The Wasm filter:
1. Buffers headers (via `allow_on_headers_stop_iteration`)
2. Parses JSON-RPC body to extract method and tool name
3. Looks up server from JWT claims or prefix matching
4. Sets routing headers (`:authority`, `:path`)
5. Rewrites body to strip tool prefix if needed
6. Continues to upstream

Sources:
- [Istio WasmPlugin](https://istio.io/latest/docs/reference/config/proxy_extensions/wasm-plugin/)
- [Istio Wasm Module Distribution](https://istio.io/latest/docs/tasks/extensibility/wasm-module-distribution/)
- [Building OCI Images for Wasm](https://github.com/istio-ecosystem/wasm-extensions/blob/master/doc/how-to-build-oci-images.md)
