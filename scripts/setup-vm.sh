#!/usr/bin/env bash
# Inside-the-VM setup. Installs:
#   T0.4 — base build tools
#   T0.5 — Go 1.23.4
#   T0.6 — Firecracker v1.10.1 + jailer + ACL on /dev/kvm
#   T0.7 — stock guest kernel vmlinux-5.10
#   T0.10 — clone firefork repo
set -euo pipefail

GO_VERSION=1.23.4
FC_VERSION=v1.10.1
ARCH=x86_64
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-5.10.225"
KERNEL_DST=/var/lib/firefork/kernels/vmlinux-5.10.225
REPO_URL="https://github.com/JustAnotherDevv/firefork-ai.git"
REPO_DST="$HOME/firefork"

log() { printf "\n\033[1;36m==> %s\033[0m\n" "$*"; }

# ---------------------------------------------------------------------
# T0.4 — base packages
# ---------------------------------------------------------------------
log "T0.4 Base build tools"
sudo apt-get update
sudo apt-get install -y git make curl jq unzip build-essential e2fsprogs acl ca-certificates

# ---------------------------------------------------------------------
# T0.5 — Go
# ---------------------------------------------------------------------
log "T0.5 Installing Go $GO_VERSION"
if ! command -v go >/dev/null 2>&1 || [[ "$(go version 2>/dev/null)" != *"go${GO_VERSION}"* ]]; then
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | sudo tar -C /usr/local -xz
fi
if [[ ! -f /etc/profile.d/go.sh ]]; then
    echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' | sudo tee /etc/profile.d/go.sh >/dev/null
    sudo chmod +x /etc/profile.d/go.sh
fi
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
go version

# ---------------------------------------------------------------------
# T0.6 — Firecracker + jailer
# ---------------------------------------------------------------------
log "T0.6 Installing Firecracker $FC_VERSION + jailer"
if ! command -v firecracker >/dev/null 2>&1 || [[ "$(firecracker --version 2>/dev/null | head -1)" != *"$FC_VERSION"* ]]; then
    tmp=$(mktemp -d)
    cd "$tmp"
    curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz" -o fc.tgz
    tar -xzf fc.tgz
    sudo mv release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH} /usr/local/bin/firecracker
    sudo mv release-${FC_VERSION}-${ARCH}/jailer-${FC_VERSION}-${ARCH}   /usr/local/bin/jailer
    sudo chmod +x /usr/local/bin/firecracker /usr/local/bin/jailer
    cd - >/dev/null
    rm -rf "$tmp"
fi
firecracker --version | head -1
jailer --version | head -1

log "Granting current user access to /dev/kvm"
sudo setfacl -m "u:${USER}:rw" /dev/kvm
ls -la /dev/kvm

# ---------------------------------------------------------------------
# T0.7 — kernel
# ---------------------------------------------------------------------
log "T0.7 Downloading guest kernel to $KERNEL_DST"
sudo mkdir -p "$(dirname "$KERNEL_DST")"
if [[ ! -f "$KERNEL_DST" ]]; then
    sudo curl -fsSL -o "$KERNEL_DST" "$KERNEL_URL"
    sudo chmod 644 "$KERNEL_DST"
fi
file "$KERNEL_DST"
ls -lh "$KERNEL_DST"

# ---------------------------------------------------------------------
# T0.10 — clone firefork
# ---------------------------------------------------------------------
log "T0.10 Cloning firefork repo to $REPO_DST"
if [[ ! -d "$REPO_DST/.git" ]]; then
    git clone "$REPO_URL" "$REPO_DST"
else
    cd "$REPO_DST" && git pull --rebase --autostash
fi
cd "$REPO_DST"
go build ./...
echo "go build OK"

log "All Phase 0 host-prep complete."
echo "Next: T0.8 (build minimal Alpine rootfs), T0.9 (smoke-boot)."
