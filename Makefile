PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
DATADIR ?= $(PREFIX)/share/convo-relay
DIST_DIR ?= dist/convo-relay
BINARY := bin/convo-relay
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || echo 1.0.0-dev)
LDFLAGS ?= -X main.cliVersion=$(VERSION)

CROSS_TARGETS ?= darwin/arm64 windows/amd64

.PHONY: build install install-assets fmt-check test test-race cross-compile cross-compile-tests smoke-fake-providers release-gate package clean

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
	@unformatted="$$(gofmt -l cmd internal)"; \
	test -z "$$unformatted" || { printf '%s\n' "$$unformatted"; exit 1; }

test: fmt-check
	go vet ./...
	go test ./... -count=1

test-race:
	# Explicit timeout: the raced runner suite already sits near Go's 10m default
	# (~9m measured), so a loaded machine tips it over and the gate fails for load
	# rather than for a race. CI completes it in about 2m.
	go test -race -timeout 25m ./internal/runner ./internal/store ./cmd/convo-relay -count=1

cross-compile:
	@set -eu; \
	for target in $(CROSS_TARGETS); do \
		target_os=$${target%/*}; \
		target_arch=$${target#*/}; \
		echo "Cross-compiling production packages for $$target_os/$$target_arch"; \
		CGO_ENABLED=0 GOOS=$$target_os GOARCH=$$target_arch go build ./...; \
	done

cross-compile-tests:
	@set -eu; \
	test_dir=$$(mktemp -d); \
	trap 'rm -rf "$$test_dir"' 0 HUP INT TERM; \
	for target in $(CROSS_TARGETS); do \
		target_os=$${target%/*}; \
		target_arch=$${target#*/}; \
		echo "Cross-compiling test packages for $$target_os/$$target_arch"; \
		test_packages=$$(CGO_ENABLED=0 GOOS=$$target_os GOARCH=$$target_arch go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...); \
		for package in $$test_packages; do \
			test_name=$$(printf '%s' "$$package" | sed 's#[/.]#_#g'); \
			test_suffix=; \
			if [ "$$target_os" = windows ]; then test_suffix=.exe; fi; \
			CGO_ENABLED=0 GOOS=$$target_os GOARCH=$$target_arch go test -c "$$package" -o "$$test_dir/$$target_os-$$target_arch-$$test_name.test$$test_suffix"; \
		done; \
	done

smoke-fake-providers:
	CONVO_RELAY_RUN_SMOKE_MATRIX=1 go test ./cmd/convo-relay -run TestGoOnlySmokeMatrix -count=1 -v

release-gate: cross-compile cross-compile-tests smoke-fake-providers

package: build
	rm -rf "$(DIST_DIR)"
	mkdir -p "$(DIST_DIR)/bin"
	mkdir -p "$(DIST_DIR)/share/convo-relay"
	install -m 0755 "$(BINARY)" "$(DIST_DIR)/bin/convo-relay"
	cp -R skill "$(DIST_DIR)/share/convo-relay/skill"

clean:
	rm -rf bin dist
