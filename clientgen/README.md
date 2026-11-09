# Generated clients

`clientgen.TypeScript(openapi)` and `clientgen.Go(openapi, "client")` generate standalone clients without network access. The source is deterministic and uses the same OpenAPI document as the app's docs.

The Go client uses only the standard library. The TypeScript client uses `fetch`, `Headers`, `FormData`, and `URLSearchParams`; compile it with a modern DOM library and run it in a browser or a runtime with those web APIs.

```go
document, err := app.OpenAPI()
if err != nil {
    return err
}
source, err := clientgen.TypeScript(document)
```

Operations produce named input types and typed JSON, text, or no-content results. Declared 2xx and 3xx statuses are accepted, preserving HTTP status and headers along with the declared response body. Non-declared statuses produce an `APIError` with status, stable code, public message, request ID, and field errors. Path values are escaped and repeated query values remain repeated.

Header arrays use OpenAPI simple comma-separated lists. The server combines repeated physical headers and trims separator whitespace. Generated clients reject array items containing commas, control characters, or surrounding whitespace because those values cannot round-trip unambiguously in that representation.

When declared statuses have different response bodies, TypeScript generates a union discriminated by `status`: checking `result.status === 202` selects that status's `data` type. Go generates an operation-specific result with a typed pointer for each declared status and accessors such as `result.As202()`. Exactly one variant is populated. Each status uses its own decoder; empty variants have no body and native variants retain a caller-owned response stream. Operations whose statuses share one response shape keep the ordinary `Result` API.

HEAD operations and 304 responses return no-content results while preserving status and headers, even when the contract describes a representation schema.

Typed `Events[T]` routes carry `x-trest-stream` event schemas and return typed streams. TypeScript exposes `EventStream<OperationEvent>` with `response`, `close()`, and async iteration over `{id, event, data, retry?}`. Go returns `*EventStream[OperationEvent]` with `Response`, `Next()`, `Event()`, `Err()`, and `Close()`; events contain `ID`, `Event`, `Data`, and optional `RetryMillis`. Empty event names use `message`. Event IDs persist across frames unless reset. Both clients parse CR, LF, CRLF, comments, multiline data, and UTF-8 across arbitrary transport chunks. Malformed JSON, invalid UTF-8, oversized events, and incomplete final frames fail and close the body. Event frames default to a 1 MiB limit through `MaxEventBytes` / `maxEventBytes`.

```ts
const feed = await client.invoiceEvents({ query: { watch: true } }, { signal });
for await (const event of feed) {
  console.log(event.event, event.data.snapshot, event.data.change);
  if (finished) break; // cancels the response body
}
```

```go
stream, err := client.InvoiceEvents(ctx, InvoiceEventsInput{})
if err != nil { return err }
defer stream.Close()
for stream.Next() {
    event := stream.Event()
    fmt.Println(event.Event, event.Data.Snapshot)
}
return stream.Err()
```

Typed WebSocket routes declared with `WebSocketMessages[I,O]` or the optional adapter's `NewJSON[I,O]` carry `x-trest-websocket` schemas. Their methods return a typed `Socket` and await a successful connection. Go uses `Socket[OperationMessage,OperationOutput]` with `Send(ctx, value)`, `Receive(ctx)`, `Close()`, and a native `Transport` field. Set `Client.SocketDialer` explicitly; its structural interface keeps the generated client standard-library-only. The optional adapter provides the compatible native transport:

```go
client.SocketDialer = func(ctx context.Context, target string, headers http.Header) (SocketTransport, error) {
    transport, _, err := trestws.DialJSONTransport(ctx, target, trestws.ClientOptions{
        HTTPHeader: headers,
        MaxMessageBytes: client.MaxMessageBytes,
    })
    return transport, err
}
socket, err := client.Chat(ctx, ChatInput{})
if err != nil { return err }
defer socket.Close()
if err := socket.Send(ctx, ChatMessage{Text: "hello"}); err != nil { return err }
reply, err := socket.Receive(ctx)
```

TypeScript uses the native `WebSocket` by default and exposes `send(value)`, `messages()` / async iteration, `close()`, and `raw`. Configure `socketFactory(url, {headers, signal})` for a compatible custom transport. The default browser transport rejects configured HTTP headers because the browser cannot send them; it never silently drops Authorization or moves credentials into the URL. Browser socket cookies follow browser policy, while fetch `credentials` applies to HTTP operations.

