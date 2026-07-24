package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/namedinputs"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const rootNamedInputIntegrityTestBundle = `{
  "schema_version": "relay-integration-bundle-v1",
  "id": "neutral/integrity-integration-v1",
  "contracts": {
    "neutral/integrity-contract-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Inspect the retained input."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Check the retained input."}
      ],
      "reducer": {"instructions": "Return the final object."},
      "inputs": {
        "payload": {
          "required": true,
          "cardinality": "one",
          "media_type": "application/json",
          "max_bytes": 4096,
          "schema": {"type": "object"}
        }
      },
      "result": {
        "transport": "json",
        "schema": {"type": "object"}
      }
    }
  }
}`

func TestRootRetainedInputIntegrityRejectsSuccessfulProviderMutations(t *testing.T) {
	tests := []struct {
		role       string
		reducer    bool
		wantCalls  int
		wantTurns  int
		wantLedger int
	}{
		{role: "participant", wantCalls: 1, wantTurns: 0, wantLedger: 0},
		{role: "facilitator", wantCalls: 2, wantTurns: 1, wantLedger: 0},
		{role: "reducer", reducer: true, wantCalls: 5, wantTurns: 2, wantLedger: 2},
	}
	for _, test := range tests {
		t.Run(test.role, func(t *testing.T) {
			const rejectedOutput = "REJECTED-PROVIDER-OUTPUT-MUST-NOT-PERSIST"
			sessionDir := filepath.Join(t.TempDir(), "session")
			launchCWD := t.TempDir()
			writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"original"}`)
			retainedPath := filepath.Join(sessionDir, "execution", "inputs", "000001")
			recorder := &rootBackendRecorder{}
			var mutation sync.Once
			var mutationErr error
			recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
				target := (test.role == "participant" && call.SlotID != "facilitator" && call.SlotID != "reducer") ||
					(test.role == "facilitator" && call.SlotID == "facilitator") ||
					(test.role == "reducer" && call.SlotID == "reducer")
				if target {
					mutation.Do(func() {
						mutationErr = tamperRootRetainedInput(retainedPath, `{"value":"tampered"}`)
					})
					if mutationErr != nil {
						return TurnResult{}, mutationErr
					}
					return successfulRootTurn(call.Backend, `{"value":"`+rejectedOutput+`"}`), nil
				}
				if call.SlotID == "facilitator" {
					return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
				}
				return successfulRootTurn(call.Backend, `{"value":"accepted"}`), nil
			}

			result, err := RunRecipe(context.Background(), rootNamedInputRecipeOptions(
				sessionDir,
				launchCWD,
				test.reducer,
				recorder,
			))
			failure := assertRootNamedInputIntegrityTerminal(
				t,
				result,
				err,
				test.role,
				namedinputs.IntegrityBoundaryAfterAttempt,
			)
			if got := len(recorder.snapshotCalls()); got != test.wantCalls {
				t.Fatalf("provider calls = %d, want %d: %#v", got, test.wantCalls, recorder.snapshotCalls())
			}
			if got := len(asSlice(result["transcript"])); got != test.wantTurns {
				t.Fatalf("transcript turns = %d, want %d: %#v", got, test.wantTurns, result["transcript"])
			}
			if got := len(asSlice(result["facilitator_output_refs"])); got != test.wantLedger {
				t.Fatalf("facilitator refs = %d, want %d: %#v", got, test.wantLedger, result["facilitator_output_refs"])
			}
			if len(asSlice(result["provider_failures"])) != 0 {
				t.Fatalf("successful rejected attempt invented provider failures: %#v", result["provider_failures"])
			}
			if failure["provider_failure"] != nil {
				t.Fatalf("successful rejected attempt invented a secondary failure: %#v", failure)
			}
			if test.role == "reducer" {
				if len(asSlice(result["reducer_attempt_refs"])) != 0 {
					t.Fatalf("rejected reducer output created attempts: %#v", result["reducer_attempt_refs"])
				}
				for _, field := range []string{
					"latest_reducer_attempt_ref", "raw_result_ref",
					"result_validation_ref", "canonical_result_ref",
				} {
					if result[field] != nil {
						t.Fatalf("rejected reducer output created %s: %#v", field, result[field])
					}
				}
			}
			assertSessionOmitsText(t, sessionDir, rejectedOutput)
		})
	}
}

