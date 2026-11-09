package trest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"trest.local/trest"
	"trest.local/trest/operation"
	"trest.local/trest/telemetry"
)

func request(t *testing.T, app http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func auth() trest.Component {
	return trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
		if r.Header.Get("Authorization") == "acme" {
			return trest.Principal{ID: "actor", TenantID: "acme"}, nil
		}
		return trest.Principal{}, nil
	})
}

func TestHelloAndDetachedContract(t *testing.T) {
	t.Parallel()
	app, err := trest.New(trest.Info("Example", "1"), trest.Get("/", trest.Const("Hello Trest")).Name("hello"), trest.Docs("/docs"))
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, app, "GET", "/", "", "")
	if w.Code != 200 || w.Body.String() != "Hello Trest" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("hello: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing request id")
	}
	routes := app.Routes()
	routes[0].Name = "changed"
	routes[0].OutputSchema[0] = 'x'
	if app.Routes()[0].Name != "hello" {
		t.Fatal("metadata alias")
	}
	a, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := app.OpenAPI()
	if string(a) != string(b) || !json.Valid(a) {
		t.Fatal("unstable contract")
	}
	if strings.Contains(string(a), "/docs") {
		t.Fatal("docs endpoints leaked into application client contract")
	}
	if w := request(t, app, "GET", "/docs", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Example") {
		t.Fatalf("docs: %d", w.Code)
	}
	if w := request(t, app, "GET", "/docs/openapi.json", "", ""); w.Body.String() != string(a) {
		t.Fatal("docs contract differs")
	}
}

func TestPublicSourceAndTracing(t *testing.T) {
	t.Parallel()
	recorder, err := telemetry.NewRecorder(50)
	if err != nil {
		t.Fatal(err)
	}
	app, err := trest.New(trest.Observe(recorder), trest.Concurrency(2), trest.Get("/", trest.Const("ok")).Name("hello"))
	if err != nil {
		t.Fatal(err)
	}
	info := app.Routes()[0]
	if info.Source.File != "app_test.go" || info.Source.Line == 0 {
		t.Fatalf("source: %+v", info.Source)
	}
	metadata, _ := app.Registry().Lookup(info.OperationID)
	if metadata.Source != info.Source {
		t.Fatal("invoker lost declaration source")
	}
	w := request(t, app, "GET", "/", "", "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	traces := recorder.Traces()
	if len(traces) == 0 {
		t.Fatal("observer not attached")
	}
	for _, trace := range traces {
		for _, event := range trace.Events {
			if event.CorrelationID != w.Header().Get("X-Request-ID") {
				t.Fatal("lost correlation")
			}
		}
	}
}

func TestTypedInputsAuthorizationAndSharedInvocation(t *testing.T) {
	t.Parallel()
	type query struct {
		ID    string `path:"id" json:"id"`
		Limit int    `query:"limit" default:"10" min:"1" max:"20" json:"limit"`
	}
	type result struct {
		ID     string `json:"id"`
		Limit  int    `json:"limit"`
		Tenant string `json:"tenant"`
	}
	var calls atomic.Int32
	app, err := trest.New(auth(), trest.Group("/api", trest.RequireAuth(), trest.Get("/items/{id}", func(ctx context.Context, in query) (result, error) {
		calls.Add(1)
		p, _ := trest.Identity(ctx)
		return result{in.ID, in.Limit, p.TenantID}, nil
	}).Name("getItem").Authorize(func(_ context.Context, p trest.Principal, in query) error {
		if in.ID != p.TenantID {
			return trest.NewError(trest.Forbidden, "wrong owner", nil)
		}
		return nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, token string
		status            int
	}{{"anonymous", "/api/items/acme", "", 401}, {"foreign", "/api/items/other", "acme", 403}, {"invalid", "/api/items/acme?limit=21", "acme", 400}, {"unknown", "/api/items/acme?tenant=other", "acme", 400}, {"valid", "/api/items/acme", "acme", 200}} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, app, "GET", tc.path, "", tc.token)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("denied handler calls: %d", calls.Load())
	}
	ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "actor", TenantID: "acme"})
	id := app.Routes()[0].OperationID
	if _, err = app.Registry().InvokeJSON(ctx, id, json.RawMessage(`{"id":"other","limit":1}`)); operation.CodeOf(err) != operation.Forbidden {
		t.Fatalf("JSON bypassed auth: %v", err)
	}
	if _, err = app.Registry().InvokeJSON(ctx, id, json.RawMessage(`{"id":"acme","limit":1}`)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("shared invocation failed")
	}
}

