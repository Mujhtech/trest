# Milestone 4: optional AI tools

Status: experimental prototype. The core stays independent of provider SDKs. The implementation supplies explicit tool exposure, shared authorization, bounded execution, durable approval pauses, model-call receipts, and a separately versioned MCP adapter. It does not run an autonomous model loop or promise exactly-once external effects.

## Application tools

Create an `aitool.Catalog` with a frozen registry, explicit `Tool` declarations, an explicit `Store`, and an application approval policy. Only named declarations can become tools. Create a `Session` after verifying the caller's credentials; its context supplies the trusted principal and tenant. A second allowlist selects which catalog tools that session can see or call. Empty allowlists expose nothing.

`Session.Call` validates and authorizes through `Registry.PrepareJSON`, then executes through `Registry.InvokeJSON`. Both use the shared operation pipeline. Models can propose business arguments; they cannot set the session identity. Authorization checks resource ownership using the trusted principal and typed input. Tool descriptions and side-effect declarations describe behavior; they do not grant access or prove that a handler is read-only.

Session defaults are eight tool attempts, eight model turns, a 30-second cooperative wall deadline, one concurrent tool, 1 MiB input, and 64 KiB output. Rejected capacity attempts consume a tool attempt. Side-effecting or undeclared operations are serialized unless `AllowParallelWrites` is explicitly enabled. Handlers and providers must honor context cancellation. Recreating sessions resets these per-run budgets, so applications must control session creation and apply any account-wide limits separately.

Returned errors omit private causes and bound public messages. An invocation error after execution starts is `uncertain`, because an effect may precede an error or cancellation. The framework never automatically retries it. Oversized results are also uncertain: truncating a response does not reverse its effect.

## Approval flow

1. A tool marked `RequireApproval` prepares and authorizes its arguments, stores their canonical typed JSON, and returns a pending approval ID and digest without invoking the handler.
2. A trusted application UI shows the exact arguments, requester, tenant, operation version, expiry, and digest. Approval is not an MCP tool and cannot be granted by generated text.
3. The application's approval endpoint calls `Catalog.Approve` with that unchanged digest and a verified approver context. A nonnil `ApprovalPolicy` must explicitly authorize the approver. Missing policy denies all grants.
4. `Session.ExecuteApproval` checks the original actor and tenant, allowlist, operation, digest, and expiry. It reruns preparation and authorization; changed canonical arguments require a new approval.
5. The store atomically changes `approved` to `running` before invocation. Competing attempts cannot acquire the same approval. The normal invoker checks authorization again immediately before the handler.
6. Success and its result are persisted. Any failure after the claim, panic, lost persistence receipt, or restart with a running record is uncertain. No automatic replay is permitted. An operator must inspect the external system and reconcile the outcome before requesting new work.

The SHA-256 digest binds the record version, approval ID, tool, operation version, actor, tenant, exact canonical arguments, and expiry. It detects stale approval and changed scope. It is not a signature protecting against an attacker who controls the storage directory. Protect store access as application authority.

`MemoryStore` is ephemeral and intended for tests. `FileStore` uses a private directory, private files, a nonblocking OS ownership lock, atomic replacement, file fsync, and directory fsync. One process owns a store; process exit releases the lock. Reopening marks interrupted running receipts uncertain. This implementation supports local POSIX filesystems on macOS/Linux/BSD. Distributed deployments should implement `Store` using transactional compare-and-update in a database. It does not support shared network filesystems or claim multi-host coordination.

Approval arguments and results can contain application data. Store them only where the application's privacy policy permits, encrypt the underlying storage when needed, and supply an explicit retention/reconciliation policy. No automatic cleanup removes approval evidence.

## Model budgets and uncertain provider calls

Applications call `ReserveModelCall` before sending a model request. It admits a bounded estimated cost and turn, then writes a running receipt before provider acceptance. Costs use millionths of the application's chosen billing currency; the default estimated session budget is 1,000,000 such units. These are admission estimates, not exact billing limits.

`CompleteModelCall` records trusted provider usage and request ID. Actual cost can exceed an estimate. Admission conservatively retains every reservation and adds per-call overruns, including alongside outstanding calls; it never refunds an estimate. Later calls are denied when that committed total exceeds the budget. Integer overflow is rejected before updating receipts. `MarkModelUncertain` records a lost result after provider acceptance without refunding the turn or estimate. Restart preserves that uncertainty. Provider adapters and operator reconciliation own usage lookup and any explicit retry policy; receipts can be inspected through the trusted `Store` API. Never trust model-generated usage numbers.

## Optional MCP adapter

`adapters/mcp` is a separate module using the official MCP Go SDK, pinned in its `go.mod` and `go.sum`. It uses SDK server/client transports rather than implementing JSON-RPC. It lists only a session's tools, preserves Trest's strict input validation, returns bounded structured outcomes, and exposes no approval, shell, database, or development inspection tools.

`trestmcp.NewServer` requires `Enabled: true` and an explicit environment. Production also requires `AllowProduction: true`. Construction starts no listener. Create one server per verified actor/session, then connect only that actor's transport. The application owns authentication, session lifecycle, origin restrictions, transport request limits, and shutdown. Do not reuse one actor's server across other clients.

MCP requires object input schemas; incompatible operations are rejected when constructing the adapter. Approval pauses return a pending record. The application handles approval and execution outside the model's tool interface.

## Verification and reproducible evaluation

Run from the repository root:

```sh
go test -race ./aitool
go test ./aitool -run TestRecordedEvaluation -v
cd adapters/mcp
go test -race ./...
go vet ./...
```

`aitool/testdata/evaluation.v1.json` versions the prompt, model configuration, tool contract, proposed tool calls, and expected outcomes together. It replays recorded proposals using an in-memory application fake. Tests verify allowlists, resource scope, injected identity rejection, domain validation, and approval pauses. No real model or production operation runs. This proves protocol/enforcement behavior, not model quality.

Other tests cover denied calls never invoking handlers, tenant preservation, digest/argument/actor/expiry tampering, revocation after approval, competing claims, panic uncertainty, restart recovery, exclusive store ownership, cost/turn/time/call bounds, and default write serialization. MCP tests perform actual SDK in-memory list/call exchanges and verify hidden tools, denied calls, and pending approvals.

Live evaluation remains an application-owned experiment: use a dedicated test environment, an explicit model configuration and spend limit, application-authored assertions, and isolated data. Compare tool selection, latency, quality, and provider-reported usage separately from deterministic protocol tests. A live run is not required for normal builds.

## Separate developer MCP server

`adapters/mcp/dev` is an independent SDK server with only `trest_inspect`, `trest_check`, and optional `trest_trace`. Construction requires `Enabled`, the `local` environment, and a trusted developer principal. Production is rejected. The application must authenticate the transport and keep it dedicated to that developer; localhost alone does not authenticate anyone.

Inspect reads the frozen application model, check validates registry/contract metadata with no configured commands, and trace reads the bounded recorder. No tool executes scenarios, application operations, Go checks, shell commands, or database queries. Its tool registry is separate from application tools. SDK tests verify the separation, versioned output, rejection of supplied command arguments, required authentication, and disabled/production rejection.