func TestRootRetainedInputIntegrityWinsOverEveryProviderOutcomeAndPreventsRetry(t *testing.T) {
	tests := []struct {
		name            string
		outcome         func(context.Context, rootBackendCall, string) (TurnResult, error)
		wantCause       string
		wantTimedOut    bool
		wantStalled     bool
		wantRetryable   bool
		wantCanceledErr bool
		cancelOuter     bool
	}{
		{
			name: "provider error",
			outcome: func(_ context.Context, call rootBackendCall, output string) (TurnResult, error) {
				return successfulRootTurn(call.Backend, output), BackendRunError{Label: call.Label, Detail: "provider failed after mutation"}
			},
			wantCause: RootFailureCauseProviderFailed,
		},
		{
			name: "timeout",
			outcome: func(_ context.Context, call rootBackendCall, output string) (TurnResult, error) {
				result := successfulRootTurn(call.Backend, output)
				result.TimedOut = true
				return result, nil
			},
			wantCause:    RootFailureCauseProviderFailed,
			wantTimedOut: true,
		},
		{
			name: "stall",
			outcome: func(_ context.Context, call rootBackendCall, output string) (TurnResult, error) {
				result := successfulRootTurn(call.Backend, output)
				result.Stalled = true
				return result, nil
			},
			wantCause:   RootFailureCauseProviderFailed,
			wantStalled: true,
		},
		{
			name: "provider cancellation",
			outcome: func(_ context.Context, call rootBackendCall, output string) (TurnResult, error) {
				return successfulRootTurn(call.Backend, output), context.Canceled
			},
			wantCause:       RootFailureCauseProviderCanceled,
			wantCanceledErr: true,
		},
		{
			name: "outer cancellation",
			outcome: func(ctx context.Context, call rootBackendCall, output string) (TurnResult, error) {
				return successfulRootTurn(call.Backend, output), ctx.Err()
			},
			wantCause:       RootFailureCauseProviderCanceled,
			wantCanceledErr: true,
			cancelOuter:     true,
		},
		{
			name: "outer cancellation after provider success",
			outcome: func(_ context.Context, call rootBackendCall, output string) (TurnResult, error) {
				return successfulRootTurn(call.Backend, output), nil
			},
			wantCause:       RootFailureCauseProviderCanceled,
			wantCanceledErr: true,
			cancelOuter:     true,
		},
		{
			name: "retryable failure",
			outcome: func(_ context.Context, call rootBackendCall, output string) (TurnResult, error) {
				return successfulRootTurn(call.Backend, output), RetryableProviderError{
					Label:  call.Label,
					Detail: "temporary network error after mutation",
				}
			},
			wantCause:     RootFailureCauseProviderFailed,
			wantRetryable: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const rejectedOutput = "REJECTED-FAILED-PROVIDER-OUTPUT"
			sessionDir := filepath.Join(t.TempDir(), "session")
			launchCWD := t.TempDir()
			writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"original"}`)
			retainedPath := filepath.Join(sessionDir, "execution", "inputs", "000001")
			runCtx := context.Background()
			cancel := func() {}
			if test.cancelOuter {
				var cancelRun context.CancelFunc
				runCtx, cancelRun = context.WithCancel(context.Background())
				cancel = cancelRun
			}
			defer cancel()

			recorder := &rootBackendRecorder{}
			recorder.handler = func(ctx context.Context, call rootBackendCall) (TurnResult, error) {
				if call.SlotID == "facilitator" || call.SlotID == "reducer" {
					return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
				}
				if err := tamperRootRetainedInput(retainedPath, `{"value":"tampered"}`); err != nil {
					return TurnResult{}, err
				}
				if test.cancelOuter {
					cancel()
				}
				return test.outcome(ctx, call, rejectedOutput)
			}

			result, err := RunRecipe(runCtx, rootNamedInputRecipeOptions(
				sessionDir,
				launchCWD,
				false,
				recorder,
			))
			failure := assertRootNamedInputIntegrityTerminal(
				t,
				result,
				err,
				"participant",
				namedinputs.IntegrityBoundaryAfterAttempt,
			)
			if got := len(recorder.snapshotCalls()); got != 1 {
				t.Fatalf("tampered attempt was retried: calls=%d %#v", got, recorder.snapshotCalls())
			}
			causes := asSlice(result["failure_causes"])
			if len(causes) != 2 || causes[0] != RootFailureCauseNamedInputIntegrity || causes[1] != test.wantCause {
				t.Fatalf("ordered failure causes = %#v", causes)
			}
			providerFailure, _ := failure["provider_failure"].(map[string]any)
			if providerFailure == nil ||
				providerFailure["timed_out"] != test.wantTimedOut ||
				providerFailure["stalled"] != test.wantStalled ||
				providerFailure["retryable"] != test.wantRetryable {
				t.Fatalf("secondary provider failure = %#v", providerFailure)
			}
			if test.wantCanceledErr && !errors.Is(err, context.Canceled) {
				t.Fatalf("terminal error does not retain cancellation: %v", err)
			}
			assertSessionOmitsText(t, sessionDir, rejectedOutput)
		})
	}
}

func TestRootRetainedInputIntegrityFailsClosedWhenPostVerifierErrorsOrExpires(t *testing.T) {
	tests := []struct {
		name      string
		timeout   time.Duration
		verifier  rootRetainedInputVerifier
		wantError error
	}{
		{
			name:    "verifier error",
			timeout: time.Second,
			verifier: func(ctx context.Context, st *store.Store, ref map[string]any, role string, boundary string) error {
				if boundary == namedinputs.IntegrityBoundaryAfterAttempt {
					return contracts.NewDiagnosticError(
						"Forced retained-input verifier failure.",
						contracts.NewDiagnostic(
							namedinputs.DiagnosticCodeIntegrity,
							contracts.DiagnosticPhasePolicy,
							"",
							"Forced retained-input verifier failure.",
							map[string]any{"role": role, "attempt_boundary": boundary},
						),
					)
				}
				return namedinputs.VerifyRetained(ctx, st, ref, role, boundary)
			},
		},
		{
			name:    "verifier deadline",
			timeout: 15 * time.Millisecond,
			verifier: func(ctx context.Context, st *store.Store, ref map[string]any, role string, boundary string) error {
				if boundary == namedinputs.IntegrityBoundaryAfterAttempt {
					<-ctx.Done()
					return ctx.Err()
				}
				return namedinputs.VerifyRetained(ctx, st, ref, role, boundary)
			},
			wantError: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const rejectedOutput = "REJECTED-VERIFIER-OUTPUT"
			sessionDir := filepath.Join(t.TempDir(), "session")
			launchCWD := t.TempDir()
			writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"original"}`)
			recorder := &rootBackendRecorder{}
			recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
				return successfulRootTurn(call.Backend, `{"value":"`+rejectedOutput+`"}`), nil
			}
			options := rootNamedInputRecipeOptions(sessionDir, launchCWD, false, recorder)
			options.retainedInputVerifier = test.verifier
			options.retainedInputVerificationTimeout = test.timeout

			result, err := RunRecipe(context.Background(), options)
			assertRootNamedInputIntegrityTerminal(
				t,
				result,
				err,
				"participant",
				namedinputs.IntegrityBoundaryAfterAttempt,
			)
			if len(recorder.snapshotCalls()) != 1 {
				t.Fatalf("verifier failure retried provider: %#v", recorder.snapshotCalls())
			}
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("terminal error = %v, want %v", err, test.wantError)
			}
			if len(asSlice(result["provider_failures"])) != 0 {
				t.Fatalf("verifier failure invented provider failure: %#v", result["provider_failures"])
			}
			assertSessionOmitsText(t, sessionDir, rejectedOutput)
		})
	}
}

