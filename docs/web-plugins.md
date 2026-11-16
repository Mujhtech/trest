# Web plugins

These packages compose with the public `trest.New` and `trest.Group` APIs. Their constructors perform no network I/O and start no background work. Authentication and resource authorization remain application-owned. CORS, cookies, static files, and scheduling use the standard library; WebSocket, JWT, and OpenTelemetry integrations are optional nested modules.

## CORS

```go
browserAccess, err := cors.New(cors.Options{
    Origins:     []string{"https://app.example.com"},
    Methods:     []string{"GET", "POST", "DELETE"},
    Headers:     []string{"Content-Type", "Authorization"},
    Credentials: true,
})
```

Pass the returned component into the group that should allow these browser requests. Origin matching uses complete origins, not substrings or domain suffixes. Wildcard origins cannot be combined with credentials. Preflights are handled before endpoint authentication without invoking the application handler; actual requests still authenticate and authorize. CORS controls browser access and is not an authentication mechanism. `cors.Handler` exposes the same policy as ordinary `net/http` middleware.

## Signed cookies

`cookie.New[Claims](cookie.Options{...})` creates an immutable typed signer. `Sign`/`Verify` work with encoded values; `Set`/`Read`/`Delete` operate on HTTP cookies. Supply random keys from application configuration. Each key must contain at least 32 random bytes and a distinct identifier. The first key signs new values; remaining keys verify previous generations during rotation.

The HMAC envelope binds version, key ID, expiration, payload, cookie name, and path. Defaults are host-only, Secure, HttpOnly, SameSite=Lax, path `/`, one-hour expiry, and a 3,500-byte encoded-value limit. `Insecure` is an explicit opt-in for local HTTP. Duplicate matching cookies are rejected. Key slices are copied, so later mutation of caller-owned configuration cannot change a live signer.

Values are signed, not encrypted. Keep secrets out of the payload. Application authorization, session revocation, and CSRF policy are separate responsibilities. Removing a verification key invalidates cookies signed with it; there is no hidden session database.

## Static files

```go
assets, err := static.New("/assets", embeddedFiles, static.Options{
    CacheControl: "public, max-age=3600",
})
```

The filesystem must implement `fs.FS`. Only regular files are served; directory listings and dotfiles are disabled by default. Encoded traversal and invalid filesystem paths are rejected. The default serving limit is 32 MiB per file. Range and HEAD requests use standard HTTP behavior.

The filesystem itself is trusted. `os.DirFS` can follow symlinks; use embedded files or a filesystem implementation with an appropriate root boundary when the underlying file tree is writable by untrusted actors. URL path validation does not establish filesystem ownership.

## Interval tasks

```go
maintenance, err := schedule.New(schedule.Task{
    Name:    "refresh-cache",
    Every:   time.Minute,
    Timeout: 10 * time.Second,
    Run:     cache.Refresh,
    OnError: reportError,
})
```

Pass the component to `trest.New` and serve with `App.Run`, `Listen`, or `RunServer`, or explicitly own a runtime with `App.Start` and `Runtime.Shutdown`. Paired lifecycle hooks start the task and cancel it during shutdown. Each interval begins after the previous callback finishes: runs cannot overlap, and missed ticks are not queued. Callbacks must honor context cancellation. Shutdown has a deadline, and a still-running callback prevents a second start from acquiring the same component. Construct a fresh component for an independent application.

Panics become a generic task error. `OnError`, if supplied, receives errors on the scheduler goroutine and must return promptly; no default logger exposes application data. This is an in-process interval scheduler, not durable cron or a replacement for the PostgreSQL job queue. Process loss loses any pending interval work.

## WebSockets

The [optional WebSocket module](../adapters/websocket/README.md) uses Coder's SDK and requires Go 1.23. It integrates with scoped authentication, guards, permission checks, and app lifecycle, and applies explicit origin, message-size, connection-count, and deadline bounds.

`trestws.New` exchanges native frames. `NewJSON[Input, Output]` adds schema-validated JSON text messages and typed contract metadata:

```go
type ClientMessage struct {
    Text string `json:"text" minLength:"1" maxLength:"200"`
}
type ServerMessage struct {
    Text string `json:"text"`
}

socket, err := trestws.NewJSON("/echo", trestws.Options{
    Name:       "echo",
    Permission: "echo:use",
    Origins:    []string{"https://app.example.com"},
}, func(ctx context.Context, connection *trestws.JSONConn[ClientMessage, ServerMessage]) error {
    for {
        message, err := connection.Read(ctx)
        if err != nil {
            return err
        }
        if err := connection.Write(ctx, ServerMessage{Text: message.Text}); err != nil {
            return err
        }
    }
})
```

Supply application authentication and the `echo:use` permission policy when composing the returned component. Origin checks remain separate from identity. Invalid JSON, unknown or duplicate fields, invalid message constraints, and binary frames are rejected before `Read` yields a value. Invalid output is rejected before sending it. Handlers still authorize any resource referenced by each message; successful handshake authorization is not a permanent permission grant.

Typed routes expose `x-trest-websocket` message schemas for generated clients. They remain native upgrade routes and are excluded from JSON jobs/tools. The HTTP docs explorer displays their contract and disables HTTP Send; use a typed or native WebSocket client to connect. The adapter guide covers browser credential limits, cancellation, backpressure, and connection shutdown.

## JWT authentication

The [optional JWT module](../adapters/jwt/README.md) requires Go 1.23 and wraps `golang-jwt/jwt/v5`. `trestjwt.New[Claims](Options[Claims])` returns a scoped authentication component. Explicit options select one supported signing algorithm, issuer, audience, a fresh typed claims factory, a key resolver, and a verified-claims-to-principal mapper. Signature, expiration, issuer, audience, and time constraints are checked before the mapper runs.

Missing credentials leave the request anonymous; add `trest.RequireAuth()` or a route permission to protect it. Present malformed or invalid bearer credentials return a uniform 401. Key selection and rotation are application-owned, with no discovery or network fetch. This adapter verifies credentials; the application owns token issuance, identity-provider flows, revocation, and resource authorization. See the module guide for limits and a runnable local example using a synthetic key.

## OpenTelemetry tracing

The [optional OpenTelemetry module](../adapters/otel/README.md) requires Go 1.26. Inject the application's provider and propagator, then install request middleware and the operation observer at app scope:

```go
instrumentation, err := trestotel.New(trestotel.Options{
    Provider:   provider,
    Propagator: propagation.TraceContext{},
})
if err != nil {
    return err
}
app, err := trest.New(
    trest.Use(instrumentation.Middleware),
    trest.Observe(instrumentation),
    routes,
)
```

Request spans use the matched route template, method, and observed status. Completed operation phases become sibling child spans with their recorded timestamps and classified errors. The adapter omits bodies, credentials, actual path/query values, principal and tenant IDs, and error messages. It creates no exporter or background pipeline and never gets or sets OTel global providers. The application owns sampling, exporter configuration, flushing, and shutdown. The module guide explains outer-mux coverage and panic recovery timing.
