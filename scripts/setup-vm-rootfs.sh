#!/usr/bin/env bash
# Download the prebuilt Ubuntu 22.04 rootfs + matching SSH key from
# Firecracker's CI bucket. Skips the alpine-make-rootfs build entirely
# for the v0.1 smoke test. We'll build a custom AI-workload rootfs later
# during Phase 7.
set -euo pipefail

DST=/var/lib/firefork/rootfs
sudo mkdir -p "$DST"

base="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64"
for f in ubuntu-22.04.ext4 ubuntu-22.04.id_rsa ubuntu-22.04.manifest; do
    dst="$DST/$f"
    if [[ ! -f "$dst" ]]; then
        echo "==> Downloading $f"
        sudo curl -fsSL -o "$dst" "$base/$f"
    fi
done

sudo chmod 600 "$DST/ubuntu-22.04.id_rsa"
sudo chmod 644 "$DST/ubuntu-22.04.ext4" "$DST/ubuntu-22.04.manifest"

echo
echo "==> Rootfs ready:"
ls -lh "$DST"
echo
echo "==> Manifest contents:"
cat "$DST/ubuntu-22.04.manifest" || true