func TestRootRetainedInputIntegrityRunsImmediatelyBeforeResultValidation(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"original"}`)
	retainedPath := filepath.Join(sessionDir, "execution", "inputs", "000001")
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, `{"value":"candidate"}`), nil
	}
	var mu sync.Mutex
	boundaries := []string{}
	var mutated bool
	verifier := func(ctx context.Context, st *store.Store, ref map[string]any, role string, boundary string) error {
		mu.Lock()
		boundaries = append(boundaries, role+":"+boundary)
		if boundary == namedinputs.IntegrityBoundaryResultValidation && !mutated {
			mutated = true
			if err := tamperRootRetainedInput(retainedPath, `{"value":"tampered"}`); err != nil {
				mu.Unlock()
				return err
			}
		}
		mu.Unlock()
		return namedinputs.VerifyRetained(ctx, st, ref, role, boundary)
	}
	options := rootNamedInputRecipeOptions(sessionDir, launchCWD, false, recorder)
	options.retainedInputVerifier = verifier

	result, err := RunRecipe(context.Background(), options)
	assertRootNamedInputIntegrityTerminal(
		t,
		result,
		err,
		"result_validation",
		namedinputs.IntegrityBoundaryResultValidation,
	)
	if result["raw_result_ref"] == nil {
		t.Fatalf("candidate was not persisted before validation boundary: %#v", result)
	}
	if result["result_validation_ref"] != nil || result["canonical_result_ref"] != nil {
		t.Fatalf("validation continued after integrity failure: %#v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(boundaries) == 0 || boundaries[len(boundaries)-1] != "result_validation:"+namedinputs.IntegrityBoundaryResultValidation {
		t.Fatalf("verification boundaries = %#v", boundaries)
	}
}

func TestRootNamedInputIntegrityAndSourceMutationPreserveOrderedCauses(t *testing.T) {
	sourceRoot := t.TempDir()
	runTestGit(t, sourceRoot, "init")
	runTestGit(t, sourceRoot, "config", "user.email", "integrity@example.invalid")
	runTestGit(t, sourceRoot, "config", "user.name", "Integrity Test")
	writeRootRecipeTestFile(t, filepath.Join(sourceRoot, "tracked.txt"), "original source\n")
	writeRootRecipeTestFile(t, filepath.Join(sourceRoot, "payload.json"), `{"value":"original"}`)
	runTestGit(t, sourceRoot, "add", "tracked.txt", "payload.json")
	runTestGit(t, sourceRoot, "commit", "-m", "fixture")

	sessionDir := filepath.Join(t.TempDir(), "session")
	retainedPath := filepath.Join(sessionDir, "execution", "inputs", "000001")
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		if err := tamperRootRetainedInput(retainedPath, `{"value":"tampered"}`); err != nil {
			return TurnResult{}, err
		}
		if err := os.WriteFile(filepath.Join(sourceRoot, "tracked.txt"), []byte("mutated source\n"), 0o644); err != nil {
			return TurnResult{}, err
		}
		return successfulRootTurn(call.Backend, `{"value":"rejected"}`), nil
	}
	options := rootNamedInputRecipeOptions(sessionDir, sourceRoot, false, recorder)
	options.RuntimeConfig.RelayRecipes["neutral-root"]["lifecycle"].(map[string]any)["workspace_isolation"] = "ephemeral"

	result, err := RunRecipe(context.Background(), options)
	assertRootNamedInputIntegrityTerminal(
		t,
		result,
		err,
		"participant",
		namedinputs.IntegrityBoundaryAfterAttempt,
	)
	if result["source_mutated"] != true {
		t.Fatalf("source mutation was not retained: %#v", result)
	}
	causes := asSlice(result["failure_causes"])
	if len(causes) != 2 ||
		causes[0] != RootFailureCauseNamedInputIntegrity ||
		causes[1] != RootFailureCauseSourceMutated {
		t.Fatalf("ordered combined causes = %#v", causes)
	}
	var mutation *workspace.SourceMutatedError
	if !errors.As(err, &mutation) {
		t.Fatalf("combined terminal error lost source mutation: %v", err)
	}
	show, inspectErr := inspect.BuildShowTranscriptReport(sessionDir, 0, "")
	if inspectErr != nil {
		t.Fatalf("inspect combined terminal state: %v", inspectErr)
	}
	projection := rootRetainedIntegrityFromReport(t, show)
	if projection["source_mutated"] != true {
		t.Fatalf("inspection lost source mutation: %#v", projection)
	}
	inspectedCauses := asSlice(projection["failure_causes"])
	if len(inspectedCauses) != 2 ||
		inspectedCauses[0] != RootFailureCauseNamedInputIntegrity ||
		inspectedCauses[1] != RootFailureCauseSourceMutated {
		t.Fatalf("inspection reordered combined causes: %#v", inspectedCauses)
	}
	executionCWD := strings.TrimSpace(stringFromAny(result["execution_cwd"]))
	if executionCWD != "" {
		runTestGit(t, sourceRoot, "worktree", "remove", "--force", executionCWD)
	}
}

func TestRootNamedInputIntegritySurvivesNonSourceWorkspaceFinalizationFailure(t *testing.T) {
	fixture := newIsolatedSessionFixture(t, "running")
	st := store.New(fixture.sessionDir)
	meta, err := st.LoadMeta()
	if err != nil {
		t.Fatalf("load fixture metadata: %v", err)
	}
	workspaceRef, _ := meta.Get("execution_workspace_ref").(map[string]any)
	workspacePath, err := st.ArtifactPathForRef(workspaceRef)
	if err != nil {
		t.Fatalf("resolve workspace artifact: %v", err)
	}
	workspacePath = filepath.Join(st.Root, filepath.FromSlash(workspacePath))
	if err := os.WriteFile(workspacePath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("corrupt workspace artifact: %v", err)
	}
	integrityCause := contracts.NewDiagnosticError(
		"Retained named input changed.",
		contracts.NewDiagnostic(
			namedinputs.DiagnosticCodeIntegrity,
			contracts.DiagnosticPhasePolicy,
			"",
			"Retained named input changed.",
			nil,
		),
	)
	state := &rootExecutionState{
		st:         st,
		preflight:  &recipePreflight{sessionDir: fixture.sessionDir},
		meta:       meta,
		transcript: model.EmptyTranscript(),
		startedAt:  time.Now(),
	}
	result, err := state.markNamedInputIntegrityFailed(&rootNamedInputIntegrityError{
		cause:    integrityCause,
		role:     "participant",
		boundary: namedinputs.IntegrityBoundaryAfterAttempt,
	})
	assertRootNamedInputIntegrityTerminal(
		t,
		result,
		err,
		"participant",
		namedinputs.IntegrityBoundaryAfterAttempt,
	)
	causes := asSlice(result["failure_causes"])
	if len(causes) != 2 ||
		causes[0] != RootFailureCauseNamedInputIntegrity ||
		causes[1] != stopReasonWorkspaceIntegrityFailed {
		t.Fatalf("workspace failure overwrote integrity metadata: %#v", result)
	}
	if !errors.Is(err, integrityCause) ||
		isSourceMutationError(err) ||
		(!strings.Contains(err.Error(), "artifact") && !strings.Contains(err.Error(), "digest")) {
		t.Fatalf("terminal error lost workspace finalization failure: %v", err)
	}
}

func TestRootNamedInputPromptsExcludeFacilitatorProjectionAndCheckpointsRetainDescriptor(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"original"}`)
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		switch call.SlotID {
		case "facilitator":
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		case "reducer":
			return successfulRootTurn(call.Backend, `{"value":"final"}`), nil
		default:
			return successfulRootTurn(call.Backend, `{"value":"participant"}`), nil
		}
	}
	result, err := RunRecipe(context.Background(), rootNamedInputRecipeOptions(
		sessionDir,
		launchCWD,
		true,
		recorder,
	))
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	retainedRef, _ := result["retained_input_materialization_ref"].(map[string]any)
	if retainedRef == nil {
		t.Fatalf("missing retained descriptor ref: %#v", result)
	}
	retainedPath := filepath.Join(sessionDir, "execution", "inputs", "000001")
	for _, call := range recorder.snapshotCalls() {
		switch call.SlotID {
		case "facilitator":
			for _, forbidden := range []string{
				"Named Inputs (Data Only)",
				"materialized_path",
				retainedPath,
				"Execution Workspace Provenance",
				"workspace_content_source",
			} {
				if strings.Contains(call.Prompt, forbidden) {
					t.Fatalf("facilitator prompt exposed %q:\n%s", forbidden, call.Prompt)
				}
			}
		default:
			for _, required := range []string{
				"Named Inputs (Data Only)",
				"materialized_path",
				retainedPath,
				"Execution Workspace Provenance",
				"workspace_content_source",
			} {
				if !strings.Contains(call.Prompt, required) {
					t.Fatalf("%s prompt missing %q:\n%s", call.SlotID, required, call.Prompt)
				}
			}
		}
	}
	st := store.New(sessionDir)
	checkpointRefs := asSlice(result["root_checkpoint_refs"])
	if len(checkpointRefs) != 5 {
		t.Fatalf("checkpoint refs = %#v", checkpointRefs)
	}
	for ordinal, rawRef := range checkpointRefs {
		checkpoint := assertRootRecipeArtifact(
			t,
			st,
			rawRef,
			contracts.RootArtifactKindRootCheckpoint,
			ordinal+1,
		)
		checkpointRef, _ := checkpoint["retained_input_materialization_ref"].(map[string]any)
		if err := requireMatchingArtifactRef(retainedRef, checkpointRef, fmt.Sprintf("checkpoint %d retained input", ordinal+1)); err != nil {
			t.Fatalf("checkpoint %d retained ref: %v", ordinal+1, err)
		}
	}
	events, err := st.ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	foundStartRef := false
	for _, event := range events {
		payload, _ := event["payload"].(map[string]any)
		startRef, _ := payload["retained_input_materialization_ref"].(map[string]any)
		if startRef != nil && requireMatchingArtifactRef(retainedRef, startRef, "start event retained input") == nil {
			foundStartRef = true
			break
		}
	}
	if !foundStartRef {
		t.Fatalf("start event omitted retained descriptor: %#v", events)
	}
}

