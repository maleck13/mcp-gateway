## Test Cases


### [Happy] Test registering multiple MCP servers with the gateway

- When a developer creates multiple MCPServerRegistration resources with their corresponding HTTPRoutes, the gateway should register all servers and make their tools available. Each server's tools should be prefixed with the server's toolPrefix to avoid naming conflicts. A tools/list request should return tools from all registered servers.


### [Happy] Test unregistering MCP servers from the gateway

- When an MCPServerRegistration resource is deleted from the cluster, the gateway should remove the server and its tools should no longer be available. A subsequent tools/list request should not include any tools with that server's prefix.


### [Happy] Test invoking tools through the gateway

- When a client calls a tool through the gateway using the prefixed tool name, the gateway should forward the request to the appropriate backend MCP server and return the result. The tool should execute successfully and return the expected response.


### [Happy] Test MCP server registration with credentials

- When an MCPServerRegistration resource references a credential secret, the gateway should use those credentials to authenticate with the backend MCP server. If the credentials are invalid, the server should not be registered and its tools should not be available. When the credentials are updated to valid values, the server should become registered and its tools should appear in the tools/list.


### [Happy] Test backend MCP session reuse

- When a client makes multiple tool calls to the same backend MCP server, the gateway should reuse the same backend session for efficiency. The backend session ID should remain consistent across multiple calls from the same client. When a client disconnects and reconnects, a new backend session should be created.


### [Happy] Test MCPVirtualServer behaves as expected when defined

- When a developer defines an MCPVirtualServer resource and specifies the value of the `X-Mcp-Virtualserver` header as the name in the format `namespace/name`, where the namespace and name come from the created MCPVirtualServer resource, they should only get the tools specified in the MCPVirtualServer resource when they do a tools/list request to the MCP Gateway host.


### [Happy] Test tools are filtered down based on x-authorized-tools header

- When the value of the `x-authorized-tools` header is set as a JWT signed by a trusted key to a set of tools, the MCP Gateway should respond with only tools in that list.


### [Happy] Test notifications are received when a notifications/tools/list_changed notification is sent

- When an MCPServerRegistration is registered with the MCP Gateway, a `notifications/tools/list_changed` should be sent to any clients connected to the MCP Gateway. This notification should work for a single connected client as well as multiple connected clients. They should all receive the same notification at least once. The clients should receive these notifications within one minute of the MCPServerRegistration having reached a ready state.

- When a registered backend MCP Server, emits a `notifications/tools/list_changed` a notification should be received by the connected clients. When the clients receive this notification they should get a changed tools/list. 

### [Happy] Test no two mcp-session-ids are the same

- When a client initializes with the gateway, the session id it receives should be unique. So if two clients connect at basically the same time, each of those clients should get a unique session id.

- If a client is closed and disconnects, if it connects to the gateway and initializes it should receive a new mcp-session-id

### [Happy] Test Hostname backendRef registers MCPServerRegistration

- When an HTTPRoute uses a Hostname backendRef (`kind: Hostname, group: networking.istio.io`) with a URLRewrite filter, and an MCPServerRegistration references that HTTPRoute, the controller should correctly handle the external endpoint configuration and the MCPServerRegistration should become ready. Tool discovery is not tested as it requires actual HTTPS connectivity to external services.

### [Full] Gracefully handle an MCP Server becoming unavailable

- When a backend MCP Server becomes unavailable, the gateway should no longer show its tools in the tools/list response and a notification should be sent to the client within one minute. When the MCP Server becomes available again, the tools/list should be updated to include the tools again. While unavailable any tools/call should result in a 503 response

### [Happy] MCP Server status

- When a backend MCPServerRegistration is added but the backend MCP is invalid because it doesn't meet the protocol version the status of the MCPServerRegistration resource should report the reason for the MCPSever being invalid

- When a backend MCPServerRegistration is added but the backend MCP is invalid because it has conflicting tools due to tool name overlap with another server that has been added, the status of the MCPServerRegistration resource should report the reason for the MCPSever being invalid

