package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestRootRecoveryCompletesAfterEveryPostParticipantCheckpointWithoutProviderReplay(t *testing.T) {
	for _, checkpointOrdinal := range []int{2, 3, 4, 5} {
		t.Run(string(rune('0'+checkpointOrdinal)), func(t *testing.T) {
			launchCWD := t.TempDir()
			contextPath := filepath.Join(launchCWD, "context.md")
			settingsPath := filepath.Join(launchCWD, "settings.toml")
			writeRootRecipeTestFile(t, contextPath, "persisted context before interruption\n")
			writeRootRecipeTestFile(t, settingsPath, "original settings bytes\n")
			config := rootRecipeRuntimeConfig("")
			config.SettingsPath = settingsPath
			sessionDir := filepath.Join(t.TempDir(), "session")
			recorder := &rootBackendRecorder{}
			interrupted := errors.New("checkpoint interruption")
			fired := false
			rootCheckpointAfterSave = func(ordinal int) error {
				if ordinal == checkpointOrdinal && !fired {
					fired = true
					return interrupted
				}
				return nil
			}
			_, runErr := RunRecipe(context.Background(), RecipeOptions{
				SessionDir:     sessionDir,
				Task:           "Recover only durable post-participant phases",
				RecipeID:       "neutral-root",
				ContextFiles:   []string{contextPath},
				LaunchCWD:      launchCWD,
				RuntimeConfig:  config,
				ReadinessCheck: readyRootRecipeCheck,
				backendFactory: recorder.factory(),
			})
			rootCheckpointAfterSave = nil
			t.Cleanup(func() { rootCheckpointAfterSave = nil })
			if !fired || !errors.Is(runErr, interrupted) {
				t.Fatalf("checkpoint %d interruption = %v, fired=%v", checkpointOrdinal, runErr, fired)
			}
			writeRootRecipeTestFile(t, contextPath, "changed context after interruption\n")
			writeRootRecipeTestFile(t, settingsPath, "changed settings after interruption\n")

			factoryCalls := 0
			result, resumeErr := Resume(context.Background(), sessionDir, ResumeOptions{
				backendFactory: func(string, string, string, string, string, SlotConfig) (Backend, error) {
					factoryCalls++
					return nil, errors.New("root recovery constructed a participant or facilitator")
				},
			})
			if resumeErr != nil {
				t.Fatalf("resume checkpoint %d: %v", checkpointOrdinal, resumeErr)
			}
			if factoryCalls != 0 {
				t.Fatalf("recovery provider constructions = %d", factoryCalls)
			}
			if result["status"] != "completed" || result["execution_phase"] != "result_complete" || intFromAny(result["actual_participant_turns"], 0) != 2 {
				t.Fatalf("recovered result = %#v", result)
			}
			if result["next_unsealed_participant_turn"] != nil || intFromAny(result["participant_prompt_sealed_through"], 0) != 2 {
				t.Fatalf("recovered participant sealing state = %#v", result)
			}
			checkpoints := result["root_checkpoint_refs"].([]any)
			if len(checkpoints) != 5 {
				t.Fatalf("recovered checkpoints = %#v", checkpoints)
			}
			assertRootRecipeArtifact(t, store.New(sessionDir), checkpoints[4], contracts.RootArtifactKindRootCheckpoint, 5)
		})
	}
}

func TestRootRecoveryRetriesOnlyFailedReducerWithNextAttemptOrdinal(t *testing.T) {
	launchCWD := t.TempDir()
	contextPath := filepath.Join(launchCWD, "context.md")
	settingsPath := filepath.Join(launchCWD, "settings.toml")
	writeRootRecipeTestFile(t, contextPath, "original reducer recovery context\n")
	writeRootRecipeTestFile(t, settingsPath, "original reducer settings\n")
	config := rootRecipeRuntimeConfig("")
	config.SettingsPath = settingsPath
	config.RelayRecipes["neutral-root"]["result_source"] = integration.ResultSourceReducer
	config.RelayRecipes["neutral-root"]["provider_retry"] = recipes.ProviderRetryAllow
	sessionDir := filepath.Join(t.TempDir(), "session")
	initial := &rootBackendRecorder{}
	initial.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "reducer" {
			return TurnResult{}, errors.New("first reducer attempt failed")
		}
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, "participant result"), nil
	}
	_, runErr := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Retry only the root reducer",
		RecipeID:       "neutral-root",
		ContextFiles:   []string{contextPath},
		LaunchCWD:      launchCWD,
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: initial.factory(),
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "first reducer attempt failed") {
		t.Fatalf("initial reducer error = %v", runErr)
	}
	writeRootRecipeTestFile(t, contextPath, "changed reducer recovery context\n")
	writeRootRecipeTestFile(t, settingsPath, "changed reducer settings\n")

	recovery := &rootBackendRecorder{}
	recovery.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID != "reducer" {
			return TurnResult{}, errors.New("recovery replayed participant or facilitator")
		}
		if !strings.Contains(call.Prompt, "original reducer recovery context") || strings.Contains(call.Prompt, "changed reducer recovery context") {
			return TurnResult{}, errors.New("reducer did not use persisted launch context")
		}
		return successfulRootTurn(call.Backend, "recovered reducer result"), nil
	}
	result, err := Resume(context.Background(), sessionDir, ResumeOptions{backendFactory: recovery.factory()})
	if err != nil {
		t.Fatalf("resume failed reducer: %v", err)
	}
	calls := recovery.snapshotCalls()
	if len(calls) != 1 || calls[0].SlotID != "reducer" {
		t.Fatalf("recovery calls = %#v", calls)
	}
	if result["reducer_failed"] != nil || result["root_recovery_pending"] != nil || result["failed_at"] != nil {
		t.Fatalf("successful recovery retained stale failure state = %#v", result)
	}
	attemptRefs := result["reducer_attempt_refs"].([]any)
	if len(attemptRefs) != 2 {
		t.Fatalf("reducer attempts = %#v", attemptRefs)
	}
	st := store.New(sessionDir)
	first := assertRootRecipeArtifact(t, st, attemptRefs[0], contracts.RootArtifactKindReducerAttempt, 1)
	second := assertRootRecipeArtifact(t, st, attemptRefs[1], contracts.RootArtifactKindReducerAttempt, 2)
	if first["status"] != "failed" || second["status"] != "completed" || second["content"] != "recovered reducer result" {
		t.Fatalf("reducer attempts = %#v / %#v", first, second)
	}
	reducerInvocations := persistedRootInvocationRecordsForPhase(t, st, result["invocation_refs"], "reducer")
	if len(reducerInvocations) != 2 ||
		reducerInvocations[0]["invocation_id"] != "reducer:000001" ||
		reducerInvocations[1]["invocation_id"] != "reducer:000001" ||
		reducerInvocations[0]["runner_attempt"] != 1 ||
		reducerInvocations[1]["runner_attempt"] != 2 {
		t.Fatalf("reducer invocation attempts = %#v", reducerInvocations)
	}
	beforeRepeat := snapshotRootRecoverySession(t, sessionDir)
	providerConstructions := 0
	repeated, repeatErr := Resume(context.Background(), sessionDir, ResumeOptions{
		backendFactory: func(string, string, string, string, string, SlotConfig) (Backend, error) {
			providerConstructions++
			return nil, errors.New("completed reducer recovery constructed a provider")
		},
	})
	if repeatErr != nil || repeated["status"] != "completed" {
		t.Fatalf("repeat completed recovery = %#v, %v", repeated, repeatErr)
	}
	if providerConstructions != 0 {
		t.Fatalf("repeat recovery provider constructions = %d", providerConstructions)
	}
	if afterRepeat := snapshotRootRecoverySession(t, sessionDir); !equalRootRecoverySnapshots(beforeRepeat, afterRepeat) {
		t.Fatal("repeat completed reducer recovery mutated the session")
	}
}

