# convo-relay

Orchestrate live dialogues between AI coding backends such as **OpenAI Codex CLI**, **Claude Code**, **Gemini CLI**, and a composite **Relay** backend.

Give it a task like "review this auth module for vulnerabilities", "debate monorepo vs polyrepo", or "collaboratively write integration tests", and it runs a structured back-and-forth conversation between two backend slots. Every debater turn is saved, and a separate facilitator tracks what is settled, contested, and withdrawn. The common case is `codex,claude`, but the relay also supports reversed ordering, mixed pairs such as `codex,gemini`, same-backend pairs such as `claude,claude` or `codex,codex`, the composite `relay` backend, and single-value shorthand like `--agents claude`, `--agents codex`, or `--agents gemini`.

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

For a release install with Go:

```bash
go install github.com/charlesnpx/convo-relay/cmd/convo-relay@latest
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
make cross-compile-tests
make smoke-fake-providers
make package
```

`make smoke-fake-providers` is the Go-only release smoke gate. It builds the CLI, shadows `codex`, `claude`, and `gemini` with deterministic fake provider binaries in `PATH`, writes sessions into a temp relay home, and checks the required provider matrix plus `resume`, `clean --all`, `clean`, and `show --graph --json`. Live Codex, Claude, and Gemini runs are useful local release evidence, but they are optional and are not required for CI.

Cross-compilation is a release gate, not a runtime certification claim. `make cross-compile` builds every production package for `darwin/arm64` and `windows/amd64`, while `make cross-compile-tests` compiles the repository's practical test packages for those targets. These gates do not execute foreign binaries and do not certify runtime support on macOS or Windows.

## Usage

### Using the relay skills

<img width="840" height="161" alt="image" src="https://github.com/user-attachments/assets/9d0a8658-e095-4808-942f-c437e3d14cba" />


### Start a dialogue

```bash
convo-relay run "Review src/auth/ for security vulnerabilities" --verbose
```

The default pairing is `codex,claude`. To have Claude open instead:

```bash
convo-relay run "Propose a caching strategy for the API layer" --agents claude,codex
```

Use the same backend in both slots:

```bash
convo-relay run "Pressure-test this rollout plan" --agents claude,claude
```

Use single-value shorthand to run two slots plus facilitator on the same backend:

```bash
convo-relay run "Pressure-test this rollout plan" --agents claude
convo-relay run "Pressure-test this rollout plan" --agents codex
```

Mixed backend runs default the facilitator to Codex with `gpt-5.5` and fallback models. Use `--facilitator-backend` to choose a different facilitator. `--agents codex` defaults the facilitator to `gpt-5.5` with `medium` effort unless you override the facilitator model explicitly.

By default, `run` continues until the relay converges: either the facilitator has no contested disagreements left, or both sides explicitly signal completion. Auto-stop mode still has a safety cap of 50 rounds.

Force an exact round count:

```bash
convo-relay run "Pressure-test this rollout plan" --rounds 4
```

Raise or lower the auto-stop safety cap:

```bash
convo-relay run "Pressure-test this rollout plan" --max-rounds 12
```

Use Gemini in a mixed relay:

```bash
convo-relay run "Compare implementation risks" --agents codex,gemini
```

Use a relay as one backend slot. The relay backend runs a nested relay through a recipe and returns one collapsed parent-facing result:

```bash
convo-relay run "Pressure-test this design" --agents codex,relay --model-b review-panel
convo-relay run "Compare two approaches" --agents relay,relay --model-a review-panel --model-b one-pass-review
```

Nested relay recursion is bounded by `CONVO_RELAY_BACKEND_MAX_DEPTH` and defaults to one relay-backend layer.

Pin different models or effort levels per slot:

```bash
convo-relay run "Compare two implementation strategies" \
  --agents claude,codex \
  --model-a sonnet --effort-a high \
  --model-b gpt-5.4 --effort-b medium
```

Use a different debate mode:

```bash
convo-relay run "Evaluate our persistence strategy" --mode steelman
```

