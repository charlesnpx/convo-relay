# Go Migration Plan

## Why This Exists

Convo-relay has grown from a simple two-agent relay into a small orchestration system with durable sessions, strict portable contracts, dynamic child relays, relay-as-backend, and now nested relay profiles. The Python implementation is still working, but the shape of the system has become more about contract replay, process orchestration, session stores, and portable inspection than about Python-specific application logic.

Go is attractive for this project because those responsibilities map well to a compiled CLI:

- single-binary installation and fewer Python environment issues
- stronger typed boundaries for contracts, recipes, graph events, and backend state
- better process/signal control for long-running relay sessions
- easier portability for contract validators and inspectors
- a cleaner base for future composition graphs, pipelines, and richer execution planning

The goal is not a big-bang rewrite. The right next step is a Go shadow implementation that can read and validate Python-created sessions before it owns live orchestration.

## How We Got Here

The original philosophy that worked should remain intact:

- normal relays stay simple
- two-agent debates stay low ceremony
- transcripts remain readable
- recipes are configuration, not a framework tax
- advanced behavior should compile down to the same event/artifact primitives

At the start of the migration, the Python implementation had already moved the repository toward a Go-portable shape:

- `convo_relay.contracts` defines strict v1 JSON contracts and canonical digests.
- `convo_relay.graph` owns the durable graph, artifact index, event log, and strict validation.
- `convo_relay.child_contracts` owns child relay contract bundle persistence.
- `convo_relay.cli_contracts` owns `contracts` report building and formatting.
- `convo_relay.cli_dynamic` owns proposal approval/rejection/listing service logic.
- `convo_relay.engine` owns in-process child relay execution and typed child result normalization.
- `convo_relay.recipes` owns backend profile and relay recipe normalization/compilation.

The nested relay profile work is the current proof point. In `0.4.0`, recipe participants can reference a relay backend profile:

```toml
[backend_profiles.impl-panel]
backend = "relay"
model = "implementation-review"
effort = "2"
description = "Nested implementation review panel."
capabilities = ["composite", "code", "review"]

[relay_recipes.parent-review]
participants = ["codex-deep", "impl-panel"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "adversarial"
max_rounds = 2
max_depth = 2
auto_approval = "ask"
```

The parent relay sees that relay participant as one backend turn. Internally, the relay backend starts a child relay, collapses the result upward, and persists:

- `recipe_ref`
- `compiled_plan_ref`
- `child_invocation_ref`
- `child_result_ref`
- child trace artifacts
- relay-backend child graph nodes
- `composition_path` values such as `root.slot_1` and `root.slot_1.slot_0`

That is the key design constraint for Go: richer execution shapes must compile down to portable event, graph, and artifact primitives.

## Migration Strategy

Port from the outside inward.

The first Go implementation should be read-only and compatibility-focused. It should prove that Go can consume the Python session store exactly as written. Only after that should Go own mutation, child execution, or provider backends.

### Phase 1: Go Contract Inspector

Create a Go module that can read an existing session directory and reproduce the contract inspection surface.

Minimum behavior:

- load `events.jsonl`
- parse strict v1 `session_event` contracts
- load `artifacts/index.json`
- resolve artifact refs by `id` and optional `digest`
- verify canonical JSON digests
- validate nested artifact refs recursively
- emit a JSON report equivalent to `convo-relay contracts --json`
- include relay-backend and dynamic child bundles with `composition_path`

Acceptance criteria:

- Go validates the fixtures in `testdata/contracts/`.
- Go validates real Python-created sessions from recent nested relay runs.
- Go and Python produce materially equivalent `contracts --json` for the same session.
- Python remains the production CLI.

### Phase 2: Go Graph Reader And Repair

Add read-only graph support.

Minimum behavior:

- load `graph.json`
- replay `events.jsonl` into a repaired graph
- preserve root nodes, dynamic child nodes, relay-backend child nodes, edges, artifact refs, and composition paths
- emit JSON equivalent to `show --graph --json`
- produce a compact text summary equivalent to `show --graph`

