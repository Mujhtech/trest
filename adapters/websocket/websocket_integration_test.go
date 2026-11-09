//go:build integration

package trestws

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/coder/websocket"
	"trest.local/trest"
	"trest.local/trest/operation"
)

func testComponents(endpoint *endpoint) []trest.Component {
	route := trest.Raw("GET", "/ws", http.HandlerFunc(endpoint.serve)).WebSocket().Permission("socket:connect")
	return []trest.Component{
		trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
			if r.Header.Get("Authorization") == "Bearer fixture" {
				return trest.Principal{ID: "alice", TenantID: "acme"}, nil
			}
			return trest.Principal{}, nil
		}),
		trest.Authorization(operation.AuthorizerFunc(func(_ context.Context, p operation.Principal, permission string, _ any) error {
			if p.ID != "alice" || p.TenantID != "acme" || permission != "socket:connect" {
				return operation.NewError(operation.Forbidden, "denied", nil)
			}
			return nil
		})),
		trest.Lifecycle(endpoint.start, endpoint.stop), route,
	}
}
func testServer(t *testing.T, options Options, handler Handler) (*endpoint, string) {
	t.Helper()
	endpoint, err := newEndpoint(options, handler)
	if err != nil {
		t.Fatal(err)
	}
	app, err := trest.New(testComponents(endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := endpoint.stop(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		server.Close()
	})
	return endpoint, "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
}
func dial(t *testing.T, url string) (*sdk.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	client, response, err := sdk.Dial(ctx, url, &sdk.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://app.example"}, "Authorization": []string{"Bearer fixture"}}})
	if err != nil {
		if response != nil {
			t.Fatalf("dial: status=%d %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	return client, ctx
}
func TestWebSocketEchoAndAuthorization(t *testing.T) {
	var calls atomic.Int32
	_, url := testServer(t, Options{Permission: "socket:connect", Origins: []string{"https://app.example"}}, func(ctx context.Context, conn *Conn) error {
		calls.Add(1)
		p, ok := trest.Identity(ctx)
		if !ok || p.ID != "alice" || p.TenantID != "acme" {
			return errors.New("lost principal")
		}
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				return err
			}
			if err = conn.Write(ctx, kind, data); err != nil {
				return err
			}
		}
	})
	for _, tc := range []struct {
		name, origin, auth string
		status             int
	}{{"missing auth", "https://app.example", "", 401}, {"wrong origin", "https://evil.example", "Bearer fixture", 403}, {"missing origin", "", "Bearer fixture", 403}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			header := http.Header{}
			if tc.origin != "" {
				header.Set("Origin", tc.origin)
			}
			if tc.auth != "" {
				header.Set("Authorization", tc.auth)
			}
			conn, response, err := sdk.Dial(ctx, url, &sdk.DialOptions{HTTPHeader: header})
			if conn != nil {
				_ = conn.CloseNow()
			}
			if err == nil || response == nil || response.StatusCode != tc.status {
				t.Fatalf("response=%v err=%v", response, err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("denied request entered websocket handler")
	}
	client, ctx := dial(t, url)
	if err := client.Write(ctx, sdk.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	kind, data, err := client.Read(ctx)
	if err != nil || kind != sdk.MessageText || string(data) != "hello" {
		t.Fatalf("%v %q %v", kind, data, err)
	}
}
func TestWebSocketMessageLimitAndCapacity(t *testing.T) {
	entered := make(chan struct{}, 1)
	_, url := testServer(t, Options{Permission: "socket:connect", Origins: []string{"https://app.example"}, MaxMessageBytes: 8, MaxConnections: 1}, func(ctx context.Context, c *Conn) error { entered <- struct{}{}; _, _, err := c.Read(ctx); return err })
	client, ctx := dial(t, url)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler did not enter")
	}
	second, response, err := sdk.Dial(ctx, url, &sdk.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://app.example"}, "Authorization": []string{"Bearer fixture"}}})
	if second != nil {
		_ = second.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != 503 {
		t.Fatalf("capacity response=%v err=%v", response, err)
	}
	if err = client.Write(ctx, sdk.MessageText, []byte("too large")); err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Read(ctx)
	if err == nil {
		t.Fatal("oversized message accepted")
	}
}
func TestWebSocketReadDeadline(t *testing.T) {
	failed := make(chan error, 1)
	_, url := testServer(t, Options{Permission: "socket:connect", Origins: []string{"https://app.example"}, ReadTimeout: 20 * time.Millisecond}, func(ctx context.Context, c *Conn) error { _, _, err := c.Read(ctx); failed <- err; return err })
	_, ctx := dial(t, url)
	select {
	case err := <-failed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("read timeout not enforced")
	}
}
func TestWebSocketLifecycleCancellation(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	endpoint, err := newEndpoint(Options{Permission: "socket:connect", Origins: []string{"https://app.example"}}, func(ctx context.Context, c *Conn) error {
		close(entered)
		defer close(exited)
		_, _, err := c.Read(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := trest.New(testComponents(endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- app.Run(ctx, listener) }()
	client, clientCtx := dial(t, "ws://"+listener.Addr().String()+"/ws")
	select {
	case <-entered:
	case <-clientCtx.Done():
		t.Fatal("no handler")
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not drain websocket")
	}
	select {
	case <-exited:
	default:
		t.Fatal("handler still running")
	}
	if _, _, err = client.Read(clientCtx); err == nil {
		t.Fatal("socket remained open")
	}
}