func TestRootNamedInputIntegrityTerminalRejectsResumeBeforeConstructionOrWrites(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"original"}`)
	retainedPath := filepath.Join(sessionDir, "execution", "inputs", "000001")
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if err := tamperRootRetainedInput(retainedPath, `{"value":"tampered"}`); err != nil {
			return TurnResult{}, err
		}
		return successfulRootTurn(call.Backend, `{"value":"rejected"}`), nil
	}
	result, runErr := RunRecipe(context.Background(), rootNamedInputRecipeOptions(
		sessionDir,
		launchCWD,
		false,
		recorder,
	))
	assertRootNamedInputIntegrityTerminal(
		t,
		result,
		runErr,
		"participant",
		namedinputs.IntegrityBoundaryAfterAttempt,
	)
	before := snapshotRootRecoverySession(t, sessionDir)
	constructions := 0
	baseFactory := (&rootBackendRecorder{}).factory()
	factory := func(backend string, sessionRoot string, slotID string, label string, cwd string, config SlotConfig) (Backend, error) {
		constructions++
		return baseFactory(backend, sessionRoot, slotID, label, cwd, config)
	}
	_, err := Resume(context.Background(), sessionDir, ResumeOptions{backendFactory: factory})
	if err == nil || !strings.Contains(err.Error(), "cannot be resumed") {
		t.Fatalf("terminal integrity resume = %v", err)
	}
	if constructions != 0 {
		t.Fatalf("resume constructed %d providers", constructions)
	}
	after := snapshotRootRecoverySession(t, sessionDir)
	if !equalRootRecoverySnapshots(before, after) {
		t.Fatal("terminal integrity resume rejection changed session bytes")
	}
}

func TestRejectNamedInputIntegrityTerminalRecognizesMapFailureCause(t *testing.T) {
	meta := model.NewSessionMeta(map[string]any{
		"status": "failed",
		"failure_causes": []any{
			map[string]any{"code": RootFailureCauseNamedInputIntegrity},
		},
	})
	if err := rejectNamedInputIntegrityTerminal(meta); err == nil {
		t.Fatal("map-shaped named-input integrity cause was not terminal")
	}
}

func TestRootRecoveryPreservesPresentRetainedInputMismatchBeforeAnyWrite(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"original"}`)
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, `{"value":"candidate"}`), nil
	}
	interrupted := errors.New("interrupt after participant checkpoint")
	rootCheckpointAfterSave = func(ordinal int) error {
		if ordinal == 2 {
			return interrupted
		}
		return nil
	}
	_, runErr := RunRecipe(context.Background(), rootNamedInputRecipeOptions(
		sessionDir,
		launchCWD,
		false,
		recorder,
	))
	rootCheckpointAfterSave = nil
	t.Cleanup(func() { rootCheckpointAfterSave = nil })
	if !errors.Is(runErr, interrupted) {
		t.Fatalf("checkpoint interruption = %v", runErr)
	}
	retainedPath := filepath.Join(sessionDir, "execution", "inputs", "000001")
	if err := tamperRootRetainedInput(retainedPath, `{"value":"tampered"}`); err != nil {
		t.Fatalf("tamper retained input: %v", err)
	}
	before := snapshotRootRecoverySession(t, sessionDir)
	constructions := 0
	recoveryRecorder := &rootBackendRecorder{}
	baseFactory := recoveryRecorder.factory()
	factory := func(backend string, sessionRoot string, slotID string, label string, cwd string, config SlotConfig) (Backend, error) {
		constructions++
		return baseFactory(backend, sessionRoot, slotID, label, cwd, config)
	}
	_, err := Resume(context.Background(), sessionDir, ResumeOptions{backendFactory: factory})
	if err == nil {
		t.Fatal("recovery accepted changed retained input")
	}
	assertRootRecipeDiagnostic(t, err, namedinputs.DiagnosticCodeIntegrity)
	var diagnosticErr *contracts.DiagnosticError
	if !errors.As(err, &diagnosticErr) ||
		len(diagnosticErr.Diagnostics) == 0 ||
		diagnosticErr.Diagnostics[0].Details["mismatch_category"] != namedinputs.IntegrityMismatchMode {
		t.Fatalf("recovery did not report the preserved mismatch: %v", err)
	}
	if constructions != 0 || len(recoveryRecorder.snapshotCalls()) != 0 {
		t.Fatalf("recovery reached providers: constructions=%d calls=%#v", constructions, recoveryRecorder.snapshotCalls())
	}
	after := snapshotRootRecoverySession(t, sessionDir)
	if !equalRootRecoverySnapshots(before, after) {
		t.Fatal("recovery mismatch detection changed session evidence")
	}
	data, readErr := os.ReadFile(retainedPath)
	if readErr != nil || string(data) != `{"value":"tampered"}` {
		t.Fatalf("recovery overwrote mismatch = %q, %v", data, readErr)
	}
}

