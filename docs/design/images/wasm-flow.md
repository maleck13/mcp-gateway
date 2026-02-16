# Wasm Router Request Flow

## Request Processing Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              CLIENT REQUEST                                  │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         OnHttpRequestHeaders                                 │
│  • Extract mcp-session-id header                                            │
│  • Parse tool mappings from JWT payload                                     │
│  • Return ActionPause (wait for body)                                       │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                          OnHttpRequestBody                                   │
│  • Parse JSON-RPC MCP request (method, tool name, id)                       │
│  • Route based on method                                                    │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                    ┌─────────────────┼─────────────────┐
                    │                 │                 │
                    ▼                 ▼                 ▼
            ┌───────────┐     ┌─────────────┐    ┌───────────┐
            │tools/call │     │ initialize  │    │  other    │
            └───────────┘     │ initialized │    │ methods   │
                    │         └─────────────┘    └───────────┘
                    │                 │                 │
                    ▼                 │                 │
         ┌──────────────────┐         │                 │
         │  handleToolCall  │         └────────┬────────┘
         └──────────────────┘                  │
                    │                          ▼
                    │              ┌─────────────────────┐
                    │              │ handleBrokerRequest │
                    │              └─────────────────────┘
                    │                          │
                    ▼                          │
    ┌───────────────────────────┐              │
    │ resolveToolToServer       │              │
    │ • Check JWT tool mappings │              │
    │ • Match by prefix         │              │
    └───────────────────────────┘              │
                    │                          │
                    ▼                          │
    ┌───────────────────────────┐              │
    │ getBackendSession         │              │
    │ • Check shared data cache │              │
    │ • Validate expiration     │              │
    └───────────────────────────┘              │
                    │                          │
         ┌──────────┴──────────┐               │
         │                     │               │
    [has session]        [no session]          │
         │                     │               │
         │                     ▼               │
         │     ┌────────────────────────┐      │
         │     │    triggerLazyInit     │      │
         │     │ • Store pending state  │      │
         │     │ • DispatchHttpCall     │      │
         │     │   (hairpin to gateway) │      │
         │     │ • Return ActionPause   │      │
         │     └────────────────────────┘      │
         │                     │               │
         │                     ▼               │
         │     ┌────────────────────────┐      │
         │     │    onInitCallback      │      │
         │     │ • Extract backend      │      │
         │     │   session from headers │      │
         │     │ • setBackendSession    │      │
         │     │   (with JWT expiry)    │      │
         │     │ • resumePendingToolCall│      │
         │     └────────────────────────┘      │
         │                     │               │
         └──────────┬──────────┘               │
                    │                          │
                    ▼                          │
    ┌───────────────────────────┐              │
    │ routeToolCallToBackend    │              │
    │ • Set :authority header   │              │
    │ • Set :path header        │              │
    │ • Set mcp-session-id      │              │
    │ • Set x-mcp-* headers     │              │
    │ • Rewrite tool name       │              │
    │   (strip prefix)          │              │
    │ • Return ActionContinue   │              │
    └───────────────────────────┘              │
                    │                          │
                    │         ┌────────────────┘
                    │         │
                    │         ▼
                    │  ┌──────────────────────┐
                    │  │ Check hairpin header │
                    │  │ (mcp-init-host)      │
                    │  └──────────────────────┘
                    │         │
                    │    ┌────┴────┐
                    │    │         │
                    │ [hairpin] [normal]
                    │    │         │
                    │    ▼         ▼
                    │  ┌─────┐  ┌─────────────────┐
                    │  │route│  │ Route to broker │
                    │  │to   │  │ • Set :authority│
                    │  │back-│  │ • Set :path     │
                    │  │end  │  │ • Set headers   │
                    │  └─────┘  └─────────────────┘
                    │    │         │
                    └────┴────┬────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           ENVOY ROUTES TO UPSTREAM                           │
└─────────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                        OnHttpResponseHeaders                                 │
│  • Capture upstream mcp-session-id                                          │
│  • Check for 404 → deleteBackendSession (reactive cleanup)                  │
└─────────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                              CLIENT RESPONSE                                 │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Key Data Structures

### Shared Data (Session Cache)

```
┌─────────────────────────────────────────────────────┐
│ Key: mcp:session:{gatewaySessionID}:{serverID}      │
│ Value: {expiresAt}:{backendSessionID}               │
└─────────────────────────────────────────────────────┘
```

- Sessions are stored with expiration time from JWT `exp` claim
- On access, expiration is checked and expired sessions are deleted
- 404 responses from backends also trigger session deletion (reactive cleanup)

## Hairpin Flow (Lazy Init)

When a tool call arrives for a backend server that hasn't been initialized:

```
Client ──► Gateway/Wasm ──► DispatchHttpCall ──► Gateway ──► Backend MCP
                                                    │
                                                    ▼
Client ◄── Gateway/Wasm ◄── onInitCallback ◄────────┘
                │
                └──► ResumeHttpRequest ──► Backend MCP ──► Client
```

1. **triggerLazyInit**: Pauses the original request, dispatches async HTTP call
2. **Hairpin request**: Goes through gateway with `mcp-init-host` and `router-key` headers
3. **Backend MCP**: Responds with `mcp-session-id` header
4. **onInitCallback**: Stores session, resumes original request with backend session ID

## Session Cleanup

Two mechanisms ensure sessions are cleaned up:

1. **Proactive**: Check JWT expiry on each `getBackendSession()` call
2. **Reactive**: Delete session when backend returns 404 (session not found)
