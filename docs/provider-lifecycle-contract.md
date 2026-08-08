# Provider Lifecycle Contract

This is the Phase 8-10 audit artifact for the Go migration.

## Provider Lifecycle Behavior

The historical Python provider implementations exposed a common `TurnResult` with:

- `content`
- `timed_out`
- `stalled`
- `recovered`

Current transport behavior differs by backend:

- Codex uses the embedded agentbus v0.9.1 Codex adapter over `codex app-server` JSON-RPC, tracks the provider-confirmed `thread_id`, resumes through engine `Resume`, and uses the relay's trusted (`dangerFullAccess`) write posture.
- Gemini runs `gemini --output-format json`, parses `response`, `text`, `content`, `message`, or structured `error`, uses slot-scoped `HOME` and `GEMINI_CLI_HOME`, and can recover text from timeout or nonzero output.
- Claude uses the embedded agentbus v0.9.1 Claude adapter to drive the Claude CLI with stream-json input and output, tracks the provider-confirmed `session_id`, resumes through engine `Resume`, and receives assistant text from stream events rather than project JSONL polling. The CLI can still write project transcripts; embedded cleanup and legacy-compatible cleanup remove relay-owned Claude artifacts.

Codex and Claude make one engine `Session.Turn` per attempt. An errored or stalled turn drops the live session, and the next attempt resumes with the provider-confirmed id. Ordinary run and resume use the relay's internal readiness checks; they do not call engine `Backend.Preflight`.

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

For root recipes, every provider is a trusted same-user process running with
the invoking user's authority. Slot-scoped homes and detached worktrees
organize lifecycle state; they are not sandboxes and do not isolate a provider
from source repositories, session files, credentials, the network, or other
same-user-visible resources.

When an integration contract has named inputs, the retry boundary verifies
the retained snapshots immediately before and after each participant,
facilitator, and reducer attempt. The post-attempt verification uses a finite
orchestration-owned context that remains live after provider or caller
cancellation. Retained inputs are not immutable or filesystem read-only, so a
provider can change them between checks. A failed boundary check suppresses
retry and rejects the response before it can enter transcript, facilitator,
reducer, raw-result, validation, or canonical-result artifacts. A provider
error, timeout, stall, or cancellation from the same attempt is retained only
as a secondary, sanitized cause.

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
- `stalled`: the provider had no stream events before the stall timeout, so the relay interrupted its current session. Any stream event, including an agentbus `Progress` heartbeat, resets the watchdog.
- `recovered`: the process had an abnormal outcome, but usable response text was recovered.
- `return_code`: provider process exit code when known.
- `recovery_source`: where recovered text came from, such as `event_stream`, `stdout`, or `output`.
- `warnings`: human-readable lifecycle warnings suitable for show/debug output.
- `retryable_error`: normalized provider error text that should be eligible for retry/backoff handling.

## Go Test Boundary

The permanent Go test boundary uses embedded-adapter protocol fakes and temporary fake provider binaries in `PATH`, not live provider credentials.

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
- Gemini success, JSON shape variants, plain-text fallback, timeout recovery, nonzero failure, nonzero recovery, state, and slot-scoped home behavior
- embedded Codex `app-server` JSON-RPC and Claude stream-json protocol fakes, including start, resume, and provider-confirmed id capture
- stream-event stall-watchdog behavior, including agentbus `Progress` heartbeat resets, interruption, and partial recovery
- hard-timeout and stalled soft outcomes with and without recovered text
- cleanup of relay-owned Claude project transcripts, including legacy-compatible paths
- Python orchestrator retryable provider backoff

Implemented in Go Phase 9:

- Gemini backend adapter
- Gemini direct agent and profile resolution
- Gemini slot restore and resume compatibility
- Gemini output shape parsing and plain-text fallback
- Gemini slot-scoped `HOME` and `GEMINI_CLI_HOME`
- Gemini config seeding from `~/.gemini`, excluding `tmp` and `logs`
- Gemini timeout, nonzero-exit recovery, retryable error, missing-binary, and stderr-only behavior

Implemented in the current embedded-adapter runtime:

- Codex `app-server` JSON-RPC and Claude stream-json transports through embedded agentbus adapters
- one `Session.Turn` per attempt, provider-confirmed id capture, and resume after live-session disposal
- Codex slot-scoped `CODEX_HOME` with linked auth/config and the trusted (`dangerFullAccess`) write posture
- event-inactivity stall watchdogs that interrupt the session while preserving stalled and recovered outcomes
- Claude stream-event assistant text and cleanup of relay-owned project transcripts, with legacy-compatible cleanup for pre-migration sessions
- wire-compatible slot state: `thread_id` or `session_id`, `started`, `cwd`, `profile_id`, `model`, and `effort`
- relay-owned readiness checks rather than engine `Backend.Preflight` during ordinary run or resume

Future provider changes should extend this same fake-provider conformance boundary before relying on live provider smokes.
