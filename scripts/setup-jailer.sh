#!/usr/bin/env bash
# Provision the host for firefork's jailer-based isolation.
#
# Creates a single shared `firefork-jail` user (uid=10000) that every
# jailed Firecracker drops privileges to, plus the /srv/jailer base
# directory the jailer uses as its chroot root.
#
# Per-UID rotation (one user per fork) is deferred to v0.3 alongside
# network namespaces + cgroup limits. v0.2.2 uses a single uid; chroot
# is what does the isolating, not the uid uniqueness.
#
# Idempotent: safe to rerun.
#
# Run on the multipass host VM:
#   sudo bash scripts/setup-jailer.sh

set -euo pipefail

JAIL_USER="${FIREFORK_JAIL_USER:-firefork-jail}"
JAIL_UID="${FIREFORK_JAIL_UID:-10000}"
JAIL_GID="${FIREFORK_JAIL_GID:-10000}"
JAIL_BASE="${FIREFORK_JAIL_BASE:-/srv/jailer}"

log() { printf "\n\033[1;36m==> %s\033[0m\n" "$*"; }

if [[ $EUID -ne 0 ]]; then
    echo "must run as root (try: sudo $0)" >&2
    exit 1
fi

# ---------------------------------------------------------------------
# 1. jailer + firecracker binaries present?
# ---------------------------------------------------------------------
log "Checking required binaries"
for bin in /usr/local/bin/jailer /usr/local/bin/firecracker; do
    if [[ ! -x "$bin" ]]; then
        echo "missing $bin — install firecracker first" >&2
        exit 1
    fi
    printf "  %s -> %s\n" "$bin" "$($bin --version 2>&1 | head -1 || true)"
done

# ---------------------------------------------------------------------
# 2. firefork-jail user (uid=10000) — idempotent.
# ---------------------------------------------------------------------
log "Ensuring $JAIL_USER user (uid=$JAIL_UID)"
if id -u "$JAIL_USER" >/dev/null 2>&1; then
    existing_uid=$(id -u "$JAIL_USER")
    if [[ "$existing_uid" -ne "$JAIL_UID" ]]; then
        echo "user $JAIL_USER exists with uid=$existing_uid, expected $JAIL_UID" >&2
        exit 1
    fi
    printf "  already present (uid=%s)\n" "$existing_uid"
else
    useradd \
        --no-create-home \
        --shell /sbin/nologin \
        --uid "$JAIL_UID" \
        --user-group \
        "$JAIL_USER"
    printf "  created uid=%s\n" "$JAIL_UID"
fi

# ---------------------------------------------------------------------
# 3. /srv/jailer base directory — owned by firefork-jail.
# ---------------------------------------------------------------------
log "Ensuring $JAIL_BASE base directory"
mkdir -p "$JAIL_BASE"
# 0o755 so the privilege-dropped jailer process can traverse + create
# per-instance chroots underneath. Each per-fork chroot below gets
# locked down individually by PrepareChroot (0o700 + chown).
chmod 0755 "$JAIL_BASE"
chown "$JAIL_USER":"$JAIL_USER" "$JAIL_BASE"
printf "  %s -> owner=%s mode=%s\n" "$JAIL_BASE" "$JAIL_USER" "0755"

# ---------------------------------------------------------------------
# 4. /dev/kvm + /dev/net/tun reachable for jailed firecracker?
# ---------------------------------------------------------------------
log "Sanity-checking /dev/kvm + /dev/net/tun perms"
for dev in /dev/kvm /dev/net/tun; do
    if [[ -e "$dev" ]]; then
        # Jailer bind-mounts these into the chroot. As long as the host
        # node exists and is readable by the kvm group (typically),
        # the jailed firecracker will work.
        ls -l "$dev"
    else
        echo "  WARN: $dev missing — guest boot will fail"
    fi
done

# ---------------------------------------------------------------------
# 5. Report.
# ---------------------------------------------------------------------
log "Done."
echo "  jail user:   $JAIL_USER (uid=$JAIL_UID, gid=$JAIL_GID)"
echo "  chroot base: $JAIL_BASE (owner=$JAIL_USER, mode=0755)"
echo
echo "Use this UID/GID in your fork.Request.Jailer / Builder.Jailer config."
