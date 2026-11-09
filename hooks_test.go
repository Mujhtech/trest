package trest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"trest.local/trest"
	"trest.local/trest/operation"
)

func TestResolvedResourceAndResultHooksShareEveryInvocationPath(t *testing.T) {
	t.Parallel()
	type input struct {
		ID string `path:"id" json:"id" minLength:"2"`
	}
	type resource struct{ ID, Owner string }
	type output struct {
		Value string `json:"value" minLength:"2"`
	}
	key := trest.NewKey[resource]()
	var order []string
	route := trest.Get("/{id}", func(ctx context.Context, in input) (output, error) {
		order = append(order, "handler")
		found, ok := trest.Value(ctx, key)
		if !ok || found.ID != in.ID {
			return output{}, errors.New("missing resolved resource")
		}
		return output{Value: found.ID}, nil
	}).Name("resolved").Validate(func(context.Context, input) error { order = append(order, "validate"); return nil }).Authorize(func(ctx context.Context, p trest.Principal, _ input) error {
		order = append(order, "authorize")
		found, ok := trest.Value(ctx, key)
		if !ok || found.Owner != p.TenantID {
			return trest.NewError(trest.Forbidden, "not yours", nil)
		}
		return nil
	}).After(func(_ context.Context, _ input, out output) (output, error) {
		order = append(order, "after")
		out.Value += "!"
		return out, nil
	})
	route = trest.Resolve(route, key, func(_ context.Context, in input) (resource, error) {
		order = append(order, "resolve")
		return resource{ID: in.ID, Owner: "acme"}, nil
	})
	app, err := trest.New(trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
		tenant := r.Header.Get("Authorization")
		if tenant == "" {
			return trest.Principal{}, nil
		}
		return trest.Principal{ID: "user", TenantID: tenant}, nil
	}), route)
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, app, "GET", "/ok", "", "acme")
	if w.Code != 200 || w.Body.String() != "{\"value\":\"ok!\"}" {
		t.Fatalf("HTTP: %d %s", w.Code, w.Body)
	}
	want := []string{"validate", "resolve", "authorize", "handler", "after"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order: %v", order)
	}
	operationID := app.Routes()[0].OperationID
	ctx := operation.WithPrincipal(context.Background(), trest.Principal{ID: "user", TenantID: "acme"})
	order = nil
	if _, err = app.Registry().PrepareJSON(ctx, operationID, json.RawMessage(`{"id":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, want[:3]) {
		t.Fatalf("preparation ran effects: %v", order)
	}
	order = nil
	result, err := app.Registry().InvokeJSON(ctx, operationID, json.RawMessage(`{"id":"ok"}`))
	if err != nil || string(result) != `{"value":"ok!"}` {
		t.Fatalf("JSON: %s %v", result, err)
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("JSON order: %v", order)
	}
	for _, tc := range []struct {
		name, path, tenant string
		status             int
		want               []string
	}{
		{"invalid", "/x", "acme", 400, nil},
		{"anonymous", "/ok", "", 401, []string{"validate"}},
		{"denied", "/ok", "other", 403, []string{"validate", "resolve", "authorize"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order = nil
			w := request(t, app, "GET", tc.path, "", tc.tenant)
			if w.Code != tc.status || !reflect.DeepEqual(order, tc.want) {
				t.Fatalf("status=%d order=%v", w.Code, order)
			}
		})
	}
}

func TestResultHookOutputIsValidated(t *testing.T) {
	t.Parallel()
	type result struct {
		Code string `json:"code" enum:"safe"`
	}
	app, err := trest.New(trest.Get("/", trest.Const(result{Code: "safe"})).After(func(_ context.Context, _ trest.Empty, out result) (result, error) {
		out.Code = "private"
		return out, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if w := request(t, app, "GET", "/", "", ""); w.Code != 500 || w.Body.String() == `{"code":"private"}` {
		t.Fatalf("unvalidated output: %d %s", w.Code, w.Body)
	}
}
