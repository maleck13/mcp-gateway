# Isolated MCP Gateway Deployment

This guide demonstrates how to deploy MCP Gateway instances for your environment. Each deployment maintains its own configuration based on the MCPGatewayExtension resource that defines which Gateway it serves.

## Overview

The MCP Gateway controller requires an `MCPGatewayExtension` resource to operate. This resource:

- Defines which Gateway the MCP Gateway instance is responsible for
- Determines where configuration secrets are created (same namespace as the MCPGatewayExtension)
- Enables isolation by allowing multiple MCP Gateway instances to target different Gateways

MCPServerRegistration resources are only processed when a valid MCPGatewayExtension exists for the Gateway their HTTPRoute is attached to. Without a matching MCPGatewayExtension, registrations will show a NotReady status.

## Prerequisites

- Kubernetes cluster with Gateway API support
- Istio installed as Gateway API provider
- MCP Gateway CRDs installed
- Helm 3.x

## Architecture

```
┌───────────────────────────────────────────────────────────────────────┐
│                        MCP System Namespace                           │
├───────────────────────────────────────────────────────────────────────┤
│                    MCP Controller (cluster-wide)                      │
└───────────────────────────────────────────────────────────────────────┘
                                    │
              ┌─────────────────────┴─────────────────────┐
              ▼                                           ▼
┌───────────────────────────────┐     ┌───────────────────────────────┐
│          Team A NS            │     │          Team B NS            │
├───────────────────────────────┤     ├───────────────────────────────┤
│ MCPGatewayExtension           │     │ MCPGatewayExtension           │
│   → Gateway A                 │     │   → Gateway B                 │
│ MCP Broker/Router             │     │ MCP Broker/Router             │
│ Config Secret                 │     │ Config Secret                 │
│ MCPServerRegistrations        │     │ MCPServerRegistrations        │
└───────────────────────────────┘     └───────────────────────────────┘
```

Each MCPGatewayExtension targets a different Gateway. The controller creates configuration secrets in the same namespace as the MCPGatewayExtension, which are mounted into the broker/router deployments.

For cross-namespace Gateway references, a ReferenceGrant must exist in the Gateway's namespace.

## Step 1: Deploy the MCP Controller

The MCP Controller runs cluster-wide and reconciles MCPGatewayExtension and MCPServerRegistration resources. Deploy it once in a central namespace:

```bash
helm install mcp-controller ./charts/mcp-gateway \
  --namespace mcp-system \
  --create-namespace \
  --set broker.enabled=false \
  --set envoyFilter.create=false
```

## Step 2: Create the Team Namespace

Create a namespace for the isolated MCP Gateway deployment:

```bash
kubectl create namespace team-alpha
```

## Step 3: Create ReferenceGrant (Cross-Namespace Only)

If the MCPGatewayExtension is in a different namespace than the Gateway, create a ReferenceGrant in the Gateway's namespace to authorize the cross-namespace reference:

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-team-alpha
  namespace: gateway-system
spec:
  from:
    - group: mcp.kagenti.com
      kind: MCPGatewayExtension
      namespace: team-alpha
  to:
    - group: gateway.networking.k8s.io
      kind: Gateway
EOF
```

Skip this step if the MCPGatewayExtension will be in the same namespace as the Gateway.

## Step 4: Create MCPGatewayExtension

Create the MCPGatewayExtension to associate the team's namespace with the target Gateway:

```bash
kubectl apply -f - <<EOF
apiVersion: mcp.kagenti.com/v1alpha1
kind: MCPGatewayExtension
metadata:
  name: team-alpha-gateway
  namespace: team-alpha
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: mcp-gateway
    namespace: gateway-system
EOF
```

Wait for the MCPGatewayExtension to become ready:

```bash
kubectl wait --for=condition=Ready mcpgatewayextension/team-alpha-gateway -n team-alpha --timeout=60s
```

## Step 5: Deploy MCP Gateway Instance

Deploy the broker and router into the team's namespace:

```bash
helm install mcp-gateway ./charts/mcp-gateway \
  --namespace team-alpha \
  --set envoyFilter.create=true \
  --set envoyFilter.namespace=istio-system \
  --set gateway.publicHost=team-alpha.mcp.example.com
```

The Helm chart creates:
- Broker/Router deployment
- Service for the broker
- EnvoyFilter to route traffic to this instance
- ServiceAccount and RBAC

## Step 6: Register MCP Servers

Create MCPServerRegistration resources in the team's namespace. The controller will automatically add their configuration to the team's config secret:

```bash
kubectl apply -f - <<EOF
apiVersion: mcp.kagenti.com/v1alpha1
kind: MCPServerRegistration
metadata:
  name: team-alpha-server
  namespace: team-alpha
