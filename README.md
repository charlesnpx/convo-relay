# convo-relay

Orchestrate live dialogues between AI coding backends such as **OpenAI Codex CLI**, **Claude Code**, and **Gemini CLI**.

Give it a task like "review this auth module for vulnerabilities", "debate monorepo vs polyrepo", or "collaboratively write integration tests", and it runs a structured back-and-forth conversation between two backend slots. Every debater turn is saved, and a separate facilitator tracks what is settled, contested, and withdrawn. The common case is `codex,claude`, but the relay also supports reversed ordering, mixed pairs such as `codex,gemini`, same-backend pairs such as `claude,claude` or `codex,codex`, and single-value shorthand like `--agents claude`, `--agents codex`, or `--agents gemini`.

## Depends on / Depended on by

**Depends on:** `github.com/charlesnpx/agentbus` (embeds `engine/` and its
adapters, pinned at `v0.14.4`).

**Depended on by:** `witness`, which invokes the `convo-relay` CLI and, once its
migration lands, imports `github.com/charlesnpx/convo-relay/v2/plan`,
`github.com/charlesnpx/convo-relay/v2/result`, and
`github.com/charlesnpx/convo-relay/v2/bundle`. These public packages are a
published boundary; a change to them is a breaking change. See
[docs/public-contract-versions.md](docs/public-contract-versions.md).

## Why this approach

