# Go Migration Plan, Part 3

## Why This Exists

`docs/golang-migration-part-2.md` took the Go port through provider parity, PDF boundary definition, Go-default packaging, and the Phase 13 Python retirement gate. At the end of Phase 13, the repository still keeps Python orchestration code intentionally:

- `backends/`
- `convo_relay/`
- `relay.py`
- `display.py`
- Python orchestration tests under `tests/`
- Python packaging metadata for the temporary `convo-relay-python` entrypoint

Part 3 is the deletion plan. The goal is to remove Python as a second implementation of the relay while preserving the one explicit boundary that is still allowed: the optional PDF helper script.

## Current State After Phase 13

Go owns the first-class runtime:

- `run` and `resume`
- Codex, Claude, Gemini, and Relay backend slots
- Codex, Claude, and Gemini facilitators
- backend profiles and relay recipes
- provider lifecycle metadata
- fake-provider conformance tests
- session inspection and mutation
- graph repair
- dynamic child relay flows
- display HTML generation
- optional PDF helper invocation
- skill installation
- default `convo-relay` packaging/docs path

Python still exists as:

- compatibility implementation code
- transition tests
- historical fixture producers
- optional PDF rendering helper runtime

That split is temporary. After Part 3, Python should either be gone entirely or exist only as `scripts/render_display_pdf.py`.

## Definition Of Done

The Go migration is complete when all of these are true:

- The repository has no Python relay orchestration modules.
- The default and only relay CLI implementation is the Go binary.
- There is no `convo-relay-python` entrypoint.
- CI no longer runs the Python orchestration test suite.
- Any retained Python file is explicitly scoped to PDF rendering and imports no relay orchestration modules.
- Docs, README examples, skills, scripts, and tests do not point users at `relay.py`, `convo_relay`, `backends`, `display.py`, or Python provider paths.
- Go tests cover the behavior formerly protected by the Python transition suite.
- Phase 13 corpus and provider conformance gates still pass after deletion.

## Phase 14: Python Removal Audit And Test Transfer

Do not start deletion by removing files blindly. First prove that every Python surface is either dead, replaced by Go coverage, or intentionally retained as the PDF helper boundary.

Minimum behavior:

- Inventory every Python path and classify it as:
  - delete now
  - convert fixture/test coverage to Go first
  - keep as optional PDF helper
- Compare Python tests against Go tests and identify any behavior that is still only covered by pytest.
- Transfer any missing high-value coverage into Go before deleting the Python suite.
- Confirm all Python-created historical session shapes still exist in Go retirement tests.
- Confirm `install-skills` behavior is covered by Go tests and no longer depends on Python.
- Confirm the optional PDF helper import-boundary test remains in Go.

Specific coverage to audit:

- backend lifecycle edge cases
- facilitator parsing and fallback model behavior
- CLI flag parsing for daily commands
- session discovery and prefix resolution
- stop, kill, clean, and cleanup behavior
- display HTML escaping and PDF helper failure cleanup
- contract fixtures and strict validation
- dynamic child relay graph behavior
- steering/resume queue behavior
- update/install-skill delegated JSON shape

Acceptance criteria:

- A deletion manifest exists in the phase implementation notes or PR description.
- Any pytest-only behavior that matters after Python deletion has an equivalent Go test.
- `go test ./... -count=1` passes before deletion starts.
- `python3 -m pytest` still passes before deletion starts, so failures introduced during deletion are attributable to intentional removal rather than drift.

Phase 14 implementation notes and the deletion manifest live in `docs/golang-migration-phase-14-audit.md`.

## Phase 15: Delete Python Orchestration

Remove the old implementation once Phase 14 proves that Go owns the behavior.

Minimum behavior:

- Delete Python orchestration modules:
  - `backends/`
  - `convo_relay/`
  - `relay.py`
  - `display.py`
- Delete Python orchestration tests under `tests/`.
- Remove Python package metadata if it exists only to expose the transition CLI:
  - `pyproject.toml`
  - Python build-system metadata
  - `convo-relay-python`
- Keep `scripts/render_display_pdf.py` only if PDF export remains helper-backed.
- Keep or add Go tests proving:
  - HTML display works without Python orchestration
  - PDF export fails clearly when the helper is absent
  - PDF helper failure leaves no partial artifacts
  - the helper imports no relay orchestration modules
- Remove docs and scripts that tell users to run Python relay entrypoints.
- Remove CI pytest steps.

Acceptance criteria:

```bash
go vet ./...
go test ./... -count=1
go run ./cmd/convo-relay install-skills --plan --json --install-root /tmp/relay-skill-stage
```