func TestLegacyAbsentRetainedInputsMaterializeOnceAndThenVerifyAsPresent(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session")
	launchCWD := filepath.Join(root, "source")
	if err := os.MkdirAll(launchCWD, 0o755); err != nil {
		t.Fatalf("create source: %v", err)
	}
	inputPath := filepath.Join(launchCWD, "payload.json")
	writeRootRecipeTestFile(t, inputPath, `{"value":"legacy"}`)
	selected := selectRootNamedInputIntegrityContract(t)
	prepared, err := namedinputs.Prepare(namedinputs.Options{
		Context:                 context.Background(),
		Contract:                selected,
		Bindings:                []string{"payload=" + inputPath},
		SourceAnchor:            launchCWD,
		NamedInputMaxBytes:      4096,
		NamedInputTotalMaxBytes: 4096,
	})
	if err != nil {
		t.Fatalf("prepare legacy inputs: %v", err)
	}
	st := store.New(sessionDir)
	persistedInputs, err := namedinputs.Persist(st, prepared)
	if err != nil {
		t.Fatalf("persist legacy manifest: %v", err)
	}
	materialized, legacy, err := prepareRootRecoveryRetainedInputs(
		context.Background(),
		sessionDir,
		st,
		model.EmptySessionMeta(),
		map[int]*persistedRootRecoveryArtifact{},
		persistedInputs.ManifestRef,
		selected,
	)
	if err != nil || materialized != nil || !legacy {
		t.Fatalf("legacy absent preparation = materialized %#v legacy %v err %v", materialized, legacy, err)
	}
	state := &rootExecutionState{
		st: st,
		preflight: &recipePreflight{
			sessionDir:       sessionDir,
			selectedContract: selected,
		},
		persisted: &persistedRecipeRun{
			st:               st,
			inputManifestRef: persistedInputs.ManifestRef,
		},
		meta:       model.EmptySessionMeta(),
		transcript: model.EmptyTranscript(),
	}
	if err := state.materializeLegacyRootRecoveryInputs(context.Background()); err != nil {
		t.Fatalf("materialize legacy inputs: %v", err)
	}
	if err := namedinputs.VerifyRetained(
		context.Background(),
		st,
		state.persisted.retainedInputRef,
		"test",
		namedinputs.IntegrityBoundaryRecovery,
	); err != nil {
		t.Fatalf("verify legacy materialization: %v", err)
	}
	reloaded, legacyAgain, err := prepareRootRecoveryRetainedInputs(
		context.Background(),
		sessionDir,
		st,
		state.meta,
		map[int]*persistedRootRecoveryArtifact{},
		persistedInputs.ManifestRef,
		selected,
	)
	if err != nil || reloaded == nil || legacyAgain {
		t.Fatalf("second preparation = materialized %#v legacy %v err %v", reloaded, legacyAgain, err)
	}
	identity, err := contracts.RootArtifactIdentityFor(contracts.RootArtifactKindRetainedInputs, 0)
	if err != nil {
		t.Fatalf("retained identity: %v", err)
	}
	count := 0
	for _, raw := range asSlice(st.ArtifactIndex()["entries"]) {
		entry, _ := raw.(map[string]any)
		ref, _ := entry["ref"].(map[string]any)
		if ref != nil && ref["id"] == identity.RefID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("retained descriptor was materialized %d times", count)
	}
	events, err := st.ReadEvents()
	if err != nil || len(events) != 0 {
		t.Fatalf("legacy materialization emitted resume events: %#v, %v", events, err)
	}
}

