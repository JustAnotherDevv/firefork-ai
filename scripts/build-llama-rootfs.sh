#!/usr/bin/env bash
# Build the AI workload rootfs for Phase 7:
#
#   /var/lib/firefork/rootfs/ubuntu-llama.ext4
#
# Derived from ubuntu-22.04-firefork.ext4 (Ubuntu CI image + firefork
# agent). Bakes in:
#   - llama.cpp prebuilt CPU binaries at /opt/llama/bin/{llama-cli,llama-server}
#   - Llama-3.2-1B-Instruct Q4_K_M (GGUF) at /opt/models/llama-3.2-1b-q4.gguf
#
# Why Ubuntu (not Alpine): llama.cpp ships glibc prebuilt binaries.
# Musl/Alpine would require compiling from source in-VM (~15 min,
# ~2 GiB peak RAM during link), which is fragile on the 4 GiB host VM.
#
# Run as the unprivileged user inside the host VM:
#   bash scripts/build-llama-rootfs.sh
set -euo pipefail

SRC=/var/lib/firefork/rootfs/ubuntu-22.04-firefork.ext4
DST=/var/lib/firefork/rootfs/ubuntu-llama.ext4
EXTRA_MB=1600   # +1.6 GiB for llama + model + headroom

LLAMA_RELEASE="${LLAMA_RELEASE:-}"   # set to e.g. b9351 to pin; empty = latest

MODEL_URL="${MODEL_URL:-https://huggingface.co/bartowski/Llama-3.2-1B-Instruct-GGUF/resolve/main/Llama-3.2-1B-Instruct-Q4_K_M.gguf}"
MODEL_NAME=llama-3.2-1b-q4.gguf

TMP=$(mktemp -d)
trap 'sudo umount "$TMP/mnt" 2>/dev/null || true; rm -rf "$TMP"' EXIT

log() { printf "\n\033[1;36m==> %s\033[0m\n" "$*"; }

[[ -f "$SRC" ]] || { echo "missing source rootfs: $SRC" >&2; exit 1; }

# ---------------------------------------------------------------------
# 1. Copy + grow the rootfs.
# ---------------------------------------------------------------------
log "Copying $SRC -> $DST"
sudo cp --reflink=auto "$SRC" "$DST"

log "Growing $DST by ${EXTRA_MB} MiB"
sudo truncate -s "+${EXTRA_MB}M" "$DST"
sudo e2fsck -fy "$DST" >/dev/null
sudo resize2fs "$DST" >/dev/null
echo "  -> $(stat -c'%s' "$DST" | numfmt --to=iec)"

# ---------------------------------------------------------------------
# 2. Resolve llama.cpp release tag.
# ---------------------------------------------------------------------
if [[ -z "$LLAMA_RELEASE" ]]; then
    log "Resolving latest llama.cpp release from github.com/ggml-org/llama.cpp"
    LLAMA_RELEASE=$(curl -fsSL https://api.github.com/repos/ggml-org/llama.cpp/releases/latest \
                    | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name": ?"([^"]+)".*/\1/')
fi
[[ -n "$LLAMA_RELEASE" ]] || { echo "could not resolve llama.cpp release tag" >&2; exit 1; }
LLAMA_ARCHIVE="llama-${LLAMA_RELEASE}-bin-ubuntu-x64.tar.gz"
LLAMA_URL="https://github.com/ggml-org/llama.cpp/releases/download/${LLAMA_RELEASE}/${LLAMA_ARCHIVE}"
echo "  -> $LLAMA_RELEASE @ $LLAMA_URL"

# ---------------------------------------------------------------------
# 3. Download llama.cpp + model to TMP.
# ---------------------------------------------------------------------
log "Downloading llama.cpp ($LLAMA_ARCHIVE)"
curl -fsSL -o "$TMP/llama.tar.gz" "$LLAMA_URL"
echo "  -> $(stat -c'%s' "$TMP/llama.tar.gz" | numfmt --to=iec)"

log "Downloading $MODEL_NAME from HuggingFace"
echo "  url: $MODEL_URL"
curl -fL --progress-bar -o "$TMP/$MODEL_NAME" "$MODEL_URL"
echo "  -> $(stat -c'%s' "$TMP/$MODEL_NAME" | numfmt --to=iec)"

# ---------------------------------------------------------------------
# 4. Mount the destination rootfs and install binaries + model.
# ---------------------------------------------------------------------
log "Mounting $DST and installing artefacts"
mkdir -p "$TMP/mnt"
sudo mount -o loop "$DST" "$TMP/mnt"

# 4a. llama.cpp binaries → /opt/llama/bin
sudo mkdir -p "$TMP/mnt/opt/llama/bin"
mkdir -p "$TMP/llama-extract"
tar -xzf "$TMP/llama.tar.gz" -C "$TMP/llama-extract"
# llama.cpp's Ubuntu tarball layout is flat: a single top-level dir
# (llama-bXXXX/) containing binaries + .so files side by side. Find
# the dir that holds llama-cli; everything else lives next to it.
LLAMA_BIN_DIR=$(find "$TMP/llama-extract" -name llama-cli -printf '%h\n' 2>/dev/null | head -1)
[[ -n "$LLAMA_BIN_DIR" ]] || { echo "could not find llama-cli in extracted archive" >&2; ls -la "$TMP/llama-extract"; exit 1; }
echo "  extracted dir: $LLAMA_BIN_DIR"

# Copy llama-cli, llama-server.
for f in llama-cli llama-server; do
    [[ -f "$LLAMA_BIN_DIR/$f" ]] || { echo "missing $f in release archive" >&2; ls -la "$LLAMA_BIN_DIR"; exit 1; }
    sudo install -Dm755 "$LLAMA_BIN_DIR/$f" "$TMP/mnt/opt/llama/bin/$f"
done
# Copy required shared libs (libllama.so, libggml*.so, libmtmd.so, etc.).
shopt -s nullglob
for f in "$LLAMA_BIN_DIR"/*.so*; do
    [[ -e "$f" ]] || continue
    sudo install -Dm644 "$f" "$TMP/mnt/opt/llama/bin/$(basename "$f")"
done
shopt -u nullglob

# 4b. Model → /opt/models
sudo mkdir -p "$TMP/mnt/opt/models"
sudo install -Dm644 "$TMP/$MODEL_NAME" "$TMP/mnt/opt/models/$MODEL_NAME"

# 4c. /opt/llama/bin on PATH + LD_LIBRARY_PATH for in-guest convenience.
sudo tee "$TMP/mnt/etc/profile.d/llama.sh" >/dev/null <<'EOF'
export PATH=/opt/llama/bin:$PATH
export LD_LIBRARY_PATH=/opt/llama/bin:${LD_LIBRARY_PATH:-}
EOF
sudo chmod 0755 "$TMP/mnt/etc/profile.d/llama.sh"

# Marker so we can sanity-check the install from outside.
echo "$LLAMA_RELEASE $(date -u -Iseconds)" | sudo tee "$TMP/mnt/opt/llama/VERSION" >/dev/null

sudo umount "$TMP/mnt"

# ---------------------------------------------------------------------
# 5. Report.
# ---------------------------------------------------------------------
log "Done."
echo "  rootfs:  $DST  ($(stat -c'%s' "$DST" | numfmt --to=iec))"
echo "  llama:   $LLAMA_RELEASE"
echo "  model:   $MODEL_NAME"
echo
echo "Next: build the template snapshot:"
echo "  bin/seed-template --config configs/template_llama.yaml --verbose"
