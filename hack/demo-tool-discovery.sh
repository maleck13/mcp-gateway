#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "=== Tool Discovery Demo Setup ==="
echo ""

# Step 1: Build and load images from the tool-discovery branch
echo "Building and loading images into Kind cluster..."
make -C "$PROJECT_ROOT" build-and-load-image

# Step 2: Apply annotated MCPServerRegistration CRDs
echo "Applying annotated MCPServerRegistration CRDs with discovery metadata..."
kubectl apply -f "$PROJECT_ROOT/config/samples/mcpserverregistration-test-servers-discovery.yaml"

# Step 3: Wait for broker to restart and become ready
echo "Waiting for broker pod to be ready..."
kubectl wait --for=condition=available --timeout=120s deployment/mcp-gateway -n mcp-system

echo ""
echo "================================================================"
echo "Tool Discovery Demo Ready!"
echo "================================================================"
echo ""
echo "Gateway URL: http://mcp.127-0-0-1.sslip.io:7001/mcp"
echo ""
echo "Add this to your Claude Code MCP server config:"
echo ""
echo '  {'
echo '    "mcpServers": {'
echo '      "mcp-gateway": {'
echo '        "type": "streamable-http",'
echo '        "url": "http://mcp.127-0-0-1.sslip.io:7001/mcp"'
echo '      }'
echo '    }'
echo '  }'
echo ""
echo "Then follow the walkthrough in docs/guides/tool-discovery-demo.md"
echo "================================================================"
