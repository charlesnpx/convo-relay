---
name: relay
description: "Launch a relay dialogue between backend slots such as Codex CLI, Claude Code, Gemini CLI, or same-backend pairings using the convo-relay tool."
argument-hint: "<request — e.g. 'audit this', 'pressure-test the approach', 'help me think through this'>"
---

You are initiating a structured dialogue between two backend slots using `convo-relay`. The usual pairing is Codex CLI plus Claude Code, but the tool can also run reversed ordering, Gemini pairings, same-backend pairings, or single-value shorthand like `--agents claude`, `--agents codex`, and `--agents gemini` when that is the right fit.

Your job is to translate the current conversation context and the user's request into a well-formed `convo-relay run` invocation — not to be a blind passthrough for CLI flags. Still, when the user explicitly provides a valid backend preference, recipe name, or round cap, treat it as operator intent and preserve it unless it conflicts with the task.

## Step 1: Synthesize a task brief

Read back through the conversation and extract what matters:

- What problem or decision is on the table?
- What has been proposed, and what are the open questions or tensions?
- What constraints or requirements have been established?
- What files have been read or modified that are central to the discussion?

Write a **task brief** — a self-contained description of the situation that two agents with no prior context could pick up and work with. This should be a few paragraphs at most. It should include:

- The core question or proposal
- Enough context to understand why it matters and what the constraints are
- Any specific concerns, trade-offs, or areas of uncertainty the user has raised
- What the user is hoping to get out of the dialogue (audit, refinement, alternatives, etc.)

Do NOT just copy-paste the conversation. Distill it.

## Step 2: Choose mode and parameters

Based on the nature of the request and the conversation context, decide:

**Mode** — This is a judgement call based on what would be most productive given the situation, not keyword matching. Consider:

- **adversarial**: The agents challenge each other directly. No hedging, no agreement for agreement's sake. One agent proposes or defends, the other actively tries to find flaws, and they resolve disagreements with evidence. Use this when there's a proposal that needs stress-testing, when you suspect there are hidden problems, when the user wants confidence that something will hold up, or when the discussion has converged too quickly and could benefit from forced dissent.

- **steelman**: Before challenging anything, each agent must restate the other's position in its strongest possible form. If the steelman is better than the original, adopt it. Use this when the broad direction is probably right but the details need refining, when there are multiple legitimate perspectives that need to be reconciled, or when the user wants the best version of an idea rather than a tear-down.

- **cooperative**: Agents build on each other's work constructively. Use this rarely — only when the task is genuinely generative (co-authoring something, brainstorming) rather than evaluative.

Most requests will be adversarial or steelman. Default to **adversarial** when uncertain.

**Rounds**: Default to auto-stop mode. The relay stops when the facilitator's ledger has no contested items and at least one settled or withdrawn item after both participants have replied, or when the completion-marker string `[[CONVO-RELAY: COMPLETE]]` occurs in both participants' latest turns. Participants are instructed to emit it as a standalone line; detection checks for the marker string in each turn. An empty-ledger stall can also stop with `stop_reason: stalled_no_ledger_signal`. Use `--max-rounds` to change the safety cap. `--quick` sets three participant turns for `run` or three additional participant turns for `resume`. Use `--rounds N` only when the user explicitly wants a fixed turn budget.

**Agent pairing**: Default to `--agents codex,claude`. Consider `--agents claude,codex` if you want the opening turn to come from a different perspective than what's been discussed. Use `codex,gemini`, same-backend pairs, or single-value shorthand when that comparison fits the request. Mixed pairs default to a Codex facilitator. `--agents claude`, `--agents codex`, or `--agents gemini` means two slots plus a facilitator on that backend. The CLI flag remains `--agents`; approval previews use `Recipes/slots` because each slot value may be a direct backend profile or builtin backend shorthand.

**Slot overrides**: The first backend is agent A (`slot_0`) and the second is agent B (`slot_1`). Only add `--model-a`, `--effort-a`, `--model-b`, or `--effort-b` when the user has asked for specific models or effort levels, or there is a clear reason to bias one slot.

**Context files**: Identify the 2-5 most relevant text files from the conversation — the code under discussion, architecture docs, etc. Pass these via `--context`; it accepts text files only, up to 1 MiB each and 2 MiB total. Name the worktree or Git ref that contains every cited file in the task brief, and check that checkout's branch and working-tree state before launch. The CLI labels supplied files as `ctx1`, `ctx2`, persists digests, and asks agents to cite those labels when relevant.

**Investigation mode**: Default to `--investigation auto` for code, repo, data, or operational questions. This tells agents to inspect files/data before empirical claims and cite file paths or launch context labels. Use `--investigation normal` only for conceptual debate where evidence citations would be distracting. Use `--investigation context_only` when the user provided enough context and agents should not explore beyond those files.

