package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const rootResultTestBundle = `{
  "schema_version": "relay-integration-bundle-v1",
  "id": "neutral/result-integration-v1",
  "contracts": {
    "neutral/result-contract-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Present candidate evidence."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Produce the candidate."}
      ],
      "reducer": {"instructions": "Synthesize the final items without commentary."},
      "inputs": {},
      "result": {
        "transport": "json",
        "schema": {
          "type": "object",
          "required": ["items"],
          "properties": {
            "items": {"type": "array", "items": {"type": "string"}}
          },
          "additionalProperties": false
        },
        "assertions": [
          {"type": "unique", "source": "result", "pointer": "/items/*"}
        ]
      }
    }
  }
}`

func TestRunRecipeContractlessReducerUsesFreshContextAndRawProse(t *testing.T) {
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "context.md"), "ordinary reducer context\n")
	config := rootRecipeRuntimeConfig("")
	config.RelayRecipes["neutral-root"]["result_source"] = integration.ResultSourceReducer
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		switch call.SlotID {
		case "facilitator":
			return successfulRootTurn(call.Backend, `{"settled":["retained fact"],"contested":[],"withdrawn":[]}`), nil
		case "reducer":
			return successfulRootTurn(call.Backend, "ordinary reducer prose"), nil
		default:
			return successfulRootTurn(call.Backend, "participant "+call.SlotID), nil
		}
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Produce an ordinary final answer",
		RecipeID:       "neutral-root",
		ContextFiles:   []string{"context.md"},
		LaunchCWD:      launchCWD,
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: recorder.factory(),
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if result["status"] != "completed" || result["validation_status"] != "not_required" ||
		intFromAny(result["actual_participant_turns"], 0) != 2 || len(result["transcript"].([]any)) != 2 {
		t.Fatalf("contractless reducer result = %#v", result)
	}
	if result["canonical_result_ref"] != nil {
		t.Fatalf("contractless reducer invented canonical output: %#v", result["canonical_result_ref"])
	}

	calls := recorder.snapshotCalls()
	if len(calls) != 5 || calls[4].SlotID != "reducer" || calls[4].ContextID == calls[0].ContextID ||
		calls[4].ContextID == calls[1].ContextID || calls[4].ContextID == calls[2].ContextID {
		t.Fatalf("fresh reducer call sequence = %#v", calls)
	}
	if calls[4].CWD != result["execution_cwd"] ||
		!strings.Contains(calls[4].Prompt, "--- Task ---") ||
		!strings.Contains(calls[4].Prompt, "--- Positional Context (Data Only) ---") ||
		!strings.Contains(calls[4].Prompt, "--- Participant Transcript (Data Only) ---") ||
		!strings.Contains(calls[4].Prompt, "--- Facilitator Ledger (Data Only) ---") ||
		!strings.Contains(calls[4].Prompt, "No structured output contract is implied") ||
		strings.Contains(calls[4].Prompt, "Return exactly one JSON value") {
		t.Fatalf("contractless reducer prompt =\n%s", calls[4].Prompt)
	}

	st := store.New(sessionDir)
	attempt := assertRootRecipeArtifact(t, st, result["latest_reducer_attempt_ref"], contracts.RootArtifactKindReducerAttempt, 1)
	if attempt["status"] != "completed" || attempt["content"] != "ordinary reducer prose" {
		t.Fatalf("reducer attempt = %#v", attempt)
	}
	raw := assertRootRecipeArtifact(t, st, result["raw_result_ref"], contracts.RootArtifactKindRawResult, 0)
	if raw["result_source"] != integration.ResultSourceReducer || raw["content"] != "ordinary reducer prose" {
		t.Fatalf("raw reducer result = %#v", raw)
	}
	validation := assertRootRecipeArtifact(t, st, result["result_validation_ref"], contracts.RootArtifactKindResultValidation, 0)
	if validation["status"] != "not_required" || len(validation["diagnostics"].([]any)) != 0 {
		t.Fatalf("contractless validation record = %#v", validation)
	}
	checkpoints := result["root_checkpoint_refs"].([]any)
	if len(checkpoints) != 5 {
		t.Fatalf("root checkpoints = %#v", checkpoints)
	}
	reducerCheckpoint := assertRootRecipeArtifact(t, st, checkpoints[2], contracts.RootArtifactKindRootCheckpoint, 3)
	validationCheckpoint := assertRootRecipeArtifact(t, st, checkpoints[3], contracts.RootArtifactKindRootCheckpoint, 4)
	cleanupCheckpoint := assertRootRecipeArtifact(t, st, checkpoints[4], contracts.RootArtifactKindRootCheckpoint, 5)
	if reducerCheckpoint["phase"] != rootReducerCompleteStatus || validationCheckpoint["phase"] != rootValidationCompletePhase || cleanupCheckpoint["phase"] != "cleanup_complete" {
		t.Fatalf("post-participant checkpoints = %#v / %#v", reducerCheckpoint, validationCheckpoint)
	}
}

