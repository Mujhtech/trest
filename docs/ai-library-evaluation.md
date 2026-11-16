# AI library evaluation

Decision date: 8 November 2026. Evaluate an existing model library before adding a Trest provider abstraction, as required by the design paper's fourth milestone.

## Candidate: Genkit for Go

Genkit already supplies model integration, typed flows, structured generation, streaming, and developer tooling. Its Go entry guide currently requires Go 1.25 or later and puts provider plugins in its Go module. Initialization is application-owned; configuration failures can panic, and request timeouts are not installed by default. Those properties argue for explicit initialization at the application's composition root and an optional integration module rather than a dependency in Trest's core. [Official Go getting-started guide](https://genkit.dev/docs/go/get-started/)

Genkit's tool APIs support typed tools, model-selected calls, limits on tool-loop iterations, explicit tool handling, and interrupts. Trest should preserve its own invocation and approval boundaries when integrating: a Genkit tool implementation forwards proposed business arguments to a trusted `aitool.Session`, rather than invoking a domain service directly. An interrupt can carry a pending approval reference, but it cannot substitute for Trest's persisted scope and authorization checks. [Official tool-calling guide](https://genkit.dev/docs/go/tool-calling/)

Genkit provides evaluation datasets, CLI evaluation, configurable evaluators, and comparisons. These are useful for a later application-specific quality experiment. Trest's committed recorded proposals test deterministic enforcement without a model provider; they are deliberately separate from evaluations that judge generated answers. [Official evaluation guide](https://genkit.dev/docs/go/evaluation/)

## Decision

Do not create a generic provider wrapper in this milestone. It would duplicate existing model APIs and obscure differences in usage, tool support, caching, structured output, finish reasons, and streaming. Keep Genkit or another selected provider library in the application's optional module and preserve its native response and error types.

The implemented integration boundary is provider-neutral but does not pretend to implement model capabilities: `Session.ReserveModelCall`, `CompleteModelCall`, and `MarkModelUncertain` track budgets and receipts; `Session.Call` enforces tool selection and execution. The application keeps provider request IDs and usage, controls streaming buffers and disconnect policy, and forwards the session deadline into the provider call. Partial streaming output is not a committed business result and must not trigger side effects automatically.

For MCP, the official Go SDK is already an implementation of the protocol and offers server/client primitives. The optional `adapters/mcp` package uses that SDK directly, with real in-memory transport tests. Its session-scoped tool list is separate from development tooling and it starts no network listener. [Official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)

## Integration recipe

1. Initialize the chosen Genkit/provider instance explicitly outside the Trest registry.
2. Authenticate the application request and create an `aitool.Session` with only that actor's allowed tools.
3. Reserve estimated cost and a turn before each provider request; use the session's remaining time budget as the provider deadline and configure a bounded model turn loop.
4. Route tool proposals through `Session.Call`; never derive principal or tenant from generated arguments.
5. Stop on a pending approval and let a trusted application endpoint obtain approval for the exact saved digest. Execute only through `Session.ExecuteApproval` after authorization is rechecked.
6. Reconcile trusted usage and provider request IDs. If acceptance occurred but the response was lost, mark uncertainty and require an explicit retry/reconciliation decision.
7. Keep prompt version, provider/model configuration, tool contract version, and evaluation data in source control. Use recorded responses for deterministic tests and an isolated live suite for quality and cost.

This is a documentation and boundary evaluation, not a claim that every Genkit provider or streaming mode was tested. The default build makes no model calls and requires no provider credentials.
