#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "=== Tool Discovery Demo Setup ==="
echo ""

# Step 1: Apply annotated MCPServerRegistration CRDs
echo "Applying annotated MCPServerRegistration CRDs with discovery metadata..."
kubectl apply -f "$PROJECT_ROOT/config/samples/mcpserverregistration-test-servers-discovery.yaml"

# Step 2: Enable tool discovery on the broker
echo "Enabling --enable-tool-discovery on the broker deployment..."
kubectl patch deployment/mcp-gateway -n mcp-system --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/command/-","value":"--enable-tool-discovery"}]'

# Step 3: Wait for broker to restart and become ready
echo "Waiting for broker pod to roll out..."
kubectl rollout status deployment/mcp-gateway -n mcp-system --timeout=120s

echo ""
echo "================================================================"
echo "Tool Discovery Demo Ready!"
echo "================================================================"
echo ""
echo "Gateway URL: http://mcp.127-0-0-1.sslip.io:8001/mcp"
echo ""
echo "Add the MCP server to Claude Code by running:"
echo ""
echo "  claude mcp add --transport http mcp-gateway http://mcp.127-0-0-1.sslip.io:8001/mcp"
echo ""
echo "Then follow the walkthrough in docs/guides/tool-discovery-demo.md"
echo "================================================================"