```ts
const socket = await client.chat({}, { signal });
socket.send({ text: 'hello' });
for await (const message of socket.messages()) {
  console.log(message.echo);
  break; // closes the connection
}
```

Socket messages default to 64 KiB through `MaxMessageBytes` / `maxMessageBytes`. A Go transport must also bound incoming frames while reading; the adapter helper does so. TypeScript bounds received queues to `maxQueuedMessages` (default 32) and bounds the native send buffer by the configured message limit times queue capacity. Invalid JSON, binary messages, receive overflow, and transport failures terminate the stream. Go and TypeScript support one sequential reader; request contexts/signals cancel active reads. Clients never automatically reconnect or replay events or messages. HTTP operations preserve `APIError`; browser WebSocket handshake failures expose transport errors because the browser hides the HTTP response. A Go dialer can inspect the native handshake response returned by `DialJSONTransport`.

Native streaming/binary responses without event schemas return `Response` in TypeScript and `*http.Response` in Go. The caller reads or cancels/closes the stream. WebSocket routes without message schemas are skipped with an explanatory comment; use a native client for those routes.

Form and multipart inputs expose the declared scalar, array, and upload fields. TypeScript accepts a typed body object, using `File` or `Blob` for files and arrays of those for repeated uploads; the client creates the form and boundary. Go generates an operation-specific body struct, with `UploadFile{Filename, ContentType, Reader}` for files. The caller owns each file reader. Required fields and field types appear in generated declarations; value constraints remain server-enforced.

Native forms remain available explicitly: pass `FormData` or `URLSearchParams` as the TypeScript body, or use Go's `RawBody` with `MultipartBody` or `url.Values`. Go rejects providing both `Body` and `RawBody`. Forms with an undeclared or non-scalar field schema retain their native body type.

Configure credentials explicitly. Header defaults are copied into each request. Browser cookies are managed by the browser through `credentials`; omit generated `cookies` inputs in a browser. Explicit cookie inputs are for server-side clients. Default clients do not follow redirects automatically. TypeScript uses manual redirect handling; Node and compatible server runtimes expose the declared response, while browser fetch hides redirects as opaque responses and the client raises an explanatory error. Use a server-side client when redirect status, headers, or bodies are needed. A supplied Go `http.Client` retains the caller's redirect policy.

JSON, text, and error reads default to 4 MiB. Configure a positive `MaxResponseBytes` / `maxResponseBytes` when appropriate. Go form encoding and typed multipart buffering are bounded by `MaxRequestBytes`, which defaults to 1 MiB including multipart headers and boundaries. File read errors, invalid upload headers, cancellation, and size-limit failures are returned before sending the request. Native multipart readers remain caller-controlled. Go client configuration must not be mutated concurrently with requests.

Supported contracts are OpenAPI 3.1, identifier operation names, local component-schema references, primitives, nullable values, arrays, closed objects, and string-key maps. Recursive named models retain their types, including pointers, recursive slices/maps, and mutually recursive models. Nullable references use `anyOf` with a reference and `null`. String/number/boolean enums appear as TypeScript unions; Go uses their underlying type. Date-time values remain RFC3339 strings on both generated clients. Numeric bounds, string patterns, and other value constraints are enforced by the server. JavaScript's integer precision limit still applies; use a string representation for values outside its safe integer range.

Generation rejects recursion without a pointer, optional field, array, or map boundary; general unions/combinators; unsupported parameter serialization; external references; and colliding language identifiers. It never fetches referenced URLs. HTTP contracts must give a concrete 2xx or 3xx status and at most one content type per successful response. Typed event contracts require `text/event-stream` for each declared success status. Typed WebSocket contracts require a GET upgrade without a request body. A schema change requires regeneration; include the resulting diff in review.

Run compiler and runtime verification explicitly:

```sh
TREST_TSC=/path/to/typescript/bin/tsc go test -race -tags=integration ./clientgen
```

Without `TREST_TSC`, the test uses `tsc` on PATH if available and otherwise reports the TypeScript compiler check as skipped. Go compiler checks always run. Tests prove that wrong Go input types fail to compile, and TypeScript rejects wrong input types, missing bodies, and invalid enums. Both generated clients execute requests against controlled transports, including errors and streaming. SSE fixtures exercise real streaming responses, byte-split parsing, cancellation, early close, and malformed frames. WebSocket fixtures use the optional typed adapter for real Go and Node connections, plus bounded fake transports for malformed messages, queue overflow, and aborts.
