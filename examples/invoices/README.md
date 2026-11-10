# Invoice workshop

This application uses Trest's public application API: routes, scoped authentication, typed path/query/body inputs, validation constraints, interactive docs, uploads, and a tenant-scoped event stream. The domain service in `service.go` imports no framework package.

From the repository root:

```sh
export TREST_DEMO_TOKEN='choose-a-local-test-token'
go run ./cmd/trest-demo
```

Open `http://127.0.0.1:3000/docs`. Set its Authorization field to `Bearer ` followed by your token. The server maps that token to tenant `acme`; use `customer-acme` when creating invoices. `/` and `/readyz` are public. Stop with Ctrl-C for graceful shutdown.

```sh
curl http://127.0.0.1:3000/api/invoices \
  -H "Authorization: Bearer $TREST_DEMO_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"customer_id":"customer-acme","amount_minor":12500,"currency":"NGN"}'

curl 'http://127.0.0.1:3000/api/invoices?currency=NGN&limit=10' \
  -H "Authorization: Bearer $TREST_DEMO_TOKEN"
```

The returned invoice ID is used by `GET`, `PUT`, and `DELETE /api/invoices/{id}`. Updating requires an `amount_minor` and `status` (`draft` or `paid`), with an optional memo. The `json`, `min`, `max`, `enum`, and string-length tags contribute to both validation and documentation.

Upload a local receipt using multipart field `file` at `POST /api/invoices/{id}/attachment`. Files are limited to 512 KiB; this teaching app saves only the name, length, and SHA-256 digest, not file content. All invoices and attachment metadata live in memory and disappear when the process stops.

`GET /api/invoices/events` emits an initial snapshot and ends. Add `?watch=true` to receive live tenant updates until the request is cancelled. Snapshot and subscription are acquired atomically. The initial snapshot contains up to 100 invoices and reports the total; use the list endpoint for pagination. Each subscriber has a 16-event buffer; a slow subscriber is disconnected and must request a fresh snapshot. This is a local event feed, not a durable delivery log.

The main command also exports without starting a server or needing a token:

```sh
go run ./cmd/trest-demo -openapi > /tmp/invoices.openapi.json
go run ./cmd/trest-demo -manifest > /tmp/invoices.routes.json
go run ./cmd/trest-demo -client-ts > examples/invoices/client/client.ts
go run ./cmd/trest-demo -client-go > /tmp/invoiceclient.go
```

`client/workshop.ts` is a typed client example that creates, reads, updates, attaches a receipt, lists, reads a snapshot, and deletes an invoice. Pass it your local base URL and token explicitly. Its generated client preserves status, public errors, and response types; event streams expose typed async iteration and retain the native `response`. The snapshot event carries `data.snapshot`; live change events carry `data.change`. Breaking iteration cancels the response.

For an installed TypeScript compiler:

```sh
TREST_TSC=/path/to/typescript/bin/tsc \
  go test -race -tags=integration ./clientgen ./examples/invoices
```

These checks compile generated Go and TypeScript. A generated Go client exercises the actual application in memory, including uploads, tenant denial, and streaming. Normal `go test -race ./examples/invoices` checks CRUD, invalid input without effects, tenant scope, and stream cancellation. No production credentials or database are used.

Authentication here is an explicit local token map, not a production identity provider. Application code supplies authentication and stores; business methods enforce ownership again so calling them outside Trest does not bypass tenant boundaries.