func TestRootRecoveryDiscoversParticipantCheckpointWhenMetadataWasNotAdvanced(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	interrupted := errors.New("participant checkpoint persisted before metadata")
	rootCheckpointAfterSave = func(ordinal int) error {
		if ordinal == 2 {
			return interrupted
		}
		return nil
	}
	_, runErr := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Discover graph-backed participant checkpoint",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  rootRecipeRuntimeConfig(""),
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: (&rootBackendRecorder{}).factory(),
	})
	rootCheckpointAfterSave = nil
	t.Cleanup(func() { rootCheckpointAfterSave = nil })
	if !errors.Is(runErr, interrupted) {
		t.Fatalf("checkpoint interruption = %v", runErr)
	}
	st := store.New(sessionDir)
	meta, err := st.LoadMeta()
	if err != nil {
		t.Fatalf("load interrupted metadata: %v", err)
	}
	refs := meta.Slice("root_checkpoint_refs")
	if len(refs) < 2 {
		t.Fatalf("interrupted checkpoint refs = %#v", refs)
	}
	stale := meta.
		With("root_checkpoint_refs", []any{refs[0]}).
		With("latest_root_checkpoint_ref", refs[0]).
		WithStatus("running").
		With("execution_phase", "participant").
		With("participant_turns_completed", 1).
		With("actual_participant_turns", 1)
	if err := st.SaveMeta(stale); err != nil {
		t.Fatalf("save stale metadata fixture: %v", err)
	}

	result, err := Resume(context.Background(), sessionDir, ResumeOptions{})
	if err != nil {
		t.Fatalf("resume graph-backed checkpoint: %v", err)
	}
	if result["status"] != "completed" || len(result["root_checkpoint_refs"].([]any)) != 5 {
		t.Fatalf("recovered graph-backed checkpoint = %#v", result)
	}
}

func TestRootRecoveryNoOpsAfterTerminalCheckpointEventAndGraphAreComplete(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Already complete root recovery",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  rootRecipeRuntimeConfig(""),
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: (&rootBackendRecorder{}).factory(),
	})
	if err != nil || result["status"] != "completed" {
		t.Fatalf("complete root fixture = %#v, %v", result, err)
	}
	before := snapshotRootRecoverySession(t, sessionDir)
	repeated, err := Resume(context.Background(), sessionDir, ResumeOptions{})
	if err != nil || repeated["status"] != "completed" {
		t.Fatalf("repeat completed resume = %#v, %v", repeated, err)
	}
	after := snapshotRootRecoverySession(t, sessionDir)
	if !equalRootRecoverySnapshots(before, after) {
		t.Fatal("completed root resume reran a terminal phase or mutated session history")
	}
}

func TestRootRecoveryRepairsEveryInterruptedTerminalWriteAfterCleanupCheckpoint(t *testing.T) {
	for _, stage := range []string{"metadata", "event", "graph"} {
		t.Run(stage, func(t *testing.T) {
			sessionDir := filepath.Join(t.TempDir(), "session")
			interrupted := errors.New("terminal " + stage + " interruption")
			fired := false
			rootResultCompletionAfterWrite = func(completedStage string) error {
				if completedStage == stage && !fired {
					fired = true
					return interrupted
				}
				return nil
			}
			_, runErr := RunRecipe(context.Background(), RecipeOptions{
				SessionDir:     sessionDir,
				Task:           "Repair terminal root writes",
				RecipeID:       "neutral-root",
				LaunchCWD:      t.TempDir(),
				RuntimeConfig:  rootRecipeRuntimeConfig(""),
				ReadinessCheck: readyRootRecipeCheck,
				backendFactory: (&rootBackendRecorder{}).factory(),
			})
			rootResultCompletionAfterWrite = nil
			t.Cleanup(func() { rootResultCompletionAfterWrite = nil })
			if !fired || !errors.Is(runErr, interrupted) {
				t.Fatalf("terminal interruption = %v, fired=%v", runErr, fired)
			}
			result, err := Resume(context.Background(), sessionDir, ResumeOptions{})
			if err != nil || result["status"] != "completed" {
				t.Fatalf("terminal recovery = %#v, %v", result, err)
			}
			checkpoint := result["root_checkpoint_refs"].([]any)[4].(map[string]any)
			terminalEvents := 0
			events, err := store.New(sessionDir).ReadEvents()
			if err != nil {
				t.Fatalf("read terminal events: %v", err)
			}
			for _, event := range events {
				if event["event_type"] != "node_completed" {
					continue
				}
				payload, _ := event["payload"].(map[string]any)
				ref, _ := payload["root_checkpoint_ref"].(map[string]any)
				if ref != nil && requireMatchingArtifactRef(ref, checkpoint, "terminal checkpoint") == nil {
					terminalEvents++
				}
			}
			if terminalEvents != 1 {
				t.Fatalf("terminal completion events = %d", terminalEvents)
			}
		})
	}
}

