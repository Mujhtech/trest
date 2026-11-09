package trestws

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	sdk "github.com/coder/websocket"
)

// ClientOptions configures a native Go JSON text transport. HTTPHeader supplies
// explicit authentication and Origin headers. HTTPClient can configure TLS and
// proxy behavior. Zero limits default to 64 KiB messages, a 10s handshake,
// 30s reads, and 10s writes.
type ClientOptions struct {
	HTTPClient      *http.Client
	HTTPHeader      http.Header
	MaxMessageBytes int64
	DialTimeout     time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
}

// JSONTransport implements the structural Read/Write/Close transport consumed by
// generated Go clients. It bounds text frames without importing generated code.
// Use one reader; concurrent writes are supported. The caller owns Close.
type JSONTransport struct {
	socket                    *sdk.Conn
	maxBytes                  int64
	readTimeout, writeTimeout time.Duration
}

// DialJSONTransport performs a WebSocket handshake. ctx bounds the handshake;
// subsequent I/O uses the context passed to Read or Write. No background message
// queue, reconnect loop, or automatic retry is started.
func DialJSONTransport(ctx context.Context, url string, options ClientOptions) (*JSONTransport, *http.Response, error) {
	if ctx == nil {
		return nil, nil, errors.New("websocket: dial context required")
	}
	if options.MaxMessageBytes == 0 {
		options.MaxMessageBytes = 64 << 10
	}
	if options.ReadTimeout == 0 {
		options.ReadTimeout = 30 * time.Second
	}
	if options.DialTimeout == 0 {
		options.DialTimeout = 10 * time.Second
	}
	if options.WriteTimeout == 0 {
		options.WriteTimeout = 10 * time.Second
	}
	if options.MaxMessageBytes < 1 || options.MaxMessageBytes > 64<<20 || options.DialTimeout < 0 || options.ReadTimeout < 0 || options.WriteTimeout < 0 {
		return nil, nil, errors.New("websocket: invalid client limits")
	}
	bounded, cancel := context.WithTimeout(ctx, options.DialTimeout)
	defer cancel()
	socket, response, err := sdk.Dial(bounded, url, &sdk.DialOptions{
		HTTPClient:      options.HTTPClient,
		HTTPHeader:      options.HTTPHeader.Clone(),
		CompressionMode: sdk.CompressionDisabled,
	})
	if err != nil {
		return nil, response, err
	}
	socket.SetReadLimit(options.MaxMessageBytes)
	return &JSONTransport{socket: socket, maxBytes: options.MaxMessageBytes, readTimeout: options.ReadTimeout, writeTimeout: options.WriteTimeout}, response, nil
}

// Read returns one bounded text frame. Binary/non-UTF-8 frames close the socket.
func (c *JSONTransport) Read(ctx context.Context) ([]byte, error) {
	if c == nil || c.socket == nil || ctx == nil {
		return nil, errors.New("websocket: transport and context required")
	}
	bounded, cancel := context.WithTimeout(ctx, c.readTimeout)
	defer cancel()
	kind, data, err := c.socket.Read(bounded)
	if err != nil {
		return nil, err
	}
	if kind != sdk.MessageText || !utf8.Valid(data) {
		_ = c.Close()
		return nil, errors.New("websocket: JSON transport requires UTF-8 text messages")
	}
	return data, nil
}

// Write sends one text frame with synchronous backpressure and a deadline.
// Oversized or invalid UTF-8 payloads are rejected before any frame is sent.
func (c *JSONTransport) Write(ctx context.Context, data []byte) error {
	if c == nil || c.socket == nil || ctx == nil {
		return errors.New("websocket: transport and context required")
	}
	if int64(len(data)) > c.maxBytes || !utf8.Valid(data) {
		return errors.New("websocket: outgoing JSON frame exceeds limits or is not UTF-8")
	}
	bounded, cancel := context.WithTimeout(ctx, c.writeTimeout)
	defer cancel()
	return c.socket.Write(bounded, sdk.MessageText, data)
}

// Close immediately closes the connection and unblocks pending reads/writes.
func (c *JSONTransport) Close() error {
	if c == nil || c.socket == nil {
		return nil
	}
	err := c.socket.CloseNow()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
