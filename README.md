# mcp-gateway

An Envoy-based MCP Gateway

## Quick Start

```bash
# Setup
make local-env-setup    # Create a Kind cluster with Istio, Gateway API, MetalLB
make deploy-mock-mcp    # Deploy mock MCP server for testing

# Local Development
make dev                # Configure cluster to use local services
make run-router         # Run router locally (port 9002)
make run-broker         # Run broker locally (port 8080)
make dev-gateway-forward # Forward gateway to localhost:8888

# Inspection
make urls               # Show all service URLs
make status             # Show status of all components
make inspect-mock       # Open MCP Inspector for mock server

# Cleanup
make dev-stop          # Stop local processes
make undeploy-mock-mcp # Remove mock server
make local-env-teardown # Destroy cluster
```

Run `make help` to see all available commands.