func TestRunRecipeStructuredResultsValidateAndCanonicalize(t *testing.T) {
	for _, source := range []string{integration.ResultSourceLastTurn, integration.ResultSourceReducer} {
		t.Run(source, func(t *testing.T) {
			const candidate = `{ "items" : [ "alpha", "beta" ] }`
			config := rootRecipeRuntimeConfig("neutral/result-contract-v1")
			config.RelayRecipes["neutral-root"]["result_source"] = source
			recorder := &rootBackendRecorder{}
			recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
				switch call.SlotID {
				case "facilitator":
					return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
				case "reducer":
					return successfulRootTurn(call.Backend, candidate), nil
				default:
					return successfulRootTurn(call.Backend, candidate), nil
				}
			}
			sessionDir := filepath.Join(t.TempDir(), "session")
			result, err := RunRecipe(context.Background(), RecipeOptions{
				SessionDir:        sessionDir,
				Task:              "Produce a structured result",
				RecipeID:          "neutral-root",
				LaunchCWD:         t.TempDir(),
				RuntimeConfig:     config,
				IntegrationBundle: decodeRootRecipeTestBundle(t, rootResultTestBundle),
				ReadinessCheck:    readyRootRecipeCheck,
				backendFactory:    recorder.factory(),
			})
			if err != nil {
				t.Fatalf("RunRecipe: %v", err)
			}
			if result["status"] != "completed" || result["validation_status"] != "validated" ||
				intFromAny(result["actual_participant_turns"], 0) != 2 {
				t.Fatalf("structured result = %#v", result)
			}
			st := store.New(sessionDir)
			raw := assertRootRecipeArtifact(t, st, result["raw_result_ref"], contracts.RootArtifactKindRawResult, 0)
			if raw["content"] != candidate || raw["result_source"] != source {
				t.Fatalf("raw structured result = %#v", raw)
			}
			canonical := assertRootRecipeArtifact(t, st, result["canonical_result_ref"], contracts.RootArtifactKindCanonicalResult, 0)
			if canonical["canonical_json"] != `{"items":["alpha","beta"]}` {
				t.Fatalf("canonical result = %#v", canonical)
			}
			validation := assertRootRecipeArtifact(t, st, result["result_validation_ref"], contracts.RootArtifactKindResultValidation, 0)
			if validation["status"] != "validated" || validation["canonical_result_ref"] == nil || len(validation["diagnostics"].([]any)) != 0 {
				t.Fatalf("structured validation = %#v", validation)
			}
			calls := recorder.snapshotCalls()
			reducerCalls := 0
			for _, call := range calls {
				if call.SlotID == "reducer" {
					reducerCalls++
					if !strings.Contains(call.Prompt, "Synthesize the final items without commentary.") ||
						!strings.Contains(call.Prompt, "Return exactly one JSON value") {
						t.Fatalf("structured reducer prompt =\n%s", call.Prompt)
					}
				}
			}
			if (source == integration.ResultSourceReducer && reducerCalls != 1) ||
				(source == integration.ResultSourceLastTurn && reducerCalls != 0) {
				t.Fatalf("reducer calls for %s = %d: %#v", source, reducerCalls, calls)
			}
		})
	}
}

