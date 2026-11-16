# Trest direction after reviewing Elysia

Design recorded 8 November 2026; implementation checkpoint updated 10 November 2026. The public application API below is implemented; the local framework completion record is in the status document. See the [framework guide](framework.md) for current usage and [status](status.md) for verification and remaining limits.

The user clarified that the reference is **the whole Elysia developer experience**. Implementing the original paper's infrastructure milestones did not deliver that experience. The earlier framework required users to understand its internals before writing a useful endpoint. Completing tests for those internals was insufficient evidence of product completion.

## What the reference changes

Elysia starts with a small application declaration: routes contain handlers, handlers return values, and the application can serve requests. Its request context brings body, path, query, headers, and cookies together. Trest should make those ordinary tasks immediately accessible. [Elysia handlers](https://elysiajs.com/essential/handler), [routing](https://elysiajs.com/essential/route)

Composition is part of the application model. Elysia instances can be reused as plugins, with explicit dependencies and scoped lifecycle behavior. Groups and guards apply shared configuration across routes. Trest needs a similarly coherent composition model, with Go-appropriate dependency passing. [Elysia plugins](https://elysiajs.com/essential/plugin)

Schema information connects validation, server types, documentation, and client communication. Elysia's Eden derives a typed client from the server's TypeScript type without code generation. Trest should deliver contract-derived Go and TypeScript clients; a Go server cannot export its Go type directly into the TypeScript compiler. Generation is the implemented bridge and must be automatic in the normal developer workflow. [Elysia validation](https://elysiajs.com/essential/validation), [Eden](https://elysiajs.com/eden/overview)

## Whole-product scope

This is the implemented Go product scope, with explicit language and transport boundaries. Elysia informed the experience; its TypeScript mechanisms are not a compatibility target.

| Experience | Implemented on 10 November | Boundaries |
| --- | --- | --- |
| First application | One import, `New`, typed routes, returned values, explicit `Listen` | Module identity and public release version are maintainer choices |
| Typed HTTP inputs | Path, query, JSON, headers, cookies, forms, multipart uploads, custom scalar codecs | Outer structs with explicit source tags use named fields; whole JSON bodies support embedding |
| Responses | JSON, text, HTML, cookies, status variants with different typed bodies, redirects, files, streams | Native `Reply` content types are explicitly documented |
| Composition | Groups, prefixes, scoped auth/guards/middleware, named plugins and prerequisites | Typed dependencies remain explicit; no import-time registration |
| Shared schemas | Recursive models, JSON embedding, enum/range/length/pattern constraints, explicit custom codec schemas | Arbitrary business validators are opaque to the contract; body defaults are annotations |
| Request context | Principal, request ID, typed keys and reusable resolvers after validation and before authorization | Resolvers must not cause privileged effects before authorization |
| Lifecycle and errors | Typed result hooks, middleware, start/rollback/stop, custom servers, draining, mapped errors, source diagnostics | Cancellation is cooperative; active handlers retain resources until they finish |
| API documentation | Offline interactive docs and deterministic OpenAPI from the app | WebSocket upgrades use native/generated clients, not the HTTP explorer |
| Typed clients | Go/TypeScript models, forms/uploads, status unions, recursive types, typed SSE/JSON WebSockets | Regenerate after contract changes; JavaScript numeric precision still applies |
| Real-time behavior | Typed JSON events and sockets with runtime schema validation, bounds, cancellation, backpressure | No automatic replay/reconnect or durable event log; authorize message resources explicitly |
| Standard plugins | CORS, signed cookies, static files, scheduling, docs, JWT verification, OpenTelemetry | Identity provider, keys, exporters, storage, and deployment policy belong to the application |
| Testing | In-memory app tests, compiler acceptance/rejection, live client/protocol tests, isolated database faults | Production workload and independent adoption studies are separate activities |
| Local development | Scaffold, reload, last-good-build recovery, client generation, inspect, project checks and baselines | Application binaries implement the documented export/readiness flags |
| Performance | Startup compilation, repeated hello and authenticated typed-request baselines | Measurements are local; no Elysia speed/parity claim |
| Learning | Progressive framework guide, plugin guide, developer workflow, authenticated invoice workshop | Public package publication is a separate maintainer action |

The lifecycle and reusable-policy direction is informed by [Elysia lifecycle events](https://elysiajs.com/essential/life-cycle) and [macros](https://elysiajs.com/patterns/macro). Interactive documentation should be as easy to enable as an ordinary plugin. [Elysia OpenAPI plugin](https://elysiajs.com/plugins/openapi)

## Public API direction

The organizing unit is now an application. Its routes, groups, plugins, hooks, and contract compile together. Construction handles registry and adapter assembly internally while dependencies remain explicit.

The implemented shape uses generic package functions so handler types can be inferred without unsupported generic methods or an untyped `any` handler dispatcher. This abbreviated service example assumes the application types and authorization configuration described below.

```go
type GetInvoice struct {
    ID string `path:"id"`
}

type CreateInvoiceRequest struct {
    Body CreateInvoice `body:"json"`
}

func Invoices(service *InvoiceService) *trest.Router {
    return trest.Group("/invoices",
        trest.Get("/{id}",
            func(ctx context.Context, in GetInvoice) (Invoice, error) {
                return service.Get(ctx, in.ID)
            },
        ).Name("getInvoice").Permission("invoices:read"),

        trest.Post("",
            func(ctx context.Context, in CreateInvoiceRequest) (Invoice, error) {
                return service.Create(ctx, in.Body)
            },
        ).Name("createInvoice").Permission("invoices:write").Status(201),
    )
}

app, err := trest.New(
    authentication,
    Invoices(service),
    trest.Docs("/docs"),
)
if err != nil {
    return err
}
return app.Listen(ctx, ":3000")
```

The example assumes application-owned `Invoice`, `CreateInvoice`, `InvoiceService`, and an authentication plugin. Permission checks must use verified request identity and typed resource input. Group configuration must have explicit inheritance and override rules; mounting a plugin must not accidentally make sibling routes protected or public. Application services remain ordinary Go functions.

Returning an `http.Handler` must remain available for existing servers and tests. App construction must not open sockets or start background work. One app validation phase should report route, schema, dependency, and hook errors together with useful source locations.

Simple static routes use `trest.Get("/", trest.Const("Hello"))`. Request and response types should be inferred where Go permits it. The final hello example and a realistic CRUD example must both be reviewed before this API is treated as settled.

## Go-specific decisions to prove

- Preserve typed handlers and ordinary compiler errors. Fluent composition is useful, but copying TypeScript syntax must not force dynamic calls or weaken checking.
- Compile route schemas and input bindings once. Distinguish compile-time guarantees from startup checks: a struct tag matching a path string is not automatically a Go compiler guarantee.
- Use typed dependencies and request-local values. Go cannot reproduce arbitrary TypeScript context augmentation through method chains; avoid requiring unchecked map lookups throughout handlers.
- Generate clients from the same contract used at runtime. Test type errors with the actual Go and TypeScript compilers. Do not claim Eden's no-generation mechanism for a cross-language Go server.
- Share declarative constraints; keep custom business validation explicit. Never suggest an arbitrary Go validator can be reverse-engineered into JSON Schema.
- Make raw handlers, custom codecs, and response types deliberate extension points. Existing strict JSON limits must not become permanent restrictions on ordinary web applications.

## How to judge the replacement

Start with a single realistic application: authenticated invoice CRUD, typed filtering and pagination, an upload, a streamed update, reusable route modules, interactive docs, and a small typed client. Implement and exercise it through public APIs. It must require no knowledge of registries, adapter construction, or internal packages.

From a fresh project, verify scaffolding, first request, a validation failure, an authorization failure, route composition, live reload, contract/client regeneration, an in-memory test, and graceful shutdown. Then check streaming/WebSocket cancellation, plugin scope, malformed input, generated-client errors, and integration with an existing `net/http` server. These are acceptance requirements, exercised by the integration suite and the completion record in [status](status.md).

Use the existing core only where it serves this public experience. Preserve its useful authorization, uncertainty, and durability work, but do not let the previous architecture dictate awkward application code. Jobs and AI tools should integrate into the resulting application model after the basic web experience is coherent.

The implementation now includes that public API and the [invoice workshop](../examples/invoices/README.md), with authenticated CRUD, filtering, uploads, bounded events, docs, and generated clients. Fresh-project and live protocol checks exercise the workflow. Passing those checks establishes working behavior within the documented limits; independent application trials are still needed to judge the overall experience.
