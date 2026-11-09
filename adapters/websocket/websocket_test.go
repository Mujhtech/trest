package trestws

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"
	"trest.local/trest"
)

func TestWebSocketConfigurationAndOriginPolicy(t *testing.T) {
	handler := func(context.Context, *Conn) error { return nil }
	for _, tc := range []struct {
		name    string
		options Options
	}{
		{"implicit public", Options{Origins: []string{"https://app.example"}}},
		{"conflicting access", Options{Public: true, Permission: "read", Origins: []string{"https://app.example"}}},
		{"wildcard origin", Options{Public: true, Origins: []string{"*"}}},
		{"origin path", Options{Public: true, Origins: []string{"https://app.example/path"}}},
		{"negative timeout", Options{Public: true, AllowMissingOrigin: true, ReadTimeout: -time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New("/ws", tc.options, handler); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
	endpoint, err := newEndpoint(Options{Public: true, Origins: []string{"https://app.example"}}, handler)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, origin string
		want         bool
	}{{"allowed", "https://app.example", true}, {"suffix", "https://app.example.evil", false}, {"wrong scheme", "http://app.example", false}, {"opaque", "null", false}, {"absent", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://socket.example/ws", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if endpoint.originAllowed(r) != tc.want {
				t.Fatal("wrong origin decision")
			}
		})
	}
}

func TestWebSocketProtocolMetadata(t *testing.T) {
	component, err := New("/ws", Options{Name: "events", Public: true, Origins: []string{"https://app.example"}}, func(context.Context, *Conn) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	app, err := trest.New(component)
	if err != nil {
		t.Fatal(err)
	}
	routes := app.Routes()
	if len(routes) != 1 || routes[0].Protocol != "websocket" || !routes[0].NativeOnly {
		t.Fatalf("routes=%+v", routes)
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(document, []byte(`"x-trest-protocol": "websocket"`)) {
		t.Fatal("missing protocol metadata")
	}
	if len(app.Registry().Operations()) != 0 {
		t.Fatal("native websocket route leaked into JSON operations")
	}
}
