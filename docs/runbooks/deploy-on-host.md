# Runbook: Deploy firefork on a fresh Linux host

Audience: platform / infra engineer doing a one-time provisioning of a
firefork-capable host.

Time budget: ~20 minutes for the host (Firecracker + jailer setup);
template build times vary (3 s for alpine, 30 s for llama-1B-Q4).

## 0. Pre-flight

- Linux kernel ≥ 5.10 with `/dev/kvm` present (`ls -la /dev/kvm`).
- For VMs (GCP nested-virt, Multipass + Hyper-V): confirm
  `egrep -c '(svm|vmx)' /proc/cpuinfo` ≥ 1.
- Outbound network for downloading kernel + rootfs + Firecracker.
- ~5 GiB free disk for kernel + base rootfs + sample templates.

## 1. Install Firecracker + jailer

```sh
FC_VER=v1.10.1
ARCH=$(uname -m)   # x86_64 or aarch64
RELEASE=https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VER}/firecracker-${FC_VER}-${ARCH}.tgz

curl -L "$RELEASE" | sudo tar xz -C /tmp
sudo install -m 755 /tmp/release-${FC_VER}-${ARCH}/firecracker-${FC_VER}-${ARCH} /usr/local/bin/firecracker
sudo install -m 755 /tmp/release-${FC_VER}-${ARCH}/jailer-${FC_VER}-${ARCH} /usr/local/bin/jailer

firecracker --version    # confirm
jailer --version
```

## 2. Install firefork binaries

```sh
# From a release tarball (replace VERSION with the tag you want):
VERSION=v0.1.0
curl -L https://github.com/JustAnotherDevv/firefork-ai/releases/download/${VERSION}/firefork-${VERSION}-linux-amd64.tar.gz \
  | sudo tar xz -C /tmp
sudo install -m 755 /tmp/firefork /tmp/seed-template /tmp/fork /tmp/bench /tmp/firefork-server /usr/local/bin/
```

Verify cosign signature (optional but recommended — see release notes
for the exact command).

## 3. Provision the jailer user + chroot base

```sh
git clone https://github.com/JustAnotherDevv/firefork-ai
cd firefork-ai
sudo bash scripts/setup-jailer.sh
```

This creates:

- `firefork-jail` user, uid + gid `10000`
- `/srv/jailer/firecracker/` (mode 0700, owned by `firefork-jail`)

Verify:

```sh
id firefork-jail
ls -la /srv/jailer/firecracker
```

## 4. Stage a kernel + rootfs

firefork ships scripts under `images/` that build a 5.10.223 vmlinux
and an Alpine 3.20 rootfs.ext4. Or download prebuilt artefacts to
`/var/lib/firefork/{kernels,rootfs}/`:

```sh
sudo mkdir -p /var/lib/firefork/{kernels,rootfs}
sudo wget -O /var/lib/firefork/kernels/vmlinux-5.10.223 \
  https://your-mirror/vmlinux-5.10.223
sudo wget -O /var/lib/firefork/rootfs/alpine-firefork.ext4 \
  https://your-mirror/alpine-firefork.ext4
```

## 5. Build a template

```sh
sudo -E /usr/local/bin/seed-template \
  --config /etc/firefork/configs/template_python.yaml \
  --jailer /usr/local/bin/jailer
```

The registry file lives at `/var/lib/firefork/registry/templates.json`
(mode 0600). Verify:

```sh
sudo cat /var/lib/firefork/registry/templates.json | jq '.entries | keys'
# → ["python/v1"]
```

## 6. Smoke-test a fork

```sh
sudo -E /usr/local/bin/fork --template python/v1 --count 1 --hold 5s
```

Should print a `forked 1/1 microVMs from python/v1 in wall=~6ms` line
and exit cleanly.

## 7. Run the server (production deployment)

```sh
# Generate an auth token (32 bytes hex):
sudo install -d -m 0700 /etc/firefork
openssl rand -hex 32 | sudo tee /etc/firefork/auth.token > /dev/null
sudo chmod 0600 /etc/firefork/auth.token

# Systemd unit (see scripts/firefork-server.service if shipped; otherwise:)
sudo tee /etc/systemd/system/firefork-server.service > /dev/null <<EOF
[Unit]
Description=firefork HTTP orchestrator
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/firefork/auth.token
Environment=FIREFORK_AUTH_TOKEN=
ExecStart=/usr/local/bin/firefork-server \
    --addr=:8080 \
    --jailer=/usr/local/bin/jailer \
    --registry=/var/lib/firefork/registry/templates.json \
    --log-json
Restart=on-failure
RestartSec=5s

# Hardening:
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/firefork /srv/jailer /tmp
ProtectKernelTunables=true
ProtectKernelModules=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now firefork-server
sudo systemctl status firefork-server
```

Note: `FIREFORK_AUTH_TOKEN=` should read from the env file — the
example above is illustrative. Production setups typically use
`systemctl edit` to load secrets from a vault.

## 8. Verify health

```sh
curl -s localhost:8080/healthz | jq
# → {"ok":true,"live_forks":0}
```

## Tear-down

```sh
sudo systemctl stop firefork-server
sudo pkill -9 firecracker
sudo rm -rf /tmp/firefork-fork-* /srv/jailer/firecracker/*
sudo rm /var/lib/firefork/registry/templates.json   # optional
```
