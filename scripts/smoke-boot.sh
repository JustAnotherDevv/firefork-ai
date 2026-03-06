#!/usr/bin/env bash
# Smoke-boot a Firecracker microVM by hand to confirm the host stack
# works end-to-end. Prints the guest serial console and exits when the
# guest reaches the login prompt (or after 30 s).
set -euo pipefail

KERNEL=/var/lib/firefork/kernels/vmlinux-5.10.223
ROOTFS=/var/lib/firefork/rootfs/ubuntu-22.04.ext4
SOCK=/tmp/firecracker-smoke.sock
CONFIG=/tmp/firecracker-smoke.json
LOG=/tmp/firecracker-smoke.log

rm -f "$SOCK" "$CONFIG" "$LOG"

# Use a writable copy of the rootfs so the smoke run doesn't dirty the
# original.
ROOTFS_COPY=/tmp/smoke-rootfs.ext4
cp "$ROOTFS" "$ROOTFS_COPY"

cat > "$CONFIG" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off"
  },
  "drives": [{
    "drive_id": "rootfs",
    "path_on_host": "$ROOTFS_COPY",
    "is_root_device": true,
    "is_read_only": false
  }],
  "machine-config": {
    "vcpu_count": 1,
    "mem_size_mib": 256
  }
}
EOF

echo "==> launching firecracker in background, serial -> $LOG"
firecracker --api-sock "$SOCK" --config-file "$CONFIG" > "$LOG" 2>&1 &
FC_PID=$!

# Give it up to 30 s to reach a recognisable serial milestone.
deadline=$((SECONDS + 30))
booted=0
while [ $SECONDS -lt $deadline ]; do
    if [ -s "$LOG" ] && grep -qE '(login:|Welcome|systemd|Started)' "$LOG"; then
        booted=1
        break
    fi
    sleep 0.5
done

echo
echo "==> serial output so far:"
echo "----"
tail -40 "$LOG"
echo "----"

# Kill firecracker and clean up.
kill -TERM $FC_PID 2>/dev/null || true
wait $FC_PID 2>/dev/null || true
rm -f "$SOCK" "$ROOTFS_COPY"

if [ "$booted" = "1" ]; then
    echo
    echo "==> SMOKE TEST PASSED: microVM reached login / systemd."
    exit 0
else
    echo
    echo "==> SMOKE TEST FAILED: no login banner in serial output within 30 s."
    exit 1
fi
