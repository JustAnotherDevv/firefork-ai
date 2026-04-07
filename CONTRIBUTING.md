# Contributing to firefork

Thanks for considering a contribution. This document covers the bare
minimum you need to land a clean PR.

## Channels

- **Bugs / features** -> [GitHub Issues](https://github.com/JustAnotherDevv/firefork-ai/issues) (templates provided)
- **Discussion / design questions** -> open a GitHub Discussion or a draft PR
- **Design context** -> [`docs/architecture/`](./docs/architecture/) (ADRs)

## What firefork is, what it isn't

firefork is **a binary + reference implementation**, not (yet) a stable
public Go library:

- `cmd/*` binaries -- public surface. CLI flags and behaviour are
  semver-guarded.
- `cmd/firefork-server` HTTP API -- public surface. The `/v1/*` prefix
  is guaranteed stable across `v0.1.x`.
- `internal/*` packages -- **not importable** from outside the module
  by design. Internal API may break in any release.

If you want to embed firefork as a Go library in your service:

1. Vendor the repo (fork or `go mod` replace), or
2. Drive firefork via the HTTP API (`firefork-server`).

A stable public `firefork` Go package is planned for `v1.0`. Until
then, don't open PRs to promote `internal/` packages to `pkg/`.

## Development setup

You need a Linux host with `/dev/kvm` and Firecracker. Tested on:

- Bare-metal Linux
- GCP `n2-standard-4` with nested virt
- Local Multipass + Hyper-V (Windows 10 Pro / 11 host)

```sh
# One-time per host:
make setup-jailer        # creates firefork-jail uid 10000

# Build everything:
make build               # -> bin/{firefork,seed-template,fork,bench,firefork-server}

# Unit tests (skip integration tests automatically without /dev/kvm):
make test

# Integration tests (need sudo + /dev/kvm):
make test-int

# Lint:
make lint                # go vet + staticcheck

# Format:
make fmt
```

## Local state lives outside the repo

`~/.firefork-state/` holds Multipass VM state including SSH private
keys. This directory must never appear inside the repo tree. CI fails
the build if anything under `multipass-data/` ever becomes tracked.

## PR conventions

### Commit messages

[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/)
format strongly preferred:

```
feat(server): add /v1/exec endpoint
fix(snapshot): bound decompression by guest RAM + 64 MiB slack
docs(readme): add HTTP API curl quickstart
chore(deps): bump aws-sdk-go-v2 to v1.41.7
test(workload): add 11 parseAck cases
```

`feat`, `fix`, `docs`, `chore`, `test`, `refactor`, `build`, `ci`, `perf`.

### What we look for in a PR

| Required | Why |
|---|---|
| Tests covering the new behaviour | Includes happy path + at least one failure case |
| `make lint` clean | `go vet` + `staticcheck` |
| `make test` passes locally on Linux | CI will catch regressions but please don't waste runs |
| For public-surface changes: README updated | Keep docs in sync with code |

### What we push back on

- New external dependencies without justification (the SDK already
  drags in a lot of transitive deps; we don't want to add more without
  reason)
- API surface added under `pkg/` or root-level (see "What firefork is"
  above)
- Changes to crypto primitives, jailer config, or vsock auth without a
  threat-model paragraph in the PR description
- PRs that include `multipass-data/` files (CI will reject; fix locally)

## Testing strategy

| Tag | Runs on | Trigger |
|---|---|---|
| `_test.go` (no tag) | Unit tests | Every push + PR via `make test` / CI |
| `//go:build integration` | Real Firecracker boot | `make test-int` locally (needs `/dev/kvm` + sudo) |

If your change is hard to unit-test (jailer interactions, snapshot
restore), prefer an integration test under `//go:build integration`
over no test at all.

## Adding a new template

Templates live in `configs/template_<name>.yaml`. Minimal example:

```yaml
name: my-template
version: v1
vcpus: 1
mem_mib: 256
kernel: /var/lib/firefork/kernels/vmlinux-5.10.223
rootfs: /var/lib/firefork/rootfs/alpine-firefork.ext4
setup:
  - apk add --no-cache curl
warmup:
  - curl -s http://localhost/health || true
warmup_sleep_ms: 200
notes: |
  What's in this template. Plain ASCII only (no em-dashes,
  no smart quotes). The HMAC vsock canonical-JSON encoder
  emits raw UTF-8 while the Python agent default is
  ensure_ascii=True; non-ASCII breaks signature verification.
```

Build it: `sudo -E bin/seed-template --config configs/template_my-template.yaml --jailer /usr/local/bin/jailer`.

## Reviewing PRs

Maintainers should:

- Pull the PR locally; `make lint test` before approving
- Squash-merge unless commit history is meaningful (rare)

## Release process

1. Tag: `git tag -a vX.Y.Z -m "release vX.Y.Z"`.
2. Push tag: `git push origin vX.Y.Z`.
3. `release.yml` workflow fires:
   - `goreleaser` cross-compiles linux/amd64 + linux/arm64
   - Generates tarballs, sha256, SBOMs (syft)
   - Cosign signs all artifacts (keyless via Sigstore)
   - Creates GitHub Release with auto-generated changelog
4. Smoke-test the release tarball on a fresh Linux host.

## License

By contributing, you agree your contributions are licensed under the
project's [MIT License](./LICENSE).
