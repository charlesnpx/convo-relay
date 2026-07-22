PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
DATADIR ?= $(PREFIX)/share/convo-relay
DIST_DIR ?= dist/convo-relay
BINARY := bin/convo-relay
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || echo 1.0.0-dev)
LDFLAGS ?= -X main.cliVersion=$(VERSION)

.PHONY: build install install-assets install-skills test test-without-optional-defaults smoke-fake-providers package clean

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
	mkdir -p "$(DATADIR)/scripts"
	install -m 0755 scripts/render_display_pdf.py "$(DATADIR)/scripts/render_display_pdf.py"

install-skills:
	go run ./cmd/convo-relay install-skills

test:
	go vet ./...
	go test ./... -count=1
	$(MAKE) test-without-optional-defaults

test-without-optional-defaults:
	go test -tags=convo_relay_acceptance_no_optional_defaults ./... -count=1

smoke-fake-providers:
	CONVO_RELAY_RUN_SMOKE_MATRIX=1 go test ./cmd/convo-relay -run TestGoOnlySmokeMatrix -count=1 -v

package: build
	rm -rf "$(DIST_DIR)"
	mkdir -p "$(DIST_DIR)/bin"
	mkdir -p "$(DIST_DIR)/share/convo-relay/scripts"
	install -m 0755 "$(BINARY)" "$(DIST_DIR)/bin/convo-relay"
	cp -R skill "$(DIST_DIR)/share/convo-relay/skill"
	install -m 0755 scripts/render_display_pdf.py "$(DIST_DIR)/share/convo-relay/scripts/render_display_pdf.py"

clean:
	rm -rf bin dist