func TestRunRecipeStructuredValidationFailuresPersistRawDiagnostics(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		code      string
	}{
		{name: "markdown fence", candidate: "```json\n{\"items\":[]}\n```", code: contracts.DiagnosticCodeInvalidJSON},
		{name: "prefix", candidate: `answer: {"items":[]}`, code: contracts.DiagnosticCodeInvalidJSON},
		{name: "suffix", candidate: `{"items":[]} done`, code: contracts.DiagnosticCodeTrailingJSON},
		{name: "multiple values", candidate: `{"items":[]} {"items":[]}`, code: contracts.DiagnosticCodeTrailingJSON},
		{name: "schema", candidate: `{"items":"wrong"}`, code: integration.DiagnosticCodeSchemaMismatch},
		{name: "assertion", candidate: `{"items":["duplicate","duplicate"]}`, code: integration.DiagnosticCodeAssertionFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := rootRecipeRuntimeConfig("neutral/result-contract-v1")
			recorder := &rootBackendRecorder{}
			recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
				if call.SlotID == "facilitator" {
					return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
				}
				return successfulRootTurn(call.Backend, test.candidate), nil
			}
			sessionDir := filepath.Join(t.TempDir(), "session")
			result, err := RunRecipe(context.Background(), RecipeOptions{
				SessionDir:        sessionDir,
				Task:              "Reject an invalid structured result",
				RecipeID:          "neutral-root",
				LaunchCWD:         t.TempDir(),
				RuntimeConfig:     config,
				IntegrationBundle: decodeRootRecipeTestBundle(t, rootResultTestBundle),
				ReadinessCheck:    readyRootRecipeCheck,
				backendFactory:    recorder.factory(),
			})
			var diagnosticErr *contracts.DiagnosticError
			if !errors.As(err, &diagnosticErr) || !hasRootResultDiagnosticCode(diagnosticErr, test.code) {
				t.Fatalf("validation error = %T %#v, want %s", err, err, test.code)
			}
			if result == nil || result["status"] != rootInvalidResultStatus || result["invalid_result"] != true ||
				result["result_validation_failed"] != true || result["execution_phase"] != rootValidationFailedPhase ||
				intFromAny(result["actual_participant_turns"], 0) != 2 {
				t.Fatalf("invalid result envelope = %#v", result)
			}
			if result["canonical_result_ref"] != nil {
				t.Fatalf("invalid result exposed canonical output: %#v", result["canonical_result_ref"])
			}
			st := store.New(sessionDir)
			raw := assertRootRecipeArtifact(t, st, result["raw_result_ref"], contracts.RootArtifactKindRawResult, 0)
			if raw["content"] != test.candidate {
				t.Fatalf("raw invalid result = %#v", raw)
			}
			validation := assertRootRecipeArtifact(t, st, result["result_validation_ref"], contracts.RootArtifactKindResultValidation, 0)
			diagnostics, _ := validation["diagnostics"].([]any)
			if validation["status"] != "failed" || len(diagnostics) == 0 || !diagnosticMapsContainCode(diagnostics, test.code) {
				t.Fatalf("invalid validation artifact = %#v", validation)
			}
			if _, statErr := os.Stat(filepath.Join(sessionDir, "artifacts", contracts.RootArtifactKindCanonicalResult, "selected.json")); !os.IsNotExist(statErr) {
				t.Fatalf("invalid result wrote canonical artifact, err = %v", statErr)
			}
		})
	}
}