Acceptance criteria:

- Go graph repair matches Python graph repair on fixtures and dogfood sessions.
- Relay-backend child nodes remain inspectable by `composition_path`.
- Missing/corrupt artifact refs produce explicit validation errors, not silent partial output.

### Phase 3: Go Recipe/Profile Compiler

Port the configuration compiler without running relays yet.

Minimum behavior:

- load default backend profiles and relay recipes
- load TOML settings from an explicit file, `CONVO_RELAY_SETTINGS`, or the default settings path
- normalize backend profiles and relay recipes
- compile recipes into portable compiled plans
- support relay participant profiles
- reject relay facilitators/reducers
- reject unknown profiles and recipe cycles
- enforce `max_depth` and relay-backend depth policy semantics

Acceptance criteria:

- Go compiled plans have the same canonical digests as Python for equivalent inputs.
- Nested relay recipes compile with the same composition paths as Python.
- Invalid nested recipes fail before launch with structured errors.

### Phase 4: Go Session Store Mutations

Only after read-only compatibility is solid, let Go write session artifacts and events.

Minimum behavior:

- create a session directory
- append canonical strict v1 events
- write artifacts atomically
- update artifact index entries without losing historical refs
- write and repair graph state
- preserve Python-readable output

Acceptance criteria:

- Python can inspect a Go-created session with `show --graph --json` and `contracts --json`.
- Go can inspect the same session and produce equivalent reports.
- Mixed Python/Go compatibility fixtures are added.

### Phase 5: Go Runner Shell

Port the orchestration shell before porting every backend implementation detail.

Minimum behavior:

- create sessions
- build slots from backend names and profile settings
- manage turn loop state
- handle fixed rounds and auto-stop metadata
- call backend adapters
- run facilitator updates
- persist transcripts, meta, graph, and events
- handle stop/kill/interruption consistently

Backend strategy:

- start with subprocess adapters that call existing provider CLIs
- keep Python available as a fallback during transition
- do not port nested relay execution until the store, recipes, and contracts are proven compatible

Acceptance criteria:

- a Go-created basic `codex,codex` session can be inspected by Python
- resume semantics are compatible
- interruption and orphan cleanup behavior is at least as good as Python

### Phase 6: Go Owns Relay Backend And Dynamic Expansion

Move the subtle recursive orchestration pieces last.

Minimum behavior:

- relay-as-backend runs child relays in-process or through an internal runner boundary
- nested relay profiles honor `composition_path`, `max_depth`, and settings propagation
- dynamic proposals, approvals, child traces, and parent collapse are Python-compatible
- contested ledger items and child results are preserved through resume/display

Acceptance criteria:

- `relay,relay` with nested recipes works from Go
- `resume`, `contracts --json`, `show --graph --json`, and `display` work on Go-created sessions
- Python can still inspect Go-created nested sessions
- Go can inspect historical Python sessions

### Phase 7: Go CLI Parity And Production Handoff

Make the Go binary usable for the daily session-management and inspection workflows that still require the Python CLI.

Minimum behavior:

- resolve sessions by id or prefix under `CODEX_CLAUDE_HOME`/`--home`, not only by explicit `--session-dir`
- expose Go commands for `list`, `show`, `show --graph`, `show --trace`, `diff`, `steer`, `display --html-only`, `clean`, and `cleanup`
- keep existing Go runner commands compatible with session id/prefix resolution
- consume queued steering prompts during Go `run`/`resume` turns and preserve steering history
- keep Python-readable session state after Go session-management commands mutate metadata or sidecar files
- document remaining intentional Python fallbacks, especially PDF display rendering and non-Codex provider backends

Acceptance criteria:

- Go can list, show, diff, display, steer, clean, and cleanup Go-created sessions
- Go can run `contracts`, `show`, `show --graph`, `proposals`, `approve`, `reject`, `resume`, `stop`, and `kill` against a session id/prefix without requiring `--session-dir`
- Python can inspect a Go-managed session after steering, cleanup, display export, and dynamic child collapse
- Go command output is stable enough for smoke testing and JSON consumers where `--json` exists
- full Go and Python test suites still pass

Intentional handoff fallbacks:

- Go `display` owns HTML export only. PDF rendering remains a Python CLI workflow until the renderer and its browser/runtime dependencies are ported or replaced.
- Go runner support is production-focused on Codex subprocess backends and relay profiles built from them. Provider-specific Claude/Gemini lifecycle handling remains a Python fallback during this handoff.

## Compatibility Contracts To Preserve

These are the portable surfaces Go should treat as compatibility targets:

- strict v1 contract objects:
  - `artifact_ref`
  - `recipe`
  - `compiled_plan`
  - `child_invocation`
  - `child_result`
  - `transcript_entry`
  - `session_event`
  - `session_log`
  - `artifact_index`
- canonical JSON:
  - UTF-8 JSON
  - sorted keys
  - compact separators
  - no trailing newline in digest input
  - digest excludes runtime storage fields already defined in Python
- artifact refs:
  - `id`
  - `digest`
  - digest verification on load
  - support for duplicate ref ids with different digests
- session events:
  - append-only `events.jsonl`
  - strict v1 fields, especially `event_type`, `timestamp`, and nested `payload`
  - no legacy aliases in strict input
- child relay contract refs:
  - `recipe_ref`
  - `compiled_plan_ref`
  - `child_invocation_ref`
  - `child_result_ref`
- composition metadata:
  - `composition_path` belongs in graph nodes, events, traces, reports, and run context
  - it does not need to become a required strict v1 artifact field yet

## What Not To Port First

Do not start with the live relay runner. It is the highest-risk part because it combines provider process behavior, facilitator updates, retry logic, signal handling, nested recursion, settings propagation, transcript compression, and durable state mutation.

Do not start with pipelines or arbitrary DAG recipes. The current long-term direction is composition graphs, where pipelines are one possible shape. The MVP is nested relay profiles, and that should remain the compatibility testbed.

Do not remove Python until Go can inspect Python sessions and Python can inspect Go sessions.

## Risk Areas

The migration will be easy to underestimate in these places:

- canonical JSON must match Python byte-for-byte for digests
- TOML defaults and override merging must match Python behavior
- artifact index updates must preserve historical refs
- graph repair must be event-authoritative
- relay-backend recursion must avoid both runaway nesting and false-positive blocks
- same-thread nested child relay execution differs from concurrent child relay execution
- stop/kill/orphan handling depends on process behavior, not just data shape
- provider-specific backend state must remain restorable across resume
- display depends on session outputs staying readable and stable
- installed `convo-relay` may lag local code, so smoke tests must use local module execution when testing migration work

## Proposed Repository Shape

Keep the Python package in place while adding Go alongside it.

Suggested layout:

```text
cmd/convo-relay/
  main.go
internal/contracts/
internal/store/
internal/graph/
internal/recipes/
internal/inspect/
testdata/contracts/
```

The first Go binary should probably expose only shadow commands, for example:

```bash
go run ./cmd/convo-relay contracts --session-dir ~/.convo-relay/sessions/<id> --json
go run ./cmd/convo-relay show-graph --session-dir ~/.convo-relay/sessions/<id> --json
```

Avoid taking over the installed `convo-relay` command until the read-only compatibility story is proven.

## Practical Next Step

Start with Phase 1 and Phase 2 as one implementation slice:

- create the Go module and contract structs
- port canonical JSON and digest logic
- read artifact indexes and event logs
- validate contract fixtures
- render `contracts --json`
- render `show --graph --json`

This is enough to prove Go can preserve the durable system boundary without touching live orchestration. Once that works, recipe/profile compilation is the next good slice because nested relay profiles are now the most important advanced behavior to keep portable.
