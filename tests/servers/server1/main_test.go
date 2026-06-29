package main

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestGreetHandler(t *testing.T) {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": "Fred"}
	res, err := greetHandler(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	require.Equal(t, "Hi Fred", tc.Text)
}

func TestTimeHandler(t *testing.T) {
	res, err := timeHandler(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)
}

func TestHeadersHandler(t *testing.T) {
	res, err := headersHandler(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 0)
}

func TestSlowHandler(t *testing.T) {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"seconds": float64(0)}
	res, err := slowHandler(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
}
