package trest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"trest.local/trest"
)

func TestTypedStatusResponsesMatchRuntimeAndContract(t *testing.T) {
	t.Parallel()
	type completed struct {
		ID string `json:"id" minLength:"2"`
	}
	type queued struct {
		Job string `json:"job" minLength:"2"`
	}
	type query struct {
		Queued bool `query:"queued" default:"false"`
	}
	app, err := trest.New(
		trest.Get("/work", func(_ context.Context, in query) (trest.Either[completed, queued], error) {
			if in.Queued {
				return trest.Second[completed, queued](queued{Job: "job1"}).WithHeader("Retry-After", "3"), nil
			}
			return trest.First[completed, queued](completed{ID: "ok"}), nil
		}).Name("work").Statuses(201, 202),
		trest.Get("/single", trest.Const(trest.JSON(201, completed{ID: "ok"}).WithHeader("X-Test", "yes"))).Name("single").Statuses(200, 201),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/work", 201, `{"id":"ok"}`}, {"/work?queued=true", 202, `{"job":"job1"}`}, {"/single", 201, `{"id":"ok"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := request(t, app, "GET", tc.path, "", "")
			if w.Code != tc.status || w.Body.String() != tc.body || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("response: %d %s", w.Code, w.Body)
			}
		})
	}
	if w := request(t, app, "GET", "/work?queued=true", "", ""); w.Header().Get("Retry-After") != "3" {
		t.Fatal("typed response header missing")
	}
	if w := request(t, app, "HEAD", "/single", "", ""); w.Code != 201 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %s", w.Code, w.Body)
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err = json.Unmarshal(document, &parsed); err != nil {
		t.Fatal(err)
	}
	for status, field := range map[string]string{"201": "id", "202": "job"} {
		if _, ok := parsed.Paths["/work"]["get"].Responses[status].Content["application/json"].Schema.Properties[field]; !ok {
			t.Fatalf("status %s lost body type: %s", status, document)
		}
	}
	for _, route := range app.Routes() {
		if !route.NativeOnly || len(route.Responses) != 2 {
			t.Fatalf("typed HTTP boundary: %+v", route)
		}
		if _, err := app.Registry().InvokeJSON(context.Background(), route.OperationID, []byte(`{}`)); err == nil {
			t.Fatal("HTTP status envelope accepted as JSON operation")
		}
	}
}

func TestInvalidTypedResponseNeverWritesBody(t *testing.T) {
	t.Parallel()
	type body struct {
		Value string `json:"value" enum:"safe"`
	}
	for _, tc := range []struct {
		name  string
		route trest.Component
	}{
		{"undeclared status", trest.Get("/", trest.Const(trest.JSON(202, body{Value: "safe"}))).Statuses(200, 201)},
		{"invalid body", trest.Get("/", trest.Const(trest.JSON(200, body{Value: "private"})))},
		{"unset branch", trest.Get("/", trest.Const(trest.Either[body, body]{})).Statuses(200, 201)},
		{"invalid header", trest.Get("/", trest.Const(trest.JSON(200, body{Value: "safe"}).WithHeader("Bad Header", "private")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, err := trest.New(tc.route)
			if err != nil {
				t.Fatal(err)
			}
			w := request(t, app, "GET", "/", "", "")
			if w.Code != 500 || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("invalid response exposed: %d %s", w.Code, w.Body)
			}
		})
	}
	for _, route := range []trest.Component{
		trest.Get("/", trest.Const("hello")).Statuses(200, 201),
		trest.Get("/", trest.Const(trest.First[body, body](body{Value: "safe"}))),
		trest.Get("/", trest.Const(trest.JSON(200, body{Value: "safe"}))).Statuses(200, 200),
		trest.Get("/", trest.Const(trest.JSON(200, trest.Text("private")))),
	} {
		if _, err := trest.New(route); err == nil {
			t.Fatal("invalid typed response declaration accepted")
		}
	}
	app, err := trest.New(trest.Get("/", trest.Const(trest.JSON(http.StatusNoContent, trest.Empty{}))).Status(http.StatusNoContent))
	if err != nil {
		t.Fatal(err)
	}
	if w := request(t, app, "GET", "/", "", ""); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("no content: %d %s", w.Code, w.Body)
	}
}
