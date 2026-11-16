# Framework guide

Trest's root package composes routes, configuration, groups, and plugins into an immutable `http.Handler`. This is the public web API; ordinary applications do not need to create a registry, freeze operations, or bind routes themselves. The lower-level packages remain available for applications that need those boundaries explicitly.

## Construct an application

```go
app, err := trest.New(
    trest.Info("My API", "0.1.0"),
    trest.Get("/", trest.Const("Hello Trest")).Name("hello"),
    trest.Docs("/docs"),
)
```

Handle the construction error before serving. `New` checks route conflicts, names, input sources, supported schemas, policies, and plugin dependencies, then freezes the operation registry. It starts no listener, worker, or background goroutine.

`app.Listen(ctx, address)` opens a listener and runs lifecycle hooks. `app.Run(ctx, listener)` takes ownership of an existing listener. `app.RunServer(ctx, server, listener)` also accepts an application-configured `http.Server`, including a mux that contains the app; the server must not already be serving. Supply a TLS listener when needed. These entry points cancel requests, drain handlers, and shut down lifecycle resources when `ctx` is canceled.

For a server or mux with independent ownership, call `runtime, err := app.Start(ctx)` and mount the returned `*trest.Runtime`. It starts lifecycle resources without opening a listener. Call `runtime.Shutdown(shutdownCtx)` explicitly, even if the start context was canceled. Directly mounting `app` as an `http.Handler` starts no lifecycle hooks and is useful for in-memory request tests.

## Typed handlers and dependencies

Routes accept `func(context.Context, Input) (Output, error)`. Input must be a struct. Use `trest.Empty` when there is no request data. The handler's concrete types are checked by Go; Trest compiles their supported schema at construction.

```go
type GreetingInput struct {
    Name string `path:"name" minLength:"2"`
}

type Greeting struct {
    Message string `json:"message"`
}

route := trest.Get("/greet/{name}", func(ctx context.Context, in GreetingInput) (Greeting, error) {
    return Greeting{Message: "Hello, " + in.Name}, nil
}).Name("greet").Summary("Greet someone")
```

`Get`, `Post`, `Put`, `Patch`, `Delete`, `Head`, and `Options` use the same handler shape. Paths use Go's `/{name}` and `/{rest...}` capture syntax. Every capture on a typed route needs a matching `path` field. GET routes also accept HEAD requests through `http.ServeMux`; responses omit the body. Trest supplies OPTIONS handling unless an explicit OPTIONS route exists.

Give client-facing routes a stable `.Name("getInvoice")`. Otherwise Trest generates a name from the method and path. Names must be identifiers and unique within the app. `.Status(http.StatusCreated)` declares the normal success status; `.Timeout(5*time.Second)` sets a cooperative operation deadline. `.Effect("read")` or `.Effect("write")` supplies tooling metadata, not a purity guarantee. Route configuration methods return copies.

Construct stores and business services explicitly and close over them in route handlers. Pass `ctx` to service calls so cancellation reaches their I/O. The [invoice service](../examples/invoices/service.go) imports no Trest package; its [route composition](../examples/invoices/app.go) translates transport inputs, identity, and domain errors.

Use `trest.Identity(ctx)` for a principal established by authentication and `trest.RequestID(ctx)` for the server-generated correlation ID. `trest.SetHeader` and `trest.SetCookie` stage response metadata without replacing a typed JSON return value. They fail outside HTTP or after the response starts. Use request context values on the request goroutine, not after the handler returns.

For application-specific request values, create a `trest.NewKey[T]()` once, pass it explicitly to middleware and handlers, and use `trest.WithValue` / `trest.Value`. There is no global application container or implicit dependency lookup.

## Groups, authentication, and guards

```go
app, err := trest.New(
    trest.Get("/", trest.Const("public")),
    trest.Group("/api",
        trest.Authenticate(authenticate),
        trest.RequireAuth(),
        invoiceRoutes,
    ),
)
```

