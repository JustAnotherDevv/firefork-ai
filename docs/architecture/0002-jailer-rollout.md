# 0002. Per-fork jailer chroot + uid drop

- **Status:** Accepted
- **Date:** 2026-05-27

## Context

Pre-jailer firefork ran every fork as the same uid (whatever launched
`bin/fork`) with full host filesystem access. A compromised guest doing
a host-escape via vsock or Firecracker exploit would read `/etc/shadow`,
poke `/proc/self/mem`, etc.

Production cannot ship that posture. The fix has to:

- Confine each fork to its own filesystem view
- Run as a non-root, non-privileged uid distinct from the orchestrator
- Not regress the headline fork-cold number (5 ms)
- Be migratable from existing snapshots (which embed `vsock.sock` host
  paths at build time)

## Decision

Each fork launches under `/usr/local/bin/jailer` with:

- Fresh chroot at `/srv/jailer/firecracker/<fork-id>/root/`
- UID + GID dropped to `firefork-jail` (uid 10000) on a separate
  account from the orchestrator
- Required snapshot files **hardlinked** into the chroot at canonical
  paths (`/memfile.bin`, `/state.bin`, `/vsock.sock`, `/vmlinux`,
  `/rootfs.ext4`) — see `fc.DefaultChrootLayout()`.
- `vsock.sock` referenced inside the snapshot is a **chroot-relative**
  path (`/vsock.sock`), not a host absolute path. This is the migration
  blocker for legacy snapshots.

Jailer is **opt-in** in v0.1.x via `--jailer` (CLI), `Builder.Jailer`
(library), or `fork.WithJailer` (server flag). Default-off retains the
legacy non-jailed path for templates built before the chroot-relative
fix. Default-on flip planned for v0.3.

## Alternatives considered

- **gVisor.** User-space kernel emulation. Adds latency (microsecond
  syscall overhead × millions of syscalls during model load) and an extra
  trust boundary. Rejected for v0.1.
- **AppArmor / seccomp profiles only.** Confine without chroot. Less
  defense-in-depth; doesn't address filesystem visibility.
- **Container runtime (runc, crun).** Heavier setup; competes with
  Firecracker's own isolation story. Rejected as overlap.
- **Custom seccomp filters in Firecracker itself.** Firecracker already
  has built-in seccomp; jailer adds the chroot + uid layer on top of it,
  which is the more impactful addition.

## Consequences

- **Positive:** Host `/etc/shadow` and `/proc/self/mem` unreachable from
  a compromised guest (verified via `scripts/diag-jailer.sh`).
- **Positive:** Each fork's chroot is independently torn down on Shutdown;
  no cross-fork file leakage.
- **Positive:** `fork-cold` measurement at 5 ms held — jailer cost is
  one-time chroot setup (sub-ms), not per-syscall overhead.
- **Negative:** Legacy snapshots built before v0.2.x must be **rebuilt**
  to migrate to jailer (vsock UDS path was previously a host absolute
  path; chroot-relative resolution requires a new builder). Old snapshots
  fail to restore inside a chroot.
- **Negative:** Warm-pool with jailer is not yet exercised by the bench
  harness (the `TestForkJailed_UltraWarmPool` unit test passes at 495 µs,
  but the bench's chroot-setup gap means fan-out plots use the
  fork-cold mode). Tracked as a v0.3 closeout item.
- **Negative:** `fork.Pool.Shutdown()` and `Release(id)` now need
  `fc.CleanupChroot` calls in addition to `RemoveAll`. Documented in
  the pool code.

## References

- `internal/fc/jailer.go`, `internal/fc/chroot.go`
- `internal/fork/pool.go` — `forkOneJailed`
- `scripts/setup-jailer.sh` — host bootstrap (creates `firefork-jail` user)
- `scripts/diag-jailer.sh` — `/proc/<pid>/root` inspection helper
