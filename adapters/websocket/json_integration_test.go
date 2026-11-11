//go:build integration

package trestws

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/coder/websocket"
	"github.com/mujhtech/trest"
)

func jsonServer[I, O any](t *testing.T, options Options, handler func(context.Context, *JSONConn[I, O]) error) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	options.Permission = "socket:connect"
	options.Origins = []string{"https://app.example"}
	component, err := NewJSON[I, O]("/ws", options, handler)
	if err != nil {
		t.Fatal(err)
	}
	app, err := trest.New(
		trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
			if r.Header.Get("Authorization") == "Bearer fixture" {
				return trest.Principal{ID: "alice", TenantID: "acme"}, nil
			}
			return trest.Principal{}, nil
		}),
		trest.Authorization(trest.AuthorizerFunc(func(_ context.Context, p trest.Principal, permission string, _ any) error {
			if p.ID != "alice" || permission != "socket:connect" {
				return trest.NewError(trest.Forbidden, "denied", nil)
			}
			return nil
		})),
		component,
	)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- app.Run(ctx, listener); close(finished) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("typed WebSocket shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("typed WebSocket app did not drain")
		}
	})
	return "ws://" + listener.Addr().String() + "/ws", cancel, finished
}

func clientHeaders() http.Header {
	return http.Header{"Origin": []string{"https://app.example"}, "Authorization": []string{"Bearer fixture"}}
}

func TestJSONSocketEchoAndHandshakeAuthorization(t *testing.T) {
	var entered, processed atomic.Int32
	url, _, _ := jsonServer(t, Options{}, func(ctx context.Context, c *JSONConn[echoMessage, echoMessage]) error {
		entered.Add(1)
		identity, ok := trest.Identity(ctx)
		if !ok || identity.ID != "alice" || identity.TenantID != "acme" {
			return errors.New("principal lost across typed socket")
		}
		for {
			message, err := c.Read(ctx)
			if err != nil {
				return err
			}
			processed.Add(1)
			if err := c.Write(ctx, message); err != nil {
				return err
			}
		}
	})
	for _, tc := range []struct {
		name, origin, auth string
		status             int
	}{
		{"unauthenticated", "https://app.example", "", 401},
		{"wrong origin", "https://evil.example", "Bearer fixture", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, response, err := sdk.Dial(ctx, url, &sdk.DialOptions{HTTPHeader: http.Header{"Origin": []string{tc.origin}, "Authorization": []string{tc.auth}}})
			if conn != nil {
				_ = conn.CloseNow()
			}
			if err == nil || response == nil || response.StatusCode != tc.status {
				t.Fatalf("response=%v err=%v", response, err)
			}
		})
	}
	if entered.Load() != 0 {
		t.Fatal("denied handshake entered typed handler")
	}
	conn, ctx := dial(t, url)
	wire := []byte(`{"text":"hello","count":2,"next":{"text":"leaf","count":1}}`)
	if err := conn.Write(ctx, sdk.MessageText, wire); err != nil {
		t.Fatal(err)
	}
	kind, data, err := conn.Read(ctx)
	if err != nil || kind != sdk.MessageText {
		t.Fatalf("typed echo: %v %v", kind, err)
	}
	var got echoMessage
	if err := json.Unmarshal(data, &got); err != nil || got.Text != "hello" || got.Next == nil || got.Next.Text != "leaf" || processed.Load() != 1 {
		t.Fatalf("echo=%s processed=%d error=%v", data, processed.Load(), err)
	}
}

func TestJSONSocketRejectsBeforeDeliveringMessages(t *testing.T) {
	type result struct {
		err    error
		closed bool
	}
	results := make(chan result, 1)
	var delivered atomic.Int32
	url, _, _ := jsonServer(t, Options{}, func(ctx context.Context, c *JSONConn[echoMessage, echoMessage]) error {
		_, err := c.Read(ctx)
		if err == nil {
			delivered.Add(1)
			results <- result{}
			return nil
		}
		writeErr := c.Write(ctx, echoMessage{Text: "ok", Count: 1})
		results <- result{err: err, closed: writeErr != nil}
		return err
	})
	for _, tc := range []struct {
		name, body string
		kind       sdk.MessageType
	}{
		{"malformed", `{"text":`, sdk.MessageText},
		{"unknown", `{"text":"hello","count":1,"secret":true}`, sdk.MessageText},
		{"duplicate", `{"text":"hello","text":"other","count":1}`, sdk.MessageText},
		{"missing", `{"text":"hello"}`, sdk.MessageText},
		{"null", `{"text":null,"count":1}`, sdk.MessageText},
		{"constraint", `{"text":"hello","count":0}`, sdk.MessageText},
		{"nested required", `{"text":"hello","count":1,"next":{"text":"leaf"}}`, sdk.MessageText},
		{"binary", `{"text":"hello","count":1}`, sdk.MessageBinary},
		{"invalid UTF8", "{\"text\":\"\xff\",\"count\":1}", sdk.MessageText},
		{"deep recursion", strings.Repeat(`{"text":"ok","count":1,"next":`, 130) + `null` + strings.Repeat("}", 130), sdk.MessageText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, ctx := dial(t, url)
			if err := conn.Write(ctx, tc.kind, []byte(tc.body)); err != nil {
				t.Fatal(err)
			}
			if _, data, err := conn.Read(ctx); err == nil || len(data) != 0 {
				t.Fatalf("invalid message received response: %q %v", data, err)
			}
			select {
			case result := <-results:
				var invalid *MessageError
				if !errors.As(result.err, &invalid) || invalid.Direction != "input" || !result.closed {
					t.Fatalf("message delivered or socket remained open: %+v", result)
				}
			case <-ctx.Done():
				t.Fatal("typed reader did not reject")
			}
		})
	}
	if delivered.Load() != 0 {
		t.Fatalf("invalid messages delivered: %d", delivered.Load())
	}
}