Here `authenticate` has type `func(*http.Request) (trest.Principal, error)` and `invoiceRoutes` is a component constructed with the application's services. Configuration applies to all routes in its containing group regardless of its position in the component list. Child groups inherit a snapshot; their changes do not affect siblings. `trest.Use` adds ordinary `net/http` middleware, with the first middleware outermost.

Routes are public unless a scoped guard, `.Authorize`, or `.Permission` protects them. `Authenticate` verifies credentials; by itself it does not require a principal. An empty principal with no error permits anonymous access to public routes. An authentication error rejects the request even on a public route in that scope.

`RequireAuth()` requires a nonempty verified principal. `Guard(func(context.Context, trest.Principal) error)` adds a shared policy and also requires authentication. A child cannot disable inherited guards. `.Authorize(func(context.Context, trest.Principal, Input) error)` adds a typed resource policy. Alternatively, use `.Permission("invoices:write")` with `trest.Authorization(authorizer)`, where the application-owned authorizer implements `operation.Authorizer`. If a route supplies a typed authorizer, it owns that route's policy decision, including any permission label.

The request path authenticates, decodes and structurally validates input, runs optional `.Validate`, resolves request resources, applies guards and resource authorization, calls the handler, and runs successful result hooks before output validation and delivery. Anonymous protected calls are rejected before resource resolution. Validators and resolvers must not perform privileged side effects. Denied authorization prevents handler execution. Authentication and authorization are application policy: request fields, query parameters, cookies, and arbitrary headers do not establish trusted identity by themselves.

For JSON-capable routes, guards and typed authorization also apply through the shared registry's direct JSON, job, and tool invocation paths. Trusted internal callers can supply a principal through `operation.WithPrincipal`; that is a trust boundary, not credential verification. Calling a business service directly bypasses Trest, so the service must preserve any invariants required of every caller.

### Resolve resources and transform results

Use an explicitly constructed typed key to share a resource between resolution, authorization, and the handler:

```go
type InvoiceInput struct {
    ID string `path:"id" json:"id"`
}
type Invoice struct {
    ID    string `json:"id"`
    Label string `json:"label"`
}
type InvoiceResource struct {
    Invoice
    TenantID string
}

invoiceKey := trest.NewKey[InvoiceResource]()
route := trest.Get("/invoices/{id}", func(ctx context.Context, in InvoiceInput) (Invoice, error) {
    resource, ok := trest.Value(ctx, invoiceKey)
    if !ok {
        return Invoice{}, trest.NewError(trest.Internal, "", nil)
    }
    return resource.Invoice, nil
}).Authorize(func(ctx context.Context, principal trest.Principal, in InvoiceInput) error {
    resource, ok := trest.Value(ctx, invoiceKey)
    if !ok || resource.TenantID != principal.TenantID {
        return trest.NewError(trest.Forbidden, "Access denied.", nil)
    }
    return nil
}).After(func(ctx context.Context, in InvoiceInput, out Invoice) (Invoice, error) {
    out.Label = strings.TrimSpace(out.Label)
    return out, nil
})
route = trest.Resolve(route, invoiceKey, func(ctx context.Context, in InvoiceInput) (InvoiceResource, error) {
    return store.Lookup(ctx, in.ID)
})
```

Here `store` is an application-owned dependency and authentication is supplied by the enclosing scope. `Resolve` receives validated input and runs in declaration order before resource authorization. Resolving a resource does not grant access to it. Resolution and authorization also run during `PrepareJSON`; preparation does not run the handler or `.After` hooks, and later invocation resolves and authorizes again.

`.After` transforms a successful result in declaration order, before final output validation. It runs through HTTP and every enabled direct/job/tool invocation path. An error from a result hook does not undo handler effects. Use `AfterResponse` to observe HTTP delivery instead of modifying a result.

## Bind request data

Without source tags, POST, PUT, and PATCH treat the entire input struct as a required JSON body. For multiple sources, give every exported input field one source tag, or exclude an unused field with `json:"-"`:

