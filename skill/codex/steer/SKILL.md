---
name: "relay:steer"
description: "Queue new operator context or direction for the next resume of a convo-relay session with `convo-relay control steer`. Use when the user invokes $relay:steer or asks to steer, redirect, add context to, or pass instructions into a relay."
argument-hint: "<new context or direction for the next resume>"
---

You are adding direction to an existing `convo-relay` session. `convo-relay control steer` only accepts a session that is not running; the queued direction takes effect at its next resume. Identify the session, distill the user's new direction, and queue it with `convo-relay control steer`.

## Identify the target relay

Use the conversation first. The target is obvious when:

- the user named a session ID or prefix
- this conversation launched exactly one relay and the user refers to it
- the user clearly refers to the most recent relevant relay and there is only one plausible session

If the target is not obvious, inspect sessions:

```bash
convo-relay list --limit 10
```

For plausible sessions, get enough context to summarize them:

```bash
convo-relay show <session-id> --json
```

Use the top-level `status` and `stop_reason`, `summary`, `meta`, `diagnostics`, `transcript`, `incomplete`, and `export_ready` in `show --json` to identify the session and summarize it. Do not infer failure from a quiet running session.

`doctor --json` wraps health under `health`; its attention flag is `health.diagnostics.attention_required`.

If multiple running relays are plausible, ask the user to choose and do not queue anything yet:

```text
Which relay were you referring to?
1. <session-id> - <short summary: status, agents, rounds, topic>
2. <session-id> - <short summary: status, agents, rounds, topic>
```

If the session is `running`, do not queue steering yet. The operator must interrupt the relay process first; after it stops, queue the direction for the next resume. If it is `interrupted`, queue the direction for the next resume. If it is `awaiting_decision`, inspect and resolve the pending proposal before deciding whether to queue direction or resume. For `failed` or `invalid_result`, inspect the session report before choosing a recovery action. If the session is `completed`, continue it with `convo-relay resume <session-id> "<direction>" --rounds <N>`; this grants up to N additional turns and the relay may stop earlier if it converges. `--quick` grants three additional participant turns on resume. Resume rejects new `--context` or `--skill` files and does not apply `--max-rounds` or resume-time settings, mode, timeout, model, effort, facilitator, or backend-replacement overrides.

## Queue the steering

Distill the user's new context into a concise steering prompt. Include the concrete instruction and any relevant conversation context needed by agents that do not see this chat. Do not dump the whole conversation unless the user explicitly asks for that.

Run:

```bash
convo-relay control steer <session-id> "<new direction>"
```

Report that steering was queued, include the session ID, and summarize the queued direction in one sentence.
