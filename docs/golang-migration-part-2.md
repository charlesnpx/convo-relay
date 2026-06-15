# Go Migration Plan, Part 2

## Why This Exists

`docs/golang-migration.md` covered the first production handoff. Phases 1 through 7 made the Go implementation capable of inspecting, mutating, running, resuming, displaying HTML, cleaning, and managing sessions, but Phase 7 intentionally kept two fallback areas:

- Go `display` supports `--html-only`; Python still owns PDF export.
- Go runner support is production-focused on Codex subprocess backends and relay profiles built from them; Claude and Gemini provider lifecycle handling remain Python fallback paths.

Part 2 is the Python retirement plan. The goal is not just to make Go pass a happy-path relay. The goal is for Go to own every first-class backend lifecycle and every durable session-management behavior needed before most or all of the Python implementation can be deleted.

PDF export can remain a small, named helper boundary if native Go/browser PDF support is not worth the migration cost. Claude and Gemini cannot stay as Python fallbacks if they are first-class providers.

## Current State After Phase 7

Go currently owns:

- strict contract inspection
- graph reading and repair
- recipe and profile compilation
- session store mutations
- runner shell behavior
- relay-as-backend and dynamic child execution
- CLI session resolution for daily commands
- HTML display export

The remaining important mismatch is provider execution:

- Go recipe defaults know about `claude` and `gemini` profiles.
- Go runner backend construction only instantiates `codex` and `relay`.
- Go facilitator execution is Codex-only.
- Python still owns Claude JSONL monitoring, Claude session lifecycle, Gemini output parsing, retryable provider classification, provider cleanup, and several provider-specific timeout recovery paths.

## Definition Of Done

The Go migration is deletion-ready when all of these are true:

- `convo-relay run` and `convo-relay resume` support `codex`, `claude`, `gemini`, and `relay` slots through the Go runner.
- Backend profiles such as `claude-code` and `gemini-vision` work in Go-created and Go-resumed sessions.
- Facilitator execution supports non-relay providers through the same Go backend factory.
- Go can restore, validate, and clean Python-era Codex, Claude, Gemini, relay-backend, dynamic-child, nested, steering, display, graph, diff, and contract sessions.
- Provider lifecycle outcomes are durable enough for resume, inspection, cleanup, and debugging.
- Fake-provider conformance tests cover success, failure, timeout, recovery, retryable errors, and cleanup without requiring live provider credentials.
- Live provider smokes are available as release confidence checks, but they are not the only deletion gate.
- The only remaining Python entrypoint is an explicitly optional PDF helper, or there is no Python runtime dependency at all.

## Phase 8: Provider Lifecycle Contract And Conformance Harness

Before porting Claude and Gemini, make the provider result contract explicit. The current Go `TurnResult` only carries `Content` and `TimedOut`, which is too narrow for Python's provider behavior.

Minimum behavior:

- Expand `TurnResult` or introduce a `ProviderResult` type that can represent:
  - `content`
  - `timed_out`
  - `stalled`
  - `recovered`
  - `return_code`
  - `recovery_source`
  - `warnings`
  - retryable provider error classification
- Port retryable provider error classification from Python into Go.
- Decide where provider lifecycle metadata is persisted:
  - transcript entry optional fields if strict contracts allow them
  - a `turn_completed` event payload if that is already the durable runtime surface
  - a `provider_result_ref` artifact if inline fields would make strict v1 contracts too loose
- Preserve old session readability. Existing Python-era transcript entries without provider result metadata must still inspect cleanly.
- Add a deterministic fake-provider harness for Go tests. Prefer temporary stub executables in `PATH` over live CLIs.
- Keep a temporary cross-language baseline while Python still exists: run the same fake Claude/Gemini scenarios through Python and Go, then freeze the behavior as Go conformance tests.

Fake-provider scenarios:

- normal success
- streamed or partial output
- malformed JSON output
- plain text fallback output
- stderr-only failure
- missing binary
- nonzero exit with no response
- nonzero exit with recoverable response
- timeout with recoverable output
- timeout with no recoverable output
- cancellation or interruption
- retryable provider errors
- non-retryable local errors

Additional Claude-specific fake scenarios:

- JSONL assistant response extraction
- JSONL growth until hard timeout
- JSONL stall with no growth
- subagent JSONL growth counted as activity
- stdout fallback when JSONL is missing
- unstarted session-id collision followed by retry with a fresh UUID
- relay-owned JSONL and session directory cleanup

