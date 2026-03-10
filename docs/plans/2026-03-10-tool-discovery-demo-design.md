# Tool Discovery Demo Design

## Goal

A reproducible demo showing tdt-powered tool discovery working with the mcp-gateway and Claude Code. Targets docs/README users who want to try it themselves.

## Components

### 1. Setup Script (`hack/demo-tool-discovery.sh`)

Assumes a `make local-env-setup` cluster is already running.

Steps:
1. `make build-and-load-image` — builds broker/controller from the `tool-discovery` branch and loads into Kind
2. Apply `config/samples/mcpserverregistration-test-servers-discovery.yaml` — annotated CRDs with category/tags/hint
3. Wait for broker pod to restart and become ready
4. Print gateway URL and Claude Code MCP server config snippet

### 2. Annotated CRDs (`config/samples/mcpserverregistration-test-servers-discovery.yaml`)

Same as base test server registrations but with discovery metadata:

- **server1** (Go SDK — hi, time, slow, headers): category `utilities`, tags `{"sdk": "go-sdk", "team": "platform"}`, hint `"General-purpose utilities: greeting, time, request inspection, and latency testing"`
- **server2** (mcp-go — hello_world, time, headers, auth1234, slow, set_time, pour_chocolate): category `utilities`, tags `{"sdk": "mcp-go", "team": "platform"}`, hint `"Extended utilities: greeting, time management, authentication testing, and chocolate molding"`

### 3. Walkthrough Guide (`docs/guides/tool-discovery-demo.md`)

Sections:
- **Prerequisites** — existing local-env-setup cluster, Claude Code installed
- **Setup** — run the demo script
- **Configure Claude Code** — add gateway as MCP server (streamable-http, `http://mcp.127-0-0-1.sslip.io:7001/mcp`)
- **Walkthrough step 1** — Prompt: "What tools are available on the gateway?" → Claude calls `discover_tools`, gets catalog JSON
- **Walkthrough step 3** — Prompt: "Say hello to the MCP Gateway team" → Claude uses catalog to pick a greeting tool

Each step shows: the prompt, what tool Claude calls, expected output.
