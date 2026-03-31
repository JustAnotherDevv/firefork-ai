# Architecture Decision Records

Each ADR documents one significant design decision, the alternatives considered,
and the trade-offs accepted. The format follows
[Michael Nygard's template](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions).

ADRs are numbered sequentially and never edited after acceptance -- a
decision that gets reversed produces a new ADR that supersedes the old
one.

## Index

| # | Title | Status | Date |
|---|---|---|---|
| 0001 | [Snapshot format: `(memfile, state, manifest)`](./0001-snapshot-format.md) | Accepted | 2026-05-26 |
| 0002 | [Per-fork jailer chroot + uid drop](./0002-jailer-rollout.md) | Accepted | 2026-05-27 |
| 0003 | [HMAC-signed vsock IPC with canonical JSON](./0003-vsock-hmac.md) | Accepted | 2026-05-26 |
| 0004 | [Warm-pool tiers: cold, warm, ultra-warm](./0004-warm-pool-tiers.md) | Accepted | 2026-05-26 |
| 0005 | [HTTP orchestrator as thin reference, not new product](./0005-http-orchestrator.md) | Accepted | 2026-05-27 |

## Adding an ADR

1. Copy [`TEMPLATE.md`](./TEMPLATE.md) to `00NN-short-name.md` (next free number).
2. Status starts as `Proposed`; flip to `Accepted` once consensus is reached
   (open a PR; reviewers approve = acceptance).
3. Add a row to the index above in the same PR.
