package dev_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"trest.local/trest/adapters/mcp/dev"
	"trest.local/trest/operation"
	"trest.local/trest/telemetry"
)

func TestReadOnlyDevelopmentServer(t *testing.T) {
	t.Parallel()
	r := operation.NewRegistry(nil)
	_, err := operation.Register(r, operation.Definition[struct{}, struct{}]{ID: "mutating.v1", Summary: "A mutation", Access: operation.Public(), SideEffect: "write"}, func(context.Context, struct{}) (struct{}, error) {
		t.Fatal("dev server executed an application operation")
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	recorder, err := telemetry.NewRecorder(10)
	if err != nil {
		t.Fatal(err)
	}
	ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "developer"})
	for _, config := range []dev.Config{{}, {Enabled: true, Environment: "production"}} {
		if _, err := dev.NewServer(ctx, r, nil, recorder, config); err == nil {
			t.Fatal("disabled or production dev server allowed")
		}
	}
	config := dev.Config{Enabled: true, Environment: "local"}
	if _, err := dev.NewServer(context.Background(), r, nil, recorder, config); err == nil {
		t.Fatal("unauthenticated developer access allowed")
	}
	server, err := dev.NewServer(ctx, r, nil, recorder, config)
	if err != nil {
		t.Fatal(err)
	}
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "development-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 3 {
		t.Fatalf("expected three metadata tools, got %d", len(listed.Tools))
	}
	for _, tool := range listed.Tools {
		if tool.Name != "trest_inspect" && tool.Name != "trest_check" && tool.Name != "trest_trace" {
			t.Fatalf("unexpected capability %s", tool.Name)
		}
	}
	for _, name := range []string{"trest_inspect", "trest_check"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("metadata tool failed: %+v", res)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &body); err != nil {
			t.Fatal(err)
		}
		if body["schema_version"] == nil {
			t.Fatal("missing versioned result")
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "trest_check", Arguments: map[string]any{"commands": []string{"go test ./..."}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("dev tool accepted execution configuration")
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "mutating.v1", Arguments: map[string]any{}}); err == nil {
		t.Fatal("application mutation callable on dev server")
	}
}
