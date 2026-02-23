Use the test cases file as context: $ARGUMENTS

If no file path was provided above, default to: tests/e2e/test_cases.md

## Context

- MCP Protocol spec: https://modelcontextprotocol.io/specification/2025-11-25
- Test MCP servers under tests/servers provide tools that can be called during e2e tests
- Helper code goes in tests/e2e/commons.go

## Workflow

1. **Read test_cases.md** and compare against existing tests in tests/e2e/happy_path_test.go
2. **Identify missing test cases** - skip cases already covered
3. **Look for cases that can be extended without creating entirely new test cases**
3. **Check for untagged cases** - warn if a test case lacks a tag like [Happy]
4. **Add new tests** following patterns below
5. **Run with FIt()** to verify the new test passes
6. **Remove FIt()** before completing - change back to `It()` so all tests run in CI

## Test Organization

- `[Happy]` tagged cases → tests/e2e/happy_path_test.go
- Other tags (e.g., `[Error]`, `[Edge]`) → create new file named after the tag (e.g., error_test.go)
- Prefix test descriptions with the tag: `It("[Happy] should do something", func() { ... })`

## Test Patterns

### Resource cleanup (required)
```go
var testResources = []client.Object{}

// In test:
registration := NewMCPServerResourcesWithDefaults("test-name", k8sClient)
testResources = append(testResources, registration.GetObjects()...)
registeredServer := registration.Register(ctx)

// AfterEach handles cleanup via testResources slice
```

### Waiting for conditions
```go
Eventually(func(g Gomega) {
    g.Expect(VerifyMCPServerRegistrationReady(ctx, k8sClient, name, namespace)).To(BeNil())
}, TestTimeoutLong, TestRetryInterval).To(Succeed())
```

### Do NOT use
- `defer` for cleanup - use BeforeEach/AfterEach instead
- Modify existing tests without prompting for permission

## Running Tests Locally

```bash
# Run specific test with focus
cd tests/e2e && go test -v -tags=e2e -run TestE2E -ginkgo.focus="test description" -timeout 10m

# Or use ginkgo CLI
ginkgo run -v --tags=e2e --focus="test description" tests/e2e/
```

## Available Test Servers

- **server1** (mcp-test-server1): greet, time, slow, headers, add_tool
- **server2** (mcp-test-server2): hello_world, time, headers, auth1234, slow, set_time
- **server3** (mcp-test-server3): time, add, dozen, pi, get_weather, slow
- **api-key-server**: requires Bearer token authentication
- **broken-server**: intentionally broken for error testing
- **everything-server**: prompts, tools, resources, sampling