Reference audit:

```bash
rg -n "relay.py|convo_relay|backends/|display.py|convo-relay-python|python3 -m pytest|pipx install|pipx upgrade" .
```

Allowed matches after Phase 15:

- historical migration docs
- `scripts/render_display_pdf.py`
- docs that explicitly describe the optional PDF helper
- Go tests that assert the PDF helper boundary

No allowed match should describe Python as a relay runtime or fallback.

Phase 15 implementation notes:

- Deleted the Python relay orchestration modules, transition test suite, contract fixtures under `tests/`, `relay.py`, `display.py`, and `pyproject.toml`.
- Kept `scripts/render_display_pdf.py` as the explicit optional PDF helper boundary.
- Removed the CI Python transition job; CI now runs Go vet and Go tests only.
- Removed stale non-historical Python API inventory docs.
- Updated the Phase 13 retirement corpus to read contract fixtures from `testdata/contracts/`.
- Local verification passed:
  - `go vet ./...`
  - `go test ./... -count=1`
  - `go run ./cmd/convo-relay install-skills --plan --json --install-root /tmp/relay-skill-stage`

## Phase 16: Go-Only Packaging And CI Hardening

After deletion, make the repository feel like a Go project instead of a Python project with a Go binary.

Minimum behavior:

- Keep README install and update instructions centered on `go build` or a chosen Go release mechanism.
- Ensure `.github/workflows/tests.yml` runs Go gates only.
- Add a small packaging/release script or Makefile target if repeated commands are now too easy to mistype.
- Ensure `install-skill.sh` continues to use the Go CLI.
- Ensure skill docs call `convo-relay`, not Python entrypoints.
- Ensure release artifacts include the `skill/` directory and optional PDF helper if the binary expects to discover them near the executable.
- Decide whether `CONVO_RELAY_SKILL_DIR` and `CONVO_RELAY_PDF_HELPER` are enough for non-source installs, or whether packaging should place helper assets in a standard location.

Acceptance criteria:

```bash
go vet ./...
go test ./... -count=1
go run ./cmd/convo-relay install-skills --plan --json --install-root /tmp/relay-skill-stage
go run ./cmd/convo-relay display --help
```

The final command can be replaced with whichever help/smoke path exists after CLI help behavior is formalized.

Phase 16 implementation notes:

- Added a Go-oriented `Makefile` with `build`, `install`, `install-assets`, `install-skills`, `test`, `package`, and `clean` targets.
- Chose a standard installed asset layout instead of relying only on environment variables for non-source installs:
  - binary: `bin/convo-relay`
  - bundled skills: `share/convo-relay/skill/`
  - optional PDF helper: `share/convo-relay/scripts/render_display_pdf.py`
- Updated skill and PDF helper discovery so the binary checks executable-relative package paths, while still supporting source checkouts and explicit `CONVO_RELAY_SKILL_DIR` / `CONVO_RELAY_PDF_HELPER` overrides.
- Updated README install, update, release packaging, and PDF-helper docs around `make install` / `make package`.
- Updated CLI usage text and bundled skill docs to point at `convo-relay` / `make install`, not source-only command strings.
- Hardened CI to run Go vet, Go tests with `-count=1`, the package-layout smoke, `install-skills --plan`, and `display --help`.
- Local verification passed:
  - `make package`
  - packaged binary from `dist/convo-relay/bin/convo-relay` running `install-skills --plan --json` from `/tmp`
  - `go vet ./...`
  - `go test ./... -count=1`
  - `go run ./cmd/convo-relay install-skills --plan --json --install-root /tmp/relay-skill-stage`
  - `go run ./cmd/convo-relay display --help`
  - `git diff --check`
  - Python file scan still reports only `scripts/render_display_pdf.py`

## Phase 17: Go-Only Smoke Matrix

Run fake-provider smokes as the release gate. Live smokes are useful, but they are not the required proof.

Minimum fake-provider smokes:

- `codex,codex`
- `codex,gemini`
- `gemini,codex`
- `gemini,gemini`
- `claude,codex`
- `codex,claude`
- `claude,claude`
- profile-driven `claude-code`
- profile-driven `gemini-vision`
- `codex,relay`
- `relay,codex`
- `relay,relay`
- resume after each first-class provider has state
- cleanup after each first-class provider has state
- `contracts --json`
- `show --graph --json`
- `display --html-only`

Minimum behavior:

- Smokes must run without live provider credentials.
- Smokes must use deterministic fake provider binaries in `PATH`.
- Smokes must write sessions into a temp relay home.
- Smokes must verify exit code and key session artifacts, not just command completion.
- Smokes must leave no running provider child processes.