```go
type UpdateBody struct {
    Amount int64  `json:"amount_minor" min:"1" max:"100000000"`
    Status string `json:"status" enum:"draft,paid"`
}

type UpdateRequest struct {
    ID      string     `path:"id" json:"id"`
    DryRun  bool       `query:"dry_run" json:"dry_run" default:"false"`
    Version *string    `header:"If-Match" json:"version,omitempty"`
    Body    UpdateBody `body:"json" json:"body"`
}
```

Bind this input to a path such as `Put("/invoices/{id}", handler)`. The HTTP JSON body is the `UpdateBody` object itself. The `body` wrapper name describes the complete input for direct JSON invocation; it is not an extra HTTP body envelope.

| Tag | Source | Supported shape |
| --- | --- | --- |
| `path:"id"` | Matching route capture | Scalar or pointer to scalar; always required |
| `query:"tag"` | URL query | Scalar, pointer, repeated slice or fixed array |
| `header:"X-Region"` | HTTP header | Scalar, pointer, repeated slice or fixed array |
| `cookie:"theme"` | HTTP cookie | Scalar or pointer; no implicit signature verification |
| `body:"json"` | Entire JSON body | One supported schema value |
| `form:"name"` | Form body | Scalar, pointer, repeated slice or fixed array; uploads as below |

Scalars include strings, booleans, integers, finite floats, `time.Time` using RFC3339, and explicitly declared custom codecs with a supported scalar wire shape. Duplicate values for scalar fields fail. Unknown query and form fields fail; unrelated headers and cookies are not rejected. Declared JSON fields are case-sensitive. Malformed JSON, duplicate object keys, unknown object fields, wrong types, missing required values, and numeric overflow fail before the handler runs.

JSON bodies require `Content-Type: application/json`. Text-only forms use `application/x-www-form-urlencoded`; declaring an upload selects `multipart/form-data`. A request cannot mix form fields and a JSON body. Bodyless routes reject unexpected bodies. Form parsing keeps query and form values separate.

Nonpointer source fields are required by default; pointers are optional. Use `required:"true"` or `required:"false"` to override this, except that path captures remain required. A scalar `default` supplies a value for an absent query, header, cookie, or form field and makes that HTTP field optional. Defaults do not apply to an explicitly supplied empty or invalid value. Path fields and uploads cannot have defaults.

### Validation and schema rules

| Tag | Meaning |
| --- | --- |
| `json:"name,omitempty"` | JSON name and optional presence; `json:"-"` excludes the field |
| `required:"true"` / `"false"` | Override presence requirements |
| `min` / `max` | Inclusive numeric bounds within the Go type's range |
| `minLength` / `maxLength` | String length bounds |
| `minItems` / `maxItems` | Array or slice length bounds |
| `pattern` | Go regular expression for a string |
| `enum:"draft,paid"` | Comma-separated scalar choices |
| `default` | Validated schema annotation; also applied by non-body HTTP binding |
| `description` | Field documentation |

JSON field names default to exported Go field names; explicit tags make the wire format clearer. Nonpointer JSON fields without `omitempty` are required unless overridden. Required means present, not nonzero. Pointers are nullable; slices and string-key maps are nullable because nil Go values encode as `null`.

JSON body and response defaults are documentation annotations: they do not populate missing fields or make fields optional. For example, `Currency string` with `json:"currency" default:"NGN"` still requires `currency` in a JSON body. Direct JSON invocations likewise supply the complete input rather than receiving HTTP parameter defaults. An optional nonpointer field retains its Go zero value when absent; that value must still satisfy any constraints when the complete input is validated. Use a pointer when absence must remain distinct from a zero value.

Supported JSON values include nested and recursive structs, primitives, pointers, slices, fixed arrays, string-key maps, `time.Time` as RFC3339, and byte slices as base64. Recursive models use finite local schema references; request JSON still has bounded nesting, and cyclic Go values cannot be JSON encoded. Constraints validate both input and typed output and appear in OpenAPI. Invalid declarations fail during construction. `.Validate` adds business rules but arbitrary Go validator code is not converted into schema constraints.