I found myself often in a situation where I was building a spec, diving into code or a problem, or other similarly complex problems, where I would want to paste context from one AI agent dialogue into another, to see if I could escape one being tunnel visioned, or to benefit from having a different perspective on things. Different models come to different conclusions, which compounds the degree to which the differences between different toolsets (like the differences between how codex and claude code operate) lead to different conclusions (I won't say "different" again in this README, I promise). This approach is a convenient way to automate this process.

Both agents have full access to system tools, web search etc.

The elegant way to create this is of course to implement your own agentic loop with tool calling for both, using their respective APIs, similar to how codex or claude code are themselves implemented, but this is a much more complicated thing to implement and is more difficult to maintain. It's something to pursue down the road if it ever makes sense to expand the scope of this. Additionally, we gotta take advantage of those susbsidized subscription plans while we can!


## How it works

1. Creates an isolated session directory under `~/.codex-claude/sessions/`
2. Initializes a git repo in that directory so Codex always has a valid repository
3. Runs alternating turns across two backend slots. Codex and Claude Code use embedded agentbus v0.9.1 engine adapters: Codex via `codex app-server` JSON-RPC and Claude via its stream-json CLI transport; Gemini remains `gemini --output-format json`
4. Uses symmetric framing driven by `--mode`: `cooperative`, `adversarial`, or `steelman`
5. Runs a lightweight facilitator after every turn to maintain a ledger of settled, contested, and withdrawn points
6. Persists the original launch directory so resume and clean keep using the same project context
7. Saves the transcript and session metadata progressively so interrupted sessions remain resumable and steerable
8. Writes a durable execution graph and event log so dynamic relay expansion can be inspected separately from the parent-visible transcript

## Install

```bash
# If your GitHub access is via SSH
git clone git@github.com:charlesnpx/convo-relay.git

# If your GitHub access is via HTTPS credentials / PAT
git clone https://github.com/charlesnpx/convo-relay.git

# Build and install the Go CLI from the checkout
cd convo-relay
make install
```

Requires Go 1.23+ to build. `make install` writes the binary to `~/.local/bin/convo-relay` and bundled assets to `~/.local/share/convo-relay/`. Override `PREFIX`, `BINDIR`, or `DATADIR` if you use a different install prefix. For a private repo, choose the SSH or HTTPS form that matches how `git clone` already works on your machine. Any backend you use must be on your PATH. Mixed pairs and explicit two-value configurations default to a Codex facilitator using `gpt-5.5` with fallback models. The single-value shorthand `--agents claude`, `--agents codex`, or `--agents gemini` uses that backend for both slots and the facilitator.

The Go binary is the only relay CLI. Python is not used for relay orchestration.

### Programmatic plan and result boundary

Programs that compile their own verification recipe or execution profile can
build a complete plan with the public `/v2/plan` package, validate it, write it,
and hand it to `convo-relay run --plan`. Decode `run --json` with `/v2/result`
and portable `export create --portable` manifests with `/v2/bundle`; none of
these consumers need an `internal/` import.

This complete example writes `plan.json` for a one-turn supplied run:

```go
package main

import (
	"log"
	"os"

	"github.com/charlesnpx/convo-relay/v2/plan"
)

func main() {
	value := plan.Plan{
		Kind:          plan.PlanKind,
		SchemaVersion: plan.SchemaVersion,
		SessionID:     "verification-profile",
		Provenance:    plan.ProvenanceSupplied,
		Task:          "verify the supplied execution profile",
		Timeouts:      plan.Timeouts{TurnSeconds: 60, StallSeconds: 30},
		Mode:          plan.ModeCooperative,
		Investigation: plan.InvestigationNormal,
		Actors:        []plan.Actor{{ID: "verifier", Backend: plan.ActorBackendCodex}},
		Schedule:      plan.Schedule{Kind: plan.ScheduleSequence, Turns: 1, Order: []string{"verifier"}},
		ProviderRetry: plan.ProviderRetry{Mode: plan.ProviderRetryForbid, MaxAttempts: 1},
		Workspace:     plan.Workspace{Mode: plan.WorkspaceModeCurrent},
		Inputs:        []plan.Input{},
		Context:       []plan.Input{},
		Skills:        []plan.Input{},
		ChildPolicy:   plan.ChildPolicy{Mode: plan.ChildPolicyDeny, AllowedRecipes: []string{}},
		Result:        plan.Result{Source: plan.ResultSourceLastTurn, Format: plan.ResultFormatText},
	}
	if err := plan.Validate(value); err != nil {
		log.Fatal(err)
	}
	body, err := plan.CanonicalBytes(value)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("plan.json", body, 0o600); err != nil {
		log.Fatal(err)
	}
}
```

For a release install with Go:

```bash
go install github.com/charlesnpx/convo-relay/v2/cmd/convo-relay@latest
```

`go install` installs only the executable. Use the release archive or
`make install` when you also want the bundled skill assets beside the binary.

For a source-only development build, use:

```bash
go build -o ./bin/convo-relay ./cmd/convo-relay
```

Skill installation is packaging-managed and is not a `convo-relay` session
command. The Go port starts at `v1.0.0`; earlier `v0.x` tags belonged to the
Python package line.

### Update the CLI

```bash
cd convo-relay
git pull
make install
```

### Package a release layout

```bash
make package
```

This creates `dist/convo-relay/` with a prefix-style layout:

```text
bin/convo-relay
share/convo-relay/skill/
```

The release layout includes the bundled skill assets for package-managed
installation.

### Release confidence checks

```bash
make test
make test-race
make cross-compile
make package
```

Cross-compilation is a release gate, not a runtime certification claim. `make cross-compile` builds every production package for `darwin/arm64` and `windows/amd64`; it does not execute foreign binaries or compile test packages for those targets.

## Usage

### Using the relay skills

<img width="840" height="161" alt="image" src="https://github.com/user-attachments/assets/9d0a8658-e095-4808-942f-c437e3d14cba" />


### Start a dialogue

Foreground execution is the default. On macOS and Linux, use native
`run --detach` when the relay should outlive the launching tool shell:

```bash
session_id="auth-review-$(date +%Y%m%d-%H%M%S)-$$-$RANDOM"
convo-relay run "Review src/auth/ for security vulnerabilities" \
  --session-id "$session_id" --detach --verbose
```

The command returns a text receipt with `Detached session <id>` and
`Log: <path>`. With `--json`, it returns `kind: "launch_receipt"`,
`session_id`, and `log_path`. Each launch gets a unique log under the selected relay home's
`logs` directory; stdin is null and stdout/stderr, including `--verbose`
progress, are captured there. A zero exit confirms detached startup setup was
acknowledged; it does not mean the relay completed. Inspect the returned log
for startup or provider failures.

For flag-based `run` commands, pass a unique `--session-id` so the caller can
refer to the session directly without searching `list`. A supplied immutable
plan already carries its session ID; `run --plan` rejects `--session-id` as a
structural override. `--detach` applies only to `run`; `resume` remains a
foreground command.

The default pairing is `codex,claude`. To have Claude open instead:

```bash
convo-relay run "Propose a caching strategy for the API layer" --session-id caching-strategy --agents claude,codex
```

Use the same backend in both slots:

```bash
convo-relay run "Pressure-test this rollout plan" --session-id rollout-claude --agents claude,claude
```

Use single-value shorthand to run two slots plus facilitator on the same backend:

```bash
convo-relay run "Pressure-test this rollout plan" --session-id rollout-claude-short --agents claude
convo-relay run "Pressure-test this rollout plan" --session-id rollout-codex-short --agents codex
```

Mixed backend runs default the facilitator to Codex with `gpt-5.5` and fallback models. Use `--facilitator-backend` to choose a different facilitator. `--agents codex` defaults the facilitator to `gpt-5.5` with `medium` effort unless you override the facilitator model explicitly.

By default, `run` auto-stops when the facilitator's ledger has no contested
items and at least one settled or withdrawn item after both participants have
replied, or when the completion-marker string `[[CONVO-RELAY: COMPLETE]]`
occurs in both participants' latest turns. Participants are instructed to emit
it as a standalone line; detection checks for the marker string in each turn.
An empty-ledger stall can also stop with
`stop_reason: "stalled_no_ledger_signal"`. Auto-stop mode has a safety cap of
50 rounds.

Force an exact round count:

```bash
convo-relay run "Pressure-test this rollout plan" --session-id rollout-fixed --rounds 4
```

Raise or lower the auto-stop safety cap:

```bash
convo-relay run "Pressure-test this rollout plan" --session-id rollout-cap --max-rounds 12
```

Use Gemini in a mixed relay:

```bash
convo-relay run "Compare implementation risks" --session-id implementation-risks --agents codex,gemini
```

Pin different models or effort levels per slot:

```bash
convo-relay run "Compare two implementation strategies" --session-id implementation-strategies \
  --agents claude,codex \
  --model-a sonnet --effort-a high \
  --model-b gpt-5.4 --effort-b medium
```

Use a different debate mode:

```bash
convo-relay run "Evaluate our persistence strategy" --session-id persistence-steelman --mode steelman
```

Attach launch context files. `--context` accepts UTF-8 text files only. Context
files are preflighted before a session is created, labeled as `ctx1`, `ctx2`,
persisted as digest-checked input-bundle artifacts, and embedded in the opening
prompt. Skill files use the same preflight and artifact model with `skill1`,
`skill2` labels. Each input file must be UTF-8 text, at most 1 MiB, with a 2 MiB
total limit per input kind. Name the worktree or Git ref containing every cited
file in the task brief, and check that checkout's branch and working-tree state
before launch:

```bash
convo-relay run "Review this migration plan" --session-id migration-plan-review \
  --context docs/plan.md docs/schema.sql \
  --skill docs/review-guidance.md
```

Investigation mode defaults to `auto`, which tells agents to inspect relevant
files/data before making repository or data claims and to cite inspected paths or
launch context labels. Use `--investigation normal` for conceptual relays that
should not require evidence citations. Use `--investigation context_only` with at
least one `--context` file when agents should cite supplied context labels and
avoid repository exploration beyond those files.

Attach a launch plan to persist the plan that kicked off the relay:

```bash
convo-relay run "Pressure-test this rollout plan" --session-id rollout-task-plan \
  --task-plan docs/relay-plan.json
```

Enable dynamic expansion proposals without changing the default two-slot relay behavior:

```bash
convo-relay run "Pressure-test this rollout plan" --session-id dynamic-expansion --dynamic ask
convo-relay show dynamic-expansion --proposals
convo-relay control approve dynamic-expansion sp_123456789abc
convo-relay control reject dynamic-expansion sp_123456789abc --reason "too broad"
```

Dynamic expansion uses predeclared backend profiles and relay recipes. Built-in profiles include `codex-deep`, `codex-fast`, and `gemini-vision`; built-in recipes include `review-panel`, `vision-review`, and `one-pass-review`. Override or extend them with a TOML file:

```toml
[backend_profiles.codex-reviewer]
backend = "codex"
model = "gpt-5.5"
effort = "xhigh"
description = "Deep implementation-risk review"

[relay_recipes.review-panel]
purpose = "Use for persistent contested implementation risks."
participants = ["codex-reviewer", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-reviewer"
mode = "adversarial"
max_rounds = 6
max_depth = 1
auto_approval = "ask"
```

Static child profiles can also be participants inside recipes. In a child profile, `model` is the child recipe id and `effort` is the admitted child round count. If `effort` is omitted, the child recipe's `max_rounds` is used. Facilitator and reducer profiles must still point at normal backends.

```toml
[backend_profiles.impl-panel]
backend = "child"
model = "implementation-review"
effort = 2
description = "Nested implementation review panel."

[relay_recipes.parent-review]
purpose = "Use when a parent review needs a nested implementation panel."
participants = ["codex-deep", "impl-panel"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "adversarial"
max_rounds = 2
max_depth = 2
auto_approval = "ask"

[relay_recipes.implementation-review]
purpose = "Review implementation risk with two Codex profiles."
participants = ["codex-deep", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "adversarial"
max_rounds = 2
max_depth = 1
auto_approval = "ask"
```

Pass it with `--settings ~/.convo-relay/settings.toml`, or set `CONVO_RELAY_SETTINGS`.
For one-off session-scoped recipes, pass a TOML file with `--recipe-file`; those
recipes are merged into the launch runtime snapshot, validated through the same
recipe checks, and persisted as transient recipe artifacts:

```bash
convo-relay run "Review this patch" --session-id patch-review \
  --agents codex,claude \
  --recipe-file ./local-recipes.toml
```

Discover and inspect configured recipes:

```bash
convo-relay recipes list
convo-relay recipes list --status all --json
convo-relay recipes show review-panel
convo-relay recipes show review-panel --view resolved
convo-relay recipes doctor
convo-relay recipes compile review-panel --json
```

`recipes list` shows usable recipes by default in human output. JSON output
includes all statuses unless `--status` is supplied, including invalid or
skipped parseable records that would otherwise be hidden by runtime
normalization. `recipes show` reports declared recipe data and resolved
participant/backend readiness. `recipes doctor` validates settings parseability,
recipe/profile references, installation-only backend readiness, and grouped
root-cause diagnostics.

`recipes compile <id>` is a side-effect-free root-launch preflight. It emits a
machine-readable plan preview; it does not execute providers or create a
session.

Run a configured recipe directly as the root session:

```bash
convo-relay run "Evaluate the supplied records" --session-id bounded-procedure \
  --recipe bounded-procedure \
  --settings ./settings.toml \
  --input source=./source.json \
  --workspace head-copy \
  --json -o ./session-result.json
```

Root recipe mode uses the recipe's exact participant-turn schedule, optional fresh reducer, named inputs, and result policy. It has no compile-target flag because it always selects the root target. Ordinary runs continue to use their existing path.

For recipes with named inputs, `--input name=path` reads each source once,
stores it as a content-addressed session blob, and records its stable name and
digest in the plan.

Root recipes may set `provider_retry = "forbid"` to permit only one runner
launch per provider invocation; omission preserves the legacy `allow` policy.
Retries performed internally by a provider remain outside runner accounting.

Provider CLIs are trusted same-user processes. They run with the invoking user's authority and are not a sandbox or security boundary: they can access any source, session, credential, network, or other path the user can access. Named-input source paths do not survive ingestion; providers receive prompt material loaded from the digest-bound session blobs.

Root recipes use `--workspace current|head-copy`. `current` executes in the launch directory, while `head-copy` executes in a detached worktree at the recorded HEAD commit and tree hash. Neither mode is a security boundary.

Limit a run to 3 rounds:

```bash
convo-relay run "Pressure-test this rollout plan" --session-id rollout-quick --quick
```

Save the transcript to a specific file while running:

```bash
convo-relay run "Write integration tests for the payment module" --session-id payment-tests -o transcript.md
```

Print JSON instead of markdown:

```bash
convo-relay run "Refactor the database connection pool" --session-id database-pool-refactor --json
```

`run` writes an export only when `-o` / `--output` is supplied. To export an existing or incomplete session later, use `convo-relay export create`.

### List sessions

```bash
convo-relay list
```

### View a session

```bash
convo-relay show a1b2c3d4          # prefix match
convo-relay show a1b2c3d4 --json   # structured output
convo-relay show a1b2c3d4 --graph  # execution graph summary
convo-relay show a1b2c3d4 --graph --json
```

`show --json` returns a nested object. For status checks, use the top-level
`status` and `stop_reason`; `summary` contains round and ledger counts, `meta`
contains task and session details, `transcript` contains turns, `diagnostics`
contains recovery facts, and `incomplete` / `export_ready` describe export
suitability. The report also includes `session_id` and `session_dir`.

Session status values are `running`, `interrupted`, `awaiting_decision`,
`completed`, `failed`, and `invalid_result`. A nonterminal session is `running`
while its writer lock is held and `interrupted` after the lock is free;
`awaiting_decision` means an unresolved proposal needs an operator decision.
`invalid_result` is a failed result-validation status. `doctor <id> --json`
wraps health under `health`; its attention flag is
`health.diagnostics.attention_required`. It is true for `interrupted`,
`failed`, `invalid_result`, and `awaiting_decision`, and false for `running` and
`completed`.

After a detached launch, use `show <id> --json` to follow progress. `running`
means active work; `completed` means the transcript is ready to export. Treat
`failed` and `invalid_result` as failures. `interrupted` and
`awaiting_decision` require operator action rather than more waiting.

`show --graph --json` returns a derived graph plus canonical event summaries.
The event log remains the durable authority; the graph is an inspection view.

### Export a session

```bash
convo-relay export create a1b2c3d4 -o transcript.md
convo-relay export create a1b2c3d4 --json -o result.json
convo-relay export create a1b2c3d4 --portable -o evidence-bundle --json
convo-relay export verify evidence-bundle --json
```

`export create` requires an explicit `-o` / `--output` path. Markdown exports include session status, incomplete state, task, session id, final ledger details, compact per-turn ledger counts, and transcript turns. JSON exports include the same structured session report as `show --json`, including diagnostics, and succeed for incomplete sessions when the partial transcript can be read.

`--portable` is separate from display exports. It accepts a terminal direct
root session (a recipe or supplied plan) and atomically publishes a closed
`relay.bundle/v1` directory: one manifest plus the root-session, transcript,
and diagnostics payloads. When the plan references input, context, or skill
blobs, the bundle also carries each raw referenced blob under its `input`
inventory entry. The manifest inventory carries a BlobRef for each payload.
`export verify` rechecks the closed file set and every payload digest.
With `--json`, an invalid export writes `status: "invalid"` to stdout and exits
with status 1. Running, incomplete, tampered, or non-root sessions fail without
publishing the final target.

### Diagnose runtime and sessions

```bash
convo-relay doctor
convo-relay doctor --json
convo-relay doctor a1b2c3d4
convo-relay doctor a1b2c3d4 --probe-auth --json
```

`doctor` combines the read-only global or session health report with backend
readiness. Its JSON report preserves the complete backend readiness body under
`backends`; add `--probe-auth` for supported authentication probes. For a
session, inspect `health.diagnostics.attention_required`; it is true only for
`interrupted`, `failed`, `invalid_result`, and `awaiting_decision` statuses.

### Resume a session

```bash
convo-relay resume a1b2c3d4
convo-relay resume a1b2c3d4 "Now focus on error handling" --rounds 8
convo-relay resume a1b2c3d4 --rounds 3
```

The optional resume prompt is injected into the next resumed turn as new
direction without replacing the original launch prompt stored in session
metadata. `resume --rounds N` grants up to N additional participant turns;
`resume --quick` grants three additional participant turns. The relay may stop
earlier if it converges. If a session has already completed, provide a new turn
budget to grant it more work. Without one, a completed session has nothing left
to resume. Resume inherits the original context refs, mode, and agent pairing.
It rejects new `--context` and `--skill` files. This build does not apply
`resume --max-rounds` or resume-time settings, mode, timeout, model, effort,
facilitator, or backend-replacement overrides.
Like `run`, `resume` writes an export only when `-o` / `--output` is supplied.

`resume` keeps the original pairing and slot order. It uses the runtime config
snapshot saved at launch, so later settings file edits do not change backend
profiles or relay recipes. This build does not apply resume-time settings, mode,
timeout, model, effort, or backend-replacement overrides.

### Steer or stop a session

Queue a prompt for the next resume of a session that is not running:

```bash
convo-relay control steer a1b2c3d4 "Focus on failure modes before continuing"
```

To interrupt a running relay, send `SIGINT` to its owning process. `control
cancel` checks whether that process is still active and tells you when direct
interruption is required:

```bash
convo-relay control cancel a1b2c3d4
```

A session without a pending decision reports `running` while its writer lock
is held and `interrupted` after the lock is released without a terminal event;
use `resume` to continue its remaining work. Steering is accepted only after
the session stops and applies at its next resume. For a completed session, use
`convo-relay resume <id> "<direction>" --rounds <N>` to continue it. This build
does not accept a new context file on `resume`. `awaiting_decision` remains
visible until the pending proposal is resolved. `control cancel` does not send
a signal itself.

Use `show --diff` for contested and withdrawn ledger evolution by round, and
`show --proposals` to inspect dynamic spawn proposals. Use `control approve`
or `control reject` to resolve a proposal.

### Delete a session

```bash
convo-relay clean a1b2c3d4
convo-relay clean --all --home ~/.convo-relay --json
```

### From Claude Code (sub-agent)

The intended primary use case is to spawn a sub-agent that runs the relay and
summarizes results. Launch it with native detach:

```
Use the Agent tool to run this Bash command:

session_id="auth-review-$(date +%Y%m%d-%H%M%S)-$$-$RANDOM"
convo-relay run \
  "Review the auth module for security issues" \
  --session-id "$session_id" --max-rounds 8 --detach --verbose

Use `convo-relay show "$session_id" --json` to follow progress. When its
status is `completed`, export the actual transcript with
`convo-relay export create "$session_id" -o "/tmp/${session_id}.md"`, then read
that file and summarize what each agent found.
```

## Commands

| Command | Description |
|---------|-------------|
| `run` | Start a new relay dialogue |
| `resume` | Continue a previous session or grant it additional turns |
| `list` | List existing sessions |
| `show` | Display a session's transcript |
| `control` | Queue steering for a session's next resume, admit/reject proposals, or cancel a running session |
| `export` | Create or verify transcript and portable exports |
| `recipes` | List, show, and diagnose relay recipes |
| `clean` | Delete a session and all its artifacts |
| `doctor` | Diagnose global/session health and backend readiness |
| `version` | Report the CLI version and public format versions |

## Flags

| Flag | Commands | Description |
|------|----------|-------------|
| `--session-id ID` | run | Choose a stable ID for a flag-based launch; a supplied `--plan` already carries its ID |
| `--detach` | run | Return a launch receipt while the relay continues; supported on macOS and Linux |
| `--rounds N` | run, resume | Set the participant-turn budget. On `resume`, this grants up to `N` additional turns |
| `--max-rounds N` | run | Safety cap for auto-stop mode (default: 50) |
| `--timeout N`, `--patience N` | run | Per-turn timeout in seconds (default: 600) |
| `--mode {cooperative,adversarial,steelman}` | run | Conversation mode |
| `--agents AGENT[,AGENT]` | run | One backend for shorthand (`claude`, `codex`, or `gemini`), or an explicit backend pair (default: `codex,claude`) |
| `--model-a MODEL`, `--model-b MODEL` | run | Model for slot A / slot B |
| `--effort-a EFFORT`, `--effort-b EFFORT` | run | Effort for slot A / slot B |
| `--quick` | run, resume | Force 3 participant turns for `run` or 3 additional participant turns for `resume` |
| `--context FILE [FILE ...]` | run | Attach UTF-8 text context files and persist labeled input-bundle artifacts. Limits: 1 MiB per file, 2 MiB total |
| `--skill FILE [FILE ...]` | run | Attach UTF-8 capability files and persist labeled input-bundle artifacts |
| `--recipe-file FILE` | run | Attach session-scoped transient relay recipes from TOML and persist the source artifact |
| `--recipe ID` | run | Execute a configured recipe directly as the root session |
| `--plan FILE` | run | Execute a supplied immutable plan document as the root session |
| `--blobs DIR` | run --plan | Supply `sha256/<digest>` payload files referenced by the plan |
| `--input NAME=PATH` | run --recipe | Read a named file once and bind its content-addressed blob to the root plan |
| `--workspace {current,head-copy}` | run --recipe | Execute in the launch directory or a detached worktree at recorded HEAD |
| `--task-plan FILE` | run | Attach the launch task plan from a JSON or markdown/text file |
| `--investigation {auto,normal,context_only}` | run | Prompt policy for evidence behavior. Default `auto` cites inspected files/context; `normal` is conceptual; `context_only` requires `--context` and avoids repo exploration |
| `--dynamic {off,ask,auto-safe}` | run | Enable dynamic spawn proposal handling. Default is `off` |
| `--settings FILE` | run, doctor, recipes | Read backend profiles and relay recipes from a TOML settings file |
| `--facilitator-backend BACKEND` | run | Override the facilitator backend |
| `--facilitator-model MODEL` | run | Override the facilitator model. Default is `gpt-5.5` for Codex facilitator runs |
| `-v, --verbose` | run, resume | Print progress to stderr |
| `-s, --stream` | run | Stream live subprocess stdout to stderr |
| `-o, --output` | run, resume, export create | Write an export to a specific file. Required for `export create`; optional for `run` and `resume` |
| `--portable` | export create | Write a complete terminal successor root session as a new portable directory |
| `--json` | run, resume, list, show, control, export, recipes, clean, doctor, version | Use JSON instead of markdown |
| `--status {usable,unavailable,invalid,skipped,all}` | recipes list | Filter recipes by catalog status |
| `--view {all,declared,resolved}` | recipes show | Select declared and/or resolved recipe details |
| `--all` | clean | Mark orphaned sessions across a relay home |
| `--probe-auth` | doctor | Run supported non-model authentication probes |
| `--limit N` | list, clean --all | Max sessions to show or scan |

## Session boundaries

Session separation is an organizational and provider-state boundary, not a
security boundary. Backend processes run as the current user and retain that
user's filesystem and network authority, including access to source and
session paths outside the slot-scoped directories.

Root recipe sessions record the selected workspace mode. A `head-copy` session
owns a detached worktree, and `clean` removes that worktree and its Git
registration before removing the session files.

Each session is a managed directory below the configured relay home:

```text
<session>/
├── session.json        # immutable relay.plan/v1
├── events.jsonl        # canonical relay.event/v1 stream
├── blobs/sha256/       # content-addressed payload bytes
└── runtime/            # local provider reconstruction data
```

`session.json`, `events.jsonl`, and the referenced blobs are the durable
authority. The `runtime/` directory is local implementation state used for
resume and does not introduce another public format. `show --json` derives its
report from that authority rather than from parallel metadata or transcript
files.

`convo-relay show <id> --json` returns a report derived from the session plan,
event log, and blobs. For example:

```json
{
  "session_id": "...",
  "session_dir": "...",
  "status": "running",
  "stop_reason": "",
  "meta": {
    "task": "...",
    "title": "...",
    "status": "running",
    "stop_reason": "",
    "mode": "adversarial",
    "rounds": 50,
    "round_limit_mode": "auto",
    "max_rounds": 50,
    "actual_rounds": 1,
    "slots": [...],
    "ledger": {"settled": [], "contested": [], "withdrawn": []},
    ...
  },
  "transcript": [
    {
      "round": 1,
      "from": "Claude Code",
      "mode": "adversarial",
      "content": "...",
      "ledger": {"settled": [], "contested": [], "withdrawn": []},
      ...
    }
  ],
  "summary": {
    "status": "running",
    "mode": "adversarial",
    "agents": ["codex", "claude"],
    "configured_rounds": 50,
    "max_rounds": 50,
    "actual_rounds": 1,
    "filtered_rounds": 1,
    "ledger_counts": {
      "settled": 0,
      "contested": 0,
      "withdrawn": 0
    },
    ...
  },
  "diagnostics": {
    "abandoned_attempts": [],
    "unreferenced_blobs": [],
    "budget_state": ""
  },
  "incomplete": true,
  "export_ready": false
}
```

Provider failures that look like bad or expired credentials are non-retryable so
they do not consume the transient retry budget. Failed attempts persist
sanitized provider-failure events with category, retryability, attempt count,
phase, actor, backend, timeout/stall flags, return code, remediation, and
redacted detail; raw provider stderr is not exposed by default.

Codex runs through the embedded agentbus adapter with a slot-scoped
`CODEX_HOME`. Gemini uses a slot-scoped home. Claude uses its configured
same-user credentials. The launch workspace is recorded by the plan and its
events so resume and clean retain the same project context. Override the relay
home with `CODEX_CLAUDE_HOME`.

## License

MIT
