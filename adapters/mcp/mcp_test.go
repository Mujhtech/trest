package trestmcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	trestmcp "trest.local/trest/adapters/mcp"
	"trest.local/trest/aitool"
	"trest.local/trest/operation"
)

type input struct {
	Name string `json:"name"`
}
type output struct {
	Greeting string `json:"greeting"`
	Tenant   string `json:"tenant"`
}

func TestSDKRoundTrip(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	r := operation.NewRegistry(operation.AuthorizerFunc(func(ctx context.Context, p operation.Principal, permission string, value any) error {
		if p.ID != "alice" || p.TenantID != "tenant-a" || value.(input).Name == "denied" {
			return errors.New("private policy reason")
		}
		return nil
	}))
	for _, id := range []string{"greet.v1", "hidden.v1", "approved.v1"} {
		_, err := operation.Register(r, operation.Definition[input, output]{ID: id, Summary: "Greet", Access: operation.Permission("greet"), SideEffect: "write"}, func(ctx context.Context, in input) (output, error) {
			calls.Add(1)
			p, _ := operation.PrincipalFromContext(ctx)
			return output{"hello " + in.Name, p.TenantID}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	catalog, err := aitool.New(r, []aitool.Tool{{Name: "greet", OperationID: "greet.v1"}, {Name: "hidden", OperationID: "hidden.v1"}, {Name: "approved", OperationID: "approved.v1", RequireApproval: true}}, aitool.NewMemoryStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "alice", TenantID: "tenant-a"})
	run, err := catalog.NewSession(ctx, []string{"greet", "approved"}, aitool.Limits{MaxCalls: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	if _, err := trestmcp.NewServer(run, trestmcp.Options{}); err == nil {
		t.Fatal("default enabled tools")
	}
	if _, err := trestmcp.NewServer(run, trestmcp.Options{Enabled: true, Environment: "production"}); err == nil {
		t.Fatal("production exposed by default")
	}
	server, err := trestmcp.NewServer(run, trestmcp.Options{Enabled: true, Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "trest-test", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()
	listed, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 2 {
		t.Fatalf("wrong exposure: %+v", listed.Tools)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "hidden" {
			t.Fatal("session allowlist bypassed")
		}
		encoded, _ := json.Marshal(tool.InputSchema)
		if strings.Contains(string(encoded), "tenant_id") || strings.Contains(string(encoded), "principal") {
			t.Fatal("security context exposed in tool schema")
		}
	}
	for _, tc := range []struct {
		name       string
		args       map[string]any
		wantError  bool
		wantStatus aitool.State
	}{
		{name: "greet", args: map[string]any{"name": "Ada"}, wantStatus: aitool.Succeeded},
		{name: "greet", args: map[string]any{"name": "denied"}, wantError: true, wantStatus: aitool.Rejected},
		{name: "greet", args: map[string]any{"name": "Ada", "tenant_id": "victim"}, wantError: true, wantStatus: aitool.Rejected},
		{name: "approved", args: map[string]any{"name": "Ada"}, wantStatus: aitool.Pending},
	} {
		result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError != tc.wantError {
			t.Fatalf("unexpected error flag: %+v", result)
		}
		var outcome aitool.Result
		if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &outcome); err != nil {
			t.Fatal(err)
		}
		if outcome.Status != tc.wantStatus {
			t.Fatalf("unexpected outcome: %+v", outcome)
		}
		if outcome.Status == aitool.Succeeded && !strings.Contains(string(outcome.Data), "tenant-a") {
			t.Fatalf("tenant lost: %s", outcome.Data)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("denied or pending request executed: %d calls", calls.Load())
	}
	if _, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "hidden", Arguments: map[string]any{"name": "Ada"}}); err == nil {
		t.Fatal("unlisted tool was callable")
	}
}
