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

func TestRootRecoveryDirectAPIRejectsStructuralOverrides(t *testing.T) {
	for name, options := range map[string]ResumeOptions{
		"prompt":      {Prompt: "new prompt"},
		"context":     {ContextFiles: []string{"changed.txt"}},
		"skill":       {SkillFiles: []string{"changed.md"}},
		"mode":        {Mode: "steelman"},
		"rounds":      {Rounds: 2},
		"max_rounds":  {MaxRounds: 5},
		"replacement": {ReplaceAgents: []string{"codex"}},
		"model":       {SlotConfigs: []SlotConfig{{Model: "changed"}}},
		"effort":      {SlotConfigs: []SlotConfig{{Effort: "high"}}},
		"settings":    {SettingsPath: "changed.toml"},
		"facilitator": {FacilitatorModel: "changed"},
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
