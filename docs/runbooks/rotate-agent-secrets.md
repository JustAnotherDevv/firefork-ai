# Runbook: rotate agent HMAC secrets

The in-guest agent's HMAC shared secret (ADR-0003) is generated **per
template build** and stored in the registry at
`/var/lib/firefork/registry/templates.json` under the
`agent_secret_hex` field of each entry. The secret never leaves the
host — remote snapshots in Tigris/S3 do not include it.

You normally never rotate. The exceptions:

1. The host filesystem was compromised (someone read the registry).
2. A snapshot was migrated from one host to another (the destination
   has no secret for it).
3. Routine rotation per your security policy (e.g. quarterly).

## Option A: rebuild the template

Cleanest. The build pipeline generates a fresh secret as part of the
warmup → snapshot sequence.

```sh
sudo -E /usr/local/bin/seed-template \
  --config /etc/firefork/configs/template_<name>.yaml \
  --jailer /usr/local/bin/jailer
```

The new `agent_secret_hex` replaces the old one in the registry
atomically (`registry.Put` writes to a tmp file then renames).

**Side effect:** any live forks of the **old** snapshot still work
(they were instantiated when the agent generated its in-guest secret;
the host caches that). Forks from the **new** snapshot use the new
secret. Both can coexist until the old forks are torn down.

## Option B: rotate without rebuild (emergency only)

If you cannot rebuild (network down, time pressure):

1. Stop the server:
   ```sh
   sudo systemctl stop firefork-server
   ```

2. Drop the old entry:
   ```sh
   sudo jq 'del(.entries["<template>/<version>"])' \
     /var/lib/firefork/registry/templates.json \
     > /tmp/registry.json && \
     sudo install -m 0600 /tmp/registry.json /var/lib/firefork/registry/templates.json
   ```

3. Tear down all live forks of the affected template:
   ```sh
   sudo pkill -9 firecracker      # nuclear; affects ALL forks
   sudo rm -rf /srv/jailer/firecracker/* /tmp/firefork-fork-*
   ```

4. Re-register by rebuilding (back to Option A).

5. Restart the server:
   ```sh
   sudo systemctl start firefork-server
   ```

## Option C: explicit migration to a new host

Source host → destination host, with cleanly-rotated secret:

```sh
# On source:
sudo /usr/local/bin/firefork export-snapshot \
  --template <name>/<version> \
  --out /tmp/snap.tar.gz
# (Or: just S3 upload from the registry's manifest_key.)

# On destination:
sudo /usr/local/bin/firefork import-snapshot --in /tmp/snap.tar.gz
# This restores the snapshot AND generates a fresh agent_secret on
# first boot.
```

The destination host **never sees** the source's secret. The first
fork of the restored snapshot does a `ping` to bootstrap the new
secret.

> **Note:** `export-snapshot` and `import-snapshot` aren't shipped in
> v0.1.x — they're documented here as the v0.2 plan. Today, push to
> Tigris on the source host, pull on the destination, and the registry
> entry is re-created with a fresh secret automatically.

## Audit log

Every rotation should be logged. There's no automated audit in v0.1.x;
a manual entry in your ops journal is the suggested fallback:

```
2026-05-27 14:30  rotated agent_secret for python-sci/v1
                  reason: routine quarterly
                  operator: <name>
                  registry sha (post): <sha256 of registry file>
```

(Audit log automation is a v0.3 item — `internal/auditlog` package
planned.)

## Verification

After any rotation, smoke-test:

```sh
sudo -E /usr/local/bin/fork --template <name>/<version> --count 1 --hold 5s
```

A successful `forked 1/1` plus a clean exit confirms the new secret
works end-to-end (vsock dial + HMAC verify + ack).
