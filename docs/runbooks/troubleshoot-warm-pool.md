# Runbook: troubleshoot the warm pool

The warm pool is the source of most fork-latency variance. This is
the fix-it-fast playbook for the failure modes we've seen during
bench harness work.

## Symptom: every fork takes ~5 ms instead of ~500 µs (ultra-warm path)

The pool is **draining** — every `Take()` returns nil and the cold path
runs. Check the hit-rate summary in the CLI:

```
warm-pool: 0 hits / 32 drains (0.0% hit-rate); refill_errs=12
```

`refill_errs > 0` is the key signal. The refill goroutine can't spawn
new slots fast enough.

### Diagnostic

```sh
sudo journalctl -u firefork-server -n 200 | grep -i 'warm pool init\|refill\|spawn'
```

### Common causes

- **`/srv/jailer/firecracker/` permissions wrong.** `firefork-jail`
  user can't `mkdir` per-slot chroots. Fix: `sudo chown -R
  firefork-jail:firefork-jail /srv/jailer/firecracker; sudo chmod 0700
  /srv/jailer/firecracker`.
- **Kernel + rootfs paths missing in chroot.** Each slot needs
  hardlinks for `vmlinux`, `rootfs.ext4`. If you renamed those after
  building templates, fix the paths in `configs/template_*.yaml` and
  rebuild.
- **`firefork-jail` uid mismatch.** Setup-jailer assumed uid 10000;
  other configs (e.g. NSS LDAP) may have collided. Verify: `id
  firefork-jail`. If the uid drifted, rebuild templates with
  `--jailer-uid=<actual-uid>`.

## Symptom: `firecracker exited unexpectedly` mid-restore

The slot spawn succeeded but the snapshot load crashes the Firecracker
process. This was the bench harness gap we documented in v0.1.

### Diagnostic

```sh
# 1. Find a crashed slot's chroot:
sudo ls -lt /srv/jailer/firecracker/ | head -5

# 2. Inspect the FC API socket directory:
SLOT=<id-from-above>
sudo ls -la /srv/jailer/firecracker/$SLOT/root/

# Expected files inside the chroot root:
#   memfile.bin   (hardlink to snapshot memfile)
#   state.bin     (hardlink to snapshot state)
#   vsock.sock    (will be created by FC at runtime)
#   vmlinux       (hardlink to kernel)
#   rootfs.ext4   (hardlink to rootfs)
```

### Common causes

- **Missing `rootfs.ext4` or `vmlinux` hardlink.** Snapshots embed paths
  to these at build time. For jailed slots, they must be inside the
  chroot. Check `internal/fc/jailer.go` `DefaultChrootLayout()` for the
  canonical list. Missing files = `Virtio backend error: backing file
  /rootfs.ext4`.
- **Snapshot was built without `--jailer`.** Pre-jailer snapshots embed
  the host absolute path for `vsock.sock`, which won't resolve inside
  the chroot. Rebuild the template with `seed-template --jailer ...`.
- **Multipass on Windows host: residual file locks.** After a crashed
  run, Multipass-internal SSH-key files inside `multipass-data/` can
  remain locked, blocking `go mod tidy` and triggering `Access is
  denied` on adjacent operations. Mitigation: stop multipassd
  (`sudo systemctl stop snap.multipass.multipassd`), retry, then start
  it back up. See `multipass-data/go.mod` for the build-tooling
  shielding we ship.

## Symptom: `vsock CONNECT: dial …vsock.sock: no such file`

The host can't find the vsock UDS at the expected path inside the
chroot. Usually means the snapshot embedded a host absolute path
instead of the chroot-relative `/vsock.sock`.

### Fix

Rebuild the template under a jailered builder:

```sh
sudo -E /usr/local/bin/seed-template \
  --config configs/template_<name>.yaml \
  --jailer /usr/local/bin/jailer
```

The new builder writes `/vsock.sock` into the snapshot state file
(`internal/fc/snapshot.go` confirms via a chroot-rel path check).

## Symptom: `vsock CONNECT: ack: peer rejected`

The in-guest agent rejected the HMAC signature. Either:

- The host is using the wrong shared secret (registry corruption?).
- The command contains non-ASCII bytes (em-dash, smart quotes) that the
  Python agent's `ensure_ascii=True` encodes differently from Go's raw
  UTF-8. See ADR-0003.

### Fix

```sh
# Verify the registry still has the secret:
sudo jq -r '.entries["<template>/<version>"].agent_secret_hex' \
  /var/lib/firefork/registry/templates.json

# If empty, rebuild the template (regenerates a fresh secret).
# If present, scan your code for non-ASCII characters before they hit
# vsock.Call().
```

## Symptom: `refill backoff exhausted; slot starved`

Every spawn has failed for ≥ 5 attempts with backoff
(`refillBackoff = 500ms`). The pool will keep retrying but the bench
report will show 100% drains.

### Diagnostic

```sh
sudo journalctl -u firefork-server -n 500 | grep 'refill_err'
```

Look for the underlying cause:

- `permission denied` → host filesystem perms (step 3 above)
- `no space left on device` → `/srv/jailer/` partition full
- `firecracker: exec format error` → wrong binary architecture
- `address already in use` → `--addr` port collision

### Reset

```sh
sudo systemctl restart firefork-server
# or, for a CLI-driven run:
sudo pkill -9 firecracker firefork
sudo rm -rf /srv/jailer/firecracker/* /tmp/firefork-fork-*
```

## Always-on observability

The server exposes counters at `/v1/metrics` (auth required). Wire them
to Prometheus via a simple scrape job:

```yaml
scrape_configs:
  - job_name: firefork
    metrics_path: /v1/metrics
    static_configs:
      - targets: ['localhost:8080']
    authorization:
      type: Bearer
      credentials_file: /etc/firefork/auth.token
```

(Note: `/v1/metrics` returns JSON, not Prometheus text. A small
Prom-format adapter is a v0.3 todo if needed.)
