# firefork -- Memory-snapshot fork primitive for Firecracker microVMs

GO        ?= go
BIN_DIR   ?= bin
PKG       := ./...

.PHONY: build test vet fmt clean

build:
	$(GO) build -o $(BIN_DIR)/firefork ./cmd/firefork

test:
	$(GO) test $(PKG) -count=1

vet:
	$(GO) vet $(PKG)

fmt:
	gofmt -s -w .

clean:
	rm -rf $(BIN_DIR)
