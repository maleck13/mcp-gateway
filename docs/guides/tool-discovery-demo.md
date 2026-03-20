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

This demo uses the `/demo-dinner-plan` Claude Code command to showcase tool discovery in a realistic multi-step scenario: booking a restaurant and notifying friends. The agent starts with all tools loaded, discovers the subset it needs, and uses only those — demonstrating how tool discovery reduces token usage while keeping the agent effective.

The test servers provide the tools needed for this flow:
- **test-server3** (dining category): `restaurants` and `book_restaurant`
- **test-server1** (utilities category): `friends` and `send_message`

### Running the demo

In Claude Code, run:

```
/demo-dinner-plan
```

The command is pre-configured with `allowed-tools: mcp__mcp-gateway__discover_tools, AskUserQuestion`, so the agent can only use `discover_tools` and ask questions — it cannot call any gateway tools directly until it discovers them.

### What happens

#### Step 1: List tools (before discovery)

The agent lists all mcp-gateway tools currently available. At this point it sees every tool from all three test servers (~12+ tools, ~2,800 tokens per turn).

#### Step 2: Discover relevant tools

The agent calls `discover_tools` with a query for restaurant listing, booking, contacts, and messaging tools. The broker's BM25 index matches tools from server1 and server3:

```json
{
  "action": "search",
  "tools_matched": 4,
  "message": "Found 4 tools matching your query. Your tool list will update on the next turn..."
}
```

The agent ends its turn (as instructed by the tool description). On the next turn, it lists its tools again — now only the 4 matched tools plus `discover_tools` are in context.

**What happened behind the scenes:**
- The broker ran a BM25 ranked search across all tool names and descriptions
- The top matches (`test3_restaurants`, `test3_book_restaurant`, `test1_friends`, `test1_send_message`) were stored as the session's tool selection
- A `notifications/tools/list_changed` notification was sent
- Claude's `tools/list` refreshed between turns to show only the scoped tools

#### Steps 3–8: Use the scoped tools

With only the relevant tools in context, the agent:

1. Lists restaurants in a city using `test3_restaurants`
2. Books a table using `test3_book_restaurant`
3. Gets the friends list using `test1_friends`
4. Sends notifications using `test1_send_message`

Each turn carries only ~800 tokens of tool definitions instead of ~2,800.

#### Step 9: Summary

The agent provides a summary of everything done — restaurant booked, friends notified, message sent.

## Token Savings

### Per-turn cost comparison

Without discovery, **every turn** of the conversation carries all tool definitions:

| Scenario | Tools in context | Tokens per turn | Saving |
|----------|-----------------|-----------------|--------|
| No discovery (all tools) | 12 | ~2,800 | — |
| After scoping (4 matches) | 5 | ~800 | 71% |
| After scoping (2 matches) | 3 | ~600 | 78% |

### Over the dinner plan conversation (~10 turns)

The scoping turn costs one extra user message, but every subsequent turn is cheaper:

| Scenario | Total tool tokens | Saving |
|----------|-------------------|--------|
| No discovery | 10 × 2,800 = **28,000** | — |
| Scope on turn 1, use for 9 turns | 2,800 + 9 × 800 = **10,000** | **64%** |

### At scale (50+ tools behind the gateway)

The savings increase dramatically with more tools because the scoped cost is constant:

| Gateway Size | No Discovery (per turn) | Scoped to 4 tools (per turn) | Saving |
|-------------|-------------------------|------------------------------|--------|
| 10 tools | ~3,000 | ~800 | 73% |
| 50 tools | ~15,000 | ~800 | 95% |
| 200 tools | ~60,000 | ~800 | 99% |

## Summary

The flow:

1. **Browse** — `discover_tools()` returns the catalog (no scoping)
2. **Search** — `discover_tools(query="...")` finds and selects matching tools
3. **Wait one turn** — tool definitions update between turns via `notifications/tools/list_changed`
4. **Use** — scoped tools are available with reduced token cost on every subsequent turn
5. **Switch** — `discover_tools(reset=true, query="...")` clears and re-scopes in one call
6. **Wait one turn** — new tool definitions arrive
7. **Use** — new tools available, old tools removed from context
