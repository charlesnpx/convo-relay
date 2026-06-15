#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"
VERSION="${CONVO_RELAY_VERSION:-$(git -C "$SCRIPT_DIR" describe --tags --exact-match 2>/dev/null || true)}"
VERSION="${VERSION:-dev}"

exec go run -ldflags "-X main.cliVersion=$VERSION" ./cmd/convo-relay install-skills "$@"
