# Trest development

For Go coding, review, debugging, or setup tasks, load the `golang-how-to` skill and the relevant focused Go skills when available.

The architecture source is `docs/Go_Framework_Design_Paper.pdf`. Keep business services independent of Trest. Construct dependencies explicitly. Do not add implicit registration, global application state, or import-time goroutines.

The module path `trest.local/trest` is provisional until a repository location is selected. The core uses only the Go standard library. Optional third-party integrations live in nested modules under `adapters/` and `experiments/`; keep their dependencies out of the core.

Use `go test -race ./...`, `go vet ./...`, and `go build ./...` for core changes. Keep contracts deterministic and test authorization rejection before handler execution. Core tests use in-memory fakes and `httptest`; no production credentials are needed. Run `scripts/check-all.sh` for all modules. PostgreSQL integration tests require an isolated disposable database, never an existing application database.