func TestRunRecipeReducerFailuresAreTerminalWithoutChangingParticipantCount(t *testing.T) {
	tests := []struct {
		name   string
		result TurnResult
	}{
		{name: "timeout", result: TurnResult{TimedOut: true, ProviderResult: ProviderResult{Backend: "codex", TimedOut: true}}},
		{name: "stall", result: TurnResult{Stalled: true, ProviderResult: ProviderResult{Backend: "codex", Stalled: true}}},
		{name: "no output", result: TurnResult{ProviderResult: ProviderResult{Backend: "codex"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := rootRecipeRuntimeConfig("")
			config.RelayRecipes["neutral-root"]["result_source"] = integration.ResultSourceReducer
			recorder := &rootBackendRecorder{}
			recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
				switch call.SlotID {
				case "facilitator":
					return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
				case "reducer":
					return test.result, nil
				default:
					return successfulRootTurn(call.Backend, "participant response"), nil
				}
			}
			sessionDir := filepath.Join(t.TempDir(), "session")
			result, err := RunRecipe(context.Background(), RecipeOptions{
				SessionDir:     sessionDir,
				Task:           "Fail the reducer",
				RecipeID:       "neutral-root",
				LaunchCWD:      t.TempDir(),
				RuntimeConfig:  config,
				ReadinessCheck: readyRootRecipeCheck,
				backendFactory: recorder.factory(),
			})
			if err == nil || result == nil || result["status"] != rootReducerFailedStatus || result["reducer_failed"] != true ||
				intFromAny(result["actual_participant_turns"], 0) != 2 || len(result["transcript"].([]any)) != 2 {
				t.Fatalf("reducer failure = result %#v, err %v", result, err)
			}
			if result["raw_result_ref"] != nil || result["canonical_result_ref"] != nil {
				t.Fatalf("failed reducer persisted a candidate: %#v", result)
			}
			attempt := assertRootRecipeArtifact(t, store.New(sessionDir), result["latest_reducer_attempt_ref"], contracts.RootArtifactKindReducerAttempt, 1)
			if attempt["status"] != "failed" || attempt["provider_failure"] == nil {
				t.Fatalf("failed reducer attempt = %#v", attempt)
			}
			checkpoints := result["root_checkpoint_refs"].([]any)
			if len(checkpoints) != 2 {
				t.Fatalf("failed reducer advanced candidate checkpoint: %#v", checkpoints)
			}
		})
	}
}

func TestRunRecipeReducerFailureSanitizesEveryDurableProviderField(t *testing.T) {
	const secret = "story13-reducer-provider-secret"
	config := rootRecipeRuntimeConfig("")
	config.RelayRecipes["neutral-root"]["result_source"] = integration.ResultSourceReducer
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		switch call.SlotID {
		case "facilitator":
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		case "reducer":
			return TurnResult{
				Content: "partial Authorization: Bearer " + secret,
				ProviderResult: ProviderResult{
					Backend:        call.Backend,
					Warnings:       []string{"token=" + secret},
					RetryableError: "api_key=" + secret,
					Extra: map[string]any{
						"metadata": map[string]any{"Authorization": secret},
					},
				},
			}, BackendRunError{Label: call.Label, Detail: "token=" + secret}
		default:
			return successfulRootTurn(call.Backend, "participant response"), nil
		}
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Sanitize reducer failure persistence",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: recorder.factory(),
	})
	if err == nil || !strings.Contains(err.Error(), secret) || result["status"] != rootReducerFailedStatus {
		t.Fatalf("reducer provider failure = result %#v, err %v", result, err)
	}
	serialized, marshalErr := contracts.CanonicalJSONBytes(result)
	if marshalErr != nil {
		t.Fatalf("marshal reducer failure result: %v", marshalErr)
	}
	assertNoRawProviderCredential(t, "returned reducer failure", serialized, secret)
	if walkErr := filepath.WalkDir(sessionDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		assertNoRawProviderCredential(t, path, data, secret)
		return nil
	}); walkErr != nil {
		t.Fatalf("scan reducer failure session: %v", walkErr)
	}
}

