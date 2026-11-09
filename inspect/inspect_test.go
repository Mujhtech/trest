package inspect_test

import (
	"bytes"
	"testing"

	"trest.local/trest/httpadapter"
	"trest.local/trest/inspect"
	"trest.local/trest/internal/example/invoiceapi"
	"trest.local/trest/operation"
)

func TestBuild(t *testing.T) {
	t.Parallel()
	app, err := invoiceapi.New(map[string]operation.Principal{"super-secret-token": {ID: "private-user", TenantID: "private-tenant"}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := inspect.Build(app.Registry(), app.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	first, err := inspect.JSON(model)
	if err != nil {
		t.Fatal(err)
	}
	secondModel, err := inspect.Build(app.Registry(), app.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	second, err := inspect.JSON(secondModel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("inspection is nondeterministic")
	}
	for _, secret := range []string{"super-secret-token", "private-user", "private-tenant", "/Users/", "/home/"} {
		if bytes.Contains(first, []byte(secret)) {
			t.Fatalf("inspection contains %q", secret)
		}
	}
	if len(model.Operations) != 1 || model.Operations[0].Source.Line <= 0 || model.Operations[0].Source.File == "" {
		t.Fatal("missing real registration source")
	}
	model.Operations[0].InputSchema[0] = '!'
	model.HTTP[0].Path = "/mutated"
	fresh, err := inspect.Build(app.Registry(), app.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Operations[0].InputSchema[0] == '!' || fresh.HTTP[0].Path == "/mutated" {
		t.Fatal("inspection aliases application metadata")
	}
}

func TestBuildRejectsInvalidApplications(t *testing.T) {
	t.Parallel()
	if _, err := inspect.Build(operation.NewRegistry(nil), nil); err == nil {
		t.Fatal("unfrozen registry accepted")
	}
	app, err := invoiceapi.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspect.Build(app.Registry(), []httpadapter.Binding{{OperationID: "missing", Method: "POST", Path: "/unknown"}}); err == nil {
		t.Fatal("unknown operation binding accepted")
	}
}
