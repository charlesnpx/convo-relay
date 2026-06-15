# Provider Lifecycle Contract

This is the Phase 8-10 audit artifact for the Go migration.

## Python Behavior To Preserve

The Python provider implementations expose a common `TurnResult` with:

- `content`
- `timed_out`
- `stalled`
- `recovered`

Provider-specific behavior currently differs by backend:

- Codex reads JSON events from stdout, tracks `thread_id`, resumes with `codex exec resume --json <thread_id> -`, and can recover text from a timeout or nonzero exit when the stdout event buffer contains completed text.
- Gemini runs `gemini --output-format json`, parses `response`, `text`, `content`, `message`, or structured `error`, uses slot-scoped `HOME` and `GEMINI_CLI_HOME`, and can recover text from timeout or nonzero output.
- Claude runs `claude -p --dangerously-skip-permissions`, uses `--session-id` for first turn and `--resume` afterward, extracts assistant text from `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`, watches JSONL growth including subagents, detects hard timeout and stall timeout, falls back to stdout when needed, retries unstarted session-id collisions, and cleans relay-owned JSONL/session directories.

The historical Python policy classified provider failures as retryable when they looked like API, auth, network, rate-limit, overload, 429, or 5xx failures, while treating local setup problems such as missing binaries, invalid options, and session-id collisions as non-retryable. Current Go intentionally treats auth-looking failures as non-retryable so bad or expired credentials do not consume the full transient retry budget.

## Durable Go Shape

New Go-created backend transcript entries and `turn_completed` event payloads include:

```json
{
  "provider_result": {
    "backend": "codex",
    "timed_out": false,
    "stalled": false,
    "recovered": false,
    "return_code": 0,
    "recovery_source": "",
    "warnings": []
  }
}
```

`retryable_error` is included only when a provider result is available and the provider output was classified as retryable.

Old sessions do not need this field. Inspectors and display paths must continue accepting Python-era transcript entries and events that omit `provider_result`.

Retryable provider failures are not persisted as completed turns. They are retried by the runner with the Python-compatible backoff series `5s`, `10s`, `20s`, `40s`, `80s`, `160s`. If all attempts fail, the session is marked failed with a retry-exhaustion error and no successful transcript entry is appended for that turn.

Failed provider turns append a `provider_failure` event and mirror the sanitized failure payload into `meta.provider_failures`. Payloads include:

- `phase`: `turn` or `facilitator`
- `actor` and `backend`
- `category`: `auth`, `configuration`, `transient`, `provider_error`, or `unknown`
- `retryable` and `attempts`
- `timed_out`, `stalled`, and `return_code`
- `remediation_code` and `remediation`
- `sanitized_detail` with credential-looking values redacted
- `raw_detail_hidden`, which is always `true` for the current MVP

## Field Semantics

- `backend`: concrete provider/backend name that executed the turn.
- `timed_out`: the provider hit the hard timeout.
- `stalled`: the provider was killed because no provider-specific progress signal advanced before the stall timeout.
- `recovered`: the process had an abnormal outcome, but usable response text was recovered.
- `return_code`: provider process exit code when known.
- `recovery_source`: where recovered text came from, such as `event_buffer`, `jsonl`, `stdout`, or `output`.
- `warnings`: human-readable lifecycle warnings suitable for show/debug output.
- `retryable_error`: normalized provider error text that should be eligible for retry/backoff handling.

## Go Test Boundary

The permanent Go test boundary uses temporary fake provider binaries in `PATH`, not live provider credentials.

Covered now:

- success
- malformed output
- missing binary
- auth failure classification
- stderr-only non-retryable failure
- nonzero exit without output
- nonzero exit with recoverable output
- timeout with recovered output
- timeout without recovered output
- retryable provider classification
- retryable provider backoff and exhaustion
- auth failure short-circuiting
- sanitized `provider_failure` event persistence
- historical session inspection without `provider_result`
- interruption through the existing runner cancellation smoke

Covered by the existing Python baseline:

- Gemini success, JSON shape variants, plain-text fallback, timeout recovery, nonzero failure, nonzero recovery, state, and slot-scoped home behavior
- Claude JSONL path encoding
- Claude JSONL extraction
- Claude hard timeout with JSONL activity
- Claude stall detection
- Claude subagent JSONL activity counting
- Claude partial recovery after stall
- Claude stdout fallback when JSONL has no assistant response
- Claude timeout and stall placeholders without recovery
- Claude session-id collision retry
- Claude cleanup of relay-owned JSONL/session directories
- Python orchestrator retryable provider backoff

Implemented in Go Phase 9:

- Gemini backend adapter
- Gemini direct agent and profile resolution
- Gemini slot restore and resume compatibility
- Gemini output shape parsing and plain-text fallback
- Gemini slot-scoped `HOME` and `GEMINI_CLI_HOME`
- Gemini config seeding from `~/.gemini`, excluding `tmp` and `logs`
- Gemini timeout, nonzero-exit recovery, retryable error, missing-binary, and stderr-only behavior

Implemented in Go Phase 10:

- Claude JSONL extraction
- Claude JSONL growth and stall detection
- Claude subagent JSONL activity
- Claude stdout fallback
- Claude session-id collision retry
- Claude cleanup of relay-owned JSONL/session directories
- Claude first-turn `--session-id` and resumed `--resume` command behavior
- Claude hard timeout, stall timeout, nonzero-exit recovery, retryable error, missing-binary, and stderr-only behavior
- Claude direct agent and profile resolution for `claude`, mixed Claude/Codex pairs, same-backend Claude pairs, and `claude-code`

Future provider changes should extend this same fake-provider conformance boundary before relying on live provider smokes.