func TestRootRecoveryReusesSuccessfulReducerAttemptBeforeRawCandidate(t *testing.T) {
	config := rootRecipeRuntimeConfig("")
	config.RelayRecipes["neutral-root"]["result_source"] = integration.ResultSourceReducer
	sessionDir := filepath.Join(t.TempDir(), "session")
	initial := &rootBackendRecorder{}
	initial.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "reducer" {
			return successfulRootTurn(call.Backend, "durable reducer attempt"), nil
		}
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, "participant result"), nil
	}
	interrupted := errors.New("after reducer attempt")
	rootReducerAfterAttempt = func() error { return interrupted }
	_, runErr := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Reuse a successful durable reducer attempt",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: initial.factory(),
	})
	rootReducerAfterAttempt = nil
	t.Cleanup(func() { rootReducerAfterAttempt = nil })
	if !errors.Is(runErr, interrupted) {
		t.Fatalf("reducer interruption = %v", runErr)
	}

	providerConstructions := 0
	result, err := Resume(context.Background(), sessionDir, ResumeOptions{
		backendFactory: func(string, string, string, string, string, SlotConfig) (Backend, error) {
			providerConstructions++
			return nil, errors.New("successful attempt was replayed")
		},
	})
	if err != nil {
		t.Fatalf("resume successful attempt: %v", err)
	}
	if providerConstructions != 0 {
		t.Fatalf("provider constructions = %d", providerConstructions)
	}
	if len(result["reducer_attempt_refs"].([]any)) != 1 {
		t.Fatalf("reducer attempts = %#v", result["reducer_attempt_refs"])
	}
	raw := assertRootRecipeArtifact(t, store.New(sessionDir), result["raw_result_ref"], contracts.RootArtifactKindRawResult, 0)
	if raw["content"] != "durable reducer attempt" {
		t.Fatalf("reused raw candidate = %#v", raw)
	}
}

func TestRootRecoveryForbidRejectsAfterDurableReducerInvocationWithoutRelaunch(t *testing.T) {
	config := rootRecipeRuntimeConfig("")
	recipe := config.RelayRecipes["neutral-root"]
	recipe["participant_turns"] = 1
	recipe["max_rounds"] = 1
	recipe["result_source"] = integration.ResultSourceReducer
	recipe["provider_retry"] = recipes.ProviderRetryForbid
	sessionDir := filepath.Join(t.TempDir(), "session")
	initial := &rootBackendRecorder{}
	initial.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "reducer" {
			return successfulRootTurn(call.Backend, "durable invocation without reducer attempt"), nil
		}
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, "participant result"), nil
	}
	interrupted := errors.New("after reducer invocation record")
	fired := false
	rootProviderInvocationAfterSave = func(spec rootInvocationSpec, attempt int) error {
		if spec.phase == "reducer" && attempt == 1 && !fired {
			fired = true
			return interrupted
		}
		return nil
	}
	result, runErr := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Forbid reducer relaunch after durable invocation",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: initial.factory(),
	})
	rootProviderInvocationAfterSave = nil
	t.Cleanup(func() { rootProviderInvocationAfterSave = nil })
	if !fired || !errors.Is(runErr, interrupted) {
		t.Fatalf("reducer invocation interruption = %v, fired=%v", runErr, fired)
	}
	st := store.New(sessionDir)
	reducerInvocations := persistedRootInvocationRecordsForPhase(t, st, result["invocation_refs"], "reducer")
	if len(reducerInvocations) != 1 ||
		reducerInvocations[0]["invocation_id"] != "reducer:000001" ||
		reducerInvocations[0]["runner_attempt"] != 1 ||
		reducerInvocations[0]["provider_launch_attempted"] != true {
		t.Fatalf("durable reducer invocation = %#v", reducerInvocations)
	}
	meta, err := st.LoadMeta()
	if err != nil {
		t.Fatalf("load interrupted meta: %v", err)
	}
	if refs := meta.Slice("reducer_attempt_refs"); len(refs) != 0 {
		t.Fatalf("unexpected reducer attempt refs = %#v", refs)
	}
	if attempt, found, err := loadLatestRootRecoveryArtifact(st, contracts.RootArtifactKindReducerAttempt, 1); err != nil || found {
		t.Fatalf("unexpected reducer attempt artifact = %#v, found=%v, err=%v", attempt, found, err)
	}

	constructions := 0
	_, resumeErr := Resume(context.Background(), sessionDir, ResumeOptions{
		backendFactory: func(string, string, string, string, string, SlotConfig) (Backend, error) {
			constructions++
			return nil, errors.New("forbidden recovery constructed a provider")
		},
	})
	assertRootRecipeDiagnostic(t, resumeErr, "provider_retry_forbidden_terminal")
	if constructions != 0 || len(initial.snapshotCalls()) != 3 {
		t.Fatalf("forbidden recovery reached providers: constructions=%d initial_calls=%#v", constructions, initial.snapshotCalls())
	}
}