Embedded JSON fields follow `encoding/json` promotion and name-dominance rules. A nil embedded pointer omits its promoted fields, which are optional unless explicitly required. Put constraints on the promoted fields themselves. Whole-body JSON input and response models support embedding; when an outer request struct uses explicit HTTP source tags, its source fields must be named rather than embedded.

```go
type Audit struct {
    CreatedAt time.Time `json:"created_at"`
}
type Folder struct {
    Audit
    Name     string    `json:"name" minLength:"1"`
    Children []*Folder `json:"children,omitempty"`
}
```

Custom JSON or text codecs need an explicit `trest.SchemaProvider` contract. Implement `TrestSchema() json.RawMessage` on the transport type or a local wrapper around a domain type, together with a value-receiver encoder and matching pointer-receiver decoder:

```go
type Slug string

func (s Slug) MarshalText() ([]byte, error) { return []byte(s), nil }
func (s *Slug) UnmarshalText(data []byte) error {
    *s = Slug(data)
    return nil
}
func (Slug) TrestSchema() json.RawMessage {
    return json.RawMessage(`{"type":"string","minLength":1,"maxLength":80,"pattern":"^[a-z0-9-]+$"}`)
}
```

For JSON codecs, implement `MarshalJSON` on the value and `UnmarshalJSON` on its pointer. A text codec declares a nonnullable string wire schema. The schema method must be pure, deterministic, safe on a fresh zero value, and return at most 64 KiB. Supported declarations use the same types, constraints, local references, and nullability as generated schemas; objects require explicit closed properties or typed map values. Unknown keywords and incompatible declarations fail construction. HTTP inputs validate the original wire value before decoding and validate the decoded representation again; outputs validate the encoded wire value. A codec cannot silently normalize invalid input into an accepted value.

`trest.Schema[T]()` returns the same deterministic schema independently of an app. Untyped interfaces, general schema unions/combinators, maps with nonstring keys, unrestricted JSON Schema overrides, and codecs without a matching declaration remain unsupported. Only `omitempty` is supported as a JSON tag option. Status-specific response alternatives use the explicit response API below.

### Uploads

```go
type UploadRequest struct {
    ID   string                `path:"id" json:"id"`
    File *multipart.FileHeader `form:"file" json:"-" required:"true"`
}
```

Use `[]*multipart.FileHeader` for multiple files, optionally with `minItems` and `maxItems`. Open files with `FileHeader.Open` and close them in the handler. Trest removes multipart temporary files after the request finishes; copy data into application-owned storage before returning if it must survive. `trest.Limits` bounds the entire request body. The application must impose any additional per-file size, content, storage, and ownership policy. Upload routes are HTTP-only for JSON tool/job purposes, even when their response is typed JSON.

## Responses, errors, and streams

Normal supported Go values produce JSON. A string or named string type without a custom codec produces plain text. `.Status(204)` declares a response without a body. For native HTTP behavior, return `trest.Reply` from `Text`, `HTML`, `Bytes`, `NoContent`, `Redirect`, `File`, `Stream`, or `SSE`.

Use `trest.Response[T]` when a handler selects among declared HTTP statuses with one JSON body type:

```go
route := trest.Post("/invoices", func(ctx context.Context, in Invoice) (trest.Response[Invoice], error) {
    return trest.JSON(http.StatusCreated, in).WithHeader("Location", "/invoices/"+in.ID), nil
}).Name("createInvoice").Statuses(http.StatusOK, http.StatusCreated)
```

`Response[T]` always describes a JSON body, including when `T` is a string. `.Status` supplies a single allowed status; `.Statuses` declares several. The selected status and encoded body are checked before delivery. Undeclared statuses, invalid headers, and schema-invalid bodies fail without exposing the body.

For two different success bodies, return `trest.Either[A, B]` and declare exactly two statuses in branch order:

```go
type Completed struct { ID string `json:"id"` }
type Queued struct { JobID string `json:"job_id"` }
type WorkInput struct { Queue bool `query:"queue" default:"false"` }

route := trest.Get("/work", func(ctx context.Context, in WorkInput) (trest.Either[Completed, Queued], error) {
    if in.Queue {
        return trest.Second[Completed, Queued](Queued{JobID: "job-1"}), nil
    }
    return trest.First[Completed, Queued](Completed{ID: "result-1"}), nil
}).Name("work").Statuses(http.StatusOK, http.StatusAccepted)
```

