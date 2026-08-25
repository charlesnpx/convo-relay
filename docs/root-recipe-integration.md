# Root recipe and integration contracts

Convo Relay can execute a configured recipe as the root session. This mode is
for bounded procedures that need an exact turn schedule, named inputs ingested
as content-addressed blobs, an optional fresh reducer, declarative
`result.schema` validation, and durable inspection.

## Trust boundary

Every provider is a trusted same-user process. A provider runs with the
invoking user's authority; it is not sandboxed, containerized, mounted
read-only, placed under a separate user, or isolated from credentials or the
network. It can access any source repository, session path, or other resource
visible to that user, regardless of the selected workspace policy.

Workspace mode selects an execution directory; it does not limit a provider's
authority. Named-input source paths are ingested into digest-addressed session
blobs before execution, so later execution reads the recorded blobs rather
than the caller's source paths.

## Portability gate

Cross-compilation is a release gate, not evidence of runtime certification.
CI builds every production package and compiles practical test packages for
`darwin/arm64` and `windows/amd64`. It does not execute foreign binaries and
does not certify runtime support on macOS or Windows.

The Linux test job keeps `go vet ./...` and `go test ./... -count=1` as
separate validation steps.

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

The CLI exposes the root-plan preflight shape:

```text
convo-relay recipes compile <id> [--integration-bundle <path>]
```

`recipes compile` does not execute a provider or create a session. It always
emits the root plan and binds a matching bundle when the recipe declares a
contract. Internal callers still select explicit targets; child profiles,
proposal children, and dynamic children always select the child target.

## Running a root recipe

```bash
convo-relay run "Evaluate the supplied records" \
  --recipe bounded-procedure \
  --settings ./settings.toml \
  --integration-bundle ./integration.json \
  --input source=./source.json \
  --workspace head-copy \
  --json -o ./session-result.json
```

Recipe mode rejects flags that would replace recipe-owned structure,
including `--agents`, participant model or effort overrides, facilitator
overrides, `--mode`, round overrides, `--quick`, and `--dynamic`. Ordinary
relay runs retain their existing flags and behavior.

The root runner performs preflight before creating a session or launching a
provider. It resolves the recipe and profiles, compiles the root plan, binds
the selected contract and inputs, checks backend readiness, ingests named
inputs as blobs, and prepares the selected workspace mode before the first
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
          "max_bytes": 262144
        }
      },
      "result": {
        "transport": "json",
        "schema": {
          "type": "object"
        }
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
lifecycle, depth, approval, and conversation mode. The consumer's integration
bundle continues to own every prompt, result schema, and adjudication rule.
The relay does not interpret either generation's
contract id.

`result.schema` is validated through standard JSON Schema Draft 2020-12.
External result-schema loading is disabled.

## Named inputs

`--input <name>=<path>` reads each named source once, applies configured byte
limits, hashes the bytes, and stores them in the session blob store. The plan
and `input.ingested` event bind the logical name to that blob digest, size, and
media type; the source path is not retained after ingestion. Prompt material
is subsequently read from the bound blobs rather than from caller files.

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
```

Lifecycle rejections occur before session mutation. `stop`, `kill`, and
`clean` remain administrative controls. Recoverable sessions resume from
persisted checkpoints, runtime configuration, recipe, bundle, contract, and
input blobs. Completed participant or reducer work is not replayed when
its durable checkpoint and artifacts are valid.

Root execution uses `--workspace current|head-copy`. `current` uses the launch
directory as supplied. `head-copy` requires a Git repository and creates a
detached worktree at the recorded HEAD commit and tree hash. Neither mode
limits a trusted same-user provider; cleaning a head-copy session removes the
owned worktree and its Git registration.

## Results, persistence, and administration

JSON result transport accepts exactly one JSON value without Markdown fences,
prefixes, suffixes, or trailing values. The raw candidate is always retained.
A valid candidate is schema-checked, canonicalized, and stored as the
canonical result. Invalid output keeps the transcript and raw result,
sets the session to `invalid_result`, records typed diagnostics, and returns a
nonzero command status.

Root sessions persist digest-checked refs for the recipe, plan, runtime
configuration, optional bundle and selected contract, named inputs, workspace,
checkpoints, reducer invocation, raw result, validation report, and canonical
result when valid. Target-specific execution fields stay in their respective
root or child plan digests.

Existing administration commands use the same generic projection:

- `show`, `list`, and `export create` report root execution and validation
  state.
- `doctor` checks artifacts, input blobs, checkpoints, workspace recovery,
  cleanup state, and backend readiness.
- `clean` restores provider-owned resources, removes the managed workspace,
  and only then removes session files; retryable failures retain the session.

Add `doctor --probe-auth` when an explicit supported authentication probe is
needed.