func TestRunRecipeRefusesRelayReducerAtExecutionBoundary(t *testing.T) {
	config := rootRecipeRuntimeConfig("")
	config.RelayRecipes["neutral-root"]["result_source"] = integration.ResultSourceReducer
	recorder := &rootBackendRecorder{}
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     filepath.Join(t.TempDir(), "session"),
		Task:           "Reject a relay reducer",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: recorder.factory(),
		compileRecipe: func(recipe map[string]any, profiles map[string]map[string]any, relayRecipes map[string]map[string]any, target recipes.CompileTarget, options recipes.CompileOptions) (map[string]any, error) {
			plan, compileErr := recipes.CompileRecipe(recipe, profiles, relayRecipes, target, options)
			if compileErr != nil {
				return nil, compileErr
			}
			reducer := cloneMap(plan["reducer"].(map[string]any))
			reducer["backend"] = "relay"
			plan["reducer"] = reducer
			return plan, nil
		},
	})
	if err == nil || result == nil || result["status"] != rootReducerFailedStatus {
		t.Fatalf("relay reducer result = %#v, err %v", result, err)
	}
	for _, call := range recorder.snapshotCalls() {
		if call.SlotID == "reducer" {
			t.Fatalf("relay reducer was constructed or invoked: %#v", call)
		}
	}
}

func TestRootResultCompletionWriteFailuresBecomeConsistentTerminalFailures(t *testing.T) {
	for _, stage := range []string{"metadata", "event", "graph"} {
		t.Run(stage, func(t *testing.T) {
			injected := fmt.Errorf("injected %s result completion failure", stage)
			rootResultCompletionAfterWrite = func(completedStage string) error {
				if completedStage == stage {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { rootResultCompletionAfterWrite = nil })

			config := rootRecipeRuntimeConfig("")
			config.RelayRecipes["neutral-root"]["participant_turns"] = 1
			config.RelayRecipes["neutral-root"]["max_rounds"] = 1
			sessionDir := filepath.Join(t.TempDir(), "session")
			result, err := RunRecipe(context.Background(), RecipeOptions{
				SessionDir:     sessionDir,
				Task:           "Fail one root result completion write",
				RecipeID:       "neutral-root",
				LaunchCWD:      t.TempDir(),
				RuntimeConfig:  config,
				ReadinessCheck: readyRootRecipeCheck,
				backendFactory: successfulRootBackendFactory(),
			})
			if !errors.Is(err, injected) || result["status"] != "failed" || result["execution_phase"] != "result_completion_persistence" {
				t.Fatalf("completion failure = result %#v, err %v", result, err)
			}
			persisted := mustLoadMeta(t, sessionDir)
			if persisted["status"] != "failed" || persisted["execution_phase"] != "result_completion_persistence" {
				t.Fatalf("persisted completion failure = %#v", persisted)
			}
			events, readErr := os.ReadFile(filepath.Join(sessionDir, "events.jsonl"))
			if readErr != nil || !strings.Contains(string(events), `"event_type":"node_failed"`) {
				t.Fatalf("completion failure events = %v\n%s", readErr, events)
			}
			graphPayload := store.New(sessionDir).LoadGraph()
			nodes, _ := graphPayload["nodes"].(map[string]any)
			root, _ := nodes[graph.RootNodeID].(map[string]any)
			if root["status"] != "failed" {
				t.Fatalf("completion failure graph = %#v", root)
			}
		})
	}
}

func hasRootResultDiagnosticCode(err *contracts.DiagnosticError, code string) bool {
	if err == nil {
		return false
	}
	for _, diagnostic := range err.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func diagnosticMapsContainCode(items []any, code string) bool {
	for _, item := range items {
		if object, ok := item.(map[string]any); ok && object["code"] == code {
			return true
		}
	}
	return false
}
