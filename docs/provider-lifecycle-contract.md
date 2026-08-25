# Provider lifecycle contract

This document describes the provider behavior implemented by the current
relay runtime.

## Turn lifecycle

Codex runs through the embedded agentbus adapter over `codex app-server` and
retains the provider-confirmed thread id. Claude runs through the embedded
agentbus stream-json adapter and retains the provider-confirmed session id.
Gemini runs `gemini --output-format json` and accepts its documented response
shapes plus recoverable text output. Each engine attempt owns one provider turn.

An errored or stalled Codex or Claude live session is discarded before the
next attempt. The next attempt resumes from the provider-confirmed id when one
exists. Relay readiness checks happen before normal run and resume; the engine
does not rely on provider preflight hooks.

Providers are trusted same-user processes. Their slot-specific homes and
workspaces organize state; they are not a security boundary and do not isolate
the provider from repositories, session files, credentials, or other resources
visible to that user.

## Failure policy

The runner classifies failures as `auth`, `configuration`, `transient`,
`provider_error`, or `unknown`.

- Authentication-looking failures stop immediately and do not consume the
  transient retry budget.
- Retryable failures use bounded backoff: 5s, 10s, 20s, 40s, 80s, then 160s.
  Exhaustion marks the session failed without creating a successful transcript
  turn for the failed attempt.
- A hard timeout is recorded as a timed-out provider outcome.
- The stream watchdog treats lack of stream activity for the configured stall
  interval as a stalled turn. Any stream event, including an agentbus progress
  heartbeat, resets the watchdog.
- If usable assistant text is recovered while a process exits abnormally or is
  interrupted, the turn can complete with `recovered: true`; otherwise the
  failure policy decides whether to retry or terminate.

Failed provider turns append a canonical provider-failure event. Its durable
payload records the phase, actor, backend, category, retryability, attempt
count, timeout/stall state, return code, remediation, and sanitized detail.
Credential-looking values are redacted and raw provider detail is not retained.

## Durable record

Provider outcomes are represented through `relay.event/v1` records and
BlobRef-backed payload content. New records include enough outcome information
to distinguish normal completion, recovery, timeout, and stall without storing
machine-local provider paths. The immutable `relay.plan/v1` determines the
provider policy that applies to the session.

When an integration input is named, its bytes are read once before execution,
stored as a session blob, and bound into the plan. Provider prompts load that
recorded blob; the source path is not part of the provider lifecycle.

## Test boundary

The provider suite uses embedded-adapter protocol fakes and temporary fake
provider executables. It covers provider mapping for Codex, Claude, and
Gemini; retry classification and exhaustion; authentication short-circuiting;
watchdog behavior; and partial-output recovery. Live credentials and live
network calls are outside the automated test boundary.