Acceptance criteria:

- The smoke matrix is either automated in Go tests or a checked-in script that CI can run.
- The smoke matrix is documented as the release confidence gate.
- Live Claude/Gemini/Codex smokes are documented as optional local release evidence only.

Phase 17 implementation notes:

- Added `cmd/convo-relay/smoke_matrix_test.go` as an opt-in Go integration smoke. It builds the CLI, shadows `codex`, `claude`, and `gemini` with deterministic shell fake-provider binaries in `PATH`, uses a temp `CODEX_CLAUDE_HOME`, and checks that provider PID markers and `relay.pid` files are gone after commands complete.
- Added `make smoke-fake-providers`, which runs the smoke through `CONVO_RELAY_RUN_SMOKE_MATRIX=1 go test ./cmd/convo-relay -run TestGoOnlySmokeMatrix -count=1 -v`.
- Wired CI to run `make smoke-fake-providers` in addition to the normal Go tests.
- The smoke matrix covers:
  - `codex,codex`
  - `codex,gemini`
  - `gemini,codex`
  - `gemini,gemini`
  - `claude,codex`
  - `codex,claude`
  - `claude,claude`
  - profile-driven `claude-code`
  - profile-driven `gemini-vision`
  - `codex,relay`
  - `relay,codex`
  - `relay,relay`
  - resume, `cleanup`, and `clean` after Codex, Claude, Gemini, and Relay slots have persisted state
  - `contracts --json`
  - `show --graph --json`
  - `display --html-only`
- Each smoke verifies exit status plus session artifacts: `meta.json`, `transcript.json`, `events.jsonl`, `graph.json`, provider slot state, provider result backend/return code, relay child contract state where relevant, temp-home placement, and HTML output content.
- README now documents `make smoke-fake-providers` as the release confidence gate and treats live Codex/Claude/Gemini smokes as optional local release evidence only.

## Phase 18: Optional PDF Runtime Decision

This phase is optional. The migration can be complete with a retained Python PDF helper if the boundary remains small and explicit.

Path A: keep the helper.

- Keep `scripts/render_display_pdf.py`.
- Keep the import-boundary Go test.
- Document Playwright/Chromium setup.
- Ensure helper discovery works from source checkout and release installs.
- Ensure missing-helper and failing-helper errors stay clear.

Path B: remove Python entirely.

- Replace PDF export with native Go/browser automation or remove PDF export.
- Delete `scripts/render_display_pdf.py`.
- Remove `CONVO_RELAY_PDF_HELPER`.
- Update README and command help.
- Remove all remaining Python references outside historical migration docs.

Acceptance criteria for either path:

- `display --html-only` remains pure Go.
- `display` without `--html-only` has a clear supported behavior.
- There is no hidden dependency on Python orchestration modules.

Phase 18 implementation notes:

- Chose Path A: keep `scripts/render_display_pdf.py` as the one retained Python boundary.
- Go remains responsible for session loading, HTML generation, helper discovery, failure handling, and packaging. The helper only renders an already-generated HTML file to PDF through Playwright/Chromium.
- README now documents the supported PDF runtime setup:
  - `python3 -m pip install playwright`
  - `python3 -m playwright install chromium`
- `display --html-only` remains pure Go and skips the helper entirely.
- Missing-helper errors now explicitly point to the optional helper, Python Playwright/Chromium, `CONVO_RELAY_PDF_HELPER`, and `--html-only`.
- Added deterministic Go tests for source-checkout and installed-layout helper resolution, missing-helper messaging, helper failure cleanup, and the helper import boundary.

## Final Completion Gate

Before declaring the Go port complete, run:

```bash
go vet ./...
go test ./... -count=1
go run ./cmd/convo-relay install-skills --plan --json --install-root /tmp/relay-skill-stage
rg -n "relay.py|convo_relay|backends/|display.py|convo-relay-python|python3 -m pytest|pipx install|pipx upgrade" .
```

Then manually inspect every remaining search hit. The only acceptable non-historical Python hit is the optional PDF helper boundary.

## What Not To Do

Do not keep `convo-relay-python` after declaring the Go port complete.

Do not keep Python tests as a permanent safety net. If behavior matters, preserve it in Go tests before deletion.

Do not delete the Phase 13 corpus gate. It is the guardrail that proves Go still understands historical sessions after the old implementation is gone.

Do not treat a successful live provider run as proof that deletion is safe. Live runs do not cover old session shapes, lifecycle failure modes, or cleanup paths.

Do not leave docs or skills that teach users Python entrypoints. The completed port should have one relay implementation path: Go.
