#!/usr/bin/env bash
# Fix v2: filter S3 listing to ONLY the plain vmlinux binary (no .config, no -no-acpi variant).
set -euo pipefail

DST_DIR=/var/lib/firefork/kernels
sudo mkdir -p "$DST_DIR"
sudo rm -f "$DST_DIR"/vmlinux-* "$DST_DIR"/*.config

echo "==> Listing available v1.10 5.10 kernels..."
listing=$(curl -fsSL "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/v1.10/x86_64/vmlinux-5.10&list-type=2")

# Show full set so we can sanity-check.
echo "$listing" | grep -oP '(?<=<Key>)[^<]+(?=</Key>)'

echo "==> Filtering to plain vmlinux-5.10.NNN binaries..."
key=$(echo "$listing" | grep -oP '(?<=<Key>)[^<]+(?=</Key>)' \
    | grep -E 'vmlinux-5\.10\.[0-9]+$' \
    | sort -V \
    | tail -1)

if [[ -z "$key" ]]; then
    echo "ERROR: no plain vmlinux-5.10.NNN entries in listing" >&2
    exit 1
fi
echo "Chosen: $key"

filename=$(basename "$key")
dst="$DST_DIR/$filename"

echo "==> Downloading..."
sudo curl -fsSL -o "$dst" "https://s3.amazonaws.com/spec.ccfc.min/$key"
sudo chmod 644 "$dst"

file "$dst"
ls -lh "$dst"
echo "$dst" | sudo tee /var/lib/firefork/kernel.path >/dev/null
echo "Written to /var/lib/firefork/kernel.path"
