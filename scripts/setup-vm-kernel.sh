#!/usr/bin/env bash
# T0.7 kernel download (fix): query S3 listing for the latest 5.10 vmlinux
# rather than hardcoding a version that may not exist.
set -euo pipefail

DST_DIR=/var/lib/firefork/kernels
sudo mkdir -p "$DST_DIR"

echo "==> Querying S3 for available 5.10 kernels..."
key=$(curl -fsSL "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/v1.10/x86_64/vmlinux-5.10&list-type=2" \
  | grep -oP '(?<=<Key>)[^<]+(?=</Key>)' \
  | sort -V \
  | tail -1)

if [[ -z "$key" ]]; then
    echo "ERROR: no kernel keys found at prefix" >&2
    # Print full listing for debug
    curl -fsSL "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/v1.10/x86_64/" | head -50
    exit 1
fi

echo "Latest 5.10 kernel key: $key"
filename=$(basename "$key")
dst="$DST_DIR/$filename"

if [[ ! -f "$dst" ]]; then
    echo "==> Downloading https://s3.amazonaws.com/spec.ccfc.min/$key"
    sudo curl -fsSL -o "$dst" "https://s3.amazonaws.com/spec.ccfc.min/$key"
    sudo chmod 644 "$dst"
fi

file "$dst"
ls -lh "$dst"
echo "$dst" | sudo tee /var/lib/firefork/kernel.path >/dev/null
echo "Path written to /var/lib/firefork/kernel.path"
