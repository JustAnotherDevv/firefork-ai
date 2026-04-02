# 0005. HTTP orchestrator as thin reference, not new product

- **Status:** Accepted
- **Date:** 2026-05-27

## Context

`fork.Pool` is a Go library. Driving it from non-Go code (Python
agent runtimes, TypeScript glue, curl-based smoke tests) requires
either:

1. CGo bindings -- adds build complexity, kills the static-binary
   story.
2. A subprocess per call -- fork CLI works but high overhead per
   request.
3. An HTTP API.

Option 3 is the obvious choice for AI-agent runtimes that already
speak HTTP to everything else (LLM APIs, vector DBs, sandbox products).

The risk: HTTP API surface area tends to grow into its own product
(authentication conventions, rate limits, multi-tenancy, billing
hooks) -- none of which firefork wants to own. Production deployments
will wire `fork.Pool` directly into their existing sandbox-runtime
HTTP path, not run firefork-server standalone.

## Decision

Ship `cmd/firefork-server` as a thin reference wrapper, deliberately
scoped:

- **One pool per process.** No tenant isolation, no multi-pool
  routing, no per-tenant cgroups. One operator, one trust domain.
- **Bearer-token auth or DEMO mode.** Single shared secret in
  `FIREFORK_AUTH_TOKEN`. No JWT, no OAuth, no per-user keys.
- **API surface frozen at v1.** `/v1/fork`, `/v1/exec`,
  `/v1/forks/{id}`, `/v1/forks`, `/v1/templates`, `/v1/metrics`,
  `/healthz`. New endpoints get a `/v2/*` prefix.
- **No business logic.** Every endpoint is a direct call into
  `internal/fork`, `internal/workload`, or `internal/template`. The
  HTTP layer adds argument validation, JSON encoding, count caps
  (256), secret caching, structured logging. Nothing else.

## Alternatives considered

- **Skip HTTP entirely.** Considered seriously. Adopted partly: the
  README says explicitly that real deployments would wire `fork.Pool`
  into their existing service, not run our server. But the reference
  impl + curl loop is too useful for the demo audience to skip.
- **gRPC.** More efficient wire format. Rejected: adds a `.proto` and
  protoc dependency that defeats the static-binary distribution story.
- **Build the HTTP layer over a public `pkg/firefork/`.** Considered.
  Rejected: the public Go API needs more bake time before a v1.0
  stability commitment. `cmd/firefork-server` is allowed to import
  `internal/` because it lives in the same module.

## Consequences

- **Positive:** Non-Go consumers (Python, TypeScript) can drive forks
  via JSON. Curl works in demos.
- **Positive:** API surface is small enough to freeze in v0.1.x. No
  back-compat anxiety for v0.2 changes that touch `fork.Pool`
  internals.
- **Positive:** The server itself is ~500 LOC. Forkable and auditable
  in one read.
- **Negative:** Embedders who want different auth (mTLS, per-request
  JWT, OIDC) have to fork the server. Acceptable: they should be
  wiring `fork.Pool` directly into their own service anyway.
- **Negative:** The OpenAPI spec
  ([`docs/api/firefork-server.openapi.yaml`](../api/firefork-server.openapi.yaml))
  is a contract -- changes to `/v1/*` need an ADR.

## References

- `cmd/firefork-server/main.go` -- implementation
- `docs/api/firefork-server.openapi.yaml` -- OpenAPI 3.1 spec
- `internal/fork.Pool.Live()` + `Pool.Release(id)` -- power `/v1/forks`
  and `DELETE /v1/forks/{id}`
