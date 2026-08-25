# Long-Term Recipe Composition

## Purpose

Recipes should eventually describe composable execution graphs, not only two-slot relays or linear pipelines. The immediate goal is not to build a general workflow engine. The goal is to make the next advanced-recipes slice point in the right direction while keeping the current relay experience simple.

The recommended MVP is nested relay profiles: a recipe can use a backend profile whose backend is `relay` and whose model is another recipe id. That gives us practical composition now, using the relay backend machinery that already exists, without committing to a full pipeline abstraction too early.

## Current Model

Today, relay recipes are two-participant relay definitions. A recipe names two participant profile ids, one facilitator profile id, one reducer profile id, mode, round limits, depth policy, auto-approval policy, and selection keywords.

Backend profiles can describe real backends such as `codex`, `claude`, and `gemini`. They can also describe the `relay` backend, where the profile's `model` points at a child recipe id and `effort` controls the child relay's admitted rounds.

The relay backend already proves the key mechanism: a parent slot can run a child relay, collapse that child result into one parent-facing turn, and persist contract refs back into the parent session. The contract trail already includes:

- `recipe_ref`
- `compiled_plan_ref`
- `child_invocation_ref`
- `child_result_ref`
- `contract_refs`
- strict v1 `session_event` entries
- artifact-index validation

This is enough to support a deeper composition model if we add provenance carefully.

## Composition Graph Concept

A composition graph is the general version of "recipes using recipes." It is an execution graph made of nodes and edges.

Nodes can be:

- backend profiles, such as `codex-deep` or `gemini-vision`
- relay recipes, such as `implementation-review`
- reducers or summarizers
- dynamic expansion nodes
- future validators, checks, or tool steps

Edges describe input flow, output flow, and collapse boundaries. A pipeline is one graph shape, but not the only graph shape.

The current two-slot relay can be described as:

```text
root
  slot_0 = codex-deep
  slot_1 = codex-fast
  facilitator = codex-fast
  reducer = codex-deep
```

A nested relay profile is:

```text
root
  slot_0 = codex-deep
  slot_1 = child relay recipe: implementation-review
```

A linear pipeline is:

```text
interface-review -> implementation-review -> disagreement-reducer
```

A fan-out/fan-in review graph is:

```text
interface-review ------\
                       -> disagreement-reducer -> final result
implementation-review -/
```

The important design point is that contract inspection should not care which graph shape produced a result. It should be able to answer: which node ran, what recipe/profile it used, what input it received, what artifacts it produced, and how the result collapsed upward.

## Why Not Start With Pipelines

Pipelines are useful for known workflows, but they are too large as the first abstraction. A pipeline system needs decisions about:

- step ordering
- dependency handling
- input/output binding
- partial failure policy
- resume and checkpoint semantics
- display and inspection semantics
- reducer behavior between steps
- how operator steering targets a step

Those decisions are real, but they are not the narrowest path to differentiated value.

Convo-relay's strongest behavior is dynamic narrowing of disagreement: when a question remains contested, spawn a focused child debate, collapse the result, and carry structured uncertainty upward. Nested relay profiles exercise that behavior directly. Pipelines can come later as one composition-graph shape.

## Implemented MVP

Nested relay profiles are the first composition slice. Public TOML remains small:

```toml
[backend_profiles.impl-panel]
backend = "relay"
model = "implementation-review"
effort = "2"
description = "Nested implementation review panel."
capabilities = ["composite", "code", "review"]

[relay_recipes.parent-review]
purpose = "Use when a parent review needs a nested implementation panel."
participants = ["codex-deep", "impl-panel"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "adversarial"
max_rounds = 4
max_depth = 2
auto_approval = "ask"
```

In this MVP, relay profiles are valid only as recipe participants. A relay participant's `model` is the child recipe id. Its `effort` is the admitted child round count; if omitted, the child recipe's `max_rounds` is used. Relay profiles remain unsupported for facilitators and reducers.

Internally, treat this as composition, not as a special-case trick. Record provenance paths such as:

```text
root.slot_0
root.slot_1
root.slot_1.slot_0
root.slot_1.slot_1
```

That leaves room for future pipeline paths:

