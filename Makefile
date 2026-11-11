.PHONY: test check check-all fmt lint run openapi

test:
	go test -race ./...

check:
	go vet ./...
	go test -race ./...
	go build ./...

check-all:
	./scripts/check-all.sh

fmt:
	gofmt -w .

lint:
	golangci-lint run ./...

run:
	go run ./cmd/trest-demo

openapi:
	go run ./cmd/trest-demo -openapi