Acceptance criteria:

- Go unit tests cover provider result metadata and retryable classification.
- Python and Go agree on the temporary fake-provider baseline for Claude and Gemini.
- Historical sessions without provider result metadata still pass Go inspection.
- No live provider credentials are required for the permanent test suite.

## Phase 9: Gemini Backend In Go

Port Gemini first because it is simpler than Claude and validates the generalized adapter boundary without the JSONL watchdog complexity.

Minimum behavior:

- Add a Go Gemini backend implementing the runner `Backend` interface.
- Run `gemini --output-format json`.
- Support `--model` when configured by direct slot config or backend profile.
- Carry `effort` through state for compatibility, even if Gemini does not use it yet.
- Use slot-scoped provider home:
  - `HOME=<session>/gemini/<slot_id>`
  - `GEMINI_CLI_HOME=<session>/gemini/<slot_id>`
- Seed Gemini config from the user's existing `~/.gemini` by symlinking config entries, excluding `tmp` and `logs`.
- Parse JSON output keys `response`, `text`, `content`, `message`, and structured `error`.
- Fall back to plain text when JSON parsing fails.
- Preserve `session_ref`, `started`, `cwd`, `model`, and `effort` in backend state.
- Handle timeout, nonzero exit, recoverable output, stderr details, and retryable provider errors consistently with Python.

Acceptance criteria:

- Go tests mirror the current Python Gemini tests.
- Go `buildSlots`, `restoreSlots`, cleanup, and session metadata support `gemini`.
- Go CLI smokes with fake Gemini pass for:
  - `--agents gemini,gemini`
  - `--agents codex,gemini`
  - `--agents gemini,codex`
  - profile-driven `gemini-vision`
  - resume after a Gemini turn
  - cleanup after a Gemini session

## Phase 10: Claude Backend In Go

Port Claude after the provider result contract and Gemini adapter prove the backend boundary. Claude is higher risk because its authoritative output is the session JSONL log, not just stdout.

Minimum behavior:

- Add a Go Claude backend implementing the runner `Backend` interface.
- Generate a UUID session id for new Claude slots.
- First turn command:

```bash
claude -p --dangerously-skip-permissions --session-id <session_id> -
```

- Resume command:

```bash
claude -p --dangerously-skip-permissions --resume <session_id> -
```

- Support `--model` and `--effort` when configured.
- Use the same Claude project directory encoding as Python:
  - `~/.claude/projects/<encoded-cwd>/<session_id>.jsonl`
- Before each turn, record the current JSONL byte size.
- Extract new assistant text blocks from JSONL records after that byte offset.
- Count subagent JSONL files under `<session_id>/subagents/*.jsonl` as activity for stall detection.
- Monitor both hard timeout and stall timeout.
- Kill the provider process on hard timeout or stall timeout.
- Recover partial responses from JSONL when available.
- Fall back to stdout when JSONL has no assistant response.
- Retry an unstarted session with a fresh UUID if Claude reports that the session id is already in use.
- Classify retryable provider errors from stderr, stdout, and recovered response text.
- Clean up relay-owned Claude JSONL files and session directories.
- Preserve `session_id`, `started`, `cwd`, `model`, and `effort` in backend state.

Acceptance criteria:

- Go tests mirror the current Python Claude lifecycle tests.
- Go supports `--agents claude,claude`, `claude,codex`, `codex,claude`, and profile-driven `claude-code` using fake Claude.
- Resume switches from `--session-id` to `--resume` after the first successful or recovered turn.
- Stalls, hard timeouts, and partial recoveries are visible in durable provider result metadata.
- Cleanup removes only relay-owned Claude artifacts and does not remove unrelated user Claude history.

Implementation evidence:

- `internal/runner/claude_backend.go` owns Claude command construction, JSONL extraction, JSONL/subagent activity monitoring, partial recovery, retryable classification, session-id collision retry, and relay-owned Claude cleanup.
- `internal/runner/claude_backend_test.go` provides the permanent fake-Claude conformance suite for direct lifecycle behavior, mixed/same-backend runs, `claude-code`, resume, durable provider results, and cleanup.

## Phase 11: Backend Registry, Facilitators, Profiles, And Resume Parity

After Claude and Gemini adapters exist, wire them into every Go runner path that currently assumes Codex-only execution.

