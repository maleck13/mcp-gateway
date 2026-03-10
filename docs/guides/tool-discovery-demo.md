# Tool Discovery Demo

This guide walks you through using **tool discovery** with mcp-gateway and Claude Code. Tool discovery lets an AI agent explore available tools through a lightweight catalog instead of loading every tool definition upfront, significantly reducing token consumption.

## What You'll See

1. Claude Code discovers available tools via a single `discover_tools` call
2. The catalog returns compact metadata (categories, hints, tags) instead of full tool schemas
3. Claude uses this metadata to pick the right tool and call it through the gateway

## Prerequisites

- A running Kind cluster with mcp-gateway deployed via `make local-env-setup`
- Claude Code installed
- `kubectl` configured to talk to the Kind cluster

## Setup

Run the demo setup script from the mcp-gateway repo root:

```bash
./hack/demo-tool-discovery.sh
```

This will:
- Build and load the tool-discovery branch images into Kind
- Apply annotated MCPServerRegistration CRDs with category/tags/hint metadata
- Print the Claude Code MCP server configuration

## Configure Claude Code

Add the following to your Claude Code MCP server configuration (`.claude/settings.json` or project `.mcp.json`):

```json
{
  "mcpServers": {
    "mcp-gateway": {
      "type": "streamable-http",
      "url": "http://mcp.127-0-0-1.sslip.io:7001/mcp"
    }
  }
}
```

## Walkthrough

### Step 1: Discover Available Tools

Prompt Claude Code:

> What tools are available on the gateway?

Claude will call `discover_tools`, which returns a compact catalog like:

```json
{
  "servers": [
    {
      "name": "test-server1",
      "prefix": "test1_",
      "category": "utilities",
      "hint": "General-purpose utilities: greeting, time, request inspection, and latency testing",
      "tags": {"sdk": "go-sdk", "team": "platform"},
      "tools": ["test1_hi", "test1_time", "test1_headers", "test1_slow"]
    },
    {
      "name": "test-server2",
      "prefix": "test2_",
      "category": "utilities",
      "hint": "Extended utilities: greeting, time management, authentication testing, and chocolate molding",
      "tags": {"sdk": "mcp-go", "team": "platform"},
      "tools": ["test2_hello_world", "test2_time", "test2_headers", "test2_auth1234", "test2_slow"]
    }
  ]
}
```

Notice how this gives Claude enough context to understand what each server does without loading every tool's full JSON Schema definition.

### Step 2: Use a Discovered Tool

Prompt Claude Code:

> Say hello to the MCP Gateway team

Claude will use the catalog hints to identify that `test1_hi` or `test2_hello_world` is a greeting tool and call it through the gateway. You should see a greeting response.

## What Happened

The flow:

1. **Claude called `discover_tools`** — a single lightweight call to the broker
2. **Broker returned the catalog** — compact metadata with tool names, categories, hints, and tags (no full schemas)
3. **Claude picked the right tool** — used hints like "greeting" to select `test1_hi` or `test2_hello_world`
4. **Claude called the tool** — the gateway routed the request to the upstream MCP server
5. **Response returned** — through the gateway back to Claude

## Token Usage: Discovery vs Raw Tools

The key benefit of tool discovery is reduced token consumption. Here's why:

### Without Tool Discovery (Raw Tools List)

Every MCP tool definition is sent to the LLM on every request as part of the system prompt. Each tool includes its full JSON Schema with parameter names, types, descriptions, and nested objects. For the 9 tools across our two test servers, this might look like:

| Component | Approximate Tokens |
|---|---|
| 9 tool definitions with JSON Schema | ~2,000-3,000 |
| Sent on **every** request | cumulative cost grows fast |

As the number of registered servers grows, this scales linearly. A gateway with 50+ tools could consume **10,000+ tokens** per request just for tool definitions.

### With Tool Discovery

Only the `discover_tools` tool is sent to the LLM in the system prompt. The compact catalog is fetched once per session:

| Component | Approximate Tokens |
|---|---|
| 1 `discover_tools` tool definition | ~150 |
| Catalog response (fetched once) | ~300 |
| Individual tool schema (fetched on demand) | ~200-400 per tool |

The LLM sees ~450 tokens for discovery instead of ~2,500+ for all raw tools. Individual tool schemas are only fetched when actually needed.

### The Scaling Advantage

| Gateway Size | Raw Tools (per request) | Discovery (first request) | Discovery (subsequent) |
|---|---|---|---|
| 10 tools | ~3,000 tokens | ~500 tokens | ~150 tokens |
| 50 tools | ~15,000 tokens | ~1,500 tokens | ~150 tokens |
| 200 tools | ~60,000 tokens | ~5,000 tokens | ~150 tokens |

With discovery, subsequent requests only pay for the `discover_tools` definition (~150 tokens) since the catalog is already in context. The savings compound with scale and conversation length.
