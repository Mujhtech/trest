package contract_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"trest.local/trest/contract"
	"trest.local/trest/httpadapter"
	"trest.local/trest/operation"
)

type input struct {
	Amount int64   `json:"amount"`
	Memo   *string `json:"memo,omitempty"`
}

type output struct {
	ID string `json:"id"`
}

func registry(t *testing.T, reverse bool) (*operation.Registry, []httpadapter.Binding) {
	t.Helper()
	r := operation.NewRegistry(operation.AuthorizerFunc(func(context.Context, operation.Principal, string, any) error { return nil }))
	ids := []string{"invoice.create.v1", "invoice.update.v1"}
	if reverse {
		ids[0], ids[1] = ids[1], ids[0]
	}
	for _, id := range ids {
		if _, err := operation.Register(r, operation.Definition[input, output]{ID: id, Summary: "Write an invoice", Access: operation.Permission("invoices:write"), SideEffect: "write"}, func(context.Context, input) (output, error) { return output{ID: "one"}, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	bindings := []httpadapter.Binding{
		{OperationID: "invoice.create.v1", Method: "POST", Path: "/invoices", SuccessStatus: 201},
		{OperationID: "invoice.update.v1", Method: "PUT", Path: "/invoice", SuccessStatus: 200},
	}
	if reverse {
		bindings[0], bindings[1] = bindings[1], bindings[0]
	}
	return r, bindings
}

func TestExport(t *testing.T) {
	t.Parallel()
	r, bindings := registry(t, false)
	before := append([]httpadapter.Binding(nil), bindings...)
	first, err := contract.Export(r, bindings, contract.Info{Title: "Invoices", Version: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bindings, before) {
		t.Fatal("export mutated caller bindings")
	}
	r2, reordered := registry(t, true)
	second, err := contract.Export(r2, reordered, contract.Info{Title: "Invoices", Version: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("contract depends on registration/binding order")
	}
	var document map[string]any
	if err := json.Unmarshal(first, &document); err != nil {
		t.Fatal(err)
	}
	if document["openapi"] != "3.1.1" {
		t.Fatal("wrong openapi version")
	}
	paths := document["paths"].(map[string]any)
	post := paths["/invoices"].(map[string]any)["post"].(map[string]any)
	if post["operationId"] != "invoice.create.v1" {
		t.Fatal("operation identity changed")
	}
	if _, invented := document["security"]; invented {
		t.Fatal("export invented authentication scheme")
	}
	access := post["x-trest-access"].(map[string]any)
	if access["permission"] != "invoices:write" {
		t.Fatalf("wrong permission: %v", access)
	}
	responses := post["responses"].(map[string]any)
	for _, code := range []string{"201", "400", "401", "403", "413", "415", "429", "500", "504"} {
		if _, ok := responses[code]; !ok {
			t.Errorf("missing response %s", code)
		}
	}
	components := document["components"].(map[string]any)["schemas"].(map[string]any)
	inputSchema := components["invoice.create.v1.Input"].(map[string]any)
	if inputSchema["additionalProperties"] != false {
		t.Fatal("unknown fields not forbidden in contract")
	}
	if got := inputSchema["required"].([]any); len(got) != 1 || got[0] != "amount" {
		t.Fatalf("required=%v", got)
	}
	// A committed baseline makes accidental contract changes visible in review.
	golden := filepath.Join("testdata", "invoices.openapi.json")
	expected, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(expected) {
		t.Fatalf("contract differs from %s", golden)
	}
}

func TestExportRejectsInvalidBindings(t *testing.T) {
	t.Parallel()
	r, good := registry(t, false)
	tests := []struct {
		name     string
		bindings []httpadapter.Binding
	}{
		{"unknown operation", []httpadapter.Binding{{OperationID: "missing.v1", Method: "POST", Path: "/invoices", SuccessStatus: 200}}},
		{"duplicate operation", []httpadapter.Binding{good[0], good[0]}},
		{"duplicate route", []httpadapter.Binding{good[0], {OperationID: "invoice.update.v1", Method: "POST", Path: "/invoices", SuccessStatus: 200}}},
		{"bodyless nonempty input", []httpadapter.Binding{{OperationID: good[0].OperationID, Method: "GET", Path: "/invoices", SuccessStatus: 200}}},
		{"bad method", []httpadapter.Binding{{OperationID: good[0].OperationID, Method: "post", Path: "/invoices", SuccessStatus: 200}}},
		{"wildcard path", []httpadapter.Binding{{OperationID: good[0].OperationID, Method: "POST", Path: "/invoices/{id}", SuccessStatus: 200}}},
		{"escaped path", []httpadapter.Binding{{OperationID: good[0].OperationID, Method: "POST", Path: "/invoices%2Fone", SuccessStatus: 200}}},
		{"trailing slash", []httpadapter.Binding{{OperationID: good[0].OperationID, Method: "POST", Path: "/invoices/", SuccessStatus: 200}}},
		{"path cleanup", []httpadapter.Binding{{OperationID: good[0].OperationID, Method: "POST", Path: "/invoices/../one", SuccessStatus: 200}}},
		{"unicode path", []httpadapter.Binding{{OperationID: good[0].OperationID, Method: "POST", Path: "/café", SuccessStatus: 200}}},
		{"colon path", []httpadapter.Binding{{OperationID: good[0].OperationID, Method: "POST", Path: "/x:y", SuccessStatus: 200}}},
		{"unsupported status", []httpadapter.Binding{{OperationID: good[0].OperationID, Method: "POST", Path: "/invoices", SuccessStatus: 204}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := contract.Export(r, tt.bindings, contract.Info{Title: "Test", Version: "1"}); err == nil {
				t.Fatal("invalid binding accepted")
			}
		})
	}
}

func TestExportRequiresFrozenRegistryAndInfo(t *testing.T) {
	t.Parallel()
	if _, err := contract.Export(nil, nil, contract.Info{Title: "Test", Version: "1"}); err == nil {
		t.Fatal("nil registry accepted")
	}
	r := operation.NewRegistry(nil)
	if _, err := contract.Export(r, nil, contract.Info{Title: "Test", Version: "1"}); err == nil {
		t.Fatal("mutable registry accepted")
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.Export(r, nil, contract.Info{}); err == nil {
		t.Fatal("empty info accepted")
	}
	if _, err := contract.Export(r, nil, contract.Info{Title: "Test", Version: "1"}); err != nil {
		t.Fatal(err)
	}
}

func TestExportUsesActualBindings(t *testing.T) {
	t.Parallel()
	r := operation.NewRegistry(nil)
	h, err := operation.Register(r, operation.Definition[struct{}, output]{ID: "hello.v1", Summary: "Say hello", Access: operation.Public()}, func(context.Context, struct{}) (output, error) { return output{ID: "hello"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	a, err := httpadapter.New(http.NewServeMux(), r, httpadapter.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := httpadapter.Bind(a, h, httpadapter.Route{Method: "GET", Path: "/hello"}); err != nil {
		t.Fatal(err)
	}
	data, err := contract.Export(r, a.Bindings(), contract.Info{Title: "Hello", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"requestBody"`) {
		t.Fatal("get operation must not advertise a request body")
	}
	if !strings.Contains(string(data), `"200"`) {
		t.Fatal("default success response not exported")
	}
}