```text
root.steps.interface
root.steps.implementation
root.steps.reducer
```

The MVP should not introduce a general pipeline executor. It should make nested relay composition inspectable and bounded.

## Contract Direction

The artifact-ref model should stay stable. Composition should add provenance to the existing contracts rather than replacing them.

Compiled plans describe where a recipe/profile expansion sits in the composition. Relay-backend run context, session state, completion events, graph nodes, and trace artifacts record the composition path. Contract inspection groups relay-backend child bundles by that path without adding new required strict v1 artifact refs.

The core portable refs remain:

- `recipe_ref`
- `compiled_plan_ref`
- `child_invocation_ref`
- `child_result_ref`

For nested relay profiles, the parent should be able to show a chain like:

```text
root.slot_1
  recipe_ref: recipe:parent-review
  compiled_plan_ref: compiled_plan:...
  child_invocation_ref: child_invocation:...
  child_result_ref: child_result:...
  trace_ref: artifacts/child_traces/...

root.slot_1.slot_0
  recipe/profile: codex-deep inside implementation-review

root.slot_1.slot_1
  recipe/profile: codex-fast inside implementation-review
```

For later pipelines, the same inspection model can show:

```text
root.steps.interface
root.steps.implementation
root.steps.reducer
```

The long-term contract question is where to place composition metadata. The likely answer is:

- strict v1 child invocation/compiled plan refs remain the stable base
- higher-level artifacts describe graph composition and provenance
- `contracts` command renders the combined view

## How Current Work Helps

The Go implementation now owns the relevant boundaries:

- `internal/contracts` owns strict portable child relay contracts and digests.
- `internal/store` owns artifact persistence, artifact indexes, and strict event validation.
- `testdata/contracts/` contains portable contract fixtures for Go compatibility tests.

The contract inspector already validates nested child artifacts. The relay backend already proves that a parent session can run child relays, persist their refs, and expose them through `contracts --json` and `show --graph --json`.

This means advanced recipes do not need to invent a new logging system. They need to add composition provenance to the existing contract/event/artifact model.

## Dogfood Evidence

Post-Go-prep dogfood session:

- parent session: `f3c4627f-433a-45a3-b8f7-3d0a5e00c57b`
- first child session: `c5d3794e`
- second child session: `6f8b171f`
- resumed child session: `f3286014`

The run used a real `relay,relay` parent session with child recipes `interface-contract-review` and `test-strategy`, then resumed the parent for one additional turn.

After resume:

- strict contract validation was OK
- event count was 10
- artifact-index entry count was 10
- relay-backend child contract bundles were 3
- each `recipe_ref`, `compiled_plan_ref`, `child_invocation_ref`, and `child_result_ref` loaded and digest-validated
- display HTML rendered the resumed transcript

The lesson: the contract trail is already inspectable enough to support deeper composition. The missing UX is grouping nested child bundles by composition path rather than only by event order.

## Long-Term Shape

The long-term recipe model should be a composition graph where pipeline is one graph shape, not the whole model.

It should eventually support:

- nested relay nodes
- linear pipelines
- fan-out/fan-in review
- reducer nodes
- dynamic expansion nodes
- later validators/checks

The graph should make these questions answerable:

- What node ran?
- What recipe or profile did it use?
- What input did it receive?
- What child session did it create?
- What artifacts did it produce?
- What did it collapse upward?
- Which unresolved items remained contested?

This aligns with the Go-port direction: keep the portable contract/store model stable, and let richer execution shapes compile down to the same event/artifact primitives.

## Non-Goals For The MVP

- No browser UI.
- No general workflow engine.
- No arbitrary DAG executor.
- No validator/check nodes yet.
- No new display renderer.
- No change to the simple two-agent relay path.
- No replacement of the existing artifact-ref model.

## Open Questions

- How much composition provenance belongs in strict v1 contracts versus higher-level artifacts?
- Should maximum depth be global, per recipe, per branch, or all three?
- Should dynamic recipe selection remain keyword-driven, or move toward profile/card metadata?
- How should `display` group nested child results without overwhelming normal transcripts?
- How should steering target a nested composition path after a parent session has resumed?
