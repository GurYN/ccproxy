.PHONY: all build vet test test-race test-integration lint clean

GO ?= go
PKG ?= ./...

all: build

build:
	$(GO) build $(PKG)

vet:
	$(GO) vet $(PKG)

# Fast unit tests. Integration tests are excluded via the build tag.
test:
	$(GO) test $(PKG)

test-race:
	$(GO) test -race -count=1 $(PKG)

# Runs the //go:build integration suite. Requires a `claude` binary on PATH;
# tests that call out to it will skip cleanly if it is missing. Override the
# per-test deadline with CCPROXY_INTEGRATION_TIMEOUT (default 2m).
test-integration:
	$(GO) test -tags integration -count=1 -timeout 10m ./tests/integration/...

# Convenience: build a trimmed, static release binary.
release:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o ccproxy ./cmd/ccproxy

clean:
	rm -f ccproxy