`First` selects the first type/status and `Second` the second. The zero value of `Either` is invalid. Each branch contributes its own response schema to the contract and generated clients. Declared statuses must be distinct and in the 2xx/3xx range; 204 and 304 send no body. These wrappers describe HTTP responses and are excluded from JSON job/tool invocation. Return `Response`, `Either`, `Reply`, and `Events` as values; pointers to these wrappers and nested response wrappers fail construction with a source-bearing diagnostic.

`Reply.WithHeader` adds a header; `Reply.WithStatus` changes buffered responses. File, redirect, and stream callbacks own their status behavior and reject incompatible changes. Use `.Produces("text/html")` or the appropriate media type on a native route, and `.Status(...)` to document its success status. Trest does not infer a typed response schema from native callbacks. `HTML` does not escape user input; use `html/template` for that work.

`File(files, "report.pdf")` serves a single file from an explicit `fs.FS` with no directory listing. Paths must satisfy `fs.ValidPath`, without a leading slash. The [static plugin](web-plugins.md#static-files) serves an asset prefix.

For a typed JSON server-sent event stream:

```go
type Update struct {
    Ready bool `json:"ready"`
}

events := trest.Get("/events", func(context.Context, trest.Empty) (trest.Events[Update], error) {
    return trest.EventStream(func(ctx context.Context, writer *trest.JSONEventWriter[Update]) error {
        if err := ctx.Err(); err != nil {
            return err
        }
        return writer.Send("1", "ready", Update{Ready: true})
    }), nil
}).Name("events")
```

`Events[T]` fixes status 200 and `text/event-stream`; `.Statuses`, `.Produces`, and status values other than 200 are rejected. Each `Send` validates the encoded `T` before framing it. `Write(trest.JSONEvent[T]{...})` also accepts an optional retry interval; `Heartbeat` emits a comment. The event schema appears in `x-trest-stream` and supports typed client decoding. Clients expose reconnection metadata but do not automatically reconnect or replay.

Use the callback's `ctx` for streaming work; the typed operation ends once it returns the stream value, while runtime lifecycle ownership tracks the complete HTTP handler. The untyped `SSE`/`EventWriter` and `Stream`/`io.Writer` APIs remain available for native responses. Writes flush and use a ten-second write deadline; callbacks must honor cancellation. This supplies synchronous backpressure without durable delivery. After headers are sent, a delivery error cannot become a JSON error response; `AfterResponse` reports the outcome.

`trest.Raw(method, path, handler)` mounts an ordinary `http.Handler` with scoped middleware, authentication, guards, permission checks, and response observation. Its permission authorizer receives `*http.Request`. The [optional WebSocket adapter](../adapters/websocket/README.md) uses this extension point for real upgrades and connection lifecycle/limits. `trestws.NewJSON[Input, Output]` adds validated JSON text messages and `x-trest-websocket` input/output contracts; it requires application authorization of resources referenced by individual messages. The separate module keeps its SDK dependency out of the core.

Native `Reply`, typed HTTP status responses, typed event streams, and upload routes have `NativeOnly` metadata and `JSONDisabled` operation metadata. `Registry.PrepareJSON` and `InvokeJSON` reject them before invoking their handler; AI tool configuration also rejects JSON-disabled operations, and jobs use the same JSON preparation gate. Raw routes, including WebSockets, have no registered JSON operation. Native HTTP behavior cannot be silently exposed as a serializable tool or persisted job.

Map domain failures with `.MapError` into `trest.NewError(trest.NotFound, "Invoice not found.", err)` or another public category. Scoped `OnError` also handles request parsing and delivery errors before a response starts. Unclassified errors and panics are redacted. Public error bodies include `code`, `message`, `request_id`, and optional field details with JSON Pointer paths. Causes remain internal through Go error wrapping.

## Limits, lifecycle, and plugins

Default request and buffered response limits are 1 MiB. `Limits(bodyBytes, outputBytes)` configures the containing scope. `Concurrency(max)` configures admission at app scope; the default is 128 active operations, and excess calls are rejected rather than queued. Typed operations have a default cooperative deadline of 30 seconds. Deadlines do not forcibly stop Go code or undo effects. Streaming and native handlers need their own protocol-appropriate bounds; a buffered output limit does not bound application allocations.

`Group` composes a prefix and children; `.Use(components...)` returns an extended copy. `Plugin("name", components...)` names a reusable group. Reusing the same plugin instance in the same scope is deduplicated. Conflicting definitions with the same name fail. `.Requires("dependency")` requires that plugin to have been mounted first in the same scope. Plugins compose explicitly supplied components; there is no import-time registration.

`OnStart` runs before requests are accepted. Prefer `Lifecycle(start, stop)` for paired resource acquisition: startup failure cleans up successfully started components only, while a failed start cleans up its own partial work. `OnStop` and paired stop hooks run in reverse registration order after active handlers drain. Hooks must honor their contexts, and only one lifecycle owner can be active per app.

`Runtime.Shutdown` first rejects new requests, then cancels active requests and lifecycle work, waits for complete handlers (including streams and middleware cleanup), and finally stops resources. Its supplied context limits how long the caller waits. If that wait expires, cleanup continues after handlers drain; another `Shutdown` can await completion. Stop hooks get a separate five-second cooperative cleanup context. An uncooperative handler retains resources and prevents restart until it returns. `Run`/`Listen`/`RunServer` own this process; an app started with `Start` requires explicit `Shutdown`.

The standard-library [web plugins](web-plugins.md) provide CORS, typed signed cookies, static files, and nonoverlapping interval tasks. Optional [JWT authentication](../adapters/jwt/README.md) verifies explicit issuer/audience/key policy and maps verified claims into a principal. Signed cookies and JWTs still need application-owned authorization, revocation, and session policy. Interval tasks are in-process work, separate from durable [PostgreSQL jobs](milestone-3.md).

## Documentation, clients, and observation

`app.OpenAPI()` exports deterministic OpenAPI 3.1.1 from compiled routes. `Docs("/docs")` serves a self-contained interactive explorer and `/docs/openapi.json`, without a CDN. Place it in an authenticated group if metadata should be protected. The docs endpoints themselves are omitted from the application contract. Its HTTP Send action is disabled for WebSocket routes; use the generated typed or native WebSocket client. Business authorization is described by Trest extensions; the contract does not imply a universal authentication scheme.

`app.Routes()` returns detached metadata in path/method order, including input sources and `Source` with the declaration's file basename, line, and function. The root route constructor passes that source into the shared operation metadata, so inspection points to the route declaration rather than framework registration glue. Low-level definitions may provide their own `Source` when wrapping registration.

Use app-scoped `Observe(observer)` for operation phases, including direct JSON, job, and tool invocations. Scoped `AfterResponse(func(trest.Event))` adds HTTP decode and delivery outcomes with method, route template, status, duration, public error code, and request ID. Neither includes request or response bodies. Hooks must be fast and concurrency-safe. The request ID connects HTTP outcomes to operation traces.

The optional [OpenTelemetry adapter](../adapters/otel/README.md) supplies request middleware and an operation observer using an explicitly injected provider and propagator. It exports route templates and classified metadata, omitting credentials, payloads, actual path/query values, and identity fields. Providers, exporters, and shutdown remain application-owned; the adapter requires Go 1.26.

`clientgen.TypeScript(document)` and `clientgen.Go(document, "client")` generate clients from the same contract, including recursive model references and declared status/body alternatives. Typed forms, uploads, SSE event payloads, and WebSocket message contracts use their native transports; arbitrary untyped HTTP streams remain raw responses for callers to consume and close. WebSocket routes without message schemas require a native client. Regenerate after contract changes. See [generated clients](../clientgen/README.md) for exact generated APIs, supported schema boundaries, and connection behavior, and the [development guide](development.md) for CLI commands.
