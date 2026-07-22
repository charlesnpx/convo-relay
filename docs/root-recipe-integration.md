# Root recipe and integration contracts

Convo Relay can execute a configured recipe as the root session. This mode is
for bounded procedures that need an exact turn schedule, named immutable
inputs, an optional fresh reducer, declarative result validation, and durable
inspection. It is separate from ordinary `runner.Run` and from using `relay`
as one participant backend.

## One compiler, explicit targets

`recipes.CompileRecipe` is the only canonical exported recipe compiler. Every
Go caller supplies one of these targets:

- `recipes.CompileTargetRoot` produces `root_recipe_plan/v1`.
- `recipes.CompileTargetChild` produces the compatible `compiled_plan/v1`
  child payload.

The compiler never derives a target from `integration_contract`. A zero or
unknown target is a validation error. A contractless recipe can compile for
either target. An integration-bound recipe needs a matching bundle for the
root target and returns the typed `recipes.RootOnlyRecipeError` for the child
target.

The CLI makes the same decision explicitly:

```text
convo-relay compile-recipe --recipe <id> --target root|child
```

Omitting `--target` defaults to `child`, preserving the existing compile
command contract. `--target root` enables root-plan compilation and bundle
binding. `run --recipe` has no target flag because it always selects the root
target. Nested relay profiles, relay-backed participants, proposal children,
and dynamic children always select the child target.

## Running a root recipe

```bash
convo-relay run "Evaluate the supplied records" \
  --recipe bounded-procedure \
  --settings ./settings.toml \
  --integration-bundle ./integration.json \
  --input source=./source.json \
  --workspace-isolation ephemeral \
  --json -o ./session-result.json
```

Recipe mode rejects flags that would replace recipe-owned structure,
including `--agents`, participant model or effort overrides, facilitator
overrides, `--mode`, round overrides, `--quick`, and `--dynamic`. Ordinary
relay runs retain their existing flags and behavior.

The root runner performs preflight before creating a session or launching a
provider. It resolves the recipe and profiles, compiles the root plan, binds
the selected contract and inputs, checks backend readiness and workspace
feasibility, then persists the immutable execution inputs before the first
participant turn.

## Integration bundle

An integration bundle is a session-scoped JSON object supplied by the caller.
It is declarative data, not executable code. One bundle may implement several
opaque contract IDs; the recipe binds exactly one matching contract.

```json
{
  "schema_version": "relay-integration-bundle-v1",
  "id": "example-bundle",
  "contracts": {
    "example/bounded-procedure-v1": {
      "turns": [
        {
          "participant_turn": 1,
          "slot": "slot_0",
          "instructions": "Present the supplied material."
        },
        {
          "participant_turn": 2,
          "slot": "slot_1",
          "instructions": "Challenge the presentation."
        }
      ],
      "reducer": {
        "instructions": "Return one JSON object."
      },
      "inputs": {
        "source": {
          "required": true,
          "cardinality": "one",
          "media_type": "application/json",
          "max_bytes": 262144,
          "schema": {
            "type": "object"
          }
        }
      },
      "result": {
        "transport": "json",
        "schema": {
          "type": "object"
        },
        "assertions": []
      }
    }
  }
}
```

Bundle input is strict UTF-8 JSON with duplicate-key and trailing-value
rejection. Runtime configuration limits the raw bundle size. Contract turn
records must cover the exact alternating participant schedule, and reducer
instructions are required when `result_source` is `reducer`. Normalized bundle
and selected-contract digests are recorded separately.

The supported JSON Schema 2020-12 subset includes `type`, `required`,
`properties`, `items`, `enum`, `const`, string and array length bounds,
numeric bounds, `additionalProperties`, `oneOf`, `allOf`,
`if`/`then`/`else`, local `$defs`, and local `$ref`. Unsupported keywords,
boolean root schemas, unresolved local references, and remote or relative
references fail preflight.

Cross-document checks use four generic assertions:

- `unique` verifies the values selected by one JSON Pointer pattern.
- `set_equal` compares selected value sets without order or duplicates.
- `value_equal` compares exactly one value from each operand.
- `field_equal_by_key` compares one value per uniquely keyed array item.

JSON Pointer wildcards occupy a whole segment. Array expansion preserves
order and object expansion sorts keys lexically. Assertion declarations and
sources are validated before execution; data mismatches produce typed result
validation diagnostics.

## Named inputs

`--input <name>=<path>` may be repeated. Names must exactly match the selected
contract. Cardinality, byte size, media type, UTF-8 or JSON requirements, and
optional schemas are checked before provider launch. Repeated `many` values
retain caller order.

The session persists an ordered manifest plus content-addressed input
artifacts. The execution workspace receives verified copies. Inspection and
recovery use the persisted bytes and digests, not the current source paths.
Positional `--context` remains available for ordinary runs; it is rejected
when a selected contract declares named inputs.

## Execution, lifecycle, and recovery

`participant_turns` is the exact number of alternating participant provider
calls. Facilitator and reducer calls do not increment that count. A reducer is
a fresh provider session and never resumes a participant or facilitator
context. `result_source = "last_turn"` instead treats the final participant
response as the candidate result.

Recipes may declare:

```toml
[relay_recipes.bounded-procedure.lifecycle]
resume = "allow"       # or "forbid"
steering = "forbid"    # or "allow"
dynamic = "forbid"     # or "allow"
workspace_isolation = "ephemeral"
```

Lifecycle rejections occur before session mutation. `stop`, `kill`, and
`clean` remain administrative controls. Recoverable sessions resume from
persisted checkpoints, runtime configuration, recipe, bundle, contract, and
input snapshots. Completed participant or reducer work is not replayed when
its durable checkpoint and artifacts are valid.

Workspace policies are ordered `inherited`, `read_only`, and `ephemeral`; a
CLI request may strengthen but not weaken the recipe minimum. Ephemeral mode
uses a verified detached worktree, records source identity and cleanup state,
and detects source-tree mutation. Failed or interrupted root sessions retain
their managed workspace for inspection and retryable cleanup.

## Results, persistence, and administration

JSON result transport accepts exactly one JSON value without Markdown fences,
prefixes, suffixes, or trailing values. The raw candidate is always retained.
A valid candidate is schema- and assertion-checked, canonicalized, and stored
as the canonical result. Invalid output keeps the transcript and raw result,
sets the session to `invalid_result`, records typed diagnostics, and returns a
nonzero command status.

Root sessions persist digest-checked refs for the recipe, plan, runtime
configuration, optional bundle and selected contract, named inputs, workspace,
checkpoints, reducer invocation, raw result, validation report, and canonical
result when valid. Target-specific execution fields stay in their respective
root or child plan digests.

Existing administration commands use the same generic projection:

- `show`, `list`, and `export` report root execution and validation state.
- `contracts` digest-validates refs; `contracts --raw` is the only inspection
  mode that exposes raw artifact payloads.
- `health` checks artifacts, input bytes, checkpoints, retained workspace,
  source integrity, recovery, and cleanup state.
- `display` labels reducer output and canonical output separately.
- `clean` restores provider-owned resources, removes the managed workspace,
  and only then removes session files; retryable failures retain the session.

Backend readiness remains independent from recipe structure. Use
`convo-relay backends status` for installation-only probes and add
`--probe-auth` when an explicit supported authentication probe is needed.

## Compatibility

Ordinary `runner.Run`, `--agents codex`, `--agents relay`, contractless
recipes, existing sessions, `compiled_plan/v1` artifacts, and runtime-config
v1 snapshots retain their prior behavior. The root compiler and runner add a
new direct execution boundary without changing child payloads or introducing
a second exported recipe compiler.