Minimum behavior:

- Add `claude` and `gemini` to Go backend labels and known backend resolution.
- Replace backend-specific switch statements with a shared backend factory where practical.
- Use the shared factory from:
  - new slot construction
  - restored slot construction
  - cleanup slot construction
  - facilitator execution
  - child relay execution
- Keep `relay` valid as a participant backend.
- Continue rejecting `relay` as facilitator and reducer.
- Ensure backend profiles pass model and effort overrides into concrete provider adapters.
- Ensure `resolveBackendCWD` keeps Codex's git-repo fallback behavior without incorrectly applying it to Claude or Gemini.
- Ensure old Python-created slot state validates and restores for all first-class providers.

Acceptance criteria:

- Go can run and resume relays where Claude or Gemini is:
  - slot 0
  - slot 1
  - both slots
  - facilitator
  - inside a child relay recipe
  - inside a relay-backend profile
- `show`, `show --graph`, `show --trace`, `contracts --json`, `diff`, `display --html-only`, `clean`, and `cleanup` work on those sessions.
- Go can resume Python-era Claude/Gemini sessions whose provider state matches the documented state contract.
- Python can still inspect Go-created sessions until Python is removed.

Implementation evidence:

- `internal/runner/backends.go` owns the shared Go backend factory used by new slots and restored slots, with `codex`, `claude`, `gemini`, and `relay` in the backend label/known-backend registry.
- `internal/runner/runner.go` uses the shared factory for facilitator execution, permits `codex`, `claude`, and `gemini` facilitators, keeps `relay` rejected as a facilitator, and carries facilitator model/effort defaults and overrides into concrete adapters.
- `internal/runner/session_admin.go` uses the shared factory when constructing backends for cleanup while preserving the legacy top-level Claude cleanup path.
- `cmd/convo-relay/main.go` keeps `--agents relay` shorthand on the Codex facilitator default instead of selecting the relay backend as facilitator.
- `internal/runner/phase11_backend_test.go` covers Claude and Gemini facilitators, relay facilitator rejection, provider-backed relay profiles/child recipes, Python-era Claude/Gemini state restoration, and Codex-only git cwd fallback behavior.

## Phase 12: PDF Display Boundary

PDF export does not need to block provider parity, but it must be made explicit before deleting Python.

Recommended path:

- Keep Go as the owner of display HTML generation.
- If PDF support remains Python-backed, quarantine it as a small helper instead of keeping the full Python app alive.
- The helper should accept an HTML path and output PDF path, or accept a session path and call the Go HTML export first.
- The helper must not import Python relay orchestration modules.
- Go `display --pdf` should either:
  - call the helper when available, or
  - fail with a clear message explaining that PDF support requires the optional helper.

Alternative path:

- Replace the helper with a native Go/browser PDF implementation.
- Keep the same user-facing command and expected output paths.

Acceptance criteria:

- `display --html-only` remains pure Go.
- `display --pdf` has a documented runtime dependency and a targeted smoke test.
- Removing Python orchestration modules does not break HTML display.
- If the optional helper is absent, Go fails PDF export clearly and leaves no partial session mutation.

Implementation evidence:

- Go `display --html-only` continues to call `internal/inspect.BuildDisplayHTML` and write only the requested HTML file.
- Go `display` without `--html-only` now generates HTML in Go, calls the optional `scripts/render_display_pdf.py` helper, and writes final `transcript.html` only after PDF rendering succeeds.
- The PDF helper accepts only an input HTML path and output PDF path, imports no relay orchestration modules, and depends on Playwright/Chromium (`pip install playwright && python -m playwright install chromium`).
- `CONVO_RELAY_PDF_HELPER` can point Go at a packaged or local helper. If the helper is absent or fails, Go reports the helper failure, removes any partial PDF output, and avoids writing a final HTML artifact.
- `cmd/convo-relay/main_test.go` covers output path resolution, helper invocation, failed-helper cleanup, missing-helper error messaging, and the helper import boundary.

## Phase 13: Python Retirement Gate

Only delete Python after provider parity and display boundaries are proven.

Minimum behavior:

- Build a historical corpus of sessions created by Python and Go:
  - Codex-only
  - Claude-only
  - Gemini-only
  - mixed providers
  - relay-as-backend
  - nested relay profiles
  - dynamic child relays
  - steering and resume
  - stop and kill cleanup
  - display exports
  - contract fixtures
  - graph repair fixtures
  - diff fixtures
