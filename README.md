# firefork

> Memory-snapshot + copy-on-write fork primitive for Firecracker
> microVMs. Built on stock Linux + Firecracker, no custom kernel
> modules.

## What it does

Snapshot a Firecracker microVM (any state: post-boot, post-library-
import, post-model-load) to a `(memfile, state)` file pair, then fork
N copies in ~5ms each so AI agents can:

- Spawn fresh sandbox per user request without paying cold-start cost
- Roll back failed tool calls without restarting the whole task
- Branch into parallel exploration paths (MCTS-style search)
- Run N parallel sub-queries in roughly the cost of one

## Status

Phase 0-3 complete (host + boot + snapshot + vsock).
Phases 4+ (fork pool, storage, templates, benchmarks) in progress.

## Hardware

Linux + `/dev/kvm`. Firecracker doesn't run on macOS or Windows directly.
