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

**Rounds**: Default to auto-stop mode. Let the relay continue until it converges: either the facilitator has no contested disagreements left, or both sides explicitly signal completion. Use `--max-rounds` to raise or lower the safety cap when needed. Use `--quick` only for a strict 3-round relay. Use `--rounds N` only when the user explicitly wants an exact fixed number of rounds.

**Agent pairing**: Default to `--agents codex,claude`. Consider `--agents claude,codex` if you want the opening turn to come from a different perspective than what's been discussed. Use `codex,gemini`, same-backend pairs, or single-value shorthand when that comparison fits the request. Mixed pairs default to a Codex facilitator. `--agents claude`, `--agents codex`, or `--agents gemini` means two slots plus a facilitator on that backend. The CLI flag remains `--agents`; approval previews use `Recipes/slots` because each slot value may be a direct backend profile or builtin backend shorthand.

**Slot overrides**: The first backend is agent A (`slot_0`) and the second is agent B (`slot_1`). Only add `--model-a`, `--effort-a`, `--model-b`, or `--effort-b` when the user has asked for specific models or effort levels, or there is a clear reason to bias one slot.

**Context files**: Identify the 2-5 most relevant files from the conversation — the code under discussion, architecture docs, CLAUDE.md, etc. Pass these via `--context`. Don't dump everything; pick what grounds the discussion. Context files must be UTF-8 text, up to 1 MiB each and 2 MiB total; the CLI labels them as `ctx1`, `ctx2`, persists digests, and asks agents to cite those labels when relevant.

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

Once approved, launch the relay using the Bash tool with `run_in_background: true`:

```bash
convo-relay run "<task brief>" --mode <mode> --agents <backend_a,backend_b> --investigation <mode> --context <files...> --verbose
```

When the caller already has a complete immutable plan document, use the direct
entry point instead of reconstructing it from flags:

```bash
convo-relay run --plan <plan.json> [--blobs <blob-directory>] --verbose
```

The plan is used as written and must contain its own execution values. If it
contains payload references, `--blobs` points to a directory containing
`sha256/<lowercase-hex-digest>` files.

Do **not** append `&` — the `run_in_background` parameter handles backgrounding. You will be automatically notified when the process completes.

Always include `--verbose`. The command prints `Session <8-char-id>` to stderr immediately on launch — capture this session ID.

Add `--max-rounds <N>` when you want to adjust the auto-stop cap. Add `--rounds <N>` only when the user wants an exact fixed round count.

### Execution model

After launching, tell the user the session ID and that the relay is running. Use the current tool's normal long-running command behavior: if it provides a completion notification or command-session handle, wait on that handle when you need the final result; otherwise return control to the user with the session ID. Continue responding to the user normally while waiting.

When the relay completes, proceed to Step 5 (present results).

### Interim status

If the user asks for status while the relay is running, check the session immediately:

```bash
convo-relay show <session-id> --json
```

Report completed rounds, session status, latest turn summary, ledger counts, and which backend is likely active now. Track what you already summarized in this conversation so repeated status requests report only new rounds or materially changed metadata.

Status handling should be practical and non-refusal-shaped: perform the requested status check, wait for the active command handle, or state the current known session ID and what command can be run externally.

### Graph and portable export inspection

Use the transcript commands for ordinary status and summaries. Use graph or portable-export commands only when the user asks to inspect execution internals, child relay artifacts, a portable export, or debug state:

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
- Never say you cannot summarize until the relay finishes. Summarize what is known, label what is pending.
- Long silent periods are normal. One backend may think for minutes before the transcript changes.
- Do not cancel or restart a relay just because the first turn is taking several minutes.
- `started: false`, empty transcript, or missing session subdirectories are **not** sufficient evidence of failure while the relay process is still alive.

## Step 5: Present results

Read the transcript file from the path printed by the command and present a structured summary:

- **What was settled** — points both agents converged on
- **What remains contested** — where they disagreed, with each position summarized
- **What was withdrawn** — ideas that were raised but dropped under scrutiny
- **Key insights** — the most valuable things that emerged, especially ideas or flaws that wouldn't have surfaced from a single agent
- **Investigation evidence** — note the investigation mode and the main file paths or context labels the agents cited, or say when the relay was run in normal conceptual mode
- **Recommended next steps** — what to do with the results

Include the session ID so the dialogue can be resumed if needed.

Completion / failure handling:

- If the process exits `0`: read the final transcript/output and summarize normally.
- If the process is still running and the user asks for progress: inspect the live session and summarize the partial state instead of refusing.
- If the process is still running and the user did not ask for progress: keep waiting and say only that the relay is still in progress.
- If the process times out or is interrupted: inspect the latest session state, report whatever partial output exists, and offer `convo-relay resume`.
- If the process exits nonzero: report the actual CLI error from stderr/stdout. Do not describe the relay as broken solely because the session files did not update quickly enough.
- If you need deeper confirmation while the relay is still running, prefer checking whether the relay PID is alive before making any failure claim.

## Resuming a previous dialogue

If the user wants to continue a relay — saying things like "keep going with the relay", "the relay didn't go deep enough", "resume the relay and focus on X" — then:

1. Find the session ID from the previous relay output in this conversation, or run `convo-relay list` to find the most recent session
2. Synthesize what additional direction is needed based on what the user said and what the relay produced
3. Show the user the resume plan (similar to Step 3, but noting this is a continuation)
4. Run:

```bash
convo-relay resume <session-id> "<optional new direction>" --verbose
```

Resume inherits the mode, investigation policy, context refs, runtime config snapshot, and agent pairing from the original session. The optional quoted prompt is injected as new direction for the next resumed turn. Default to auto-stop mode with a 50-round additional cap. Use `--max-rounds <N>` to change that cap, or `--rounds <N>` only when the user wants an exact number of additional rounds. Do not add `--settings` on resume for snapshot-backed sessions. If the user wants model or effort changes on resume, use the slot-indexed flags `--model-a`, `--effort-a`, `--model-b`, and `--effort-b`. Use `--facilitator-model <MODEL>` to override the facilitator model.

Treat resume exactly like a fresh long-running relay:

- wait for the resume process itself, not for immediate transcript growth
- expect long quiet periods between rounds
- if the user asks for a summary so far during resume, inspect the live session and summarize whatever has completed
- avoid inferring failure from stale session files; if the user asks for progress, inspect the live session and summarize whatever has completed

Steering queues on an idle or interrupted session and applies at its next resume. If a relay is running, interrupt its process first, then queue the direction:

```bash
convo-relay control steer <session-id> "<new direction>"
```

If the user asks to stop a running relay, tell them to send `SIGINT` to its owning process. `convo-relay control cancel <session-id>` checks whether that process is still active and tells them when direct interruption is required; it does not send a signal itself. After it is interrupted, use `resume` to continue its remaining work.

## Error handling

- If `convo-relay` is not on PATH: tell the user to clone the repository and run `make install` from the checkout so the Go CLI and bundled assets are installed.
- If the command is still running and the user asks for status: inspect the live session and summarize partial state rather than refusing
- If the command times out: read whatever partial output exists, report it, and offer to resume
- If a selected backing CLI fails: report the specific error from the output