func TestRootRecoveryRepairsProviderInvocationAfterResultBeforeMetadata(t *testing.T) {
	fixture := newDurableProviderResultCrashFixture(t)
	if fixture.marker.ProviderResultRef != nil || fixture.marker.ProviderInvocationRef != nil {
		t.Fatalf("interrupted reducer marker = %#v", fixture.marker)
	}
	if refs := fixture.result["invocation_refs"].([]any); len(refs) != 2 {
		t.Fatalf("interrupted invocation refs = %#v", refs)
	}

	recovery := &rootBackendRecorder{}
	recovery.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID != "reducer" {
			return TurnResult{}, errors.New("recovery replayed participant or facilitator")
		}
		return successfulRootTurn(call.Backend, "recovered reducer result"), nil
	}
	resumed, err := Resume(context.Background(), fixture.sessionDir, ResumeOptions{backendFactory: recovery.factory()})
	if err != nil {
		t.Fatalf("resume provider result repair: %v", err)
	}
	reducerInvocations := persistedRootInvocationRecordsForPhase(t, fixture.st, resumed["invocation_refs"], "reducer")
	if len(reducerInvocations) != 2 ||
		reducerInvocations[0]["runner_attempt"] != 1 ||
		reducerInvocations[1]["runner_attempt"] != 2 {
		t.Fatalf("repaired reducer invocations = %#v", reducerInvocations)
	}
	repairedFirstRef := reducerInvocations[0]["provider_result_ref"].(map[string]any)
	if requireMatchingArtifactRef(repairedFirstRef, fixture.resultArtifact.ref, "repaired reducer result") != nil {
		t.Fatalf("repaired reducer result ref = %#v, want %#v", repairedFirstRef, fixture.resultArtifact.ref)
	}
	markers, err := loadRootProviderAttemptMarkers(fixture.st)
	if err != nil {
		t.Fatalf("load repaired markers: %v", err)
	}
	repairedMarker := providerAttemptMarkerFor(t, markers, fixture.marker.InvocationID, fixture.marker.RunnerAttempt)
	if repairedMarker.ProviderResultRef == nil || repairedMarker.ProviderInvocationRef == nil || repairedMarker.CompletedAt == "" || repairedMarker.Outcome == "" {
		t.Fatalf("repaired provider marker = %#v", repairedMarker)
	}
}

func TestRootRecoveryRetriesLaunchMarkerWithoutDurableResult(t *testing.T) {
	config := rootRecipeRuntimeConfig("")
	recipe := config.RelayRecipes["neutral-root"]
	recipe["participant_turns"] = 1
	recipe["max_rounds"] = 1
	recipe["result_source"] = integration.ResultSourceReducer
	recipe["provider_retry"] = recipes.ProviderRetryAllow
	sessionDir := filepath.Join(t.TempDir(), "session")
	initial := &rootBackendRecorder{}
	initial.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "reducer" {
			return TurnResult{}, errors.New("provider ran after marker interruption")
		}
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, "participant result"), nil
	}
	interrupted := errors.New("after launch marker before provider result")
	fired := false
	rootProviderLaunchMarkerAfterSave = func(spec rootInvocationSpec, attempt int) error {
		if spec.phase == "reducer" && attempt == 1 && !fired {
			fired = true
			return interrupted
		}
		return nil
	}
	result, runErr := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Retry marker-only provider attempt",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: initial.factory(),
	})
	rootProviderLaunchMarkerAfterSave = nil
	t.Cleanup(func() { rootProviderLaunchMarkerAfterSave = nil })
	if !fired || !errors.Is(runErr, interrupted) {
		t.Fatalf("provider launch marker interruption = %v, fired=%v", runErr, fired)
	}
	st := store.New(sessionDir)
	markers, err := loadRootProviderAttemptMarkers(st)
	if err != nil {
		t.Fatalf("load markers: %v", err)
	}
	reducerMarker := markers[len(markers)-1]
	if reducerMarker.Phase != "reducer" || reducerMarker.ProviderResultRef != nil || reducerMarker.ProviderInvocationRef != nil {
		t.Fatalf("interrupted reducer marker = %#v", reducerMarker)
	}
	if _, found, err := loadLatestRootRecoveryArtifact(st, contracts.RootArtifactKindProviderResult, reducerMarker.ArtifactOrdinal); err != nil || found {
		t.Fatalf("marker-only provider result = found %v, err %v", found, err)
	}
	if refs := result["invocation_refs"].([]any); len(refs) != 2 {
		t.Fatalf("interrupted invocation refs = %#v", refs)
	}

	recovery := &rootBackendRecorder{}
	recovery.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID != "reducer" {
			return TurnResult{}, errors.New("recovery replayed participant or facilitator")
		}
		return successfulRootTurn(call.Backend, "recovered reducer result"), nil
	}
	resumed, err := Resume(context.Background(), sessionDir, ResumeOptions{backendFactory: recovery.factory()})
	if err != nil {
		t.Fatalf("resume marker-only retry: %v", err)
	}
	reducerInvocations := persistedRootInvocationRecordsForPhase(t, st, resumed["invocation_refs"], "reducer")
	if len(reducerInvocations) != 1 || reducerInvocations[0]["runner_attempt"] != 2 {
		t.Fatalf("retried reducer invocations = %#v", reducerInvocations)
	}
}