func TestConstraintsAndScopedComposition(t *testing.T) {
	t.Parallel()
	type body struct {
		Name     string `json:"name" minLength:"2" maxLength:"8"`
		Currency string `json:"currency" enum:"USD,NGN"`
		Amount   int    `json:"amount" min:"1" max:"100"`
	}
	var calls atomic.Int32
	protected := trest.Group("/private", auth(), trest.RequireAuth(), trest.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Scope", "private")
			next.ServeHTTP(w, r)
		})
	}), trest.Post("/items", func(_ context.Context, in body) (body, error) { calls.Add(1); return in, nil }).Name("createItem").Status(201))
	app, err := trest.New(protected, trest.Get("/public", trest.Const("ok")))
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"name":"Test","currency":"NGN","amount":10}`
	if w := request(t, app, "POST", "/private/items", valid, ""); w.Code != 401 {
		t.Fatalf("guard: %d", w.Code)
	}
	if w := request(t, app, "POST", "/private/items", strings.Replace(valid, "NGN", "BAD", 1), "acme"); w.Code != 400 {
		t.Fatalf("enum: %d %s", w.Code, w.Body)
	}
	if w := request(t, app, "POST", "/private/items", valid, "acme"); w.Code != 201 || w.Header().Get("X-Scope") != "private" {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	w := request(t, app, "GET", "/public", "", "")
	if w.Code != 200 || w.Header().Get("X-Scope") != "" {
		t.Fatal("scope leaked")
	}
	if calls.Load() != 1 {
		t.Fatal("invalid/unauthorized call executed")
	}
	doc, _ := app.OpenAPI()
	for _, want := range []string{`"minimum": 1`, `"maximum": 100`, `"minLength": 2`, `"enum"`, `"requestBody"`} {
		if !strings.Contains(string(doc), want) {
			t.Fatalf("missing constraint %s", want)
		}
	}
	// Reusing a group creates separate registries without modifying its settings.
	if _, err := trest.New(protected); err != nil {
		t.Fatal(err)
	}
}

func TestConstructionDiagnostics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		components []trest.Component
	}{{"duplicate name", []trest.Component{trest.Get("/a", trest.Const("a")).Name("same"), trest.Get("/b", trest.Const("b")).Name("same")}}, {"missing policy", []trest.Component{trest.Get("/a", trest.Const("a")).Permission("write")}}, {"ambiguous route", []trest.Component{trest.Get("/a", trest.Const("a")), trest.Get("/a", trest.Const("b"))}}, {"dependency", []trest.Component{trest.Plugin("users").Requires("auth")}}, {"nil", []trest.Component{nil}}} {
		t.Run(tc.name, func(t *testing.T) {
			if app, err := trest.New(tc.components...); err == nil || app != nil {
				t.Fatalf("invalid construction accepted: %v", err)
			}
		})
	}
	plugin := trest.Plugin("public", trest.Get("/plugin", trest.Const("ok")))
	app, err := trest.New(plugin, plugin)
	if err != nil || len(app.Routes()) != 1 {
		t.Fatalf("dedup: %v", err)
	}
	if _, err := trest.New(trest.Group("", plugin), trest.Group("", trest.RequireAuth(), plugin)); err == nil {
		t.Fatal("plugin dedup silently discarded a different security scope")
	}
	if _, err := trest.New(trest.Get("/", trest.Const("private")).Permission("")); err == nil {
		t.Fatal("empty permission became public")
	}
	if _, err := trest.New(trest.Get("/", trest.Const("private")).Authorize(nil)); err == nil {
		t.Fatal("nil typed authorizer became public")
	}
}

func TestErrorsResponsesAndNativeBoundary(t *testing.T) {
	t.Parallel()
	var events []trest.Event
	app, err := trest.New(trest.AfterResponse(func(e trest.Event) { events = append(events, e) }),
		trest.Get("/panic", func(context.Context, trest.Empty) (string, error) { panic("private-token") }),
		trest.Get("/error", func(context.Context, trest.Empty) (string, error) { return "", errors.New("private-token") }),
		trest.Get("/empty", trest.Const(trest.Empty{})).Status(204),
		trest.Get("/html", trest.Const(trest.HTML("<h1>Hello</h1>"))).Name("html").Produces("text/html"),
		trest.Get("/file", trest.Const(trest.File(fstest.MapFS{"hello.txt": &fstest.MapFile{Data: []byte("a file")}}, "hello.txt"))),
		trest.Get("/events", trest.Const(trest.SSE(func(_ context.Context, w *trest.EventWriter) error {
			return w.JSON("1", "created", map[string]int{"id": 1})
		}))).Name("events").Produces("text/event-stream"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/panic", "/error"} {
		w := request(t, app, "GET", path, "", "")
		if w.Code != 500 || strings.Contains(w.Body.String(), "private") {
			t.Fatalf("private error: %d %s", w.Code, w.Body)
		}
	}
	if w := request(t, app, "GET", "/empty", "", ""); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("empty: %d %s", w.Code, w.Body)
	}
	if w := request(t, app, "HEAD", "/html", "", ""); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD body")
	}
	if w := request(t, app, "GET", "/file", "", ""); w.Body.String() != "a file" {
		t.Fatal(w.Body)
	}
	if w := request(t, app, "GET", "/events", "", ""); !strings.Contains(w.Body.String(), "event: created\ndata: {\"id\":1}\n\n") {
		t.Fatalf("SSE %s", w.Body)
	}
	for _, route := range app.Routes() {
		if route.Name == "events" {
			if _, err := app.Registry().InvokeJSON(context.Background(), route.OperationID, json.RawMessage(`{}`)); operation.CodeOf(err) != operation.InvalidInput {
				t.Fatal("native operation accepted JSON")
			}
		}
	}
	if len(events) != 6 || events[0].Code != operation.Internal {
		t.Fatalf("events: %+v", events)
	}
	badHook, err := trest.New(trest.OnError(func(context.Context, error) error { panic("secret") }), trest.Get("/", func(context.Context, trest.Empty) (string, error) { return "", errors.New("failure") }))
	if err != nil {
		t.Fatal(err)
	}
	if w := request(t, badHook, "GET", "/", "", ""); w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("error hook panic escaped")
	}
}

type fakeListener struct {
	done chan struct{}
	once sync.Once
}

func (l *fakeListener) Accept() (net.Conn, error) { <-l.done; return nil, net.ErrClosed }
func (l *fakeListener) Close() error              { l.once.Do(func() { close(l.done) }); return nil }
func (l *fakeListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

func TestLifecycleRollbackAndShutdown(t *testing.T) {
	t.Parallel()
	var order []string
	app, err := trest.New(trest.Lifecycle(func(context.Context) error { order = append(order, "start1"); return nil }, func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Error("cleanup context already canceled")
		}
		order = append(order, "stop1")
		return nil
	}), trest.Lifecycle(func(context.Context) error { order = append(order, "start2"); return errors.New("failed") }, func(context.Context) error { order = append(order, "stop2"); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Run(context.Background(), &fakeListener{done: make(chan struct{})}); err == nil {
		t.Fatal("startup succeeded")
	}
	if fmt.Sprint(order) != "[start1 start2 stop1]" {
		t.Fatalf("rollback ownership: %v", order)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	stopped := make(chan struct{})
	app, err = trest.New(trest.Lifecycle(func(context.Context) error { close(started); return nil }, func(context.Context) error { close(stopped); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- app.Run(ctx, &fakeListener{done: make(chan struct{})}) }()
	<-started
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown hung")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("stop hook missing")
	}
}

func TestStreamingCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app, err := trest.New(trest.Get("/", trest.Const(trest.Stream("text/plain", func(ctx context.Context, w io.Writer) error {
		cancel()
		_, err := w.Write([]byte("must not write"))
		return err
	}))))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "must not write") {
		t.Fatal("canceled stream wrote data")
	}
}

func BenchmarkHello(b *testing.B) {
	app, err := trest.New(trest.Get("/", trest.Const("Hello")))
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		handler http.Handler
	}{{"trest", app}, {"net_http", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "Hello") })}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", "/", nil)
				tc.handler.ServeHTTP(w, r)
			}
		})
	}
}