Attach launch context files. Context files are preflighted before a session is
created, labeled as `ctx1`, `ctx2`, persisted as digest-checked input-bundle
artifacts, and embedded in the opening prompt. Skill files use the same preflight
and artifact model with `skill1`, `skill2` labels. Each input file must be UTF-8
text, at most 1 MiB, with a 2 MiB total limit per input kind:

```bash
convo-relay run "Review this migration plan" \
  --context docs/plan.md docs/schema.sql \
  --skill docs/capabilities.md
```

Investigation mode defaults to `auto`, which tells agents to inspect relevant
files/data before making repository or data claims and to cite inspected paths or
launch context labels. Use `--investigation normal` for conceptual relays that
should not require evidence citations. Use `--investigation context_only` with at
least one `--context` file when agents should cite supplied context labels and
avoid repository exploration beyond those files.

Attach a launch plan to persist the plan that kicked off the relay:

```bash
convo-relay run "Pressure-test this rollout plan" \
  --task-plan docs/relay-plan.json
```

Enable dynamic expansion proposals without changing the default two-slot relay behavior:

```bash
convo-relay run "Pressure-test this rollout plan" --dynamic ask
convo-relay show a1b2c3d4 --proposals
convo-relay control approve a1b2c3d4 sp_123456789abc
convo-relay control reject a1b2c3d4 sp_123456789abc --reason "too broad"
```

Dynamic expansion uses predeclared backend profiles and relay recipes. Built-in profiles include `codex-deep`, `codex-fast`, `claude-code`, `gemini-vision`, and `relay-review`; built-in recipes include `review-panel`, `vision-review`, and `one-pass-review`. Override or extend them with a TOML file:

```toml
[backend_profiles.codex-reviewer]
backend = "codex"
model = "gpt-5.5"
effort = "xhigh"
description = "Deep implementation-risk review"
capabilities = ["code", "reasoning"]

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

Relay profiles can also be participants inside recipes. In a relay profile, `model` is the child recipe id and `effort` is the admitted child round count. If `effort` is omitted, the child recipe's `max_rounds` is used. Facilitator and reducer profiles must still point at normal backends.

```toml
[backend_profiles.impl-panel]
backend = "relay"
model = "implementation-review"
description = "Nested implementation review panel."
capabilities = ["composite", "code", "review"]

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
convo-relay run "Review this patch" \
  --agents relay,codex \
  --model-a local-review \
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

`recipes list` shows usable recipes and recipes that require an integration bundle by default in human output. JSON output includes all statuses unless `--status` is supplied, including invalid or skipped parseable records that would otherwise be hidden by runtime normalization. `recipes show` reports declared recipe data, integration binding, and resolved participant/backend readiness. `recipes doctor` validates settings parseability, recipe/profile references, nested relay profile rules, installation-only backend readiness, and grouped root-cause diagnostics. A missing integration bundle reports `requires_integration` without degrading list or doctor; pass `--integration-bundle <file>` to list, show, doctor, or `recipes compile` to bind an exact contract.

The optional Witness defaults keep all existing v1 recipe names and contract
bindings available. Parallel `witness-falsify-v2`,
`witness-falsify-v2-codex`, and `witness-falsify-v2-claude` recipes bind
`witnessed-review/witness-falsification-v2`; `economy-equivalence-v2`,
`economy-equivalence-v2-codex`, and `economy-equivalence-v2-claude` bind
`witnessed-review/economy-equivalence-v2`. These v2 names select the
reachability-classified Witness generation. The defaults declare orchestration
topology and policy only. Contract prompts, result schemas, assertions, and
adjudication remain consumer-owned bundle data, and the relay treats each
contract id as opaque.

`recipes compile <id>` is a side-effect-free root-plan preflight. It emits a
machine-readable `root_recipe_plan` and binds a matching integration bundle
when the recipe declares a contract; it does not execute providers or create a
session.

Run a configured recipe directly as the root session:

```bash
convo-relay run "Evaluate the supplied records" \
  --recipe bounded-procedure \
  --settings ./settings.toml \
  --integration-bundle ./integration.json \
  --input source=./source.json \
  --workspace-isolation ephemeral \
  --json -o ./session-result.json
```

Root recipe mode uses the recipe's exact participant-turn schedule, optional fresh reducer, lifecycle minimum, named inputs, and declarative result contract. It has no compile-target flag because it always selects the root target. Ordinary runs and nested relay execution continue to use their existing paths.

For contracts with named inputs, `--investigation context_only` accepts bound
`--input name=path` values as its authority and records their stable names and
artifact refs in `prompt-policy/v2`. This is provider guidance, not a sandbox or
proof of provider behavior.

Root recipes may set `provider_retry = "forbid"` to permit only one runner
launch per provider invocation; omission preserves the legacy `allow` policy.
Retries performed internally by a provider remain outside runner accounting.

Integration bundle v2 adds a `prompt_context` projection. Participant history
is complete; `facilitator_ledger = "trace_only"` retains facilitator artifacts
while excluding that ledger from later participant and reducer prompts.

Provider CLIs are trusted same-user processes. They run with the invoking user's authority and are not a sandbox or security boundary: they can access any source, session, credential, network, or other path the user can access. Named inputs are copied into the session and integrity-checked before and after every provider attempt, during recovery, and before result validation. Those copies are snapshots, not immutable or filesystem read-only objects; a provider can change them between checks, and orchestration detects that change at the next boundary.

The `read_only` and `ephemeral` workspace policy names are compatibility and lifecycle values. Both execute in a writable, session-managed detached worktree. They neither make the filesystem read-only nor protect the source repository or session state from a same-user provider. Failed or interrupted worktrees remain registered for inspection until cleanup succeeds.

See [Root recipe and integration contracts](docs/root-recipe-integration.md) for the unified compiler API, bundle and schema subset, assertions, recovery, persistence, inspection, cleanup, raw Git-object materialization, and trust-boundary contracts.

When a top-level slot uses the `relay` backend, `--model-a` or `--model-b` selects the relay backend recipe for that slot. Nested relay profiles do not read those top-level model flags; their recipe and profile selection comes from the settings file.

Limit a run to 3 rounds:

```bash
convo-relay run "Pressure-test this rollout plan" --quick
```

Save the transcript to a specific file while running:

```bash
convo-relay run "Write integration tests for the payment module" -o transcript.md
```

Print JSON instead of markdown:

```bash
convo-relay run "Refactor the database connection pool" --json
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

`show --json` returns a nested object with:

- `session_id` / `session_dir` for identity and location
- `meta` for persisted session metadata
- `transcript` for turn history
- `summary` for quick status fields and numeric ledger counts
- `diagnostics` for attention-required status, recent sanitized provider failures, and scan metadata
- `incomplete` / `export_ready` for export suitability

`show --graph --json` returns contract-native debug output: the mutable `graph`, raw v1 `session_event` contracts under `events`, and strict v1 `validation`. Event contracts use `event_type`, `timestamp`, and nested `payload` fields.

### Export a session

```bash
convo-relay export create a1b2c3d4 -o transcript.md
convo-relay export create a1b2c3d4 --json -o result.json
convo-relay export create a1b2c3d4 --portable -o evidence-bundle --json
convo-relay export verify evidence-bundle --json
```

`export create` requires an explicit `-o` / `--output` path. Markdown exports include session status, incomplete state, task, session id, final ledger details, compact per-turn ledger counts, and transcript turns. JSON exports include the same structured session report as `show --json`, including diagnostics, and succeed for incomplete sessions when the partial transcript can be read.

`--portable` is separate from those display exports. It accepts a terminal
successor root session, validates its complete artifact-ref closure, and
atomically publishes a new `relay-root-portable-export-v2` directory. The
manifest binds canonical JSON payloads for the root projection, transcript,
diagnostics, and every transitively referenced artifact. Source-session refs
are rewritten to directory-local payload refs that retain source artifact id
and digest. Every payload except the exact root-session, transcript, and
diagnostics projections requires that source identity; the same source id may
appear with different immutable digests, but an exact id/digest pair may appear
only once. Required absolute source, relay-home, session, retained-input, and
worktree paths are omitted. `export verify` rechecks the closed file set,
payload digests, portable source refs, and one-to-one provider
invocation/result lineage. Durable marker-only crashes can leave attempt gaps,
so an export may begin with attempt 2 or omit an earlier attempt. With `--json`,
an invalid export writes a structured `status: "invalid"` result to stdout and
exits with status 1.
Running, recovery-pending, v1, tampered, incomplete, or out-of-root sessions
fail without publishing the final target.

### Diagnose runtime and sessions

```bash
convo-relay doctor
convo-relay doctor --json
convo-relay doctor a1b2c3d4
convo-relay doctor a1b2c3d4 --probe-auth --json
```

`doctor` combines the read-only global or session health report with backend
readiness. Its JSON report preserves the complete backend readiness body under
`backends`; add `--probe-auth` for supported authentication probes.

### Resume a session

```bash
convo-relay resume a1b2c3d4
convo-relay resume a1b2c3d4 "Now focus on error handling"
convo-relay resume a1b2c3d4 --rounds 3
convo-relay resume a1b2c3d4 --mode steelman
convo-relay resume a1b2c3d4 --context docs/new-evidence.md --skill docs/review-skill.md
convo-relay resume a1b2c3d4 --model-a sonnet --model-b opus
convo-relay resume a1b2c3d4 --replace-a gemini --model-a gemini-2.5-pro
```

The optional resume prompt is injected into the next resumed turn as new direction without replacing the original launch prompt stored in session metadata. Resume `--context` and `--skill` files are persisted as labeled input-bundle refs and included in that resumed-turn direction. Free-text prompts do not change relay mode, even if they mention one. Use the typed `--mode` flag to apply a mode change at the resume boundary; mode-control events record the queued and applied rounds, and each transcript entry records the mode used for that round. Advanced backend changes are explicit resume-time slot replacements with `--replace-a` or `--replace-b`; the value may be a backend or runtime profile, and the existing `--model-a`/`--effort-a` or `--model-b`/`--effort-b` flags apply to the replacement. Replacement history is append-only, assigns generation-specific provider slot ids such as `slot_0_gen2`, and preserves prior provider artifacts with cleanup hints. Like `run`, `resume` writes an export only when `-o` / `--output` is supplied. It auto-stops once the relay converges unless you force an exact count with `--rounds`. In auto mode, convergence means either no contested disagreements remain or both sides explicitly signal completion. `--max-rounds` caps additional rounds in auto mode. Slot overrides always apply to the persisted slot order: A = logical `slot_0`, B = logical `slot_1`.

`resume` keeps the original pairing and slot order. It uses the runtime config snapshot saved at launch, so later settings file edits do not change backend profiles or relay recipes for the resumed session. New snapshot-backed sessions reject `resume --settings`; use slot and facilitator model/effort overrides for narrow runtime changes. Override events are recorded separately for slots and facilitators.

### Control a running session

Queue a prompt that will be injected before the next relay turn:

```bash
convo-relay control steer a1b2c3d4 "Focus on failure modes before continuing"
```

To interrupt a running relay, send `SIGINT` to its owning process. `control
cancel` checks whether that process is still active and tells you when direct
interruption is required:

```bash
convo-relay control cancel a1b2c3d4
```

After the owner receives `SIGINT`, the session derives as `interrupted`; use
`resume` to continue its remaining work. `control cancel` does not send a
signal itself.

Use `show --diff` for contested and withdrawn ledger evolution by round, and
`show --proposals` to inspect dynamic spawn proposals. Use `control approve`
or `control reject` to resolve a proposal.

### Delete a session

```bash
convo-relay clean a1b2c3d4
convo-relay clean --all --home ~/.convo-relay --json
```

### From Claude Code (sub-agent)

The intended primary use case — spawn a sub-agent that runs the relay and summarizes results:

```
Use the Agent tool to run:

convo-relay run \
  "Review the auth module for security issues" \
  --max-rounds 8 --verbose -o /tmp/auth-review.md

Then read /tmp/auth-review.md and summarize what each agent found.
```

## Commands

| Command | Description |
|---------|-------------|
| `run` | Start a new relay dialogue |
| `resume` | Continue a previous session until convergence or a round limit |
| `list` | List existing sessions |
| `show` | Display a session's transcript |
| `control` | Steer, admit/reject proposals, or cancel a running session |
| `export` | Create or verify transcript and portable exports |
| `recipes` | List, show, and diagnose relay recipes |
| `clean` | Delete a session and all its artifacts |
| `doctor` | Diagnose global/session health and backend readiness |
| `version` | Report the CLI version and public format versions |

## Flags

| Flag | Commands | Description |
|------|----------|-------------|
| `--rounds N` | run, resume | Run exactly `N` rounds. On `resume`, this means `N` additional rounds |
| `--max-rounds N` | run, resume | Safety cap for auto-stop mode. On `resume`, this means `N` additional rounds (default: 50) |
| `--timeout N`, `--patience N` | run, resume | Per-turn timeout in seconds (default: 600) |
| `--mode {cooperative,adversarial,steelman}` | run, resume | Conversation mode. On `resume`, applies a typed mode control for subsequent rounds |
| `--agents AGENT[,AGENT]` | run | One backend for shorthand (`claude`, `codex`, or `gemini`), or an explicit backend pair (default: `codex,claude`) |
| `--model-a MODEL`, `--model-b MODEL` | run, resume | Model for slot A / slot B. For a top-level `relay` slot, this is the relay recipe id |
| `--effort-a EFFORT`, `--effort-b EFFORT` | run, resume | Effort for slot A / slot B |
| `--replace-a BACKEND_OR_PROFILE`, `--replace-b BACKEND_OR_PROFILE` | resume | Explicitly replace logical slot A / slot B at the resume boundary and start a new slot generation |
| `--quick` | run, resume | Force exactly 3 rounds |
| `--context FILE [FILE ...]` | run, resume | Attach UTF-8 context files and persist labeled input-bundle artifacts. Limits: 1 MiB per file, 2 MiB total |
| `--skill FILE [FILE ...]` | run, resume | Attach UTF-8 capability files and persist labeled input-bundle artifacts |
| `--recipe-file FILE` | run | Attach session-scoped transient relay recipes from TOML and persist the source artifact |
| `--recipe ID` | run | Execute a configured recipe directly as the root session |
| `--input NAME=PATH` | run --recipe | Bind one named contract input; repeat for many-valued inputs |
| `--workspace-isolation {inherited,read_only,ephemeral}` | run --recipe | Request a root workspace lifecycle policy; `read_only` and `ephemeral` both use writable detached worktrees |
| `--task-plan FILE` | run | Attach the launch task plan from a JSON or markdown/text file |
| `--investigation {auto,normal,context_only}` | run | Prompt policy for evidence behavior. Default `auto` cites inspected files/context; `normal` is conceptual; `context_only` requires `--context` and avoids repo exploration |
| `--dynamic {off,ask,auto-safe}` | run | Enable dynamic spawn proposal handling. Default is `off` |
| `--settings FILE` | run, resume, doctor, recipes | Read backend profiles and relay recipes from a TOML settings file |
| `--facilitator-backend BACKEND` | run | Override the facilitator backend |
| `--facilitator-model MODEL` | run, resume | Override the facilitator model. Default is `gpt-5.5` for Codex facilitator runs |
| `-v, --verbose` | run, resume | Print progress to stderr |
| `-s, --stream` | run | Stream live subprocess stdout to stderr |
| `-o, --output` | run, resume, export create | Write an export to a specific file. Required for `export create`; optional for `run` and `resume` |
| `--portable` | export create | Write a complete terminal successor root session as a new portable directory |
| `--json` | run, resume, list, show, control, export, recipes, clean, doctor, version | Use JSON instead of markdown |
| `--status {usable,requires_integration,unavailable,invalid,skipped,all}` | recipes list | Filter recipes by catalog status |
| `--view {all,declared,resolved}` | recipes show | Select declared and/or resolved recipe details |
| `--integration-bundle FILE` | run --recipe, recipes list/show/doctor/compile | Bind a root run, catalog records, or preflight compilation to a strict integration bundle |
| `--all` | clean | Mark orphaned sessions across a relay home |
| `--probe-auth` | doctor | Run supported non-model authentication probes |
| `--limit N` | list, clean --all | Max sessions to show or scan |

## Session isolation

Session separation is an organizational and provider-state boundary, not a
security boundary. Backend processes run as the current user and retain that
user's filesystem and network authority, including access to source and
session paths outside the slot-scoped directories.

Successor root runs persist `relay-workspace-isolation-v1`. The report names
the observed inherited or detached-writable-worktree mechanism and records
source separation and post-run mutation detection independently. It always
reports filesystem, network, process, and same-user containment as `none`;
an `ephemeral` policy label does not imply any of those controls.

Each session is stored under `~/.codex-claude/sessions/<uuid>/`:

```
<uuid>/
├── .git/              # Git repo (satisfies Codex's requirement)
├── codex/             # Slot-scoped CODEX_HOME targets
│   ├── slot_0/
│   └── slot_1/
├── gemini/            # Slot-scoped Gemini HOME targets
│   └── slot_1/
├── meta.json          # Task, mode, slots, ledger, backend state
├── graph.json         # Durable execution graph: nodes, proposals, decisions, artifacts
├── events.jsonl       # Append-only graph events
├── proposals/         # Typed spawn proposals
├── artifacts/         # Child traces, result envelopes, and admitted plans
├── steering.json      # Pending operator steering prompts
└── transcript.json    # Turn-by-turn dialogue plus facilitator ledger snapshots
```

`meta.json` now looks like:

```json
{
  "task": "...",
  "initial_prompt": "...",
  "title": "first 80 chars of the opening turn",
  "round_limit_mode": "auto",
  "rounds": null,
  "max_rounds": 50,
  "mode": "adversarial",
  "slots": [
    {
      "backend": "claude",
      "slot_id": "slot_0",
      "label": "Claude Code",
      "state": {
        "session_id": "...",
        "started": true,
        "cwd": "/path/to/project",
        "model": "sonnet",
        "effort": "high"
      }
    },
    {
      "backend": "codex",
      "slot_id": "slot_1",
      "label": "Codex",
      "state": {
        "thread_id": "...",
        "started": true,
        "cwd": "/path/to/project",
        "model": "gpt-5.4",
        "effort": "xhigh"
      }
    }
  ],
  "ledger": {"settled": [], "contested": [], "withdrawn": []},
  "facilitator_backend": "codex",
  "facilitator_model": "gpt-5.5",
  "investigation_mode": "auto",
  "prompt_policy_version": "prompt-policy/v1",
  "runtime_config_version": "runtime-config/v1",
  "runtime_config_ref": {"kind": "artifact_ref", "schema_version": 1, "id": "artifacts/runtime_config/launch.json", "digest": "sha256:..."},
  "launch_context_refs": [
    {
      "label": "ctx1",
      "display_name": "plan.md",
      "digest": "sha256:...",
      "embedded": true,
      "artifact_ref": {"kind": "artifact_ref", "schema_version": 1, "id": "artifacts/launch_context/ctx1.json", "digest": "sha256:..."}
    }
  ],
  "input_bundle_refs": [
    {"label": "ctx1", "bundle_kind": "context", "phase": "launch", "artifact_ref": {"kind": "artifact_ref", "schema_version": 1, "id": "artifacts/launch_context/ctx1.json", "digest": "sha256:..."}}
  ],
  "transient_recipe_refs": [
    {"label": "recipe_file1", "recipe_ids": ["local-review"], "artifact_ref": {"kind": "artifact_ref", "schema_version": 1, "id": "artifacts/transient_recipes/recipe_file1.json", "digest": "sha256:..."}}
  ],
  "slot_replacement_history": [
    {"logical_slot_id": "slot_0", "previous_slot_id": "slot_0", "new_slot_id": "slot_0_gen2", "previous_backend": "codex", "new_backend": "gemini", "applies_from_round": 3}
  ],
  "launch_cwd": "/path/to/project",
  "created_at": "...",
  "status": "completed",
  "stop_reason": "converged"
}
```

`convo-relay show <id> --json` returns:

```json
{
  "session_id": "...",
  "session_dir": "...",
  "meta": {
    "task": "...",
    "title": "...",
    "round_limit_mode": "auto",
    "rounds": null,
    "max_rounds": 50,
    "mode": "adversarial",
    "slots": [...],
    "ledger": {"settled": [], "contested": [], "withdrawn": []},
    "status": "running"
  },
  "transcript": [
    {
      "round": 1,
      "slot_id": "slot_0",
      "from": "Claude Code",
      "mode": "adversarial",
      "content": "...",
      "ledger": {"settled": [], "contested": [], "withdrawn": []},
      "timestamp": "..."
    }
  ],
  "summary": {
    "status": "running",
    "mode": "adversarial",
    "modes": ["adversarial"],
    "agents": ["codex", "claude"],
    "configured_rounds": null,
    "max_rounds": 50,
    "actual_rounds": 1,
    "ledger_counts": {
      "settled": 0,
      "contested": 0,
      "withdrawn": 0
    }
  },
  "diagnostics": {
    "attention_required": false,
    "session_status": "running",
    "recent_failures": [],
    "scan": {
      "transcript_entries_scanned": 1,
      "event_entries_scanned": 2,
      "truncated": false
    }
  },
  "incomplete": true,
  "export_ready": true
}
```

Each `transcript.json` entry contains:

```json
{
  "round": 2,
  "slot_id": "slot_0",
  "from": "Claude Code",
  "content": "...",
  "ledger": {"settled": [], "contested": [], "withdrawn": []},
  "timestamp": "...",
  "provider_result": {
    "backend": "claude",
    "timed_out": false,
    "stalled": false,
    "recovered": false,
    "return_code": 0,
    "recovery_source": "",
    "warnings": []
  }
}
```

Provider failures that look like bad or expired credentials are treated as non-retryable so they do not consume the full transient retry budget. Failed turns persist sanitized `provider_failure` events with category, retryability, attempt count, phase, actor, backend, timeout/stall flags, return code, remediation, and redacted detail; raw provider stderr is not exposed by default.

Codex runs through an embedded agentbus adapter with `CODEX_HOME` redirected; each slot gets an isolated home that links `auth.json` and `config.toml` from `~/.codex`, and the relay deliberately uses Codex's trusted (`dangerFullAccess`) write posture. Gemini runs with a slot-scoped home while linking existing Gemini config where available. The original launch directory is persisted in `meta.json` so `resume` and `clean` stay tied to the same project context. Claude Code still uses the real `~/.claude/` config for auth and can write project transcripts; `clean` removes only relay-owned Claude artifacts for the saved launch directory, including legacy-compatible cleanup for pre-migration sessions. Override the base directory with `CODEX_CLAUDE_HOME`.

Each slot also persists its own working directory inside `slots[*].state.cwd`. Claude normally uses the original launch directory. Codex also uses that launch directory when it is still inside a git repo; otherwise it falls back to the session git repo so the Codex app-server adapter has a valid repository context. Older pre-slot sessions can still be listed, shown, diffed, and cleaned, but `resume` requires the current slot-based session format.

## License

MIT
