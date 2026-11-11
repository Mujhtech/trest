# OpenTelemetry tracing adapter

This optional module connects Trest request and operation metadata to an application-owned [OpenTelemetry Go](https://opentelemetry.io/docs/languages/go/instrumentation/) tracer provider. It pins the official [v1.47.0 release](https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.47.0), which [requires Go 1.26](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/go.mod). The SDK is used by the adapter's tests; runtime instrumentation depends on the OTel API. Trest's root module remains standard-library-only and Go 1.23 compatible.

Given a provider that the application has explicitly constructed:

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
    trest.Get("/invoices/{id}", getInvoice).Name("invoice"),
)
```

Import `trestotel "trest.local/trest/adapters/otel"` and `go.opentelemetry.io/otel/propagation`. Both dependencies are required. An empty `propagation.NewCompositeTextMapPropagator()` explicitly disables extraction of remote trace headers. The constructor creates no SDK provider, exporter, network connection, or background worker. It never gets or sets global OTel providers or propagators. The application owns exporter configuration, sampling, startup, flushing, shutdown, and retention.

Install `trest.Observe` at app scope; Trest rejects scoped operation observers. A request span starts in `Middleware`, and its context reaches authentication, guards, business handlers, and downstream calls. The injected propagator establishes an incoming trace parent. The server span name uses the normalized HTTP method and matched route template, such as `GET /invoices/{id}`. When no template is available, only the method is used. There is no fallback to the requested URL.

`trest.Use` applies to declared routes and their scoped middleware, so unmatched requests that never enter a route are outside that middleware. To also trace mux-generated 404/405 responses, the same middleware can wrap the entire app as an `http.Handler` instead of being installed through `trest.Use`; keep `trest.Observe(instrumentation)` inside the app. Use one installation per request to avoid duplicate server spans.

The operation observer creates an `INTERNAL` span for each completed phase, including validation, resolution, authorization, handler execution, invocation, and JSON preparation/invocation. It uses `Event.StartedAt` and `Duration` as the span's actual timestamps. Each phase is a sibling child of the active OTel span. These completion callbacks cannot establish active parent spans during the earlier work, and the adapter does not reconstruct a nested call tree from Trest's internal trace IDs. Direct job, tool, or JSON calls can supply an OTel parent in their context; without one, the observed phases have no shared OTel parent.

Span attributes are deliberately limited to:

- HTTP method, matched route template, and response status.
- Registered operation ID, phase, and classified error code.
- A bounded error category for HTTP failures, cancellation, writes, flushes, hijacking, or panics.

The adapter never copies request or response bodies, query strings, actual path parameters, cookies, authorization headers, host/client addresses, principal IDs, tenant IDs, correlation IDs, panic values, or error messages into spans. It does not record exception events. Trusted application route/operation names, custom propagators, SDK resources, exporters, and any application-authored spans still require an appropriate data policy. For example, a propagator may carry `tracestate` or baggage in the context; this adapter does not turn baggage into attributes.

HTTP server responses in the 4xx range leave OTel status unset; 5xx responses and detected transport failures set error status, following the relevant [HTTP span status conventions](https://opentelemetry.io/docs/specs/semconv/http/http-spans/#status). Failed operation phases carry their Trest error classification. A panic is recorded only as `panic`, then rethrown so the framework or HTTP server retains its normal recovery behavior. If no response headers were observed before that panic, its eventual HTTP status is omitted: an outer recovery hook may map it. When wrapping the whole app, a panic already recovered by Trest appears as its resulting HTTP response. Response-controller flushing, deadlines, and hijacking remain available.

This adapter exports traces. It deliberately omits some HTTP semantic-convention attributes that would contain request-specific data, so it does not claim complete HTTP semantic-convention coverage. It does not install a metrics, logging, or exporter pipeline.

## Verify

```sh
cd adapters/otel
go test -race ./...
go vet ./...
go build ./...
```

Tests inject an SDK provider with its in-memory exporter and synchronous processor. They verify real OTel parent relationships, recorded timing, error status, route templates, metadata privacy, cancellation, concurrent requests, and native HTTP writer capabilities without an external collector or credentials.
