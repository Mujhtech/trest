#!/bin/sh
# Run each module explicitly: Go's ./... does not cross nested module boundaries.
set -eu
cd "$(dirname "$0")/.."
for module in . adapters/mcp adapters/postgres adapters/websocket adapters/jwt adapters/otel experiments/adoption; do
  printf '\nChecking %s\n' "$module"
  (
    cd "$module"
    if [ "$module" = "." ] && [ "${TREST_TEST_WEB:-}" = "1" ]; then
      GOWORK=off go test -race -tags=integration ./...
    else
      GOWORK=off go test -race ./...
    fi
    GOWORK=off go vet ./...
    GOWORK=off go build ./...
  )
done
if [ -n "${TREST_TEST_POSTGRES_DSN:-}" ]; then
  (
    cd adapters/postgres
    GOWORK=off go test -race -tags=integration ./...
  )
fi
if [ "${TREST_TEST_WEBSOCKET:-}" = "1" ]; then
  (
    cd adapters/websocket
    GOWORK=off go test -race -tags=integration ./...
  )
fi