- Run Go inspection and mutation commands across the corpus.
- Define a semantic round-trip mutation contract:
  - read-only commands must not mutate sessions
  - mutating commands must declare the files or metadata fields they own
  - a second pass should be idempotent unless the command is intentionally non-idempotent
  - unknown Python-era fields are preserved unless a versioned migration explicitly owns their removal
- Remove Python fallbacks from Go runner paths.
- Update packaging so the Go binary is the default `convo-relay`.
- Update README and migration docs to describe the remaining optional PDF helper, if any.
- Delete Python orchestration modules only after Go passes the corpus and conformance tests.

Implementation evidence:

- `internal/retirement/retirement_gate_test.go` builds a Phase 13 corpus with Python-era Codex, Claude, Gemini, mixed-provider, relay-backend, nested relay profile, dynamic child relay, steering/resume, stop/kill cleanup, display export, contract, graph repair, diff, and Go-created session shapes.
- The corpus gate runs Go `show`, `show --graph`, `contracts`, `diff`, `display --html-only` builders, and session listing without allowing session files to mutate.
- The mutation gate defines current ownership explicitly:
  - graph repair owns `graph.json` and must be idempotent on a second pass
  - cleanup owns orphan status fields in `meta.json` plus `relay.pid` removal and must be idempotent after marking
  - steering owns append-only `steering.json` and is intentionally non-idempotent
- Unknown Python-era metadata fields are preserved by graph repair, cleanup, and steering unless a future versioned migration explicitly owns their removal.
- Go no longer points unsupported backends or profiles at a Python CLI fallback.
- The Go CLI now owns `install-skills`; `install-skill.sh` invokes the Go command from a checkout.
- `pyproject.toml` no longer installs `convo-relay`; the Python transition entrypoint is `convo-relay-python`, keeping `convo-relay` reserved for the Go binary.
- README install/update examples build and install `./cmd/convo-relay` as the default CLI and document Python only as the transition test surface plus optional PDF helper.
- Python orchestration modules are intentionally not deleted in this phase. Deletion remains gated on the checks below staying green and on replacing `python3 -m pytest` with helper-only tests when the Python orchestration compatibility suite is removed.

Deletion gate checks:

```bash
go vet ./...
go test ./...
python3 -m pytest
```

During the transition, `python3 -m pytest` should continue to pass. After Python orchestration is deleted, replace it with the remaining helper-specific tests or remove it entirely if no helper remains.

Suggested Go CLI smokes:

```bash
go run ./cmd/convo-relay run --agents codex,codex --rounds 1 --task "smoke"
go run ./cmd/convo-relay run --agents codex,gemini --rounds 1 --task "smoke"
go run ./cmd/convo-relay run --agents claude,codex --rounds 1 --task "smoke"
go run ./cmd/convo-relay run --agents claude,claude --rounds 1 --task "smoke"
go run ./cmd/convo-relay resume <session-id> --rounds 1
go run ./cmd/convo-relay contracts <session-id> --json
go run ./cmd/convo-relay show <session-id> --graph --json
go run ./cmd/convo-relay display <session-id> --html-only
go run ./cmd/convo-relay cleanup <session-id>
```

Use fake providers for required CI smokes. Use live provider smokes only as release evidence because they depend on local credentials, installed CLIs, network behavior, and provider availability.

## Recommended Work Order

1. Phase 8: provider lifecycle contract and fake-provider harness.
2. Phase 9: Gemini backend in Go.
3. Phase 10: Claude backend in Go.
4. Phase 11: registry, facilitator, profile, child relay, resume, and cleanup parity.
5. Phase 12: explicit PDF helper or native PDF renderer boundary.
6. Phase 13: corpus gate, packaging update, docs update, and Python deletion.

## What Not To Do

Do not delete Python provider code before the fake-provider baseline exists. Without that baseline, Go can appear correct while silently losing timeout recovery, retryable error handling, or cleanup semantics.

Do not treat live Claude or Gemini success as proof of parity. Live smokes prove that the current machine can reach a provider. They do not prove that edge cases, old sessions, or cleanup paths are compatible.

Do not keep a hidden Python runner fallback after declaring the Go port complete. If Python remains, it should be a named optional helper with a small boundary, not a second implementation of orchestration.
