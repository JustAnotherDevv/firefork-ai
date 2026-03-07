#!/usr/bin/env bash
# Build a derived rootfs from Firecracker CI's ubuntu-22.04.ext4 by
# baking the firefork agent (agent.py + systemd unit) into it.
#
# Run inside the firefork host VM:
#   sudo bash scripts/build-rootfs.sh
#
# Output: /var/lib/firefork/rootfs/ubuntu-22.04-firefork.ext4
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
SRC=/var/lib/firefork/rootfs/ubuntu-22.04.ext4
DST=/var/lib/firefork/rootfs/ubuntu-22.04-firefork.ext4
AGENT="$ROOT_DIR/images/agent.py"
UNIT="$ROOT_DIR/images/firefork-agent.service"

[[ -f "$SRC" ]]   || { echo "missing $SRC"; exit 1; }
[[ -f "$AGENT" ]] || { echo "missing $AGENT"; exit 1; }
[[ -f "$UNIT" ]]  || { echo "missing $UNIT"; exit 1; }

echo "==> Copying $SRC -> $DST"
sudo cp "$SRC" "$DST"

# Resize the image by +50 MB so we have room for agent + systemd unit
# and any package installs needed later.
echo "==> Resizing rootfs to give room for agent + future installs"
sudo truncate -s +50M "$DST"
sudo e2fsck -fy "$DST" >/dev/null
sudo resize2fs "$DST"

mnt=$(mktemp -d)
echo "==> Mounting $DST at $mnt"
sudo mount -o loop "$DST" "$mnt"
trap 'sudo umount "$mnt" >/dev/null 2>&1 || true; rmdir "$mnt" >/dev/null 2>&1 || true' EXIT

echo "==> Installing agent.py into rootfs"
sudo install -Dm755 "$AGENT" "$mnt/usr/local/bin/agent.py"

echo "==> Installing systemd unit"
sudo install -Dm644 "$UNIT" "$mnt/etc/systemd/system/firefork-agent.service"

# Enable the service by creating the wants symlink directly (the
# rootfs isn't booted so we can't call systemctl).
sudo mkdir -p "$mnt/etc/systemd/system/multi-user.target.wants"
sudo ln -sf /etc/systemd/system/firefork-agent.service \
    "$mnt/etc/systemd/system/multi-user.target.wants/firefork-agent.service"

echo "==> Verifying installed files"
ls -la "$mnt/usr/local/bin/agent.py" "$mnt/etc/systemd/system/firefork-agent.service"

echo "==> Unmounting"
sudo umount "$mnt"
rmdir "$mnt"
trap - EXIT

echo
echo "==> Done. Rootfs ready at $DST ($(stat -c%s "$DST" | numfmt --to=iec))"
echo "    Use cfg.RootFSPath = \"$DST\" to boot a VM with the firefork agent."
