# Root recipe and integration contracts

Convo Relay can execute a configured recipe as the root session. This mode is
for bounded procedures that need an exact turn schedule, named input
snapshots with boundary integrity checks, an optional fresh reducer,
declarative result validation, and durable inspection. It is separate from
ordinary `runner.Run` and from using `relay` as one participant backend.

## Trust boundary

Every provider is a trusted same-user process. A provider runs with the
invoking user's authority; it is not sandboxed, containerized, mounted
read-only, placed under a separate user, or isolated from credentials or the
network. It can access any source repository, session path, or other resource
visible to that user, regardless of the selected workspace policy.

The integrity and workspace controls below detect and report changes at
orchestration boundaries. They do not prevent a provider from making changes
between checks and are not a security boundary.

## Portability gate

Cross-compilation is a merge gate, not evidence of runtime certification.
CI builds every production package and compiles practical test packages for
`darwin/arm64` and `windows/amd64`. It does not execute foreign binaries and
does not certify runtime support on macOS or Windows.

The Linux test job keeps `go vet ./...`, `go test ./... -count=1`, and
`make test-without-optional-defaults` as separate validation steps.

A live-process graceful stop is unsupported on Windows. The request returns
an explicit error without changing session state or removing PID and cleanup
evidence; `kill` and `stop --kill` retain the force-kill path.

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
feasibility, then persists retained input snapshots and verifies them before
the first participant turn.

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

## Default Witness recipes

The optional default catalog preserves the six v1 recipe names and their v1
contract bindings. It also provides six parallel v2 names:

| Generation | Recipe ids | Opaque integration contract |
|---|---|---|
| defect v1 | `witness-falsify`, `witness-falsify-codex`, `witness-falsify-claude` | `witnessed-review/witness-falsification-v1` |
| defect v2 | `witness-falsify-v2`, `witness-falsify-v2-codex`, `witness-falsify-v2-claude` | `witnessed-review/witness-falsification-v2` |
| economy v1 | `economy-equivalence`, `economy-equivalence-codex`, `economy-equivalence-claude` | `witnessed-review/economy-equivalence-v1` |
| economy v2 | `economy-equivalence-v2`, `economy-equivalence-v2-codex`, `economy-equivalence-v2-claude` | `witnessed-review/economy-equivalence-v2` |

The v2 names select the reachability-classified Witness generation. Every
default supplies only orchestration topology and policy: participant,
facilitator, and reducer assignments; turn and result-source policy; retry,
lifecycle, isolation, depth, approval, and conversation mode. The consumer's
integration bundle continues to own every prompt, result schema, assertion,
and adjudication rule. The relay does not interpret either generation's
contract id.

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
retain caller order. The effective per-file ceiling is the smaller of the
contract's `max_bytes` and the runtime `named_input_max_bytes`; the aggregate
runtime ceiling counts raw source bytes before base64 persistence and uses
checked integer accounting.

The public limit diagnostics remain `named_input_file_too_large` for an
individual input and `named_input_total_too_large` for the aggregate ceiling.
Internal accounting may retain a more specific resource classification.

The session persists an ordered manifest plus content-addressed input
artifacts. The execution area receives retained copies, and participant and
reducer prompts receive their paths and content metadata as data. The
facilitator is integrity-checked too, but its prompt receives neither the
named-input projection nor workspace provenance.

Retained copies are writable snapshots, not immutable files and not a
filesystem read-only guarantee. Orchestration verifies their exact directory
layout, type, mode, size, and digest before and after every participant,
facilitator, and reducer attempt—including every retry—then again before
result validation. The mandatory post-attempt check uses a bounded
orchestration-owned context that survives provider or caller cancellation. A
mismatch or verifier failure discards that attempt's output, prevents retry,
and terminates with `named_input_integrity_failed`; any provider failure or
cancellation remains a secondary cause. A provider can still mutate a copy
between checks, so detection occurs at the next boundary.

Provider-boundary failures record a 1-based `provider_attempt` in the
authoritative `named_input_integrity_failure` record and its diagnostic
details. Non-provider boundaries omit that field. A post-attempt verifier that
cannot complete reports the content-free mismatch category
`verification_incomplete`.

Recovery verifies any present retained materialization before session writes
or provider construction and never repairs mismatched evidence. A narrowly
defined legacy session with no descriptor, retained directory, artifact,
index, or graph evidence may materialize the persisted manifest once and
immediately verify it. Inspection and recovery use persisted bytes and
digests, not current source paths. Positional `--context` remains available
for ordinary runs; it is rejected when a selected contract declares named
inputs.

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
CLI request may strengthen but not weaken the recipe minimum. The
`read_only` name does not create a read-only filesystem. Both `read_only` and
`ephemeral` use writable, session-managed detached worktrees. They do not
protect the source repository or session state from a trusted same-user
provider.

Preflight inventories only the source set defined by the workspace contract,
not every same-user-visible path. Required detached execution starts from the
committed tree; a dirty source requires an explicit override and still uses
committed content rather than staged, unstaged, or untracked changes.
Repository inventory includes at most eight repositories, counting the
superproject as depth 1. A ninth repository and an initialized-repository
cycle report `workspace_inventory_depth_exceeded` and
`workspace_inventory_cycle_detected`, respectively; file or byte ceilings use
`workspace_inventory_limit_exceeded`.
Orchestration records source identity and checks that inventoried set again
at terminal finalization. Failed or interrupted root sessions retain their
managed worktree and Git registration until cleanup succeeds.

Committed content is materialized from raw Git objects. This deliberately
does not run Git LFS smudging, clean/smudge filters, `working-tree-encoding`,
end-of-line conversion, export attributes, or other checkout transforms. An
LFS-managed path therefore contains its committed pointer blob. Gitlinks are
materialized as counted empty directories; submodule content is not fetched
or recursively materialized.

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
