# 0001. Snapshot format: `(memfile, state, manifest)`

- **Status:** Accepted
- **Date:** 2026-05-26

## Context

A firefork snapshot must capture enough of a running Firecracker microVM to
restore a byte-identical clone — including guest RAM, CPU registers, KVM
state, and emulated-device state. The format also has to:

- Travel over the network (object storage; CDN-friendly)
- Stream out of S3 in parallel ranges (multi-GiB files; one-shot download
  must saturate the host's NIC)
- Verify on Load without trusting the bucket
- Compress without unbounded memory on the decoder side (untrusted bucket)
- Stay backward-compatible enough that an older orchestrator can still
  reject newer snapshots cleanly

## Decision

A snapshot is a bundle of **three artefacts** sharing a key prefix:

```
<key>/memfile.bin          # raw guest RAM, optionally zstd-compressed
<key>/memfile.bin.zst      # if compressed
<key>/state.bin            # Firecracker SDK state (CPU + device emulation)
<key>/manifest.yaml        # SHA-256 digests, schema version, sizes, build metadata
```

- `memfile.bin` — host-side dump of guest RAM. Restored with `MAP_PRIVATE` so
  N forks share the host page cache while keeping per-fork CoW writes
  isolated.
- `state.bin` — Firecracker's snapshot state file (binary, SDK-defined).
- `manifest.yaml` — schema version, sizes, SHA-256 digests, agent secret
  reference (locally only — see ADR-0003), template metadata.

Optional zstd compression for `memfile.bin` (suffix `.zst`). Decompression is
gated by an `io.LimitReader` capped at guest RAM + 64 MiB slack.

## Alternatives considered

- **Single tarball.** Simpler, but defeats parallel ranged downloads and
  forces full read-before-verify of multi-GiB memfiles.
- **memfile alone (state embedded).** Considered for compactness. Rejected:
  Firecracker SDK consumes the state file via a path, not a reader, and
  re-encoding adds complexity for zero benefit.
- **userfaultfd page-fault backend.** Lazier paging, no upfront memfile copy.
  Out of scope for v0.1 (more moving parts; the MAP_PRIVATE story is good
  enough for the demo numbers and most workloads).
- **Custom binary format for manifest.** Rejected — YAML is plenty fast
  for ~1 KiB manifests, and humans can read it during incident response.

## Consequences

- **Positive:** Manifest is human-readable; SHA verification happens
  before decompression; parallel ranged downloads via `transfermanager`
  saturate the link.
- **Positive:** The `SchemaVersion` field on the manifest lets a v0.1
  orchestrator reject a v0.2 snapshot cleanly instead of crashing
  mid-restore.
- **Negative:** Three artefacts means three S3 round-trips for a cold cache
  (mitigated by `Storage.Head` for the manifest first, then parallel
  download of `memfile` while `state` streams in).
- **Negative:** zstd compression is optional; some templates ship
  uncompressed when CPU is the bottleneck and link bandwidth is plentiful.

## References

- `internal/snapshot/manifest.go` -- Manifest struct + SchemaVersion handling
- `internal/snapshot/store.go` -- Load path: SHA verify -> decompress -> restore
