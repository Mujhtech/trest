// Package trestmcp connects an authenticated Trest tool session to the official
// Model Context Protocol Go SDK. This is an optional application-tool adapter;
// it exposes no development inspection, shell, database, or approval endpoints.
package trestmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"trest.local/trest/aitool"
)

// Options makes runtime exposure explicit. Nothing listens automatically.
// Production requires both Enabled and AllowProduction plus environment="production".
type Options struct {
	Enabled         bool
	Environment     string
	AllowProduction bool
	Name            string
	Version         string
}

// NewServer binds exactly one trusted actor/session to an SDK server. The
// application must authenticate before creating the session and must not share
// this server with connections belonging to other actors. Prefer one server per
// authenticated connection. Raw MCP arguments and request metadata are untrusted.
// The caller owns transport authentication, origin checks, limits, and shutdown.
func NewServer(session *aitool.Session, opts Options) (server *mcp.Server, err error) {
	if !opts.Enabled {
		return nil, errors.New("trestmcp: application tools are disabled by default")
	}
	if session == nil {
		return nil, errors.New("trestmcp: authenticated tool session is required")
	}
	switch opts.Environment {
	case "local", "development", "test":
	case "production":
		if !opts.AllowProduction {
			return nil, errors.New("trestmcp: production exposure requires explicit opt-in")
		}
	default:
		return nil, errors.New("trestmcp: specify local, development, test, or production environment")
	}
	if opts.Name == "" {
		opts.Name = "trest-app-tools"
	}
	if opts.Version == "" {
		opts.Version = "0.1.0"
	}
	server = mcp.NewServer(&mcp.Implementation{Name: opts.Name, Version: opts.Version}, nil)
	defer func() {
		if recovered := recover(); recovered != nil {
			server = nil
			err = fmt.Errorf("trestmcp: invalid tool registration: %v", recovered)
		}
	}()
	for _, descriptor := range session.Tools() {
		var shape struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(descriptor.InputSchema, &shape); err != nil || shape.Type != "object" {
			return nil, fmt.Errorf("trestmcp: tool %q requires an object input schema", descriptor.Name)
		}
		name := descriptor.Name
		server.AddTool(&mcp.Tool{Name: name, Description: descriptor.Description, InputSchema: descriptor.InputSchema}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := request.Params.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			result := session.Call(ctx, name, args)
			data, err := json.Marshal(result)
			if err != nil {
				return nil, errors.New("trestmcp: cannot serialize tool outcome")
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, StructuredContent: result, IsError: result.Status == aitool.Rejected || result.Status == aitool.Uncertain}, nil
		})
	}
	return server, nil
}