func TestRootRecoveryRejectsInvalidDiscoveredProviderResultReadOnly(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, *durableProviderResultCrashFixture)
		wantErr string
	}{
		{
			name: "corrupt artifact",
			mutate: func(t *testing.T, fixture *durableProviderResultCrashFixture) {
				filename, err := fixture.st.ArtifactPathForRef(fixture.resultArtifact.ref)
				if err != nil {
					t.Fatalf("resolve provider result path: %v", err)
				}
				filename = filepath.Join(fixture.st.Root, filepath.FromSlash(filename))
				payload := contracts.Materialize(fixture.resultArtifact.payload).(map[string]any)
				payload["phase"] = "tampered"
				body, err := contracts.CanonicalJSONBytes(payload)
				if err != nil {
					t.Fatalf("encode corrupt provider result: %v", err)
				}
				if err := os.WriteFile(filename, body, 0o644); err != nil {
					t.Fatalf("write corrupt provider result: %v", err)
				}
			},
			wantErr: "Durable provider result discovery failed",
		},
		{
			name: "valid redigested started_at mismatch",
			mutate: func(t *testing.T, fixture *durableProviderResultCrashFixture) {
				payload := contracts.Materialize(fixture.resultArtifact.payload).(map[string]any)
				payload["started_at"] = "2026-02-02T00:00:00Z"
				payload["invocation"].(map[string]any)["started_at"] = payload["started_at"]
				if _, err := contracts.ValidateProviderResultRecord(payload); err != nil {
					t.Fatalf("validate redigested provider result: %v", err)
				}
				if _, err := saveRootArtifact(fixture.st, contracts.RootArtifactKindProviderResult, fixture.marker.ArtifactOrdinal, payload); err != nil {
					t.Fatalf("save redigested provider result: %v", err)
				}
			},
			wantErr: "Provider attempt marker does not match provider result",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDurableProviderResultCrashFixture(t)
			test.mutate(t, fixture)
			before := snapshotRootRecoverySession(t, fixture.sessionDir)
			constructions := 0
			_, err := Resume(context.Background(), fixture.sessionDir, ResumeOptions{
				backendFactory: func(string, string, string, string, string, SlotConfig) (Backend, error) {
					constructions++
					return nil, errors.New("invalid recovery constructed a provider")
				},
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("invalid discovery error = %v, want %q", err, test.wantErr)
			}
			if constructions != 0 {
				t.Fatalf("invalid recovery provider constructions = %d", constructions)
			}
			if after := snapshotRootRecoverySession(t, fixture.sessionDir); !equalRootRecoverySnapshots(before, after) {
				t.Fatal("invalid provider result recovery mutated the session")
			}
		})
	}
}

func TestRootRecoveryUsesPersistedBundleContractAndNamedInputSnapshots(t *testing.T) {
	launchCWD := t.TempDir()
	bundlePath := filepath.Join(launchCWD, "bundle.json")
	inputPath := filepath.Join(launchCWD, "payload.json")
	settingsPath := filepath.Join(launchCWD, "settings.toml")
	writeRootRecipeTestFile(t, bundlePath, rootRecipeTestBundle)
	writeRootRecipeTestFile(t, inputPath, `{"original":true}`)
	writeRootRecipeTestFile(t, settingsPath, "original settings\n")
	config := rootRecipeRuntimeConfig("neutral/contract-v1")
	config.SettingsPath = settingsPath
	sessionDir := filepath.Join(t.TempDir(), "session")
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, `{"result":"from participants"}`), nil
	}
	interrupted := errors.New("participant checkpoint interruption")
	rootCheckpointAfterSave = func(ordinal int) error {
		if ordinal == 2 {
			return interrupted
		}
		return nil
	}
	_, runErr := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:            sessionDir,
		Task:                  "Recover from persisted integration snapshots",
		RecipeID:              "neutral-root",
		IntegrationBundlePath: bundlePath,
		InputBindings:         []string{"payload=" + inputPath},
		LaunchCWD:             launchCWD,
		RuntimeConfig:         config,
		ReadinessCheck:        readyRootRecipeCheck,
		backendFactory:        recorder.factory(),
	})
	rootCheckpointAfterSave = nil
	t.Cleanup(func() { rootCheckpointAfterSave = nil })
	if !errors.Is(runErr, interrupted) {
		t.Fatalf("participant interruption = %v", runErr)
	}
	writeRootRecipeTestFile(t, bundlePath, `{"changed":true}`)
	writeRootRecipeTestFile(t, inputPath, `{"changed":true}`)
	writeRootRecipeTestFile(t, settingsPath, "changed settings\n")

	result, err := Resume(context.Background(), sessionDir, ResumeOptions{})
	if err != nil {
		t.Fatalf("resume integration snapshots: %v", err)
	}
	if result["status"] != "completed" || result["validation_status"] != "validated" {
		t.Fatalf("integration recovery result = %#v", result)
	}
	canonical := assertRootRecipeArtifact(t, store.New(sessionDir), result["canonical_result_ref"], contracts.RootArtifactKindCanonicalResult, 0)
	if canonical["canonical_json"] != `{"result":"from participants"}` {
		t.Fatalf("canonical result = %#v", canonical)
	}
}

func TestRootRecoveryRejectsInconsistentValidatedCleanupCheckpointBeforeMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{name: "phase", mutate: func(payload map[string]any, _ map[string]any) { payload["phase"] = "wrong" }},
		{name: "status", mutate: func(payload map[string]any, _ map[string]any) { payload["status"] = "failed" }},
		{name: "cleanup_status", mutate: func(payload map[string]any, _ map[string]any) { payload["cleanup_status"] = "pending" }},
		{name: "validation_status", mutate: func(payload map[string]any, _ map[string]any) { payload["validation_status"] = "failed" }},
		{name: "raw_result_ref", mutate: func(payload map[string]any, ref map[string]any) { payload["raw_result_ref"] = ref }},
		{name: "result_validation_ref", mutate: func(payload map[string]any, ref map[string]any) { payload["result_validation_ref"] = ref }},
		{name: "canonical_result_ref", mutate: func(payload map[string]any, ref map[string]any) { payload["canonical_result_ref"] = ref }},
		{name: "execution_workspace_ref", mutate: func(payload map[string]any, ref map[string]any) { payload["execution_workspace_ref"] = ref }},
		{name: "previous_checkpoint_ref", mutate: func(payload map[string]any, ref map[string]any) { payload["previous_checkpoint_ref"] = ref }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := rootRecipeRuntimeConfig("neutral/result-contract-v1")
			recorder := &rootBackendRecorder{}
			recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
				if call.SlotID == "facilitator" {
					return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
				}
				return successfulRootTurn(call.Backend, `{"items":["durable"]}`), nil
			}
			sessionDir := filepath.Join(t.TempDir(), "session")
			result, err := RunRecipe(context.Background(), RecipeOptions{
				SessionDir:        sessionDir,
				Task:              "Reject an inconsistent cleanup checkpoint",
				RecipeID:          "neutral-root",
				LaunchCWD:         t.TempDir(),
				RuntimeConfig:     config,
				IntegrationBundle: decodeRootRecipeTestBundle(t, rootResultTestBundle),
				ReadinessCheck:    readyRootRecipeCheck,
				backendFactory:    recorder.factory(),
			})
			if err != nil || result["validation_status"] != "validated" {
				t.Fatalf("complete integration fixture = %#v, %v", result, err)
			}
			st := store.New(sessionDir)
			checkpointRefs := result["root_checkpoint_refs"].([]any)
			cleanup := assertRootRecipeArtifact(t, st, checkpointRefs[4], contracts.RootArtifactKindRootCheckpoint, 5)
			wrongRef, err := contracts.ArtifactRefForPayload("wrong:selected", map[string]any{"test": test.name})
			if err != nil {
				t.Fatalf("create wrong ref: %v", err)
			}
			test.mutate(cleanup, wrongRef)
			if _, err := saveRootArtifact(st, contracts.RootArtifactKindRootCheckpoint, 5, cleanup); err != nil {
				t.Fatalf("persist inconsistent cleanup checkpoint: %v", err)
			}

			before := snapshotRootRecoverySession(t, sessionDir)
			_, err = Resume(context.Background(), sessionDir, ResumeOptions{})
			assertRootRecipeDiagnostic(t, err, diagnosticCodePersistenceIntegrity)
			after := snapshotRootRecoverySession(t, sessionDir)
			if !equalRootRecoverySnapshots(before, after) {
				t.Fatal("cleanup checkpoint integrity rejection mutated the session")
			}
		})
	}
}

