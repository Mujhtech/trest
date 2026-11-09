//go:build integration

package trestws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/coder/websocket"
)

func nativeDial(t *testing.T, url string, options ClientOptions) *JSONTransport {
	t.Helper()
	options.HTTPHeader = clientHeaders()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, response, err := DialJSONTransport(ctx, url, options)
	if err != nil {
		if response != nil {
			t.Fatalf("native dial status=%d: %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestNativeJSONTransportRoundTripAndWriteLimit(t *testing.T) {
	url, _, _ := jsonServer(t, Options{}, func(ctx context.Context, c *JSONConn[echoMessage, echoMessage]) error {
		for {
			message, err := c.Read(ctx)
			if err != nil {
				return err
			}
			if err := c.Write(ctx, message); err != nil {
				return err
			}
		}
	})
	conn := nativeDial(t, url, ClientOptions{MaxMessageBytes: 128})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, []byte(strings.Repeat("x", 129))); err == nil {
		t.Fatal("oversized write accepted")
	}
	if err := conn.Write(ctx, []byte{0xff}); err == nil {
		t.Fatal("invalid UTF8 write accepted")
	}
	wire := []byte(`{"text":"hello","count":1}`)
	if err := conn.Write(ctx, wire); err != nil {
		t.Fatal(err)
	}
	data, err := conn.Read(ctx)
	if err != nil || string(data) != string(wire) {
		t.Fatalf("native roundtrip=%q %v", data, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal("Close is not idempotent:", err)
	}
}

func TestNativeJSONTransportReadBoundsAndDeadline(t *testing.T) {
	t.Run("read limit", func(t *testing.T) {
		type output struct {
			Data string `json:"data"`
		}
		url, _, _ := jsonServer(t, Options{}, func(ctx context.Context, c *JSONConn[struct{}, output]) error {
			if err := c.Write(ctx, output{Data: strings.Repeat("x", 256)}); err != nil {
				return err
			}
			_, err := c.Read(ctx)
			return err
		})
		conn := nativeDial(t, url, ClientOptions{MaxMessageBytes: 64})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if data, err := conn.Read(ctx); err == nil || len(data) != 0 {
			t.Fatalf("oversized read returned data=%q error=%v", data, err)
		}
	})
	t.Run("read deadline", func(t *testing.T) {
		url, _, _ := jsonServer(t, Options{}, func(ctx context.Context, c *JSONConn[echoMessage, echoMessage]) error {
			_, err := c.Read(ctx)
			return err
		})
		conn := nativeDial(t, url, ClientOptions{ReadTimeout: 20 * time.Millisecond})
		if _, err := conn.Read(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("read deadline=%v", err)
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		url, _, _ := jsonServer(t, Options{}, func(ctx context.Context, c *JSONConn[echoMessage, echoMessage]) error {
			_, err := c.Read(ctx)
			return err
		})
		conn := nativeDial(t, url, ClientOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := conn.Read(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("read cancellation=%v", err)
		}
	})
}

func TestNativeJSONTransportRejectsBinary(t *testing.T) {
	_, url := testServer(t, Options{Permission: "socket:connect", Origins: []string{"https://app.example"}}, func(ctx context.Context, c *Conn) error {
		if err := c.Write(ctx, Binary, []byte(`{"text":"hello","count":1}`)); err != nil {
			return err
		}
		_, _, err := c.Read(ctx)
		return err
	})
	conn := nativeDial(t, url, ClientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if data, err := conn.Read(ctx); err == nil || len(data) != 0 {
		t.Fatalf("binary accepted: %q %v", data, err)
	}
}

func TestNativeJSONTransportHandshakeDeadline(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	conn, _, err := DialJSONTransport(context.Background(), server.URL, ClientOptions{DialTimeout: 20 * time.Millisecond})
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handshake deadline=%v", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("handshake did not reach local fixture")
	}
}

func TestNativeJSONTransportPreservesHandshakeFailures(t *testing.T) {
	url, _, _ := jsonServer(t, Options{}, func(context.Context, *JSONConn[struct{}, struct{}]) error { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, response, err := DialJSONTransport(ctx, url, ClientOptions{HTTPHeader: http.Header{"Origin": []string{"https://app.example"}}})
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("handshake failure=%v %v", response, err)
	}
	if sdk.CloseStatus(err) == sdk.StatusNormalClosure {
		t.Fatal("HTTP auth rejection lost")
	}
}
