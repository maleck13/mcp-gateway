# Tool Discovery Demo

This guide walks you through using **tool discovery** with mcp-gateway and Claude Code. Tool discovery lets an AI agent search for relevant tools, scope its session to only those tools, and reset when needed — significantly reducing token consumption on every subsequent turn.

## How It Works

Tool discovery is a two-step process:

1. **Search turn**: The agent calls `discover_tools(query="...")`. The broker finds matching tools, stores the selection, and tells the agent to end its turn.
2. **Use turn**: On the next turn, the agent's `tools/list` has been refreshed to only the matched tools. The agent can now call them normally.

This split exists because `tools/list` updates happen between turns via a `notifications/tools/list_changed` notification. The agent cannot use discovered tools in the same turn it searches — it must wait for its tool definitions to refresh.

## Prerequisites

- A running Kind cluster with mcp-gateway deployed via `make local-env-setup`
- Claude Code installed
- `kubectl` configured to talk to the Kind cluster

## Setup

Build and load images into Kind manually, then run the demo setup script:

```bash
./hack/demo-tool-discovery.sh
```

This will:
- Apply annotated MCPServerRegistration CRDs with category/tags/hint metadata
- Enable the `--enable-tool-discovery` flag on the broker
- Print the Claude Code MCP server configuration

## Configure Claude Code

Add the gateway as an MCP server:

```bash
claude mcp add --transport http mcp-gateway http://mcp.127-0-0-1.sslip.io:8001/mcp
```

Or add to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "mcp-gateway": {
      "type": "streamable-http",
      "url": "http://mcp.127-0-0-1.sslip.io:8001/mcp"
    }
  }
}
```

## Walkthrough

This demo simulates a realistic scenario: you're working on a project and need different tools at different stages — first you need to check service health, then later you need to greet users. Instead of loading all tools for the entire conversation, you scope to only what you need.

### Step 1: Explore what's available

> What tool categories are available on the gateway? Use discover_tools to find out.

Claude calls `discover_tools()` with no arguments. This returns the catalog — a lightweight overview (~300 tokens) without loading any tool schemas:

```json
{
  "categories": [
    {
      "name": "utilities",
      "servers": [
        {
          "name": "test-server1",
          "hint": "General-purpose utilities: greeting, time, request inspection, and latency testing",
          "tags": {"sdk": "go-sdk", "team": "platform"}
        },
        {
          "name": "test-server2",
          "hint": "Extended utilities: greeting, time management, authentication testing, and chocolate molding",
          "tags": {"sdk": "mcp-go", "team": "platform"}
        }
      ]
    }
  ]
}
```

No scoping happens — this is purely informational. Claude still has all 12 tools in context.

### Step 2: Scope to time tools

> I need to check the current time across our services. Search for time tools.

Claude calls `discover_tools(query="current time")`. The response:

```json
{
  "action": "search",
  "tools_matched": 2,
  "message": "Found 2 tools matching your query. Your tool list will update on the next turn. End your current turn now..."
}
```

Claude responds: *"I found 2 time-related tools. They'll be available on my next turn — go ahead and ask."*

**What happened behind the scenes:**
- The broker ran a BM25 ranked search across all tool names and descriptions
- The top matches were stored as the session's tool selection
- A `notifications/tools/list_changed` notification was sent
- Claude's `tools/list` will refresh between turns

### Step 3: Use the scoped tools

> What time is it on both servers?

Claude now has only 3 tools in context (2 matched + `discover_tools`):

| Source | Tools | Approx. Tokens |
|--------|-------|----------------|
| Matched tools | `test1_time`, `test2_time` | ~400 |
| broker | `discover_tools` (always included) | ~200 |
| **Total per turn** | **3 tools** | **~600 tokens** |

Down from ~2,800 to ~600 tokens. **78% reduction.**

Claude calls `test1_time` and `test2_time` normally. The gateway routes each request to the correct upstream server.

### Step 4: Switch to different tools

> Now I need to greet some users. Find greeting tools.

Claude calls `discover_tools(reset=true, query="greeting hello")`. This clears the time tool selection and searches in one step. The response:

```json
{
  "action": "search",
  "tools_matched": 2,
  "message": "Found 2 tools matching your query. Your tool list will update on the next turn..."
}
```

Claude responds: *"I found 2 greeting tools. They'll be available on my next turn."*

### Step 5: Use the new tools

> Say hello to Alice and Bob.

Claude now has greeting tools in context. It calls `test1_greet(name="Alice")` and `test2_hello_world(name="Bob")`.

The time tools are gone from context — only the greeting tools and `discover_tools` remain.

## Token Savings

### Per-turn cost comparison

Without discovery, **every turn** of the conversation carries all tool definitions:

| Scenario | Tools in context | Tokens per turn | Saving |
|----------|-----------------|-----------------|--------|
| No discovery (all tools) | 12 | ~2,800 | — |
| After scoping (2 matches) | 3 | ~600 | 78% |
| After scoping (1 match) | 2 | ~400 | 86% |

### Over a 20-turn conversation

The scoping turn costs one extra user message, but every subsequent turn is cheaper:

| Scenario | Total tool tokens | Saving |
|----------|-------------------|--------|
| No discovery | 20 × 2,800 = **56,000** | — |
| Scope on turn 1, use for 19 turns | 2,800 + 19 × 600 = **14,200** | **75%** |

### At scale (50+ tools behind the gateway)

The savings increase dramatically with more tools because the scoped cost is constant:

| Gateway Size | No Discovery (per turn) | Scoped to 3 tools (per turn) | Saving |
|-------------|-------------------------|------------------------------|--------|
| 10 tools | ~3,000 | ~600 | 80% |
| 50 tools | ~15,000 | ~600 | 96% |
| 200 tools | ~60,000 | ~600 | 99% |

## Summary

The flow:

1. **Browse** — `discover_tools()` returns the catalog (no scoping)
2. **Search** — `discover_tools(query="...")` finds and selects matching tools
3. **Wait one turn** — tool definitions update between turns via `notifications/tools/list_changed`
4. **Use** — scoped tools are available with reduced token cost on every subsequent turn
5. **Switch** — `discover_tools(reset=true, query="...")` clears and re-scopes in one call
6. **Wait one turn** — new tool definitions arrive
7. **Use** — new tools available, old tools removed from context