func TestRootRecoveryDirectAPIRejectsStructuralOverrides(t *testing.T) {
	for name, options := range map[string]ResumeOptions{
		"prompt":              {Prompt: "new prompt"},
		"context":             {ContextFiles: []string{"changed.txt"}},
		"skill":               {SkillFiles: []string{"changed.md"}},
		"mode":                {Mode: "steelman"},
		"rounds":              {Rounds: 2},
		"max_rounds":          {MaxRounds: 5},
		"replacement":         {ReplaceAgents: []string{"codex"}},
		"model":               {SlotConfigs: []SlotConfig{{Model: "changed"}}},
		"effort":              {SlotConfigs: []SlotConfig{{Effort: "high"}}},
		"profile":             {SlotConfigs: []SlotConfig{{ProfileID: "changed"}}},
		"slot_settings":       {SlotConfigs: []SlotConfig{{SettingsPath: "changed.toml"}}},
		"composition_path":    {SlotConfigs: []SlotConfig{{CompositionPath: "changed"}}},
		"runtime_config":      {SlotConfigs: []SlotConfig{{RuntimeConfig: rootRecipeRuntimeConfig("")}}},
		"runtime_limit_only":  {SlotConfigs: []SlotConfig{{RuntimeConfig: recipes.RuntimeConfig{Limits: recipes.RuntimeLimits{NamedInputMaxBytes: 1}}}}},
		"slot_depth":          {SlotConfigs: []SlotConfig{{Depth: 2}}},
		"slot_max_depth":      {SlotConfigs: []SlotConfig{{MaxDepth: 3}}},
		"settings":            {SettingsPath: "changed.toml"},
		"facilitator":         {FacilitatorModel: "changed"},
		"relay_backend_depth": {RelayBackendDepth: 2},
		"max_relay_depth":     {MaxRelayDepth: 3},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateRootResumeOverrides(options)
			if err == nil || !strings.Contains(err.Error(), "structural resume overrides") {
				t.Fatalf("override error = %v", err)
			}
		})
	}
	if err := validateRootResumeOverrides(ResumeOptions{TimeoutSeconds: 30, StallTimeoutSeconds: 2}); err != nil {
		t.Fatalf("operational timeout override rejected: %v", err)
	}
	cliDefaults := ResumeOptions{
		MaxRounds:           50,
		TimeoutSeconds:      600,
		StallTimeoutSeconds: 300,
		ReplaceAgents:       []string{"", ""},
		SlotConfigs:         []SlotConfig{{}, {}},
		ExplicitFields:      map[string]bool{"session-dir": true},
	}
	if err := validateRootResumeOverrides(cliDefaults); err != nil {
		t.Fatalf("unvisited CLI defaults created structural overrides: %v", err)
	}
	cliDefaults.ExplicitFields["max-rounds"] = true
	if err := validateRootResumeOverrides(cliDefaults); err == nil {
		t.Fatal("explicit CLI max-rounds override was accepted")
	}
}

func TestRootRecoveryDirectAPIRecognizesLimitOnlyRuntimeOverrideBeforeMutation(t *testing.T) {
	sessionDir := t.TempDir()
	st := store.New(sessionDir)
	if err := st.SaveMetaMap(map[string]any{
		"execution_kind": "recipe",
		"status":         "failed",
	}); err != nil {
		t.Fatalf("save root recovery metadata: %v", err)
	}
	metaPath := filepath.Join(sessionDir, "meta.json")
	before, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read root recovery metadata: %v", err)
	}

	result, err := Resume(context.Background(), sessionDir, ResumeOptions{
		SlotConfigs: []SlotConfig{{
			RuntimeConfig: recipes.RuntimeConfig{
				Limits: recipes.RuntimeLimits{RepositoryInventoryMaxBytes: 1},
			},
		}},
	})
	var diagnosticErr *contracts.DiagnosticError
	if err == nil || result != nil || !strings.Contains(err.Error(), "structural resume overrides") ||
		!errors.As(err, &diagnosticErr) || len(diagnosticErr.Diagnostics) != 1 {
		t.Fatalf("limit-only recovery override = result %#v error %v", result, err)
	}
	overrides, _ := diagnosticErr.Diagnostics[0].Details["overrides"].([]any)
	if len(overrides) != 1 || overrides[0] != "--runtime-config-a" {
		t.Fatalf("limit-only recovery override details = %#v", diagnosticErr.Diagnostics[0].Details)
	}
	after, readErr := os.ReadFile(metaPath)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("limit-only recovery override mutated metadata: read=%v equal=%v", readErr, string(after) == string(before))
	}
}

