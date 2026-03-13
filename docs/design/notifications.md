## Notifications

### Implementation Status

| Feature | Status | Notes |
|---------|--------|-------|
| `notifications/tools/list_changed` (upstream detection) | Implemented | MCPManager detects and re-fetches tools |
| `notifications/tools/list_changed` (client forwarding) | Implemented | Handled by mcp-go library; E2E tested |
| Progress updates (streamed in tool call POST) | Implemented | Handled by mcp-go library; covered by `tools-call-with-progress` conformance test |
| Elicitation request/response routing | Not implemented | Requires request ID mapping infrastructure |
| `notifications/resources/list_changed` | Not applicable | Gateway does not federate resources |
| `notifications/prompts/list_changed` | Not applicable | Gateway does not federate prompts |
| `notifications/roots/list_changed` | Not applicable | Gateway does not federate roots |

### Problem

The MCP protocol supports real-time notifications that enable servers to inform clients about changes without being explicitly requested. These notifications are crucial for maintaining synchronization between clients and servers when:

- Tool lists change (e.g., `notifications/tools/list_changed`)
- Resource lists change (e.g., `notifications/resources/list_changed`) 
- Prompt lists change (e.g., `notifications/prompts/list_changed`)
- Root lists change (e.g., `notifications/roots/list_changed`)
- Long-running tool calls emit progress updates
- Elicitation requests are made (e.g., `elicitation/create`) - This is particularly important for prompting users before destructive actions, and for out-of-band authentication URL elicitation (see [MCP GitHub issue #1036](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1036))

For more details on MCP notifications, see the [MCP Architecture documentation](https://modelcontextprotocol.io/docs/learn/architecture#notifications).

In the MCP Gateway architecture, clients connect to the gateway's `/mcp` endpoint, which is backed by the MCP Broker. The broker aggregates multiple backend MCP servers and presents them as a unified MCP server to clients. This aggregation creates a challenge: how should notifications from individual backend MCP servers be forwarded to the appropriate clients?

The gateway uses a **lazy initialization** approach where backend sessions to MCP servers are only established when a client makes a tool call. However, some notifications (like `list_changed` notifications) are logically 'broadcast' notifications and should be sent to all connected clients, not just those who have made tool calls.

### Non-Goals

At this time, the MCP Gateway does **not** support elicitation requests that require out-of-band communication over the GET `/mcp` notification channel. Only state change events that are logically 'broadcast' (safe to send to all connected clients) are supported over the GET notification channel.

This is a technical limitation due to the complexity of implementing a fan-out approach where the broker would need to maintain separate GET connections to each backend MCP server for each client, particularly when those connections require the client's authentication credentials. The challenges include:

- Managing per-client, per-server GET connections with client-specific authentication
- Connection lifecycle management and reconnection logic for multiple fan-out connections
- Resource overhead of maintaining many concurrent connections

Progress updates are streamed as events within tool call POST responses by the mcp-go library, which naturally aligns with the client's authentication context and tool call lifecycle. Elicitation support is planned but not yet implemented.

### Solution

The MCP Gateway supports two distinct types of notification mechanisms:

1. **State Change Events**: Notifications that are safe to send to all connected clients (e.g., `notifications/tools/list_changed`, `notifications/resources/list_changed`, `notifications/prompts/list_changed`, `notifications/roots/list_changed`). These are received via persistent Server-Sent Events (SSE) connections the broker maintains to all backend MCP servers using the broker's configured authentication credentials.

2. **Client-Specific Events**: Events related to specific client sessions that are streamed as part of tool call POST responses:
   - Progress updates for long-running tool calls
   - Elicitation requests that require client interaction

The broker maintains a Server-Sent Events (SSE) connection with each connected client for receiving state change events. When the broker receives a state change event from a backend MCP server, it forwards it to all currently connected clients via their respective GET connections.

> Note: For detailed information on MCP notification specifications, see the [MCP Prompts specification](https://modelcontextprotocol.io/specification/2025-06-18/server/prompts#list-changed-notification) and the [MCP Architecture documentation](https://modelcontextprotocol.io/docs/learn/architecture#notifications).

### Notification Architecture

#### State Change Events

> **Implementation Note**: The `notifications/tools/list_changed` event is fully implemented. The MCPManager detects this notification from upstream servers and re-fetches the tool list. The mcp-go library handles forwarding state change notifications to all connected clients via their GET SSE connections. Other state change events (`resources/list_changed`, `prompts/list_changed`, `roots/list_changed`) are not applicable as the gateway currently only federates tools.

State change events are notifications that are safe and appropriate to send to all connected clients. The gateway supports the following state change events:

- `notifications/tools/list_changed` - When a backend MCP server's tool list changes
- `notifications/resources/list_changed` - When a backend MCP server's resource list changes  
- `notifications/prompts/list_changed` - When a backend MCP server's prompt list changes
- `notifications/roots/list_changed` - When a backend MCP server's root list changes

**How State Change Events Work:**

```mermaid
sequenceDiagram
  participant Client1 as MCP Client 1
  participant Client2 as MCP Client 2
  participant Gateway as Gateway/MCP Router/MCP Broker
  participant Server1 as MCP Server 1
  participant Server2 as MCP Server 2

  Note over Gateway, Server2: State change event channel establishment
  Gateway ->> Server1: POST /mcp "initialize" (broker auth)
  Server1 -->> Gateway: initialize response (capabilities)
  Gateway ->> Server1: POST /mcp "notifications/initialized"
  Gateway ->> Server1: GET /mcp
  Server1 -->> Gateway: SSE connection established
  Gateway ->> Server2: POST /mcp "initialize" (broker auth)
  Server2 -->> Gateway: initialize response (capabilities)
  Gateway ->> Server2: POST /mcp "notifications/initialized"
  Gateway ->> Server2: GET /mcp
  Server2 -->> Gateway: SSE connection established
  Note over Client1, Gateway: Client notifications connection
  Client1 ->> Gateway: POST /mcp "initialize"
  Client1 ->> Gateway: POST /mcp "notifications/initialized"
  Client1 ->> Gateway: GET /mcp
  Gateway -->> Client1: SSE connection established
  Client2 ->> Gateway: POST /mcp "initialize"
  Client2 ->> Gateway: POST /mcp "notifications/initialized"
  Client2 ->> Gateway: GET /mcp
  Gateway -->> Client2: SSE connection established
  Note over Server2, Client1: State change events
  Server1 -->> Gateway: notifications/tools/list_changed
  Gateway -->> Client1: forward notification
  Gateway -->> Client2: forward notification
  Server2 -->> Gateway: notifications/prompts/list_changed
  Gateway -->> Client1: forward notification
  Gateway -->> Client2: forward notification
```

1. **Capability Checking**: When a backend MCP server is discovered, the broker first sends an `initialize` request using the broker's configured authentication credentials. The broker checks the `initialize` response to determine which state change event [capabilities](https://modelcontextprotocol.io/specification/2025-06-18/server/resources) the server supports (e.g., `notifications/tools/list_changed`, `notifications/resources/list_changed`, etc.).

2. **Persistent Broker Connections**: For each backend MCP server that supports state change events, the broker establishes a persistent GET connection:
   - Sends a `notifications/initialized` notification
   - Establishes a GET `/mcp` connection for receiving SSE notifications
   
   These connections remain open for the lifetime of the backend server connection. The broker must implement reconnection logic to handle cases where connections are dropped due to server restarts, session invalidation, or network issues.

3. **Event Reception and Forwarding**: When a backend MCP server sends a state change event (e.g., `notifications/tools/list_changed`), the broker receives it via the persistent connection and forwards it to all currently connected clients via their respective GET connections.

4. **Client Response**: Clients typically respond to `list_changed` notifications by making a new `tools/list`, `resources/list`, `prompts/list`, or `roots/list` request to refresh their understanding of available primitives.

**Why Broker Auth for State Change Events:**

The broker uses its own authentication credentials (configured at startup) rather than client credentials because state change events are not tied to any specific client session, must be received even when no clients have made tool calls yet, and allows the broker to maintain a single persistent connection per backend server rather than per client.

#### Client-Specific Events

> **Implementation Note**: Progress updates work without special gateway implementation — the mcp-go library streams progress events as part of the tool call POST response. Elicitation support is not yet implemented and requires the request ID mapping infrastructure described below.

Client-specific events are related to a particular client's tool execution and are delivered as streamed events within the tool call POST response, not via separate GET notification channels. The gateway supports two types of client-specific events:

1. **Progress Updates**: Progress notifications for long-running tool calls
2. **Elicitations**: Requests for user input during tool execution (e.g., confirming destructive actions)

> **Note**: The gateway does not currently support other client-specific notifications/events such as:
> - Log message notifications (`logging/setLevel` and `notifications/message`) - See [MCP Logging specification](https://modelcontextprotocol.io/specification/2025-06-18/server/utilities/logging#log-message-notifications)
> - Subscribe requests (`resources/subscribe` and `notifications/resources/updated`) - See [MCP SubscribeRequest schema](https://modelcontextprotocol.io/specification/2025-06-18/schema#subscriberequest)

**How Progress Updates Work:**

> **Implementation Note**: This is handled transparently by the mcp-go library. The gateway forwards the tool call POST request to the backend, and the library streams progress events back to the client as part of the same HTTP response. No special gateway logic is required.

Progress updates are streamed events sent by the backend MCP server as part of the `tools/call` POST response. The client indicates they want progress updates by including a `progressToken` field in the tool call request with an arbitrary value. The backend server uses this token to associate progress events with the specific tool call. See the [MCP Progress specification](https://modelcontextprotocol.io/specification/2025-06-18/basic/utilities/progress#progress) for more details.

```mermaid
sequenceDiagram
  participant Client as MCP Client
  participant Gateway as Gateway/MCP Router/MCP Broker
  participant Server as MCP Server

  Note over Client, Server: Long-running tool call with progress
  Client ->> Gateway: POST /mcp "tools/call" (with progressToken)
  Gateway ->> Server: POST /mcp "tools/call" (with progressToken)
  Note over Server, Client: Streamed progress events
  Server -->> Gateway: Progress event (0.2/1.0)
  Gateway -->> Client: forward progress event
  Server -->> Gateway: Progress event (0.6/1.0)
  Gateway -->> Client: forward progress event
  Server -->> Gateway: Progress event (1.0/1.0)
  Gateway -->> Client: forward progress event
  Note over Server, Client: Tool call completion
  Server -->> Gateway: tool/call response
  Gateway -->> Client: tool/call response
```

**How Elicitations Work:**

> **Implementation Note**: Elicitation support is **not yet implemented**. The design below describes the planned approach.

Elicitations allow backend MCP servers to request user input during tool execution. When an `elicitation/create` event is sent, the tool call on the server halts, waiting for the client's response. The elicitation message contains a unique request ID that the client must use when responding. See the [MCP Elicitation specification](https://modelcontextprotocol.io/specification/2025-06-18/client/elicitation#elicitation) for more details.

The gateway must intercept and modify the request ID in the elicitation message to enable proper routing of the client's follow-up response to the correct backend MCP server.

```mermaid
sequenceDiagram
  participant Client as MCP Client
  participant Gateway as Gateway/MCP Router/MCP Broker
  participant Server as MCP Server

  Note over Client, Server: Tool call with elicitation
  Client ->> Gateway: POST /mcp "tools/call"
  Gateway ->> Server: POST /mcp "tools/call"
  Note over Server, Gateway: Elicitation event
  Server -->> Gateway: Streamed event: elicitation/create (id: 42)
  Gateway ->> Gateway: Map server request ID (42) to gateway request ID (gateway-123)
  Gateway -->> Client: Streamed event: elicitation/create (id: gateway-123)
  Note over Client: User provides input
  Client ->> Gateway: POST /mcp elicitation response (id: gateway-123)
  Gateway ->> Gateway: Map gateway request ID (gateway-123) to server request ID (42)
  Gateway ->> Server: POST /mcp elicitation response (id: 42)
  Note over Server, Gateway: Tool call continues/resumes
  Server -->> Gateway: tool/call response
  Gateway -->> Client: tool/call response
```

**Example Elicitation Message:**

```json
{
  "method": "elicitation/create",
  "params": {
    "message": "Please provide inputs for the following fields:",
    "requestedSchema": {
      "type": "object",
      "properties": {
        "name": {
          "title": "Full Name",
          "type": "string",
          "description": "Your full, legal name"
        },
        "check": {
          "title": "Agree to terms",
          "type": "boolean",
          "description": "A boolean check"
        }
      },
      "required": ["name"]
    }
  },
  "jsonrpc": "2.0",
  "id": 1
}
```

**Example Client Response:**

```json
{
  "result": {
    "action": "accept"
  },
  "jsonrpc": "2.0",
  "id": 1
}
```

**Request ID Mapping for Elicitations:**

Since elicitation responses arrive as new POST requests from the client, the gateway must maintain a mapping that associates:
- The gateway-assigned request ID (sent to the client)
- The original backend server request ID
- The backend server session (for routing)

When forwarding an elicitation to the client, the gateway replaces the backend server's request ID with a gateway-specific ID. When the client responds, the gateway uses the mapping to restore the original request ID and route the response to the correct backend server session.

### Implementation Considerations

1. **Connection Management**: The broker must efficiently manage multiple concurrent connections:
   - One persistent GET connection per backend MCP server (for state change events)
   - One GET connection per connected client (for receiving state change events)
   - Long-running POST connections for tool calls that emit progress updates or elicitations

2. **Capability Detection**: The broker must check the `initialize` response from each backend MCP server to determine which state change event capabilities are supported before establishing GET notification connections.

3. **Request ID Mapping**: The broker must maintain a mapping table for elicitation request IDs that includes:
   - Gateway-assigned request ID
   - Original backend server request ID
   - Backend server session information
   - Expiration/TTL to clean up stale mappings
   
   **Request ID Type Handling**: Backend MCP servers may use request IDs of different types (string, integer, or float). The gateway implementation must:
   - Preserve the original backend server request ID type when restoring it in the client's response
   - From the gateway's perspective, gateway-assigned request IDs could use strings with a pattern (e.g., prefixing the server name) to ensure uniqueness and enable routing, though specific implementation details are left to implementation time
   - Test scenarios with string, integer, and float request IDs from backend servers to ensure proper type preservation and routing

4. **Error Handling and Reconnection**: The broker must handle:
   - Backend MCP server connection failures
   - Client connection failures
   - Event delivery failures
   - Connection retry logic with exponential backoff
   - Automatic reconnection when backend servers restart
   - Session invalidation detection and reconnection
   - Network interruption recovery

5. **Lifecycle Management**: The broker must properly:
   - Establish state change event connections when backend servers are discovered (after capability checking)
   - Clean up connections when backend servers are removed
   - Manage long-running POST connections for tool calls with progress/elicitation
   - Clean up request ID mappings when tool calls complete or timeout

### Security Considerations

1. **Event Validation**: The broker should validate that state change events received from backend MCP servers are well-formed and safe to forward before broadcasting them to clients.

2. **Authorization**: The broker forwards events as-is but relies on the backend MCP server's authorization rules, if any, tied to the broker auth credentials configured for each MCP Server.

3. **Request ID Mapping Security**: The gateway-assigned request IDs in elicitation mappings should be cryptographically random and unguessable to prevent unauthorized access to tool call sessions.

### Known Issues

#### Tool Discovery Notification Timing

When the broker's `discover_tools` handler sends a `notifications/tools/list_changed` notification from within a tool call handler, mcp-go upgrades the POST response to an SSE stream and delivers the notification event **before** the tool call result event. This creates a race condition:

1. Client receives `tools/list_changed` notification on the SSE stream
2. Client triggers a `tools/list` re-fetch (a new HTTP round-trip)
3. Client receives the `discover_tools` tool call result ("Found N tools" with tool names)
4. Client generates a response referencing the discovered tools — but the `tools/list` re-fetch from step 2 hasn't completed yet, so the agent's tool definitions are stale

The result: the agent sees "Found 2 time tools" but reports it cannot call them because their definitions aren't loaded.

**Why the first search works**: On the first `discover_tools` call, the agent already has all tool definitions from the initial `tools/list` at session start. The discovered tools are already callable. After scoping, subsequent `tools/list` calls return only the subset. When a second `discover_tools` call finds *different* tools, those tools aren't in the agent's current definitions.

**Mitigation**: The `discover_tools` response deliberately omits tool names and instead returns only the match count and a message instructing the agent to wait for its tool definitions to update via the `notifications/tools/list_changed` notification. Since the agent has no tool names to act on prematurely, it cannot attempt to call tools it doesn't yet have definitions for. The notification triggers a `tools/list` re-fetch, and the agent's tool definitions are updated for the next turn.

**Alternative fix (not implemented)**: Intercept the `discover_tools` tool call response in the router's response handler (ext_proc). When the router sees a `tools/call` response for `discover_tools` that performed a search or reset, it can send the `tools/list_changed` notification after the response has been flushed to the client. This would avoid the SSE upgrade race entirely and would allow tool names to be included in the response.

### Open Questions

1. **Elicitation Mapping Cleanup**: What is the appropriate timeout/TTL for elicitation request ID mappings? Should mappings persist for the entire tool call duration, or should there be a shorter timeout for elicitation responses?
