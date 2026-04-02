# 0004. Warm-pool tiers: cold, warm, ultra-warm

- **Status:** Accepted
- **Date:** 2026-05-26

## Context

The fork-cold number (5 ms steady-state) already beats every other
sandbox primitive. But that 5 ms includes one whole subprocess spawn
(`exec /usr/local/bin/firecracker`) plus the snapshot load.
Steady-state high-RPS workloads (server scenario) pay that spawn cost
on every request even though most of it is amortisable.

We need a path that's faster than fork-cold for steady-state RPS
without giving up the isolation guarantees of fresh chroots.

## Decision

`fork.Pool` supports three reuse strategies, increasing in heat:

| Tier | Subprocess | Snapshot loaded? | Per-Fork cost |
|---|---|---|---|
| **fork-cold** | Spawned per fork | Loaded per fork | ~5 ms |
| **fork-warm** | Pre-spawned in pool | Loaded per fork (`PUT /snapshot/load`) | ~2 ms |
| **fork-ultra** | Pre-spawned + snapshot already loaded + paused | None -- single `PATCH /vm Resumed` | ~500 us |

- **fork-cold** is always available; no pool config required.
- **fork-warm** is `Optimizations{WarmPoolSize: N}`. N idle
  Firecracker processes spin in the background; `Fork()` grabs one,
  runs `/snapshot/load` + `PATCH /vm`, returns.
- **fork-ultra** is `Optimizations{WarmPoolSize: N, UltraWarmPool:
  true}`. Each slot already has the snapshot loaded and the VM paused
  at the resume entry point. `Fork()` is one `PATCH /vm Resumed`.

The pool refills proactively in the background (with a 500ms backoff
on spawn errors). Stats are exposed via `Pool.warmpool.TakeStats()` +
`RefillErrors()` for the CLI hit-rate summary.

## Alternatives considered

- **Single tier.** Simpler. Rejected: we want both demo-friendly cold
  numbers (no setup; matches what readers can reproduce in one
  command) and steady-state RPS numbers.
- **CRIU-style checkpoint-restore in userspace.** Heavier; doesn't
  compose with Firecracker's snapshot primitive cleanly.
- **Reuse forks across requests (with cleanup).** Reuse = state
  bleed, OOM creep, scheduling complexity. Killing every fork after
  one use was a deliberate isolation choice.

## Consequences

- **Positive:** Three measurable numbers -- cold-start (32 s),
  fork-cold (5 ms), fork-ultra (500 us). Each strictly faster than
  the previous tier; the user picks the trade-off.
- **Positive:** Warm-pool refill is best-effort; pool drains do not
  block `Fork()` (drain falls through to cold path).
- **Negative:** Ultra-warm holds N copies of the snapshot in memory.
  64 GiB free host RAM supports hundreds of small templates, but the
  llama-1B-Q4 template (~1.3 GiB resident) fits ~40 slots in 64 GiB.
- **Negative:** Bench harness for fork-warm + fork-ultra with the
  jailer is incomplete. The unit test
  `TestForkJailed_UltraWarmPool` passes at 495 us per fork, but the
  bench's chroot setup misses one hardlink in the warm-slot
  bootstrap. Fan-out numbers use fork-cold as the conservative number
  until the bench gap is closed.

## References

- `internal/fork/options.go` -- `Optimizations` struct + `Validate`
- `internal/fork/warmpool.go` -- `WarmPool`, `Take`, `Refill`,
  `PrepareSnapForJailedSlot`
- `internal/fork/pool.go` -- `forkOne` (path A: warm; path B: cold)
- `internal/fork/warmpool_stats_test.go` -- counter + backoff tests