func TestRootRecoveryRejectsMissingRetainedWorktreeBeforeMutation(t *testing.T) {
	sourceRoot := t.TempDir()
	runTestGit(t, sourceRoot, "init")
	runTestGit(t, sourceRoot, "config", "user.email", "recovery@example.invalid")
	runTestGit(t, sourceRoot, "config", "user.name", "Recovery Test")
	if err := os.WriteFile(filepath.Join(sourceRoot, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, sourceRoot, "add", "tracked.txt")
	runTestGit(t, sourceRoot, "commit", "-m", "fixture")
	config := rootRecipeRuntimeConfig("")
	config.RelayRecipes["neutral-root"]["lifecycle"].(map[string]any)["workspace_isolation"] = "ephemeral"
	sessionDir := filepath.Join(t.TempDir(), "session")
	interrupted := errors.New("participant checkpoint interruption")
	rootCheckpointAfterSave = func(ordinal int) error {
		if ordinal == 2 {
			return interrupted
		}
		return nil
	}
	result, runErr := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Require the retained worktree",
		RecipeID:       "neutral-root",
		LaunchCWD:      sourceRoot,
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: (&rootBackendRecorder{}).factory(),
	})
	rootCheckpointAfterSave = nil
	t.Cleanup(func() { rootCheckpointAfterSave = nil })
	if !errors.Is(runErr, interrupted) {
		t.Fatalf("run interruption = %v", runErr)
	}
	worktreePath := stringFromAny(result["execution_cwd"])
	runTestGit(t, sourceRoot, "worktree", "remove", "--force", worktreePath)
	before := snapshotRootRecoverySession(t, sessionDir)
	if _, err := Resume(context.Background(), sessionDir, ResumeOptions{Prompt: "forbidden structural override"}); err == nil || !strings.Contains(err.Error(), "structural resume overrides") {
		t.Fatalf("structural override error = %v", err)
	}
	if afterOverride := snapshotRootRecoverySession(t, sessionDir); !equalRootRecoverySnapshots(before, afterOverride) {
		t.Fatal("structural override rejection mutated the session")
	}
	_, err := Resume(context.Background(), sessionDir, ResumeOptions{})
	if err == nil || !strings.Contains(err.Error(), "retained execution worktree") {
		t.Fatalf("missing worktree error = %v", err)
	}
	after := snapshotRootRecoverySession(t, sessionDir)
	if !equalRootRecoverySnapshots(before, after) {
		t.Fatal("missing worktree rejection mutated the session")
	}
}

func TestRootRecoveryRejectsMissingParticipantCheckpointBeforeMutation(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "slot_0" {
			return TurnResult{}, errors.New("participant failed before checkpoint")
		}
		return successfulRootTurn(call.Backend, "unused"), nil
	}
	_, runErr := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Do not recover incomplete participants",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  rootRecipeRuntimeConfig(""),
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: recorder.factory(),
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "participant failed") {
		t.Fatalf("participant failure = %v", runErr)
	}
	before := snapshotRootRecoverySession(t, sessionDir)
	_, err := Resume(context.Background(), sessionDir, ResumeOptions{})
	if err == nil || !strings.Contains(err.Error(), "participant-complete checkpoint") {
		t.Fatalf("missing participant checkpoint error = %v", err)
	}
	after := snapshotRootRecoverySession(t, sessionDir)
	if !equalRootRecoverySnapshots(before, after) {
		t.Fatal("missing participant checkpoint rejection mutated the session")
	}
}

func TestRootLifecycleForbidRejectsEveryMutationBeforeSessionWrites(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	st := store.New(sessionDir)
	if err := st.SaveMetaMap(map[string]any{
		"session_id":                       "lifecycle-forbid",
		"execution_kind":                   "recipe",
		"status":                           "running",
		"participant_turns":                2,
		"next_unsealed_participant_turn":   1,
		"sealed_participant_turns":         []any{},
		"lifecycle":                        map[string]any{"resume": "forbid", "steering": "forbid", "dynamic": "forbid"},
		"runtime_config_ref":               nil,
		"transient_recipe_refs":            []any{},
		"transient_recipe_contract_refs":   []any{},
		"root_checkpoint_refs":             []any{},
		"participant_turns_completed":      0,
		"actual_participant_turns":         0,
		"next_unsealed_participant_prompt": 1,
	}); err != nil {
		t.Fatalf("save lifecycle fixture: %v", err)
	}
	if err := st.SaveTranscriptItems([]any{}); err != nil {
		t.Fatalf("save lifecycle transcript: %v", err)
	}
	baseline := snapshotRootRecoverySession(t, sessionDir)
	actions := []struct {
		name string
		run  func() error
	}{
		{name: "resume", run: func() error { _, err := Resume(context.Background(), sessionDir, ResumeOptions{}); return err }},
		{name: "steer", run: func() error { _, err := QueueSteeringPrompt(sessionDir, "forbidden"); return err }},
		{name: "proposal_create", run: func() error {
			_, err := maybeCreateSpawnProposals(st, "ask", map[string]any{}, map[string]any{}, map[string]any{}, 2, "evt", nil, nil)
			return err
		}},
		{name: "automatic_expansion", run: func() error {
			_, err := maybeCreateSpawnProposals(st, "auto-safe", map[string]any{}, map[string]any{}, map[string]any{}, 2, "evt", nil, nil)
			return err
		}},
		{name: "approve", run: func() error {
			_, err := ApproveProposal(context.Background(), sessionDir, ApproveOptions{ProposalID: "missing"})
			return err
		}},
		{name: "reject", run: func() error {
			_, err := RejectProposal(sessionDir, RejectOptions{ProposalID: "missing"})
			return err
		}},
	}
	for _, action := range actions {
		t.Run(action.name, func(t *testing.T) {
			err := action.run()
			var diagnostic *contracts.DiagnosticError
			if !errors.As(err, &diagnostic) || len(diagnostic.Diagnostics) == 0 || diagnostic.Diagnostics[0].Code != diagnosticCodeRootLifecycleForbidden {
				t.Fatalf("lifecycle rejection = %T %[1]v", err)
			}
			after := snapshotRootRecoverySession(t, sessionDir)
			if !equalRootRecoverySnapshots(baseline, after) {
				t.Fatalf("%s lifecycle rejection mutated the session", action.name)
			}
		})
	}
	if _, err := Proposals(sessionDir); err != nil {
		t.Fatalf("proposal inspection should remain available: %v", err)
	}
}

