---
name: "relay:steer"
description: "Queue new operator context or direction into an already running convo-relay session with `convo-relay control steer`. Use when the user invokes $relay:steer or asks to steer, redirect, add context to, or pass instructions into the current running relay."
argument-hint: "<new context or direction for the running relay>"
---

You are steering an already-running `convo-relay` session. Your job is to identify the intended live session, distill the user's new direction, and queue it with `convo-relay control steer`.

## Identify the target relay

Use the conversation first. The target is obvious when:

- the user named a session ID or prefix
- this conversation launched exactly one relay that is still running
- the user clearly refers to the most recent running relay and there is only one plausible running session

If the target is not obvious, inspect sessions:

```bash
convo-relay list --limit 10
```

For plausible running sessions, get enough context to summarize them:

```bash
convo-relay show <session-id> --json
```

Use `show --json` to inspect `meta.task`, `meta.title`, `meta.status`, `summary`, agents, and the latest transcript entry. Do not infer failure from a quiet running session.

If multiple running relays are plausible, ask the user to choose and do not queue anything yet:

```text
Which relay were you referring to?
1. <session-id> - <short summary: status, agents, rounds, topic>
2. <session-id> - <short summary: status, agents, rounds, topic>
```

If no running relay exists, say that there is no running relay to steer. If there is a completed or interrupted relevant session, suggest `convo-relay resume <session-id> "<direction>"` instead.

## Queue the steering

Distill the user's new context into a concise steering prompt. Include the concrete instruction and any relevant conversation context needed by agents that do not see this chat. Do not dump the whole conversation unless the user explicitly asks for that.

Run:

```bash
convo-relay control steer <session-id> "<new direction>"
```

Report that steering was queued, include the session ID, and summarize the queued direction in one sentence.
