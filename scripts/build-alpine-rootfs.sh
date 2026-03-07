#!/usr/bin/env bash
# Build TWO Alpine-based rootfs images for firefork:
#   /var/lib/firefork/rootfs/alpine-base.ext4      — no agent
#                                                    (for Phase 4 fork tests that don't want vsock)
#   /var/lib/firefork/rootfs/alpine-firefork.ext4  — agent + OpenRC service enabled
#                                                    (for Phase 1/2/3 boot/vsock/snapshot tests)
#
# Why Alpine: ~50-80 MB instead of 300 MB Ubuntu. Smaller memfile
# snapshots, less storage, identical fork latency budget.
#
# Run as the unprivileged user inside the host VM (script will sudo
# as needed):
#   bash scripts/build-alpine-rootfs.sh
set -euo pipefail

ALPINE_BRANCH=v3.20
ALPINE_PACKAGES="openrc busybox-openrc python3 py3-pip ca-certificates"
ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
DST_DIR=/var/lib/firefork/rootfs
SIZE_MB=200      # 200 MiB ext4; plenty of headroom (agent is ~5 KiB).
BASE_OUT="$DST_DIR/alpine-base.ext4"
AGENT_OUT="$DST_DIR/alpine-firefork.ext4"
AGENT="$ROOT_DIR/images/agent.py"
INITD="$ROOT_DIR/images/agent.openrc"
TMP=$(mktemp -d)
trap 'sudo umount "$TMP/mnt" 2>/dev/null || true; rm -rf "$TMP"' EXIT

log() { printf "\n\033[1;36m==> %s\033[0m\n" "$*"; }

sudo mkdir -p "$DST_DIR"
sudo apt-get install -y -qq e2fsprogs >/dev/null

# ---------------------------------------------------------------------
# 1. Download alpine-make-rootfs into a temp dir.
# ---------------------------------------------------------------------
log "Fetching alpine-make-rootfs"
curl -fsSL "https://raw.githubusercontent.com/alpinelinux/alpine-make-rootfs/v0.7.0/alpine-make-rootfs" \
    -o "$TMP/alpine-make-rootfs"
chmod +x "$TMP/alpine-make-rootfs"

# ---------------------------------------------------------------------
# 2. Build the BASE Alpine rootfs (no agent).
# ---------------------------------------------------------------------
log "Building $BASE_OUT (Alpine $ALPINE_BRANCH + $ALPINE_PACKAGES)"
mkdir -p "$TMP/mnt"

# blank ext4 image, then loop-mount and have alpine-make-rootfs
# bootstrap into the mount point.
sudo truncate -s ${SIZE_MB}M "$TMP/base.img"
sudo mkfs.ext4 -F "$TMP/base.img" >/dev/null
sudo mount -o loop "$TMP/base.img" "$TMP/mnt"
sudo "$TMP/alpine-make-rootfs" \
    --branch="$ALPINE_BRANCH" \
    --packages="$ALPINE_PACKAGES" \
    "$TMP/mnt"

# Enable serial console (Firecracker uses ttyS0) so we can see boot.
sudo tee "$TMP/mnt/etc/inittab" >/dev/null <<'EOF'
::sysinit:/sbin/openrc sysinit
::sysinit:/sbin/openrc boot
::wait:/sbin/openrc default

# serial console
ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100

::shutdown:/sbin/openrc shutdown
EOF

# Auto-login root on ttyS0 so the test can see "login:" or shell prompt.
sudo mkdir -p "$TMP/mnt/etc/profile.d"

sudo umount "$TMP/mnt"
sudo install -Dm644 "$TMP/base.img" "$BASE_OUT"
sudo rm -f "$TMP/base.img"
echo "  -> $(stat -c'%s' "$BASE_OUT" | numfmt --to=iec) at $BASE_OUT"

# ---------------------------------------------------------------------
# 3. Build the AGENT variant by deriving from base.
# ---------------------------------------------------------------------
log "Building $AGENT_OUT (base + firefork agent + OpenRC service enabled)"
sudo cp "$BASE_OUT" "$AGENT_OUT"
# Resize +20 MB for safety (won't actually grow much).
sudo truncate -s +20M "$AGENT_OUT"
sudo e2fsck -fy "$AGENT_OUT" >/dev/null
sudo resize2fs "$AGENT_OUT" >/dev/null

sudo mount -o loop "$AGENT_OUT" "$TMP/mnt"

# Install agent script + OpenRC service file + enable at default runlevel.
sudo install -Dm755 "$AGENT" "$TMP/mnt/usr/local/bin/agent.py"
sudo install -Dm755 "$INITD" "$TMP/mnt/etc/init.d/firefork-agent"
sudo mkdir -p "$TMP/mnt/etc/runlevels/default"
sudo ln -sf /etc/init.d/firefork-agent "$TMP/mnt/etc/runlevels/default/firefork-agent"

# Verify install.
ls -la "$TMP/mnt/usr/local/bin/agent.py" "$TMP/mnt/etc/init.d/firefork-agent" \
       "$TMP/mnt/etc/runlevels/default/firefork-agent"

sudo umount "$TMP/mnt"
echo "  -> $(stat -c'%s' "$AGENT_OUT" | numfmt --to=iec) at $AGENT_OUT"

# ---------------------------------------------------------------------
# 4. Report
# ---------------------------------------------------------------------
log "Done."
ls -lh "$BASE_OUT" "$AGENT_OUT"
echo
echo "For tests:"
echo "  fork tests:           FIREFORK_ROOTFS=$BASE_OUT"
echo "  boot/snap/vsock tests: FIREFORK_ROOTFS_AGENT=$AGENT_OUT"