func TestRootLifecycleForbidDoesNotBlockStopKillOrClean(t *testing.T) {
	for _, forceKill := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "kill"}[forceKill], func(t *testing.T) {
			sessionDir := filepath.Join(t.TempDir(), "session")
			st := store.New(sessionDir)
			if err := st.SaveMetaMap(map[string]any{
				"session_id":     "administrative-root",
				"execution_kind": "recipe",
				"status":         "running",
				"task":           "Administrative root fixture",
				"lifecycle":      map[string]any{"resume": "forbid", "steering": "forbid", "dynamic": "forbid"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveTranscriptItems([]any{}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sessionDir, "relay.pid"), []byte("99999999\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := Stop(sessionDir, StopOptions{ForceKill: forceKill})
			if err != nil || report["status"] != "orphaned" {
				t.Fatalf("administrative stop/kill = %#v, %v", report, err)
			}
		})
	}

	cleanDir := filepath.Join(t.TempDir(), "session")
	cleanStore := store.New(cleanDir)
	if err := cleanStore.SaveMetaMap(map[string]any{
		"session_id":     "clean-root",
		"execution_kind": "recipe",
		"status":         "completed",
		"task":           "Clean root fixture",
		"lifecycle":      map[string]any{"resume": "forbid", "steering": "forbid", "dynamic": "forbid"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cleanStore.SaveTranscriptItems([]any{}); err != nil {
		t.Fatal(err)
	}
	report, err := CleanSession(cleanDir)
	if err != nil || report["status"] != "deleted" {
		t.Fatalf("administrative clean = %#v, %v", report, err)
	}
	if _, err := os.Stat(cleanDir); !os.IsNotExist(err) {
		t.Fatalf("cleaned root session remained: %v", err)
	}
}

func snapshotRootRecoverySession(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[filepath.ToSlash(relative)] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot session: %v", err)
	}
	return snapshot
}

type durableProviderResultCrashFixture struct {
	sessionDir     string
	st             *store.Store
	result         map[string]any
	marker         rootProviderAttemptMarker
	resultArtifact *persistedRootRecoveryArtifact
}

func newDurableProviderResultCrashFixture(t *testing.T) *durableProviderResultCrashFixture {
	t.Helper()
	config := rootRecipeRuntimeConfig("")
	recipe := config.RelayRecipes["neutral-root"]
	recipe["participant_turns"] = 1
	recipe["max_rounds"] = 1
	recipe["result_source"] = integration.ResultSourceReducer
	recipe["provider_retry"] = recipes.ProviderRetryAllow
	sessionDir := filepath.Join(t.TempDir(), "session")
	initial := &rootBackendRecorder{}
	initial.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "reducer" {
			return successfulRootTurn(call.Backend, "durable provider result without marker completion"), nil
		}
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, "participant result"), nil
	}
	interrupted := errors.New("after provider result before marker completion")
	fired := false
	rootProviderResultAfterSave = func(spec rootInvocationSpec, attempt int) error {
		if spec.phase == "reducer" && attempt == 1 && !fired {
			fired = true
			return interrupted
		}
		return nil
	}
	t.Cleanup(func() { rootProviderResultAfterSave = nil })
	result, runErr := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Repair provider result lineage",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: initial.factory(),
	})
	rootProviderResultAfterSave = nil
	if !fired || !errors.Is(runErr, interrupted) {
		t.Fatalf("provider result interruption = %v, fired=%v", runErr, fired)
	}
	st := store.New(sessionDir)
	markers, err := loadRootProviderAttemptMarkers(st)
	if err != nil {
		t.Fatalf("load interrupted markers: %v", err)
	}
	marker := markers[len(markers)-1]
	if marker.Phase != "reducer" {
		t.Fatalf("interrupted reducer marker = %#v", marker)
	}
	resultArtifact, found, err := loadLatestRootRecoveryArtifact(st, contracts.RootArtifactKindProviderResult, marker.ArtifactOrdinal)
	if err != nil || !found {
		t.Fatalf("load durable provider result = found %v, err %v", found, err)
	}
	return &durableProviderResultCrashFixture{
		sessionDir:     sessionDir,
		st:             st,
		result:         result,
		marker:         marker,
		resultArtifact: resultArtifact,
	}
}

func providerAttemptMarkerFor(t *testing.T, markers []rootProviderAttemptMarker, invocationID string, runnerAttempt int) rootProviderAttemptMarker {
	t.Helper()
	for _, marker := range markers {
		if marker.InvocationID == invocationID && marker.RunnerAttempt == runnerAttempt {
			return marker
		}
	}
	t.Fatalf("provider marker %s attempt %d not found", invocationID, runnerAttempt)
	return rootProviderAttemptMarker{}
}

func persistedRootInvocationRecordsForPhase(t *testing.T, st *store.Store, rawRefs any, phase string) []map[string]any {
	t.Helper()
	refs, ok := rawRefs.([]any)
	if !ok {
		t.Fatalf("invocation refs = %#v", rawRefs)
	}
	records := []map[string]any{}
	for _, rawRef := range refs {
		ref, _ := rawRef.(map[string]any)
		ordinal, err := rootArtifactOrdinalFromRef(ref, contracts.RootArtifactKindProviderInvocation)
		if err != nil {
			t.Fatalf("provider invocation ref ordinal: %v", err)
		}
		payload := assertRootRecipeArtifact(t, st, rawRef, contracts.RootArtifactKindProviderInvocation, ordinal)
		record, err := contracts.ValidateProviderInvocationRecord(payload["invocation"])
		if err != nil {
			t.Fatalf("validate provider invocation %d: %v", ordinal, err)
		}
		if record["phase"] == phase {
			records = append(records, record)
		}
	}
	return records
}

func equalRootRecoverySnapshots(left map[string]string, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