spec:
  toolPrefix: alpha_
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: alpha-server-route
EOF
```

The MCPServerRegistration must target an HTTPRoute that is attached to a Gateway with a valid MCPGatewayExtension in the same namespace. Otherwise, the registration will show a NotReady status.

## Verification

Check that the configuration secret was created:

```bash
kubectl get secret mcp-gateway-config -n team-alpha
```

View the configuration:

```bash
kubectl get secret mcp-gateway-config -n team-alpha -o jsonpath='{.data.config\.yaml}' | base64 -d
```

Check broker logs for tool discovery:

```bash
kubectl logs -n team-alpha deployment/mcp-gateway-broker | grep "Discovered"
```

## Multiple Teams Example

To deploy a second isolated gateway for another team targeting a different Gateway:

```bash
# Create namespace
kubectl create namespace team-beta

# Create a second Gateway for team-beta (or use an existing one)
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: team-beta-gateway
  namespace: gateway-system
spec:
  gatewayClassName: istio
  listeners:
    - name: http
      port: 8080
      protocol: HTTP
      hostname: "*.team-beta.example.com"
EOF

# Create ReferenceGrant
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-team-beta
  namespace: gateway-system
spec:
  from:
    - group: mcp.kagenti.com
      kind: MCPGatewayExtension
      namespace: team-beta
  to:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: team-beta-gateway
EOF

# Create MCPGatewayExtension targeting the team-beta Gateway
kubectl apply -f - <<EOF
apiVersion: mcp.kagenti.com/v1alpha1
kind: MCPGatewayExtension
metadata:
  name: team-beta-ext
  namespace: team-beta
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: team-beta-gateway
    namespace: gateway-system
EOF

# Deploy broker/router
helm install mcp-gateway ./charts/mcp-gateway \
  --namespace team-beta \
  --set envoyFilter.create=true \
  --set envoyFilter.namespace=istio-system \
  --set gateway.publicHost=mcp.team-beta.example.com
```

Each team now has their own isolated MCP Gateway instance targeting separate Gateways.

## Limitations

**Broker/Router must be co-located with MCPGatewayExtension**: The MCP Gateway instance (broker and router) must be deployed in the same namespace as the MCPGatewayExtension. The controller writes the configuration secret to the MCPGatewayExtension's namespace, and the broker/router mount this secret.

**One MCPGatewayExtension per namespace**: Each namespace can only have one MCPGatewayExtension. The controller writes configuration to a well-known secret name, so multiple extensions would overwrite each other.

**One MCPGatewayExtension per Gateway**: Only one MCPGatewayExtension can target a given Gateway. If multiple extensions target the same Gateway, the controller marks newer ones as conflicted. The oldest extension (by creation timestamp) wins.

**Same-namespace MCPServerRegistrations**: MCPServerRegistration resources must be in the same namespace as the MCPGatewayExtension. The controller uses the MCPGatewayExtension's namespace to determine where to write the configuration.

## Troubleshooting

### MCPGatewayExtension shows RefGrantRequired

The MCPGatewayExtension is targeting a Gateway in a different namespace, but no ReferenceGrant exists:

```bash
kubectl get mcpgatewayextension -n team-alpha -o yaml
```

Look for the condition:
```yaml
conditions:
  - type: Ready
    status: "False"
    reason: ReferenceGrantRequired
    message: "ReferenceGrant required in namespace gateway-system to allow cross-namespace reference"
```

Create the ReferenceGrant in the Gateway's namespace as shown in Step 3.

### MCPGatewayExtension shows InvalidMCPGatewayExtension

The target Gateway doesn't exist or there's a conflict:

```bash
kubectl get gateway -n gateway-system
```

If another MCPGatewayExtension already targets this Gateway, check which one is older:

```bash
kubectl get mcpgatewayextension -A -o custom-columns=NAME:.metadata.name,NAMESPACE:.metadata.namespace,CREATED:.metadata.creationTimestamp
```

### MCPServerRegistration shows NotReady

The registration can't find a valid MCPGatewayExtension for the Gateway its HTTPRoute is attached to:

```bash
kubectl get mcpserverregistration -n team-alpha -o yaml
```

Check that:
1. The HTTPRoute exists and references the correct Gateway
2. An MCPGatewayExtension exists in the same namespace targeting that Gateway
3. The MCPGatewayExtension is in Ready state

### No configuration in secret

The config secret exists but has no servers:

```bash
kubectl get secret mcp-gateway-config -n team-alpha -o jsonpath='{.data.config\.yaml}' | base64 -d
```

Check that MCPServerRegistration resources exist and are Ready:

```bash
kubectl get mcpserverregistration -n team-alpha
```

## Cleanup

To remove an isolated deployment:

```bash
# Delete MCP server registrations
kubectl delete mcpserverregistration --all -n team-alpha

# Uninstall Helm release
helm uninstall mcp-gateway -n team-alpha

# Delete MCPGatewayExtension
kubectl delete mcpgatewayextension team-alpha-gateway -n team-alpha

# Delete ReferenceGrant
kubectl delete referencegrant allow-team-alpha -n gateway-system

# Delete namespace
kubectl delete namespace team-alpha
```