func TestRootRecoveryAcceptsStoryTwoRetainedRefShapeAndRejectsConflicts(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session")
	inputPath := filepath.Join(root, "payload.json")
	writeRootRecipeTestFile(t, inputPath, `{"value":"story-two"}`)
	selected := selectRootNamedInputIntegrityContract(t)
	prepared, err := namedinputs.Prepare(namedinputs.Options{
		Context:                 context.Background(),
		Contract:                selected,
		Bindings:                []string{"payload=" + inputPath},
		SourceAnchor:            root,
		NamedInputMaxBytes:      4096,
		NamedInputTotalMaxBytes: 4096,
	})
	if err != nil {
		t.Fatalf("prepare inputs: %v", err)
	}
	st := store.New(sessionDir)
	persisted, err := namedinputs.Persist(st, prepared)
	if err != nil {
		t.Fatalf("persist inputs: %v", err)
	}
	retained, err := namedinputs.MaterializeRetained(
		context.Background(),
		st,
		persisted.ManifestRef,
		filepath.Join(sessionDir, "execution", "inputs"),
	)
	if err != nil {
		t.Fatalf("materialize retained inputs: %v", err)
	}
	meta := model.NewSessionMeta(map[string]any{
		"retained_input_materialization_ref": retained.DescriptorRef,
	})
	checkpoints := map[int]*persistedRootRecoveryArtifact{
		1: {payload: map[string]any{"retained_input_materialization_ref": retained.DescriptorRef}},
		2: {payload: map[string]any{}},
	}
	loaded, legacy, err := prepareRootRecoveryRetainedInputs(
		context.Background(),
		sessionDir,
		st,
		meta,
		checkpoints,
		persisted.ManifestRef,
		selected,
	)
	if err != nil || legacy || loaded == nil {
		t.Fatalf("Story 2 retained ref shape = loaded %#v legacy %v err %v", loaded, legacy, err)
	}
	if err := requireMatchingArtifactRef(
		retained.DescriptorRef,
		loaded.DescriptorRef,
		"Story 2 retained input",
	); err != nil {
		t.Fatalf("loaded Story 2 retained ref: %v", err)
	}

	conflicting := cloneMap(retained.DescriptorRef)
	conflicting["digest"] = contracts.DigestPrefix + strings.Repeat("0", 64)
	checkpoints[3] = &persistedRootRecoveryArtifact{
		payload: map[string]any{"retained_input_materialization_ref": conflicting},
	}
	if _, _, err := prepareRootRecoveryRetainedInputs(
		context.Background(),
		sessionDir,
		st,
		meta,
		checkpoints,
		persisted.ManifestRef,
		selected,
	); err == nil {
		t.Fatal("conflicting retained descriptor refs were accepted")
	}
}

