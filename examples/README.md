# firefork — Examples

Self-contained programs showing how to use firefork from different
languages and at different layers. Each example is intentionally small
(≤ 100 LOC) and runnable on a properly provisioned Linux host.

## Index

| Path | Language | What it shows |
|---|---|---|
| [`basic-fork/`](./basic-fork/) | Go | Direct `fork.Pool` use (in-tree library consumer) |
| [`http-client/go/`](./http-client/go/) | Go | HTTP client of `firefork-server` |
| [`http-client/python/`](./http-client/python/) | Python | Same, from Python |
| [`mcts-branching/`](./mcts-branching/) | Go | Showcase: fan-out N forks, gather, kill losers |

## Setup (all examples)

1. Follow [`docs/runbooks/deploy-on-host.md`](../docs/runbooks/deploy-on-host.md)
   on a Linux host.
2. Build at least one template:
   ```sh
   sudo -E bin/seed-template --config configs/template_python.yaml \
     --jailer /usr/local/bin/jailer
   ```
3. For HTTP examples: start `firefork-server` (see runbook step 7).

## Why direct-library examples live in-tree

`internal/fork.Pool` is unimportable from outside the module by Go's
rules. Importing it from a peer module (your application) is **not
supported in v0.1.x**. The `basic-fork/` example imports it directly
because it lives inside the same module — it serves as a reference for
what the API looks like, but consumers should either:

1. Drive firefork via the HTTP API (`http-client/`), or
2. Vendor the repo / wait for a stable public `firefork` Go package
   in v1.0.

See [`CONTRIBUTING.md`](../CONTRIBUTING.md) "What firefork is, what it
isn't" for the full reasoning.
