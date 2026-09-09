PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
DATADIR ?= $(PREFIX)/share/convo-relay
DIST_DIR ?= dist/convo-relay
BINARY := bin/convo-relay
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || echo 1.0.0-dev)
LDFLAGS ?= -X main.cliVersion=$(VERSION)

CROSS_TARGETS ?= darwin/arm64 windows/amd64

.PHONY: build install install-assets fmt-check test test-race cross-compile release-gate package clean

build:
	mkdir -p "$(dir $(BINARY))"
	go build -ldflags "$(LDFLAGS)" -o "$(BINARY)" ./cmd/convo-relay

install: build install-assets
	mkdir -p "$(BINDIR)"
	install -m 0755 "$(BINARY)" "$(BINDIR)/convo-relay"

install-assets:
	rm -rf "$(DATADIR)/skill"
	mkdir -p "$(DATADIR)"
	cp -R skill "$(DATADIR)/skill"

fmt-check:
	@unformatted="$$(gofmt -l cmd internal plan result bundle)"; \
	test -z "$$unformatted" || { printf '%s\n' "$$unformatted"; exit 1; }

test: fmt-check
	go vet ./...
	go test ./... -count=1

test-race:
	go test -race ./internal/engine ./internal/sessionstore ./cmd/convo-relay -count=1

cross-compile:
	@set -eu; \
	for target in $(CROSS_TARGETS); do \
		target_os=$${target%/*}; \
		target_arch=$${target#*/}; \
		echo "Cross-compiling production packages for $$target_os/$$target_arch"; \
		CGO_ENABLED=0 GOOS=$$target_os GOARCH=$$target_arch go build ./...; \
	done

release-gate: cross-compile

package: build
	rm -rf "$(DIST_DIR)"
	mkdir -p "$(DIST_DIR)/bin"
	mkdir -p "$(DIST_DIR)/share/convo-relay"
	install -m 0755 "$(BINARY)" "$(DIST_DIR)/bin/convo-relay"
	cp -R skill "$(DIST_DIR)/share/convo-relay/skill"

clean:
	rm -rf bin dist