func TestRootIntegrityInspectionSurfacesShareContentFreeTerminalProjection(t *testing.T) {
	const inputSecret = "INPUT-CONTENT-MUST-NOT-LEAK"
	const rejectedOutput = "INSPECTION-REJECTED-OUTPUT-MUST-NOT-LEAK"
	sessionDir := filepath.Join(t.TempDir(), "session")
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(
		t,
		filepath.Join(launchCWD, "payload.json"),
		`{"secret":"`+inputSecret+`"}`,
	)
	retainedPath := filepath.Join(sessionDir, "execution", "inputs", "000001")
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if err := tamperRootRetainedInput(retainedPath, `{"secret":"tampered"}`); err != nil {
			return TurnResult{}, err
		}
		return successfulRootTurn(call.Backend, `{"value":"`+rejectedOutput+`"}`), nil
	}
	result, runErr := RunRecipe(context.Background(), rootNamedInputRecipeOptions(
		sessionDir,
		launchCWD,
		false,
		recorder,
	))
	assertRootNamedInputIntegrityTerminal(
		t,
		result,
		runErr,
		"participant",
		namedinputs.IntegrityBoundaryAfterAttempt,
	)

	show, err := inspect.BuildShowTranscriptReport(sessionDir, 0, "")
	if err != nil {
		t.Fatalf("show report: %v", err)
	}
	exported, err := inspect.BuildExportReport(sessionDir, true)
	if err != nil {
		t.Fatalf("export report: %v", err)
	}
	contractReport, err := inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	health, err := inspect.BuildSessionHealthReport(sessionDir)
	if err != nil {
		t.Fatalf("health report: %v", err)
	}
	reports := []map[string]any{show, exported, contractReport, health}
	var want []byte
	for index, report := range reports {
		projection := rootRetainedIntegrityFromReport(t, report)
		encoded, encodeErr := contracts.CanonicalJSONBytes(projection)
		if encodeErr != nil {
			t.Fatalf("encode report %d projection: %v", index, encodeErr)
		}
		if index == 0 {
			want = encoded
		} else if string(encoded) != string(want) {
			t.Fatalf("report %d projection differs:\nwant %s\ngot  %s", index, want, encoded)
		}
		serialized, encodeErr := contracts.CanonicalJSONBytes(report)
		if encodeErr != nil {
			t.Fatalf("encode report %d: %v", index, encodeErr)
		}
		if strings.Contains(string(serialized), inputSecret) ||
			strings.Contains(string(serialized), rejectedOutput) {
			t.Fatalf("report %d exposed input or rejected output: %s", index, serialized)
		}
	}
	checks := asSlice(health["checks"])
	foundLiveFailure := false
	for _, raw := range checks {
		check, _ := raw.(map[string]any)
		if check["name"] == "root_named_input_digests" &&
			check["status"] == "error" &&
			check["retained_failure"] != nil {
			foundLiveFailure = true
		}
	}
	if !foundLiveFailure {
		t.Fatalf("health omitted live retained mismatch: %#v", checks)
	}
}