- When a backend MCPServerRegistration is added but the backend MCP is invalid because the broker cannot connect to the the backend MCP server, the MCPServerRegistration resource should report the reason for the MCPSever being invalid

### [Happy] Multiple MCP Servers without prefix

- When two servers with no prefix are used, the gateway sees and forwards both tools correctly.
- When two servers with no prefix conflict and one is then modified to have a specified prefix via the MCPServer resource, both tools should become available via the gateway and capable of being invoked

### [multi-gateway] Multiple Isolated MCP Gateways deployed to the same cluster admin

- As a platform having deployed multiple instances of the MCP Gateway using the MCPGatewayExtension resource, I should see that they become ready once I have created a valid referencegrant. Once the MCPGatewayExtension is valid, there should a unique deployment of the mcp gateway in the same namespace as the MCPGatewayExtension resources

### [multi-gateway] Multiple Isolated MCP Gateways deployed to the same cluster client
- As a client, when multiple isolated gateways are ready and available at different hostnames, I should be able to see a unique list of tools for each gateway based on the MCPServerRegistrations created by each team using the MCPGatewayExtension. Example I should see tools prefixed with team_a on one gateway and team_b on the second gateway

### [Happy] MCPGatewayExtension with sectionName targets specific listener

- When an MCPGatewayExtension is created with a valid `targetRef.sectionName` that matches a listener name on the Gateway, the extension should become Ready. The controller should read the listener port and hostname from the Gateway configuration. The EnvoyFilter should be created targeting the correct listener port, and the broker-router deployment should have the `--mcp-gateway-public-host` flag set based on the listener hostname.

### [Error] MCPGatewayExtension with invalid sectionName is rejected

- When an MCPGatewayExtension is created with a `targetRef.sectionName` that does not match any listener on the Gateway, the extension should be marked as Invalid with a status message containing "listener not found". No EnvoyFilter or broker-router deployment should be created.

### [Happy] Gateway listener status updated when MCPGatewayExtension becomes Ready

- When an MCPGatewayExtension becomes Ready, the Gateway's listener status should be updated with an MCPGatewayExtension condition. The condition should have type "MCPGatewayExtension", status "True", reason "Programmed", and a message indicating which extension and EnvoyFilter are using the listener.

### [Happy] Gateway listener status condition removed on MCPGatewayExtension deletion

- When an MCPGatewayExtension is deleted, the MCPGatewayExtension condition should be removed from the Gateway's listener status. The Gateway should no longer show the MCPGatewayExtension condition for that listener.

### [Happy] Wildcard listener hostname converted to mcp subdomain

- When a Gateway listener has a wildcard hostname like `*.example.com`, and an MCPGatewayExtension targets that listener without a public host annotation override, the broker-router deployment should have `--mcp-gateway-public-host=mcp.example.com`. The wildcard prefix should be replaced with "mcp".

### [Error] MCPGatewayExtension rejected when listener allowedRoutes does not permit namespace

- When an MCPGatewayExtension targets a listener that has `allowedRoutes.namespaces.from: Same` and the MCPGatewayExtension is in a different namespace than the Gateway, the extension should be marked as Invalid with a status message indicating the namespace is not allowed.

### [Error] Second MCPGatewayExtension in same namespace is rejected

- When a namespace already has one MCPGatewayExtension that is Ready, and a second MCPGatewayExtension is created in the same namespace, the second extension should be marked as Invalid with a status message indicating a conflict. Only one MCPGatewayExtension is allowed per namespace, and the oldest by creation timestamp wins.

### [multi-gateway] Multiple MCPGatewayExtensions on different listener ports

- When a Gateway has multiple listeners on different ports (e.g., 8080 and 8081), MCPGatewayExtensions in different namespaces can each target a different listener using `targetRef.sectionName`. Both extensions should become Ready, each with their own EnvoyFilter targeting their respective port. The Gateway should have MCPGatewayExtension conditions on both listeners.
