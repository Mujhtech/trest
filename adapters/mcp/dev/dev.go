// Package dev exposes read-only development metadata through the official MCP
// SDK. It is deliberately separate from application tools and never executes
// scenarios, Go commands, shell commands, handlers, or database queries.
package dev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mujhtech/trest/check"
	"github.com/mujhtech/trest/httpadapter"
	"github.com/mujhtech/trest/inspect"
	"github.com/mujhtech/trest/operation"
	"github.com/mujhtech/trest/telemetry"
)

// Config is an explicit opt-in. Only the local environment is supported.
// MaxOutputBytes defaults to 1 MiB and must be at least 256 bytes.
type Config struct {
	Enabled        bool
	Environment    string
	MaxOutputBytes int
}

// NewServer constructs a local, read-only server for one already-authenticated
// developer. ctx must contain a trusted principal. The application authenticates
// the transport and binds it only to this developer; no listener is started.
// Metadata may reveal architecture and trace identities, so localhost alone is
// not authentication. Use a separate server and registry for application tools.
func NewServer(ctx context.Context, registry *operation.Registry, bindings []httpadapter.Binding, recorder *telemetry.Recorder, config Config) (*mcp.Server, error) {
	if !config.Enabled || config.Environment != "local" {
		return nil, errors.New("trestmcp/dev: explicitly enable the local environment; production is unsupported")
	}
	if ctx == nil {
		return nil, errors.New("trestmcp/dev: provide an authenticated developer context")
	}
	if _, ok := operation.PrincipalFromContext(ctx); !ok {
		return nil, errors.New("trestmcp/dev: developer authentication is required")
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = 1 << 20
	}
	if config.MaxOutputBytes < 256 {
		return nil, errors.New("trestmcp/dev: output limit must be at least 256")
	}
	model, err := inspect.Build(registry, bindings)
	if err != nil {
		return nil, err
	}
	bindings = append([]httpadapter.Binding(nil), bindings...)
	server := mcp.NewServer(&mcp.Implementation{Name: "trest-development-metadata", Version: "0.1.0"}, nil)
	empty := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	server.AddTool(&mcp.Tool{Name: "trest_inspect", Description: "Read the frozen local application model", InputSchema: empty}, func(callCtx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if !emptyArguments(request.Params.Arguments) {
			return toolError("invalid input"), nil
		}
		if ctx.Err() != nil {
			return toolError("developer session ended"), nil
		}
		return result(model, config.MaxOutputBytes), nil
	})
	server.AddTool(&mcp.Tool{Name: "trest_check", Description: "Validate local registry and contract metadata; runs no subprocesses", InputSchema: empty}, func(callCtx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if !emptyArguments(request.Params.Arguments) {
			return toolError("invalid input"), nil
		}
		if ctx.Err() != nil {
			return toolError("developer session ended"), nil
		}
		// Commands and Runner are intentionally unconfigurable here.
		report := check.Run(callCtx, registry, bindings, check.Config{})
		return result(report, config.MaxOutputBytes), nil
	})
	if recorder != nil {
		server.AddTool(&mcp.Tool{Name: "trest_trace", Description: "Read retained redacted events for one local trace", InputSchema: json.RawMessage(`{"type":"object","properties":{"trace_id":{"type":"string","maxLength":128}},"required":["trace_id"],"additionalProperties":false}`)}, func(callCtx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if ctx.Err() != nil {
				return toolError("developer session ended"), nil
			}
			var args struct {
				TraceID string `json:"trace_id"`
			}
			decoder := json.NewDecoder(bytes.NewReader(request.Params.Arguments))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&args) != nil || len(args.TraceID) == 0 || len(args.TraceID) > 128 || decoder.Decode(new(any)) != io.EOF {
				return toolError("invalid input"), nil
			}
			trace, ok := recorder.Trace(args.TraceID)
			if !ok {
				return toolError("trace not found"), nil
			}
			return result(trace, config.MaxOutputBytes), nil
		})
	}
	return server, nil
}
func emptyArguments(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(data, &args) != nil || args == nil {
		return false
	}
	return len(args) == 0
}
func result(value any, maxBytes int) *mcp.CallToolResult {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxBytes {
		return toolError("metadata exceeds output limit")
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}
}
func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: message}}, IsError: true}
}
