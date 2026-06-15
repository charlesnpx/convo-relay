# Phase 14 Python Removal Audit

Phase 14 is the audit and test-transfer gate before deleting Python orchestration. It does not delete Python runtime files. Its job is to prove which Python files are temporary, which non-Python fixtures must survive, and which pytest-only behaviors needed Go coverage first.

## Deletion Manifest

Delete in Phase 15 after the verification gates pass:

- `relay.py`
- `display.py`
- `backends/__init__.py`
- `backends/base.py`
- `backends/claude.py`
- `backends/codex.py`
- `backends/gemini.py`
- `backends/registry.py`
- `backends/relay.py`
- `convo_relay/__init__.py`
- `convo_relay/child_contracts.py`
- `convo_relay/cli.py`
- `convo_relay/cli_contracts.py`
- `convo_relay/cli_dynamic.py`
- `convo_relay/contracts.py`
- `convo_relay/dynamic.py`
- `convo_relay/engine.py`
- `convo_relay/facilitator.py`
- `convo_relay/graph.py`
- `convo_relay/orchestrator.py`
- `convo_relay/prompts.py`
- `convo_relay/protocol.py`
- `convo_relay/recipes.py`
- `convo_relay/results.py`
- `convo_relay/session.py`
- `convo_relay/state.py`
- `convo_relay/updates.py`
- `tests/__init__.py`
- `tests/conftest.py`
- `tests/test_backends.py`
- `tests/test_cli.py`
- `tests/test_contract_fixtures.py`
- `tests/test_contracts.py`
- `tests/test_display.py`
- `tests/test_dynamic_graph.py`
- `tests/test_engine.py`
- `tests/test_facilitator.py`
- `tests/test_orchestrator.py`
- `tests/test_prompts.py`
- `tests/test_protocol.py`
- `tests/test_relay_integration.py`
- `tests/test_results.py`
- `tests/test_session.py`
- `tests/test_state.py`
- `tests/test_updates.py`
- `pyproject.toml`, once it exists only for `convo-relay-python`

Keep after Phase 15 if PDF remains helper-backed:

- `scripts/render_display_pdf.py`

Move or preserve outside Python tests before deleting `tests/`:

- `tests/fixtures/contracts/`

Phase 14 copied those fixtures to `testdata/contracts/` and updated Go tests to read the Go-owned copy. The Python copy can be deleted with the Python test suite in Phase 15.

Ignored generated files:

- `__pycache__/`
- `*.pyc`

These are not tracked migration surfaces and can be removed whenever cleanup is convenient.

## Coverage Transfer

Behavior already covered by Go before Phase 14:

- Backend lifecycle edge cases: `internal/runner/provider_lifecycle_test.go`, `internal/runner/gemini_backend_test.go`, and `internal/runner/claude_backend_test.go`.
- Claude JSONL growth, stall, subagent, session collision, timeout, partial output, nonzero, stderr-only, missing binary, and cleanup behavior: `internal/runner/claude_backend_test.go`.
- Gemini parse, timeout, recovered output, auth short-circuiting, retry classification, missing binary, resume, and cleanup behavior: `internal/runner/gemini_backend_test.go`.
- Codex lifecycle, retryable provider classification, durable provider results, malformed output, missing binary, timeout, and stderr-only failures: `internal/runner/provider_lifecycle_test.go`.
- Facilitator defaults, rejection of relay facilitators, Claude/Gemini facilitator support, profile-backed slots, and Python-era state restore: `internal/runner/phase11_backend_test.go`.
- Dynamic child relay and relay-backend graph behavior: `internal/runner/runner_test.go`, `internal/runner/phase11_backend_test.go`, `internal/graph/graph_test.go`, and `internal/retirement/retirement_gate_test.go`.
- Contract fixtures and strict validation: `internal/contracts/contracts_test.go`, `internal/inspect/inspect_test.go`, and `testdata/contracts/`.
- Session discovery, prefix resolution, stop, kill, clean, cleanup, and steering queue behavior: `internal/runner/runner_test.go`.
- Display HTML escaping plus PDF helper boundary/failure cleanup: `internal/inspect/session_test.go` and `cmd/convo-relay/main_test.go`.
- Install-skills delegated JSON shape, copy/hash behavior, and staged install root support: `cmd/convo-relay/main_test.go`.
- Historical Python-era session corpus, mutation ownership, and idempotency: `internal/retirement/retirement_gate_test.go`.

Pytest-only behavior transferred in Phase 14:

- Python-compatible run/resume daily CLI flags:
  - `--quick`
  - `--context`
  - `--skill`
  - `--task-plan`
  - `-o` / `--output`
  - `-v` / `--verbose`
  - `-s` / `--stream` for `run`
- Context and skill file injection into the first Go relay prompt while preserving original `task` and `initial_prompt` metadata.
- Launch task plan loading and persistence in Go session metadata.
- Markdown and JSON export writing when `-o` / `--output` is provided.
- Display rendering of launch prompt and launch plan metadata before transcript turns.
- Resume backend-turn selection that skips synthetic child-result transcript entries.
- Go contract/inspection tests now read `testdata/contracts/`, so deleting `tests/fixtures/contracts/` later will not break Go.

## Remaining Deletion Notes

The following Python behaviors are not worth porting because they describe implementation internals that disappear with Python:

- Python immutable relay state wrapper behavior.
- Python subprocess mocks around spinner rendering and stderr presentation.
- Python module re-export compatibility through `relay.py`.
- Python package entrypoint behavior for `convo-relay-python`.

The following compatibility choices are intentional:

- `--verbose` and `--stream` are accepted by Go so existing README examples and installed skills do not fail during the transition. The Go runner already writes durable session state progressively; richer live progress output can be improved later without blocking Python deletion.
- PDF export may keep the explicit helper boundary. The helper must not import `relay.py`, `display.py`, `convo_relay`, or `backends`.

## Phase 14 Verification

Required before Phase 15 deletion:

```bash
go vet ./...
go test ./... -count=1
python3 -m pytest
```

Phase 14 local verification:

- `go vet ./...` passed.
- `go test ./... -count=1` passed.
- `python3 -m pytest` passed with 227 tests.

The pytest gate is intentionally still required in Phase 14. Once Phase 15 deletes Python orchestration, pytest should be removed from CI or narrowed to helper-only tests if the PDF helper remains.