**Effort levels**: Default to high effort for both agents. For quick/narrow questions, medium is fine.

## Step 3: Present the plan for approval

Before running anything, show the user exactly what you're about to do:

````
## Relay Plan

**Task brief:**
> <the synthesized task brief>

**Mode:** <mode> — <one sentence explaining why this mode>
**Rounds:** <auto up to N, or exact N>
**Recipes/slots:** <slot_a,slot_b>
**Investigation:** <auto|normal|context_only> — <one sentence explaining evidence behavior>
**Context files:** <list>
**Recipe sources:** <none | listed sources>
**Generated/transient recipe preview:**
```toml
<full TOML exactly as it will be passed to the CLI>
```
Source digest: <sha256:...>
Recipe IDs: <ids>
Normalized recipe digests: <recipe-id=sha256:... when available>
**Other flags:** <if any>

Ready to launch?
````

Use `Recipe sources: none` and omit the `Generated/transient recipe preview` block when no transient or generated recipe source is part of the planned invocation. When any `--recipe-file` or `--generated-recipe-file` source is planned, list every source and disclose the full TOML for each source exactly as it will be passed to the CLI, followed by its source digest, recipe IDs, and normalized recipe digests when available. This disclosure is a skill/operator trust boundary: CLI ingestion still validates recipes, but the operator must see the exact transient or generated source being launched before approval.

**Wait for the user to approve, modify, or redirect before proceeding.**

## Step 4: Run the relay

Once approved, use native `run --detach` on macOS or Linux. Foreground execution remains the default, and `--detach` applies only to `run`. Do not add shell backgrounding or manual output redirections:

```bash
session_id="relay-$(date +%Y%m%d-%H%M%S)-$$-$RANDOM"
convo-relay run "<task brief>" --session-id "$session_id" \
  --mode adversarial --agents codex,claude --investigation auto --detach --verbose
```

The text receipt is `Detached session <id>` followed by `Log: <path>`. With `--json`, the receipt has `kind: "launch_receipt"`, `session_id`, and `log_path`. The unique log is under the selected relay home's `logs` directory; stdin is null and stdout/stderr, including `--verbose` progress, are captured there. A zero exit from the launch command confirms startup setup reached its acknowledgment; it does not mean the relay has completed. Inspect the returned log path for startup or provider failures.

Always provide a unique `--session-id` on flag-based `run` commands so the caller can refer to the session without searching `list`. A supplied immutable plan already carries its session ID; `run --plan` rejects `--session-id` as a structural override.

When the caller already has a complete immutable plan document, use the direct
entry point instead of reconstructing it from flags:

```bash
plan_file="/path/to/plan.json"
convo-relay run --plan "$plan_file" --detach --verbose
```

The plan is used as written and must contain its own execution values. If it
contains payload references, add `--blobs <directory>` for a directory
containing `sha256/<lowercase-hex-digest>` files.

Always include `--verbose`. The native detached launcher supplies null stdin and captures stdout/stderr in its unique log.

Add `--max-rounds <N>` when you want to adjust the auto-stop cap. Add `--rounds <N>` only when the user wants an exact fixed round count.

### Execution model

Report the launch receipt and session ID. A detached `run` returns after startup acknowledgment, so do not wait on the launcher command's completion handle for relay completion. Use `convo-relay show <session-id> --json` to follow the session. `resume` remains a foreground command; it does not support `--detach`. If the tool returns a foreground command-session handle for `resume`, wait on that handle for the resume process to finish.

After `show --json` reports `completed`, proceed to Step 5 (present results).

### Interim status

If the user asks for status while the relay is running, check the session immediately:

```bash
convo-relay show <session-id> --json
```

Use the top-level `status` and `stop_reason`, `summary` for round and ledger counts, `meta` for task and session details, `transcript` for turns, `diagnostics` for recovery facts, and `incomplete` / `export_ready` for export suitability. Report completed rounds, session status, latest turn summary, ledger counts, and which backend is likely active now. Track what you already summarized in this conversation so repeated status requests report only new rounds or materially changed metadata.

### Graph and portable export inspection

Use `convo-relay show <session-id>` for ordinary transcript inspection. To save a transcript or JSON report to a file, use `convo-relay export create <session-id> -o <file>` (add `--json` for JSON). Use graph or portable-export commands only when the user asks to inspect execution internals, child relay artifacts, a portable export, or debug state:

```bash
convo-relay show <session-id> --graph --json
convo-relay export create <session-id> --portable -o <bundle-directory> --json
convo-relay export verify <bundle-directory> --json
```

`show --graph --json` returns a derived graph and summaries from the canonical
event log. Treat BlobRef values as path-independent payload references. Use
`export verify` to validate a portable bundle directory and its manifest.

### Status interpretation

