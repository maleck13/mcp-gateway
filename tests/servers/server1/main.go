// A simple MCP server that implements a few tools
// - greet: say hi to someone
// - time: get the current time
// - slow: delay N seconds with progress notifications
// - headers: return all HTTP headers received
// - add_tool: dynamically add a new tool (triggers notifications/tools/list_changed)
// Plus a "greet" prompt and an embedded "info" resource.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9090"
	}

	hooks := &server.Hooks{}
	hooks.AddOnRegisterSession(func(_ context.Context, session server.ClientSession) {
		log.Printf("Client %s connected", session.SessionID())
	})
	hooks.AddOnUnregisterSession(func(_ context.Context, session server.ClientSession) {
		log.Printf("Client %s disconnected", session.SessionID())
	})
	hooks.AddBeforeAny(func(_ context.Context, _ any, method mcp.MCPMethod, _ any) {
		log.Printf("Processing %s request", method)
	})

	s := server.NewMCPServer(
		"test mcp server 1",
		"1.0.0",
		server.WithHooks(hooks),
		server.WithToolCapabilities(true),
		server.WithPromptCapabilities(true),
		server.WithResourceCapabilities(true, false),
	)

	s.AddTools(
		server.ServerTool{
			Tool: mcp.NewTool("greet",
				mcp.WithDescription("say hi"),
				mcp.WithString("name", mcp.Required(), mcp.Description("the name to say hi to")),
			),
			Handler: greetHandler,
		},
		server.ServerTool{
			Tool: mcp.NewTool("time",
				mcp.WithDescription("get current time"),
				mcp.WithTitleAnnotation("time"),
			),
			Handler: timeHandler,
		},
		server.ServerTool{
			Tool: mcp.NewTool("slow",
				mcp.WithDescription("delay N seconds"),
				mcp.WithNumber("seconds", mcp.Required(), mcp.Description("number of seconds to wait")),
			),
			Handler: slowHandler,
		},
		server.ServerTool{
			Tool: mcp.NewTool("headers",
				mcp.WithDescription("get headers"),
			),
			Handler: headersHandler,
		},
		server.ServerTool{
			Tool: mcp.NewTool("add_tool",
				mcp.WithDescription("dynamically add a new tool (triggers notifications/tools/list_changed)"),
				mcp.WithTitleAnnotation("add"),
				mcp.WithString("name", mcp.Required(), mcp.Description("the name of the new tool to add")),
				mcp.WithString("description", mcp.Description("the description of the new tool")),
			),
			Handler: addToolHandler(s),
		},
	)

	s.AddPrompts(server.ServerPrompt{
		Prompt: mcp.Prompt{Name: "greet"},
		Handler: func(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			name := req.Params.Arguments["name"]
			return &mcp.GetPromptResult{
				Description: "Code review prompt",
				Messages: []mcp.PromptMessage{
					{
						Role:    mcp.RoleUser,
						Content: mcp.NewTextContent("Say hi to " + name),
					},
				},
			}, nil
		},
	})

	s.AddResources(server.ServerResource{
		Resource: mcp.Resource{
			Name:     "info",
			MIMEType: "text/plain",
			URI:      "embedded:info",
		},
		Handler: func(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      req.Params.URI,
					MIMEType: "text/plain",
					Text:     "This is the hello example server.",
				},
			}, nil
		},
	})

	mux := http.NewServeMux()
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
	}

	streamableHTTPServer := server.NewStreamableHTTPServer(
		s,
		server.WithStreamableHTTPServer(httpServer),
	)
	mux.Handle("/mcp", streamableHTTPServer)

	go func() {
		fmt.Printf("Serving HTTPStreamable on http://localhost:%s/mcp\n", port)
		if err := streamableHTTPServer.Start(":" + port); err != nil {
			log.Fatalf("server error: %v", err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	log.Println("Shutting down server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := streamableHTTPServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}

func greetHandler(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("Hi " + name), nil
}

func timeHandler(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText(time.Now().String()), nil
}

func headersHandler(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	content := make([]mcp.Content, 0)
	for k, v := range req.Header {
		content = append(content, mcp.TextContent{
			Type: "text",
			Text: fmt.Sprintf("%s: %v", k, v),
		})
	}
	return &mcp.CallToolResult{Content: content}, nil
}

func slowHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	seconds, err := req.RequireInt("seconds")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var progressToken mcp.ProgressToken
	if req.Params.Meta != nil {
		progressToken = req.Params.Meta.ProgressToken
	}
	srv := server.ServerFromContext(ctx)

	startTime := time.Now()
	for {
		waited := int(time.Since(startTime).Seconds())
		if waited >= seconds {
			break
		}
		if progressToken != nil && srv != nil {
			msg := fmt.Sprintf("Waited %d seconds...", waited)
			_ = srv.SendNotificationToClient(ctx, "notifications/progress", map[string]any{
				"progress":      waited,
				"progressToken": progressToken,
				"message":       msg,
			})
		}
		time.Sleep(1 * time.Second)
	}
	return mcp.NewToolResultText("done"), nil
}

func addToolHandler(s *server.MCPServer) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, err := req.RequireString("name")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		desc := req.GetString("description", "dynamically added tool")

		s.AddTools(server.ServerTool{
			Tool: mcp.NewTool(name, mcp.WithDescription(desc)),
			Handler: func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("I am the dynamically added tool: " + name), nil
			},
		})

		return mcp.NewToolResultText(fmt.Sprintf("Added new tool: %s", name)), nil
	}
}
