# firefork — Memory-snapshot fork primitive for Firecracker microVMs
#
# Usage:
#   make build           # build all cmd binaries
#   make test            # unit tests (skips integration tests without /dev/kvm)
#   make test-int        # integration tests (requires sudo + /dev/kvm)
#   make vet
#   make fmt
#   make clean

GO        ?= go
BIN_DIR   ?= bin
PKG       := ./...
CMDS      := firefork seed-template fork bench
LDFLAGS   := -s -w

.PHONY: all test test-int vet lint build fmt clean help

help:
	@echo "Targets: build test test-int vet lint fmt clean setup-jailer"

all: vet test build

test:
	$(GO) test $(PKG) -count=1

test-int:
	sudo -E $(GO) test $(PKG) -race -count=1 -tags=integration

vet:
	$(GO) vet $(PKG)

# : `lint: vet` claimed lint coverage that wasn't actually
# there — `go vet` is the absolute bare minimum. The full lint target
# now also runs staticcheck (installed on demand under $GOBIN). CI
# can `make lint` without manual setup.
lint: vet
	@command -v staticcheck >/dev/null 2>&1 || $(GO) install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck $(PKG)

build: $(addprefix build-,$(CMDS))

build-%:
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$* ./cmd/$*

fmt:
	gofmt -s -w .

clean:
	rm -rf $(BIN_DIR)
	rm -f results/*.csv

# Provision firefork-jail user + /srv/jailer base on the multipass host.
# Idempotent. Required once per host before using jailer-enabled forks.
setup-jailer:
	sudo bash scripts/setup-jailer.sh
