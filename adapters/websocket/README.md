# WebSocket adapter

This optional module uses [Coder's WebSocket SDK](https://github.com/coder/websocket), pinned to v1.8.15. It requires Go 1.23 and keeps the SDK outside Trest's standard-library-only core.

```go
type Message struct {
    Room string `json:"room" minLength:"1" maxLength:"64"`
    Text string `json:"text" minLength:"1" maxLength:"1024"`
}

socket, err := trestws.NewJSON[Message, Message]("/events", trestws.Options{
    Name:       "events",
    Permission: "events:read",
    Origins:    []string{"https://app.example.com"},
}, func(ctx context.Context, connection *trestws.JSONConn[Message, Message]) error {
    for {
        message, err := connection.Read(ctx)
        if err != nil {
            return err
        }
        principal, ok := trest.Identity(ctx)
        if !ok || message.Room != principal.TenantID {
            return trest.NewError(trest.Forbidden, "room access denied", nil)
        }
        if err := connection.Write(ctx, message); err != nil {
            return err
        }
    }
})
if err != nil {
    return err
}
app, err := trest.New(authentication, authorization, socket)
```

A WebSocket route performs an HTTP upgrade and remains a native HTTP endpoint. `NewJSON[I, O]` publishes the input and output message schemas through OpenAPI's `x-trest-websocket` extension for typed clients. WebSocket routes do not enter the job or AI tool JSON invocation registry.

The input and output schemas use the same Go model declarations as ordinary Trest routes, including constraints, recursive models, and explicitly declared custom codecs. They compile once during construction. `Read` accepts one JSON text frame, validates its original wire value, decodes it, and checks the decoded representation before returning a value. Unknown or duplicate fields, malformed JSON, binary messages, and failed constraints or codecs immediately close the connection. `Write` checks the output schema and message size before sending any frame. A `MessageError` contains safe structural field details; private codec error text is never sent to the peer.

The application supplies authentication and the permission policy. Authentication, group guards, and authorization finish before the SDK upgrades the connection. Choose either a permission or `Public: true` explicitly. `Origins` contains complete origins with a scheme, host, and optional port; wildcards, suffix matching, and opaque origins are rejected. Origin-less nonbrowser clients require `AllowMissingOrigin: true`. Origin checking is separate from authentication.

Defaults are 64 KiB per incoming/outgoing message, 128 active connections, a 30-second read deadline, a 10-second write deadline, and a five-minute session lifetime. Backpressure is synchronous: writes await the network under their deadline, and the adapter does not enqueue messages. Compression is disabled. One reader handles close and ping frames; concurrent writes are supported by the SDK.

Trusted identity values survive the HTTP upgrade. Connection cancellation is tied to the app lifecycle and the session deadline, avoiding the SDK's documented caveat about using a hijacked HTTP request's cancellation directly. App shutdown cancels active sessions and waits for handlers within its shutdown deadline. Use `App.Run` or `Listen` for lifecycle ownership. Direct `ServeHTTP` testing starts no lifecycle hooks; session timeouts and peer close still apply. Create a fresh component per independently running application.

A handler must read or write to observe network disconnects and honor its context. Returning or calling `Close` closes the socket immediately without waiting for a peer close handshake. This bounds cleanup but peers may observe an abnormal close. Each message can name a different resource, so the application must authorize those resources after `Read` and before processing. Messages are not durable and are never retried automatically. `OnError` is an optional private application callback; errors are never sent to the peer as diagnostic text.

`New` remains available for protocol-specific text or binary connections through `Conn`. Its `ReadJSON[T]` and `WriteJSON` convenience functions encode and decode values; shared message-schema validation and typed-client metadata are provided by `NewJSON`.

## Native Go client transport

`DialJSONTransport` supplies the structural byte transport used by generated Go clients. It performs no schema inference and imports no generated package. Pass authentication and Origin headers explicitly:

```go
connection, _, err := trestws.DialJSONTransport(ctx, url, trestws.ClientOptions{
    HTTPHeader: http.Header{
        "Authorization": []string{"Bearer " + token},
        "Origin":        []string{"https://app.example.com"},
    },
})
if err != nil {
    return err
}
defer connection.Close()
```

The optional second return value preserves a rejected handshake's HTTP response. The transport implements `Read(context.Context) ([]byte, error)`, `Write(context.Context, []byte) error`, and `Close() error`. A generated client's `SocketDialer` can wrap this helper, passing its URL and headers and returning the connection and error. This keeps the generated client independent of the Coder SDK.

Client defaults are a 10-second handshake deadline, 30-second reads, 10-second writes, and 64 KiB incoming/outgoing frames. `ClientOptions` can configure these limits and the HTTP client used for TLS or proxies. The dial context controls the handshake; each read and write has its own context. The caller owns the connection and must close it. Reads reject binary or invalid UTF-8 messages, and SDK read limits apply before an oversized frame is returned. Writes apply backpressure synchronously without an internal message queue.

## Run and verify

A runnable typed echo fixture is included:

```sh
cd adapters/websocket
export TREST_WS_TOKEN='choose-a-local-demo-token'
go run ./cmd/echo
```

Connect to `ws://127.0.0.1:8082/echo` with `Authorization: Bearer` followed by that token. Send `{"room":"fixture","text":"hello"}` as a JSON text message. The example authorizes the connection and then checks the requested room against the trusted tenant on every message. It permits origin-less command-line clients and defaults to `http://localhost:3000` for browser origins. A browser's native WebSocket API cannot set a custom Authorization header; a real browser application should supply an authenticated cookie or an explicitly designed short-lived connection credential through its application authentication hook. The fixture is not a browser authentication implementation.

```sh
go test -race ./...
go test -race -tags=integration ./...
go vet ./...
go build ./...
```

From the repository root, `TREST_TEST_WEBSOCKET=1 scripts/check-all.sh` includes the WebSocket integration suite alongside all module checks.

Integration tests use temporary loopback listeners. They exchange real SDK frames and verify denied authentication/origins, identity propagation, typed echo, strict input and output contracts, codec failure redaction, connection and message limits, native-client deadlines, and app cancellation. No production system or credential is used.