func TestJSONSocketRejectsOutputBeforeWriting(t *testing.T) {
	type output struct {
		Text  string `json:"text"`
		Count int    `json:"count" min:"1"`
	}
	for _, tc := range []struct {
		name  string
		value output
	}{
		{"contract", output{Text: "private output", Count: 0}},
		{"size", output{Text: strings.Repeat("x", 256), Count: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := make(chan error, 1)
			url, _, _ := jsonServer(t, Options{MaxMessageBytes: 64}, func(ctx context.Context, c *JSONConn[struct{}, output]) error {
				err := c.Write(ctx, tc.value)
				failed <- err
				return err
			})
			conn, ctx := dial(t, url)
			if _, data, err := conn.Read(ctx); err == nil || len(data) != 0 {
				t.Fatalf("invalid output transmitted: %q %v", data, err)
			}
			select {
			case err := <-failed:
				var invalid *MessageError
				if !errors.As(err, &invalid) || invalid.Direction != "output" || strings.Contains(err.Error(), "private") {
					t.Fatalf("wrong output rejection: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("output was not rejected")
			}
		})
	}
}

func TestJSONSocketIncomingLimitAndLifecycle(t *testing.T) {
	t.Run("incoming limit", func(t *testing.T) {
		failed := make(chan error, 1)
		url, _, _ := jsonServer(t, Options{MaxMessageBytes: 32}, func(ctx context.Context, c *JSONConn[echoMessage, echoMessage]) error {
			_, err := c.Read(ctx)
			failed <- err
			return err
		})
		conn, ctx := dial(t, url)
		if err := conn.Write(ctx, sdk.MessageText, []byte(`{"text":"`+strings.Repeat("x", 64)+`","count":1}`)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := conn.Read(ctx); err == nil {
			t.Fatal("oversized input accepted")
		}
		select {
		case err := <-failed:
			if err == nil {
				t.Fatal("reader accepted oversized message")
			}
		case <-ctx.Done():
			t.Fatal("reader remained blocked")
		}
	})
	t.Run("app cancellation", func(t *testing.T) {
		entered, exited := make(chan struct{}), make(chan struct{})
		url, cancel, finished := jsonServer(t, Options{}, func(ctx context.Context, c *JSONConn[echoMessage, echoMessage]) error {
			close(entered)
			defer close(exited)
			_, err := c.Read(ctx)
			return err
		})
		conn, ctx := dial(t, url)
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("handler not entered")
		}
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("typed socket blocked shutdown")
		}
		select {
		case <-exited:
		default:
			t.Fatal("typed reader outlived app shutdown")
		}
		if _, _, err := conn.Read(ctx); err == nil {
			t.Fatal("socket survived app shutdown")
		}
	})
}

type secretCodec string

func (v secretCodec) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }
func (v *secretCodec) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if text == "panic" {
		panic("private codec panic")
	}
	return errors.New("private codec error")
}
func (secretCodec) TrestSchema() json.RawMessage { return json.RawMessage(`{"type":"string"}`) }

func TestJSONSocketRedactsCustomCodecFailures(t *testing.T) {
	failed := make(chan error, 1)
	url, _, _ := jsonServer(t, Options{}, func(ctx context.Context, c *JSONConn[secretCodec, string]) error {
		_, err := c.Read(ctx)
		failed <- err
		return err
	})
	for _, value := range []string{"error", "panic"} {
		t.Run(value, func(t *testing.T) {
			conn, ctx := dial(t, url)
			data, _ := json.Marshal(value)
			if err := conn.Write(ctx, sdk.MessageText, data); err != nil {
				t.Fatal(err)
			}
			if _, data, err := conn.Read(ctx); err == nil || len(data) != 0 {
				t.Fatalf("codec details transmitted: %q %v", data, err)
			}
			select {
			case err := <-failed:
				var invalid *MessageError
				if !errors.As(err, &invalid) || strings.Contains(err.Error(), "private") {
					t.Fatalf("codec error leaked: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("reader did not finish")
			}
		})
	}
}