func rootNamedInputRecipeOptions(
	sessionDir string,
	launchCWD string,
	reducer bool,
	recorder *rootBackendRecorder,
) RecipeOptions {
	config := rootRecipeRuntimeConfig("neutral/integrity-contract-v1")
	if reducer {
		config.RelayRecipes["neutral-root"]["result_source"] = integration.ResultSourceReducer
	}
	return RecipeOptions{
		SessionDir:        sessionDir,
		Task:              "Exercise retained named input integrity",
		RecipeID:          "neutral-root",
		InputBindings:     []string{"payload=payload.json"},
		LaunchCWD:         launchCWD,
		RuntimeConfig:     config,
		IntegrationBundle: mustDecodeRootNamedInputBundle(),
		ReadinessCheck:    readyRootRecipeCheck,
		backendFactory:    recorder.factory(),
	}
}

func mustDecodeRootNamedInputBundle() *integration.Bundle {
	bundle, err := integration.DecodeBundleBytes([]byte(rootNamedInputIntegrityTestBundle))
	if err != nil {
		panic(err)
	}
	return bundle
}

func selectRootNamedInputIntegrityContract(t *testing.T) *integration.SelectedContract {
	t.Helper()
	selected, err := integration.SelectContract(
		mustDecodeRootNamedInputBundle(),
		"neutral/integrity-contract-v1",
		integration.ScheduleRequirement{
			Turns: []integration.ScheduledTurn{
				{ParticipantTurn: 1, Slot: "slot_0"},
				{ParticipantTurn: 2, Slot: "slot_1"},
			},
			ResultSource: integration.ResultSourceLastTurn,
		},
	)
	if err != nil {
		t.Fatalf("select integrity contract: %v", err)
	}
	return selected
}

func tamperRootRetainedInput(path string, content string) error {
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func assertRootNamedInputIntegrityTerminal(
	t *testing.T,
	result map[string]any,
	err error,
	role string,
	boundary string,
) map[string]any {
	t.Helper()
	if err == nil {
		t.Fatal("expected retained named input integrity failure")
	}
	if result == nil ||
		result["status"] != "failed" ||
		result["stop_reason"] != RootNamedInputIntegrityStopReason ||
		result["execution_phase"] != RootNamedInputIntegrityExecutionPhase ||
		result["root_recovery_pending"] != false {
		t.Fatalf("integrity terminal result = %#v, err=%v", result, err)
	}
	causes := asSlice(result["failure_causes"])
	if len(causes) == 0 || causes[0] != RootFailureCauseNamedInputIntegrity {
		t.Fatalf("integrity is not primary: %#v", causes)
	}
	failure, _ := result["named_input_integrity_failure"].(map[string]any)
	if failure == nil ||
		failure["code"] != RootFailureCauseNamedInputIntegrity ||
		failure["role"] != role ||
		failure["attempt_boundary"] != boundary {
		t.Fatalf("integrity failure projection = %#v", failure)
	}
	return failure
}

func assertSessionOmitsText(t *testing.T, sessionDir string, forbidden ...string) {
	t.Helper()
	err := filepath.WalkDir(sessionDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, text := range forbidden {
			if strings.Contains(string(data), text) {
				return fmt.Errorf("%s contains rejected text %q", path, text)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func rootRetainedIntegrityFromReport(t *testing.T, report map[string]any) map[string]any {
	t.Helper()
	root, _ := report["root"].(map[string]any)
	inputs, _ := root["named_inputs"].(map[string]any)
	projection, _ := inputs["retained_integrity"].(map[string]any)
	if projection == nil || projection["status"] != "failed" {
		t.Fatalf("missing retained integrity projection: %#v", report)
	}
	return projection
}