- If the relay is still running with no completed rounds yet, say that the opening turn is still in progress rather than speculating that the relay is stuck.
- Public session statuses are `running`, `interrupted`, `awaiting_decision`, `completed`, `failed`, and `invalid_result`. A nonterminal session with a held writer lock is `running`; without the lock it is `interrupted`. `awaiting_decision` means an unresolved proposal needs an operator decision. `invalid_result` is a failed result-validation status.
- Treat `running` as active work. `completed` is ready for transcript export and summary. `failed` and `invalid_result` are failures; inspect the session report and detached log. `interrupted` and `awaiting_decision` need operator action, so do not keep waiting or report success: inspect partial state and decide whether to resume, or resolve the pending proposal.
- `doctor <session-id> --json` wraps health under `health`; its attention flag is `health.diagnostics.attention_required`. It is true for `interrupted`, `failed`, `invalid_result`, and `awaiting_decision`, and false for `running` and `completed`.
- Never say you cannot summarize until the relay finishes. Summarize what is known, label what is pending.
- Long silent periods are normal. One backend may think for minutes before the transcript changes.
- Do not cancel or restart a relay just because the first turn is taking several minutes.
- `started: false`, empty transcript, or missing session subdirectories are **not** sufficient evidence of failure while the relay process is still alive.

## Step 5: Present results

Read the session with `convo-relay show <session-id>`. If a file is needed, create it with `convo-relay export create <session-id> -o <file>`, then read that file. Present a structured summary:

- **What was settled** — points both agents converged on
- **What remains contested** — where they disagreed, with each position summarized
- **What was withdrawn** — ideas that were raised but dropped under scrutiny
- **Key insights** — the most valuable things that emerged, especially ideas or flaws that wouldn't have surfaced from a single agent
- **Investigation evidence** — note the investigation mode and the main file paths or context labels the agents cited, or say when the relay was run in normal conceptual mode
- **Recommended next steps** — what to do with the results

Include the session ID so the dialogue can be resumed if needed.

Completion / failure handling:

- A zero exit from `run --detach` is only a successful launch receipt. Check `show <session-id> --json` for relay completion.
- When status is `completed`, export the actual session transcript with `convo-relay export create <session-id> -o <file>` before summarizing.
- When status is `running` and the user asks for progress, summarize the live session. For `failed` or `invalid_result`, report the failure and inspect the returned log; for `interrupted` or `awaiting_decision`, report the needed action and any partial transcript.
- If a foreground `resume` process times out or is interrupted: inspect the latest session state, report whatever partial output exists, and offer `convo-relay resume`.
- If the detached launch exits nonzero, report the CLI error and inspect its log path when available.

## Resuming a previous dialogue

If the user wants to continue a relay — saying things like "keep going with the relay", "the relay didn't go deep enough", "resume the relay and focus on X" — then:

1. Use the explicit session ID from the original `run` command or its detached launch receipt.
2. Synthesize what additional direction is needed based on what the user said and what the relay produced
3. Show the user the resume plan (similar to Step 3, but noting this is a continuation)
4. Run:

```bash
convo-relay resume <session-id> "<optional new direction>" --verbose
```

Resume inherits the mode, investigation policy, original context refs, runtime config snapshot, and agent pairing from the original session. The optional quoted prompt is injected as new direction for the next resumed turn. To continue a completed session, add `--rounds <N>` to grant up to N additional turns; `--quick` grants three additional participant turns. The relay may stop earlier if it converges. Without a new turn budget, a completed session has no work to resume. Resume rejects new `--context` and `--skill` files. This build does not apply `resume --max-rounds` or resume-time settings, mode, timeout, model, effort, facilitator, or backend-replacement overrides.

Run `resume` in the foreground; do not add `--detach`. Wait on the foreground resume command if the tool provides a command-session handle. If the user asks for progress, inspect `show <session-id> --json` and summarize the current transcript and status.

`control steer` queues direction only when the session is not running, and applies it at the next resume. If a relay is running, interrupt its process first, then queue the direction. To continue a completed session with a direction, use `convo-relay resume <session-id> "<direction>" --rounds <N>`:

```bash
convo-relay control steer <session-id> "<new direction>"
```

If the user asks to stop a running relay, tell them to send `SIGINT` to its owning process. `convo-relay control cancel <session-id>` checks whether that process is still active and tells them when direct interruption is required; it does not send a signal itself. After it is interrupted, use `resume` to continue its remaining work.

## Error handling

- If `convo-relay` is not on PATH: tell the user to clone the repository and run `make install` from the checkout so the Go CLI and bundled assets are installed.
- If a foreground `resume` command is still running and the user asks for status: inspect the live session and summarize partial state rather than refusing
- If the command times out: read whatever partial output exists, report it, and offer to resume
- If a selected backing CLI fails: report the specific error from the output
