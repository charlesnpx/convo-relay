package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/namedinputs"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const diagnosticCodeRootResumeOverride = "root_recipe_resume_override_forbidden"

type persistedRootRecoveryArtifact struct {
	ref     map[string]any
	payload map[string]any
}

type persistedRootReducerAttempt struct {
	ordinal int
	ref     map[string]any
	payload map[string]any
}

type preparedRootRecovery struct {
	st          *store.Store
	meta        model.SessionMeta
	transcript  model.Transcript
	preflight   *recipePreflight
	persisted   *persistedRecipeRun
	workspace   *workspace.Materialized
	checkpoints map[int]*persistedRootRecoveryArtifact
	attempts    []persistedRootReducerAttempt
	candidate   *rootCandidate
	validation  *persistedRootRecoveryArtifact
	canonical   *persistedRootRecoveryArtifact
}

// resumeRootRecipe performs every prerequisite and policy check before it can
// create the session mutation lock. Missing participant state or a required
// retained worktree therefore rejects byte-for-byte without session writes.
func resumeRootRecipe(ctx context.Context, sessionDir string, opts ResumeOptions, initialMeta model.SessionMeta) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateRootResumeOverrides(opts); err != nil {
		return nil, err
	}
	if err := guardRootLifecycleMeta(initialMeta, rootLifecycleActionResume); err != nil {
		return nil, err
	}
	preparedBeforeLock, err := prepareRootRecovery(ctx, sessionDir, opts)
	if err != nil {
		return nil, err
	}
	if complete, terminalErr := preparedBeforeLock.completedResult(); complete {
		return sessionResult(sessionDir, preparedBeforeLock.meta, preparedBeforeLock.transcript), terminalErr
	}

	lock, err := lockSessionMutation(sessionDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = lock.Unlock()
	}()
	if err := ensureSessionNotRunning(sessionDir, "resuming root recovery"); err != nil {
		return nil, err
	}

	prepared, err := prepareRootRecovery(ctx, sessionDir, opts)
	if err != nil {
		return nil, err
	}
	if err := guardRootLifecycleMeta(prepared.meta, rootLifecycleActionResume); err != nil {
		return nil, err
	}
	if complete, terminalErr := prepared.completedResult(); complete {
		return sessionResult(sessionDir, prepared.meta, prepared.transcript), terminalErr
	}
	if err := writePID(sessionDir); err != nil {
		return nil, err
	}
	defer removePID(sessionDir)
	return prepared.run(ctx)
}

func (prepared *preparedRootRecovery) completedResult() (bool, error) {
	checkpoint := prepared.checkpoints[5]
	if checkpoint == nil || prepared.validation == nil || prepared.candidate == nil {
		return false, nil
	}
	if strings.TrimSpace(prepared.meta.String("cleanup_status")) != "completed" {
		return false, nil
	}
	latest, _ := prepared.meta.Get("latest_root_checkpoint_ref").(map[string]any)
	if latest == nil || requireMatchingArtifactRef(latest, checkpoint.ref, "completed root checkpoint") != nil {
		return false, nil
	}

	validationFailed := strings.TrimSpace(stringFromAny(prepared.validation.payload["status"])) == "failed"
	sourceMutated := prepared.workspace != nil && prepared.workspace.Artifact["source_mutated"] == true
	eventType := "node_completed"
	graphStatus := "completed"
	metaStatus := "completed"
	var terminalErr error
	if validationFailed {
		eventType = "node_failed"
		graphStatus = rootInvalidResultStatus
		metaStatus = rootInvalidResultStatus
		terminalErr = persistedRootValidationError(prepared.validation.payload)
	}
	if sourceMutated {
		eventType = "node_failed"
		graphStatus = "failed"
		metaStatus = "failed"
		mutationErr := &workspace.SourceMutatedError{
			BeforeDigest: strings.TrimSpace(stringFromAny(prepared.workspace.Artifact["source_before_digest"])),
			AfterDigest:  strings.TrimSpace(stringFromAny(prepared.workspace.Artifact["source_after_digest"])),
		}
		terminalErr = errors.Join(terminalErr, mutationErr)
	}
	if prepared.meta.String("status") != metaStatus || prepared.meta.String("execution_phase") == "result_completion_persistence" {
		return false, nil
	}
	events, err := prepared.st.ReadEvents()
	if err != nil {
		return false, nil
	}
	foundTerminalEvent := false
	for _, event := range events {
		if strings.TrimSpace(stringFromAny(event["event_type"])) != eventType {
			continue
		}
		payload, _ := event["payload"].(map[string]any)
		ref, _ := payload["root_checkpoint_ref"].(map[string]any)
		if ref != nil && requireMatchingArtifactRef(ref, checkpoint.ref, "completed root checkpoint event") == nil {
			foundTerminalEvent = true
			break
		}
	}
	if !foundTerminalEvent {
		return false, nil
	}
	graphPayload := prepared.st.LoadGraph()
	nodes, _ := graphPayload["nodes"].(map[string]any)
	root, _ := nodes[graph.RootNodeID].(map[string]any)
	if strings.TrimSpace(stringFromAny(root["status"])) != graphStatus {
		return false, nil
	}
	return true, terminalErr
}

func validateRootResumeOverrides(opts ResumeOptions) error {
	explicit := opts.ExplicitFields
	direct := explicit == nil
	forbidden := map[string]bool{}
	mark := func(name string, inferred bool) {
		if (direct && inferred) || (!direct && explicit[name]) {
			forbidden[name] = true
		}
	}
	mark("prompt", strings.TrimSpace(opts.Prompt) != "")
	mark("context", len(opts.ContextFiles) > 0)
	mark("skill", len(opts.SkillFiles) > 0)
	mark("mode", strings.TrimSpace(opts.Mode) != "")
	mark("rounds", opts.Rounds != 0)
	mark("max-rounds", opts.MaxRounds != 0)
	mark("quick", false)
	mark("settings", strings.TrimSpace(opts.SettingsPath) != "")
	mark("facilitator-model", strings.TrimSpace(opts.FacilitatorModel) != "")
	mark("facilitator-effort", strings.TrimSpace(opts.FacilitatorEffort) != "")
	for index, replacement := range opts.ReplaceAgents {
		mark(fmt.Sprintf("replace-%c", 'a'+rune(index)), strings.TrimSpace(replacement) != "")
	}
	for index, config := range opts.SlotConfigs {
		mark(fmt.Sprintf("model-%c", 'a'+rune(index)), strings.TrimSpace(config.Model) != "")
		mark(fmt.Sprintf("effort-%c", 'a'+rune(index)), strings.TrimSpace(config.Effort) != "")
		mark(fmt.Sprintf("agent-%c", 'a'+rune(index)), strings.TrimSpace(config.ProfileID) != "")
	}
	if len(forbidden) == 0 {
		return nil
	}
	names := make([]string, 0, len(forbidden))
	for name := range forbidden {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]any, 0, len(names))
	for _, name := range names {
		items = append(items, "--"+name)
	}
	return rootRecipeDiagnostic(
		diagnosticCodeRootResumeOverride,
		contracts.DiagnosticPhasePolicy,
		"/resume",
		"Root recipe recovery rejects structural resume overrides; start a new root run instead.",
		map[string]any{"overrides": items},
	)
}

func prepareRootRecovery(ctx context.Context, sessionDir string, opts ResumeOptions) (*preparedRootRecovery, error) {
	st := store.New(sessionDir)
	meta, err := loadSessionMeta(sessionDir)
	if err != nil {
		return nil, err
	}
	if meta.String("execution_kind") != "recipe" {
		return nil, persistenceIntegrityError("Root recovery requires a recipe session.", nil)
	}
	transcript, err := loadSessionTranscript(sessionDir)
	if err != nil {
		return nil, err
	}

	checkpoints := map[int]*persistedRootRecoveryArtifact{}
	for _, ordinal := range []int{1, 2, 3, 4, 5} {
		artifact, found, err := loadLatestRootRecoveryArtifact(st, contracts.RootArtifactKindRootCheckpoint, ordinal)
		if err != nil {
			return nil, err
		}
		if found {
			checkpoints[ordinal] = artifact
		}
	}
	checkpointOne := checkpoints[1]
	checkpointTwo := checkpoints[2]
	if checkpointOne == nil {
		return nil, persistenceIntegrityError("Root recovery requires the durable workspace-ready checkpoint.", map[string]any{"checkpoint_ordinal": 1})
	}
	if checkpointTwo == nil {
		return nil, persistenceIntegrityError("Root recovery requires the durable participant-complete checkpoint.", map[string]any{"checkpoint_ordinal": 2})
	}
	if err := validateRootRecoveryCheckpoint(checkpointOne.payload, 1, "workspace_ready", "completed"); err != nil {
		return nil, err
	}
	if err := validateRootRecoveryCheckpoint(checkpointTwo.payload, 2, "participant_turns_complete", "completed"); err != nil {
		return nil, err
	}
	for _, ordinal := range []int{3, 4, 5} {
		if checkpoint := checkpoints[ordinal]; checkpoint != nil {
			if err := validateRootRecoveryCheckpointOrdinal(checkpoint.payload, ordinal); err != nil {
				return nil, err
			}
		}
	}
	if checkpoints[4] != nil && checkpoints[3] == nil {
		return nil, persistenceIntegrityError("Root validation checkpoint exists without a candidate checkpoint.", nil)
	}
	if checkpoints[5] != nil && checkpoints[4] == nil {
		return nil, persistenceIntegrityError("Root cleanup checkpoint exists without a validation checkpoint.", nil)
	}

	baseRefs := map[string]map[string]any{}
	for _, key := range []string{
		"recipe_ref", "root_recipe_plan_ref", "runtime_config_ref", "integration_bundle_ref",
		"integration_contract_ref", "named_input_manifest_ref", "execution_workspace_ref",
	} {
		one, err := optionalArtifactRef(checkpointOne.payload[key], key)
		if err != nil {
			return nil, err
		}
		two, err := optionalArtifactRef(checkpointTwo.payload[key], key)
		if err != nil {
			return nil, err
		}
		if err := requireOptionalMatchingArtifactRef(one, two, "root checkpoint "+key); err != nil {
			return nil, err
		}
		baseRefs[key] = two
	}
	for _, key := range []string{"recipe_ref", "root_recipe_plan_ref", "runtime_config_ref", "execution_workspace_ref"} {
		if baseRefs[key] == nil {
			return nil, persistenceIntegrityError("Participant checkpoint is missing a required persisted ref.", map[string]any{"field": key})
		}
	}

	rootPlan, err := loadRootRecoveryArtifactRef(st, baseRefs["root_recipe_plan_ref"], contracts.RootArtifactKindRootRecipePlan, 0)
	if err != nil {
		return nil, err
	}
	recipePayload, err := st.LoadArtifactPayloadRaw(baseRefs["recipe_ref"])
	if err != nil {
		return nil, persistenceIntegrityError("Persisted root recipe artifact could not be loaded.", map[string]any{"cause": err.Error()})
	}
	if err := validatePersistedRootRecipe(recipePayload, baseRefs["recipe_ref"], rootPlan.payload, meta); err != nil {
		return nil, err
	}
	runtimeConfig, err := loadRuntimeConfigFromSnapshot(st, baseRefs["runtime_config_ref"])
	if err != nil {
		return nil, persistenceIntegrityError("Persisted runtime configuration snapshot is invalid.", map[string]any{"cause": err.Error()})
	}
	if err := validateRuntimeRecipeSnapshot(runtimeConfig, recipePayload, baseRefs["recipe_ref"], meta.String("recipe_id")); err != nil {
		return nil, err
	}

	bundle, selectedContract, err := loadRootRecoveryIntegration(st, rootPlan.payload, baseRefs)
	if err != nil {
		return nil, err
	}
	var assertionEvaluator *integration.AssertionEvaluator
	if selectedContract != nil {
		if baseRefs["named_input_manifest_ref"] == nil {
			return nil, persistenceIntegrityError("Integration-bound root recovery requires its named input manifest.", nil)
		}
		assertionInputs, err := namedinputs.LoadAssertionInputs(st, baseRefs["named_input_manifest_ref"], selectedContract.ID())
		if err != nil {
			return nil, err
		}
		assertionEvaluator, err = integration.PrepareAssertionEvaluator(selectedContract, assertionInputs)
		if err != nil {
			return nil, err
		}
	} else if baseRefs["named_input_manifest_ref"] != nil {
		return nil, persistenceIntegrityError("Contractless root recovery cannot reference a named input manifest.", nil)
	}

	workspaceState, err := workspace.Recover(ctx, st)
	if err != nil {
		return nil, persistenceIntegrityError("Persisted root execution workspace is not recoverable: "+err.Error(), map[string]any{"cause": err.Error()})
	}
	if _, err := loadRootRecoveryArtifactRef(st, baseRefs["execution_workspace_ref"], contracts.RootArtifactKindExecutionWorkspace, 0); err != nil {
		return nil, err
	}

	launchContexts, err := loadPersistedRootLaunchContexts(st, meta.Slice("launch_context_refs"))
	if err != nil {
		return nil, err
	}
	attempts, err := loadPersistedRootReducerAttempts(st, meta)
	if err != nil {
		return nil, err
	}
	candidateArtifact, candidateFound, err := loadLatestRootRecoveryArtifact(st, contracts.RootArtifactKindRawResult, 0)
	if err != nil {
		return nil, err
	}
	var candidate *rootCandidate
	if candidateFound {
		candidate, err = rootCandidateFromArtifact(candidateArtifact, rootPlan.payload, transcript, attempts)
		if err != nil {
			return nil, err
		}
	}
	validation, _, err := loadLatestRootRecoveryArtifact(st, contracts.RootArtifactKindResultValidation, 0)
	if err != nil {
		return nil, err
	}
	canonical, _, err := loadLatestRootRecoveryArtifact(st, contracts.RootArtifactKindCanonicalResult, 0)
	if err != nil {
		return nil, err
	}
	if err := validateRecoveredResultArtifacts(checkpoints, candidate, validation, canonical, selectedContract, assertionEvaluator); err != nil {
		return nil, err
	}

	participantTurns := intFromAny(rootPlan.payload["participant_turns"], 0)
	if participantTurns < 1 || transcript.Len() != participantTurns || intFromAny(checkpointTwo.payload["participant_turns_completed"], 0) != participantTurns {
		return nil, persistenceIntegrityError("Participant-complete checkpoint does not match the persisted root transcript and plan.", map[string]any{
			"plan_participant_turns":       participantTurns,
			"transcript_participant_turns": transcript.Len(),
			"checkpoint_participant_turns": intFromAny(checkpointTwo.payload["participant_turns_completed"], 0),
		})
	}
	if err := validateRootRecoveryMeta(meta, rootPlan.payload, baseRefs); err != nil {
		return nil, err
	}

	timeout, stallTimeout := recoveredRootTimeouts(meta, opts)
	preflight := &recipePreflight{
		options: RecipeOptions{
			SessionDir:          sessionDir,
			SessionID:           sessionIDFromDir(sessionDir),
			Task:                meta.String("task"),
			RecipeID:            meta.String("recipe_id"),
			TimeoutSeconds:      timeout,
			StallTimeoutSeconds: stallTimeout,
			backendFactory:      opts.backendFactory,
		},
		sessionDir:         sessionDir,
		sessionID:          sessionIDFromDir(sessionDir),
		launchCWD:          meta.String("source_launch_cwd"),
		runtimeConfig:      runtimeConfig,
		recipe:             contracts.Materialize(recipePayload).(map[string]any),
		rootPlan:           contracts.Materialize(rootPlan.payload).(map[string]any),
		bundle:             bundle,
		selectedContract:   selectedContract,
		assertionEvaluator: assertionEvaluator,
		launchContexts:     launchContexts,
		promptPolicy:       promptPolicyFromMeta(meta.ToMap()),
	}
	persisted := &persistedRecipeRun{
		st:                    st,
		runtimeConfigRef:      cloneMap(baseRefs["runtime_config_ref"]),
		transientRecipeRefs:   append([]any{}, meta.Slice("transient_recipe_refs")...),
		transientContractRefs: append([]any{}, meta.Slice("transient_recipe_contract_refs")...),
		recipeRef:             cloneMap(baseRefs["recipe_ref"]),
		rootPlanRef:           cloneMap(baseRefs["root_recipe_plan_ref"]),
		bundleRef:             cloneMap(baseRefs["integration_bundle_ref"]),
		contractRef:           cloneMap(baseRefs["integration_contract_ref"]),
		launchContextRefs:     append([]any{}, meta.Slice("launch_context_refs")...),
		inputManifestRef:      cloneMap(baseRefs["named_input_manifest_ref"]),
		workspaceRef:          cloneMap(workspaceState.ArtifactRef),
		executionCWD:          workspaceState.ExecutionCWD,
		checkpointRef:         cloneMap(checkpointTwo.ref),
	}
	return &preparedRootRecovery{
		st:          st,
		meta:        meta,
		transcript:  transcript,
		preflight:   preflight,
		persisted:   persisted,
		workspace:   workspaceState,
		checkpoints: checkpoints,
		attempts:    attempts,
		candidate:   candidate,
		validation:  validation,
		canonical:   canonical,
	}, nil
}

func (prepared *preparedRootRecovery) run(ctx context.Context) (map[string]any, error) {
	state := &rootExecutionState{
		st:         prepared.st,
		preflight:  prepared.preflight,
		persisted:  prepared.persisted,
		meta:       prepared.meta,
		transcript: prepared.transcript,
		startedAt:  time.Now(),
	}
	state.adoptRootRecoveryArtifacts(prepared)
	if err := state.saveProgress(); err != nil {
		return state.result(), err
	}
	if _, err := state.st.AppendSessionEventV1(
		"node_resumed",
		graph.RootNodeID,
		"Root recipe resumed for post-participant recovery",
		map[string]any{
			"recovery_only":              true,
			"participant_turns_complete": true,
			"root_checkpoint_ref":        state.meta.Get("latest_root_checkpoint_ref"),
		},
		store.EventOptions{},
	); err != nil {
		return state.markRootRecoveryPending("resume_persistence", err)
	}

	if prepared.checkpoints[5] != nil {
		return state.finishRecoveredRootCleanup(prepared)
	}
	if prepared.candidate == nil {
		if completed := latestCompletedReducerAttempt(prepared.attempts); completed != nil {
			candidate := rootCandidate{
				content:        stringFromAny(completed.payload["content"]),
				source:         integration.ResultSourceReducer,
				reducerAttempt: cloneMap(completed.ref),
			}
			if err := state.persistRootCandidate(&candidate); err != nil {
				return state.markRootRecoveryPending("result_candidate_persistence", err)
			}
			prepared.candidate = &candidate
		} else {
			if err := state.materializeRootRecoveryInputs(); err != nil {
				return state.markRootRecoveryPending("named_input_recovery", err)
			}
			return state.runRootResultPhases(ctx)
		}
	} else if prepared.checkpoints[3] == nil {
		if err := state.recordRootCandidateCheckpoint(prepared.candidate); err != nil {
			return state.markRootRecoveryPending("result_candidate_persistence", err)
		}
	}

	if prepared.validation == nil {
		if prepared.canonical != nil {
			if err := state.persistRootValidation("validated", prepared.candidate.rawResultRef, prepared.canonical.ref, nil); err != nil {
				return state.markRootRecoveryPending("result_validation_persistence", err)
			}
			return state.markRootResultCompleted()
		}
		return state.validateAndCompleteRootCandidate(*prepared.candidate)
	}
	if prepared.checkpoints[4] == nil {
		status := strings.TrimSpace(stringFromAny(prepared.validation.payload["status"]))
		if err := state.recordRootValidationCheckpoint(
			status,
			prepared.candidate.rawResultRef,
			artifactRefOrNil(prepared.canonical),
			prepared.validation.ref,
			prepared.validation.payload["diagnostics"],
		); err != nil {
			return state.markRootRecoveryPending("result_validation_persistence", err)
		}
	}
	if strings.TrimSpace(stringFromAny(prepared.validation.payload["status"])) == "failed" {
		return state.markInvalidRootResult(persistedRootValidationError(prepared.validation.payload))
	}
	return state.markRootResultCompleted()
}

func (s *rootExecutionState) adoptRootRecoveryArtifacts(prepared *preparedRootRecovery) {
	participantTurns := s.transcript.Len()
	checkpointTwo := prepared.checkpoints[2]
	sealedTurns := make([]any, 0, participantTurns)
	for ordinal := 1; ordinal <= participantTurns; ordinal++ {
		sealedTurns = append(sealedTurns, ordinal)
	}
	s.meta = s.meta.
		WithStatus(rootParticipantsCompleteStatus).
		With("execution_phase", "participant_turns_complete").
		With("participant_turns_completed", participantTurns).
		With("actual_participant_turns", participantTurns).
		WithActualRounds(participantTurns).
		With("next_unsealed_participant_turn", nil).
		With("sealed_participant_turns", sealedTurns).
		With("participant_prompt_sealed_through", participantTurns).
		With("ledger", checkpointTwo.payload["ledger"]).
		With("recipe_ref", s.persisted.recipeRef).
		With("root_recipe_plan_ref", s.persisted.rootPlanRef).
		With("runtime_config_ref", s.persisted.runtimeConfigRef).
		With("integration_bundle_ref", s.persisted.bundleRef).
		With("integration_contract_ref", s.persisted.contractRef).
		With("named_input_manifest_ref", s.persisted.inputManifestRef).
		With("execution_workspace_ref", s.persisted.workspaceRef).
		With("launch_cwd", s.persisted.executionCWD).
		With("execution_cwd", s.persisted.executionCWD).
		Without("root_recovery_pending").
		Without("error").
		Without("failed_at").
		Without("interrupted_at").
		Without("reducer_failed").
		Without("invalid_result").
		Without("result_validation_failed").
		Without("completed_at").
		Without("stop_reason")
	for _, ordinal := range []int{1, 2, 3, 4, 5} {
		if checkpoint := prepared.checkpoints[ordinal]; checkpoint != nil {
			s.meta = withRootCheckpointRef(s.meta, checkpoint.ref)
		}
	}
	refs := make([]any, 0, len(prepared.attempts))
	for _, attempt := range prepared.attempts {
		refs = append(refs, cloneMap(attempt.ref))
	}
	if len(refs) > 0 {
		s.meta = s.meta.With("reducer_attempt_refs", refs).With("latest_reducer_attempt_ref", refs[len(refs)-1])
	}
	if prepared.candidate != nil {
		phase := rootCandidateCompleteStatus
		if prepared.candidate.source == integration.ResultSourceReducer {
			phase = rootReducerCompleteStatus
		}
		s.meta = s.meta.WithStatus(phase).
			With("execution_phase", phase).
			With("raw_result_ref", prepared.candidate.rawResultRef)
	}
	if prepared.validation != nil {
		status := strings.TrimSpace(stringFromAny(prepared.validation.payload["status"]))
		s.meta = s.meta.With("validation_status", status).
			With("result_validation_ref", prepared.validation.ref).
			With("result_validation_failed", status == "failed")
		if prepared.canonical != nil {
			s.meta = s.meta.With("canonical_result_ref", prepared.canonical.ref)
		}
	}
}

func (s *rootExecutionState) materializeRootRecoveryInputs() error {
	if s.preflight.selectedContract == nil {
		return nil
	}
	projection, err := namedinputs.Materialize(s.st, s.persisted.inputManifestRef, filepath.Join(s.preflight.sessionDir, "execution", "inputs"))
	if err != nil {
		return err
	}
	s.persisted.providerInputs = projection
	s.meta = s.meta.With("provider_inputs", projection)
	return s.saveProgress()
}

func (s *rootExecutionState) finishRecoveredRootCleanup(prepared *preparedRootRecovery) (map[string]any, error) {
	checkpoint := prepared.checkpoints[5]
	s.meta = withRootCheckpointRef(s.meta, checkpoint.ref).
		With("cleanup_status", "completed").
		With("execution_workspace_ref", checkpoint.payload["execution_workspace_ref"])
	if ref, ok := checkpoint.payload["execution_workspace_ref"].(map[string]any); ok {
		s.persisted.workspaceRef = cloneMap(ref)
	}
	if prepared.validation != nil && strings.TrimSpace(stringFromAny(prepared.validation.payload["status"])) == "failed" {
		validationErr := persistedRootValidationError(prepared.validation.payload)
		s.meta = s.meta.
			WithFailed(s.transcript.Len(), utcNow(), validationErr).
			WithStatus(rootInvalidResultStatus).
			With("invalid_result", true).
			With("result_validation_failed", true).
			With("execution_phase", rootValidationFailedPhase).
			With("stop_reason", rootValidationFailedPhase).
			With("actual_participant_turns", s.transcript.Len()).
			With("participant_turns_completed", s.transcript.Len())
		var terminalErr error
		s.meta, _, terminalErr = finalizeTerminalWorkspace(context.Background(), s.st, s.meta)
		if terminalErr != nil && !isSourceMutationError(terminalErr) {
			return s.markRootRecoveryPending("result_cleanup_persistence", terminalErr)
		}
		return s.finishRootInvalidResult(validationErr, terminalErr)
	}
	s.meta = s.meta.
		WithCompleted(s.transcript.Len(), roundElapsed(s.startedAt), utcNow(), "result_completed").
		With("actual_participant_turns", s.transcript.Len()).
		With("participant_turns_completed", s.transcript.Len()).
		With("execution_phase", "result_complete")
	var terminalErr error
	s.meta, _, terminalErr = finalizeTerminalWorkspace(context.Background(), s.st, s.meta)
	if terminalErr != nil && !isSourceMutationError(terminalErr) {
		return s.markRootRecoveryPending("result_cleanup_persistence", terminalErr)
	}
	return s.finishRootResultCompleted(terminalErr)
}

func loadLatestRootRecoveryArtifact(st *store.Store, kind string, ordinal int) (*persistedRootRecoveryArtifact, bool, error) {
	identity, err := contracts.RootArtifactIdentityFor(kind, ordinal)
	if err != nil {
		return nil, false, err
	}
	var ref map[string]any
	graphPayload := st.LoadGraph()
	if artifacts, ok := graphPayload["artifacts"].(map[string]any); ok {
		if entry, ok := artifacts[kind+"/"+identity.ArtifactID].(map[string]any); ok {
			ref, _ = entry["ref"].(map[string]any)
		}
	}
	if ref == nil {
		resolved, resolveErr := st.ResolveArtifactRef(identity.RefID, "")
		if resolveErr != nil {
			var notFound store.NotFoundError
			if errors.As(resolveErr, &notFound) {
				return nil, false, nil
			}
			return nil, false, resolveErr
		}
		ref = resolved
	}
	artifact, err := loadRootRecoveryArtifactRef(st, ref, kind, ordinal)
	if err != nil {
		return nil, false, err
	}
	return artifact, true, nil
}

func loadRootRecoveryArtifactRef(st *store.Store, ref map[string]any, kind string, ordinal int) (*persistedRootRecoveryArtifact, error) {
	if ref == nil {
		return nil, persistenceIntegrityError("Required persisted root artifact ref is missing.", map[string]any{"kind": kind})
	}
	payload, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, persistenceIntegrityError("Persisted root artifact could not be loaded.", map[string]any{"kind": kind, "cause": err.Error()})
	}
	validatedRef, err := contracts.ValidateRootArtifactRef(ref, kind, ordinal, payload)
	if err != nil {
		return nil, persistenceIntegrityError("Persisted root artifact ref failed digest or identity validation.", map[string]any{"kind": kind, "cause": err.Error()})
	}
	validatedPayload, err := contracts.ValidateRootArtifact(payload, kind)
	if err != nil {
		return nil, err
	}
	return &persistedRootRecoveryArtifact{ref: validatedRef, payload: validatedPayload}, nil
}

func validateRootRecoveryCheckpoint(payload map[string]any, ordinal int, phase string, status string) error {
	if err := validateRootRecoveryCheckpointOrdinal(payload, ordinal); err != nil {
		return err
	}
	if strings.TrimSpace(stringFromAny(payload["phase"])) != phase || strings.TrimSpace(stringFromAny(payload["status"])) != status {
		return persistenceIntegrityError("Persisted root checkpoint phase or status is invalid.", map[string]any{"ordinal": ordinal, "phase": payload["phase"], "status": payload["status"]})
	}
	if payload["preflight_complete"] != true || payload["workspace_ready"] != true {
		return persistenceIntegrityError("Persisted root checkpoint is missing completed prerequisites.", map[string]any{"ordinal": ordinal})
	}
	return nil
}

func validateRootRecoveryCheckpointOrdinal(payload map[string]any, ordinal int) error {
	if intFromAny(payload["ordinal"], 0) != ordinal {
		return persistenceIntegrityError("Persisted root checkpoint ordinal is invalid.", map[string]any{"expected": ordinal, "actual": payload["ordinal"]})
	}
	return nil
}

func optionalArtifactRef(value any, field string) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	if ref, ok := value.(map[string]any); ok && len(ref) == 0 {
		return nil, nil
	}
	ref, err := contracts.ValidateArtifactRef(value)
	if err != nil {
		return nil, persistenceIntegrityError("Persisted root checkpoint ref "+field+" is invalid ("+fmt.Sprintf("%T", value)+").", map[string]any{"field": field, "cause": err.Error()})
	}
	return ref, nil
}

func requireOptionalMatchingArtifactRef(left map[string]any, right map[string]any, label string) error {
	if left == nil && right == nil {
		return nil
	}
	if left == nil || right == nil {
		return persistenceIntegrityError("Persisted "+label+" changed across root checkpoints.", nil)
	}
	return requireMatchingArtifactRef(left, right, label)
}

func validatePersistedRootRecipe(recipe map[string]any, recipeRef map[string]any, plan map[string]any, meta model.SessionMeta) error {
	if strings.TrimSpace(stringFromAny(recipe["kind"])) != "recipe" || intFromAny(recipe["schema_version"], 0) != 1 {
		return persistenceIntegrityError("Persisted root recipe artifact is invalid.", nil)
	}
	recipeID := strings.TrimSpace(stringFromAny(recipe["id"]))
	if recipeID == "" || recipeID != strings.TrimSpace(stringFromAny(plan["recipe_id"])) || recipeID != meta.String("recipe_id") {
		return persistenceIntegrityError("Persisted root recipe identity does not match the plan and session.", map[string]any{"recipe_id": recipeID})
	}
	if err := requireMatchingArtifactRef(recipeRef, mapFromAny(plan["recipe_ref"]), "root recipe"); err != nil {
		return err
	}
	normalized := recipes.RecipeContractPayload(recipe)
	want, err := contracts.ArtifactRefForPayload("recipe:"+recipeID, normalized)
	if err != nil {
		return err
	}
	return requireMatchingArtifactRef(want, recipeRef, "root recipe payload")
}

func validateRuntimeRecipeSnapshot(runtimeConfig recipes.RuntimeConfig, persistedRecipe map[string]any, recipeRef map[string]any, recipeID string) error {
	runtimeRecipe := runtimeConfig.RelayRecipes[strings.TrimSpace(recipeID)]
	if runtimeRecipe == nil {
		return persistenceIntegrityError("Persisted runtime snapshot no longer contains the selected root recipe.", map[string]any{"recipe_id": recipeID})
	}
	want, err := contracts.ArtifactRefForPayload("recipe:"+strings.TrimSpace(recipeID), recipes.RecipeContractPayload(runtimeRecipe))
	if err != nil {
		return err
	}
	if err := requireMatchingArtifactRef(want, recipeRef, "runtime snapshot recipe"); err != nil {
		return err
	}
	actual, err := contracts.CanonicalJSONBytes(recipes.RecipeContractPayload(persistedRecipe))
	if err != nil {
		return err
	}
	expected, err := contracts.CanonicalJSONBytes(recipes.RecipeContractPayload(runtimeRecipe))
	if err != nil {
		return err
	}
	if string(actual) != string(expected) {
		return persistenceIntegrityError("Persisted recipe artifact differs from the persisted runtime snapshot.", nil)
	}
	return nil
}

func loadRootRecoveryIntegration(st *store.Store, plan map[string]any, refs map[string]map[string]any) (*integration.Bundle, *integration.SelectedContract, error) {
	contractID := strings.TrimSpace(stringFromAny(plan["integration_contract_id"]))
	if contractID == "" {
		if refs["integration_bundle_ref"] != nil || refs["integration_contract_ref"] != nil || plan["integration_bundle_ref"] != nil || plan["integration_contract_ref"] != nil {
			return nil, nil, persistenceIntegrityError("Contractless root plan unexpectedly references integration artifacts.", nil)
		}
		return nil, nil, nil
	}
	if refs["integration_bundle_ref"] == nil || refs["integration_contract_ref"] == nil {
		return nil, nil, persistenceIntegrityError("Integration-bound root recovery is missing bundle or contract artifacts.", nil)
	}
	bundleArtifact, err := loadRootRecoveryArtifactRef(st, refs["integration_bundle_ref"], contracts.RootArtifactKindIntegrationBundle, 0)
	if err != nil {
		return nil, nil, err
	}
	bundleMap, ok := bundleArtifact.payload["bundle"].(map[string]any)
	if !ok {
		return nil, nil, persistenceIntegrityError("Persisted integration bundle payload is missing its bundle.", nil)
	}
	bundleBytes, err := contracts.CanonicalJSONBytes(bundleMap)
	if err != nil {
		return nil, nil, err
	}
	bundle, err := integration.DecodeBundleBytes(bundleBytes)
	if err != nil {
		return nil, nil, err
	}
	if bundle.ID() != strings.TrimSpace(stringFromAny(bundleArtifact.payload["bundle_id"])) || bundle.Digest() != strings.TrimSpace(stringFromAny(bundleArtifact.payload["bundle_digest"])) {
		return nil, nil, persistenceIntegrityError("Persisted integration bundle identity or digest is invalid.", nil)
	}
	if err := requireMatchingArtifactRef(refs["integration_bundle_ref"], mapFromAny(plan["integration_bundle_ref"]), "integration bundle"); err != nil {
		return nil, nil, err
	}
	selected, err := selectedContractForRootPlan(bundle, plan)
	if err != nil {
		return nil, nil, err
	}
	contractArtifact, err := loadRootRecoveryArtifactRef(st, refs["integration_contract_ref"], contracts.RootArtifactKindIntegrationContract, 0)
	if err != nil {
		return nil, nil, err
	}
	if selected == nil || selected.ID() != contractID || selected.ID() != strings.TrimSpace(stringFromAny(contractArtifact.payload["contract_id"])) || selected.Digest() != strings.TrimSpace(stringFromAny(contractArtifact.payload["contract_digest"])) {
		return nil, nil, persistenceIntegrityError("Persisted integration contract identity or digest is invalid.", nil)
	}
	wantContract, err := contracts.CanonicalJSONBytes(selected.ToMap())
	if err != nil {
		return nil, nil, err
	}
	gotContract, err := contracts.CanonicalJSONBytes(contractArtifact.payload["contract"])
	if err != nil || string(wantContract) != string(gotContract) {
		return nil, nil, persistenceIntegrityError("Persisted selected integration contract differs from its bundle snapshot.", nil)
	}
	if err := requireMatchingArtifactRef(refs["integration_contract_ref"], mapFromAny(plan["integration_contract_ref"]), "integration contract"); err != nil {
		return nil, nil, err
	}
	return bundle, selected, nil
}

func loadPersistedRootLaunchContexts(st *store.Store, refs []any) ([]LaunchContext, error) {
	contexts := make([]LaunchContext, 0, len(refs))
	for index, raw := range refs {
		metadata, ok := raw.(map[string]any)
		if !ok {
			return nil, persistenceIntegrityError("Persisted launch context metadata is invalid.", map[string]any{"index": index})
		}
		ref, ok := metadata["artifact_ref"].(map[string]any)
		if !ok {
			return nil, persistenceIntegrityError("Persisted launch context is missing its artifact ref.", map[string]any{"index": index})
		}
		payload, err := st.LoadArtifact(ref)
		if err != nil {
			return nil, err
		}
		content, ok := payload["content"].(string)
		if !ok || strings.TrimSpace(stringFromAny(payload["bundle_kind"])) != "context" || payload["embedded"] != true {
			return nil, persistenceIntegrityError("Persisted launch context artifact is invalid.", map[string]any{"index": index})
		}
		digest := promptInputDigest([]byte(content))
		if digest != strings.TrimSpace(stringFromAny(payload["digest"])) || len([]byte(content)) != intFromAny(payload["size_bytes"], -1) {
			return nil, persistenceIntegrityError("Persisted launch context content digest or size is invalid.", map[string]any{"index": index})
		}
		for _, key := range []string{"label", "display_name", "source_path", "digest", "size_bytes"} {
			want, _ := contracts.CanonicalJSONBytes(metadata[key])
			got, _ := contracts.CanonicalJSONBytes(payload[key])
			if string(want) != string(got) {
				return nil, persistenceIntegrityError("Persisted launch context metadata differs from its artifact.", map[string]any{"index": index, "field": key})
			}
		}
		contexts = append(contexts, LaunchContext{
			Kind:        "context",
			Label:       stringFromAny(payload["label"]),
			DisplayName: stringFromAny(payload["display_name"]),
			SourcePath:  stringFromAny(payload["source_path"]),
			Digest:      digest,
			SizeBytes:   int64(len([]byte(content))),
			Embedded:    true,
			Content:     content,
		})
	}
	return contexts, nil
}

func promptInputDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func loadPersistedRootReducerAttempts(st *store.Store, meta model.SessionMeta) ([]persistedRootReducerAttempt, error) {
	refs := map[int]map[string]any{}
	for _, raw := range meta.Slice("reducer_attempt_refs") {
		ref, _ := raw.(map[string]any)
		ordinal, ok := rootArtifactRefOrdinal(ref, contracts.RootArtifactKindReducerAttempt)
		if ok {
			refs[ordinal] = ref
		}
	}
	if indexEntries, ok := st.ArtifactIndex()["entries"].([]any); ok {
		for _, raw := range indexEntries {
			entry, _ := raw.(map[string]any)
			ref, _ := entry["ref"].(map[string]any)
			ordinal, ok := rootArtifactRefOrdinal(ref, contracts.RootArtifactKindReducerAttempt)
			if ok {
				refs[ordinal] = ref
			}
		}
	}
	graphPayload := st.LoadGraph()
	if artifacts, ok := graphPayload["artifacts"].(map[string]any); ok {
		prefix := contracts.RootArtifactKindReducerAttempt + "/"
		for key, raw := range artifacts {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			entry, _ := raw.(map[string]any)
			ref, _ := entry["ref"].(map[string]any)
			ordinal, ok := rootArtifactRefOrdinal(ref, contracts.RootArtifactKindReducerAttempt)
			if ok {
				refs[ordinal] = ref
			}
		}
	}
	ordinals := make([]int, 0, len(refs))
	for ordinal := range refs {
		ordinals = append(ordinals, ordinal)
	}
	sort.Ints(ordinals)
	attempts := make([]persistedRootReducerAttempt, 0, len(ordinals))
	for _, ordinal := range ordinals {
		artifact, err := loadRootRecoveryArtifactRef(st, refs[ordinal], contracts.RootArtifactKindReducerAttempt, ordinal)
		if err != nil {
			return nil, err
		}
		status := strings.TrimSpace(stringFromAny(artifact.payload["status"]))
		if intFromAny(artifact.payload["ordinal"], 0) != ordinal || (status != "completed" && status != "failed") {
			return nil, persistenceIntegrityError("Persisted root reducer attempt is invalid.", map[string]any{"ordinal": ordinal})
		}
		if status == "completed" && strings.TrimSpace(stringFromAny(artifact.payload["content"])) == "" {
			return nil, persistenceIntegrityError("Completed root reducer attempt has no durable content.", map[string]any{"ordinal": ordinal})
		}
		attempts = append(attempts, persistedRootReducerAttempt{ordinal: ordinal, ref: artifact.ref, payload: artifact.payload})
	}
	return attempts, nil
}

func rootArtifactRefOrdinal(ref map[string]any, kind string) (int, bool) {
	validated, err := contracts.ValidateArtifactRef(ref)
	if err != nil {
		return 0, false
	}
	id := strings.TrimSpace(stringFromAny(validated["id"]))
	artifactID := strings.TrimPrefix(id, kind+":")
	if artifactID == id {
		return 0, false
	}
	ordinal, err := contracts.RootArtifactOrdinalFromID(kind, artifactID)
	return ordinal, err == nil
}

func latestCompletedReducerAttempt(attempts []persistedRootReducerAttempt) *persistedRootReducerAttempt {
	for index := len(attempts) - 1; index >= 0; index-- {
		if strings.TrimSpace(stringFromAny(attempts[index].payload["status"])) == "completed" {
			attempt := attempts[index]
			return &attempt
		}
	}
	return nil
}

func rootCandidateFromArtifact(artifact *persistedRootRecoveryArtifact, plan map[string]any, transcript model.Transcript, attempts []persistedRootReducerAttempt) (*rootCandidate, error) {
	if artifact == nil {
		return nil, nil
	}
	source := strings.TrimSpace(stringFromAny(artifact.payload["result_source"]))
	if source != strings.TrimSpace(stringFromAny(plan["result_source"])) {
		return nil, persistenceIntegrityError("Persisted root candidate result source differs from the root plan.", nil)
	}
	content, ok := artifact.payload["content"].(string)
	if !ok || len([]byte(content)) != intFromAny(artifact.payload["content_bytes"], -1) {
		return nil, persistenceIntegrityError("Persisted root candidate content is invalid.", nil)
	}
	candidate := &rootCandidate{content: content, source: source, rawResultRef: cloneMap(artifact.ref)}
	switch source {
	case integration.ResultSourceLastTurn:
		candidate.participantTurn = intFromAny(artifact.payload["participant_turn"], 0)
		if candidate.participantTurn != transcript.Len() || candidate.participantTurn < 1 {
			return nil, persistenceIntegrityError("Persisted last-turn candidate does not reference the final participant turn.", nil)
		}
	case integration.ResultSourceReducer:
		ref, ok := artifact.payload["reducer_attempt_ref"].(map[string]any)
		if !ok {
			return nil, persistenceIntegrityError("Persisted reducer candidate is missing its reducer attempt ref.", nil)
		}
		matched := false
		for _, attempt := range attempts {
			if requireMatchingArtifactRef(ref, attempt.ref, "reducer attempt") == nil && strings.TrimSpace(stringFromAny(attempt.payload["status"])) == "completed" && stringFromAny(attempt.payload["content"]) == content {
				matched = true
				candidate.reducerAttempt = cloneMap(attempt.ref)
				break
			}
		}
		if !matched {
			return nil, persistenceIntegrityError("Persisted reducer candidate does not match a completed reducer attempt.", nil)
		}
	default:
		return nil, persistenceIntegrityError("Persisted root candidate has an unsupported result source.", nil)
	}
	return candidate, nil
}

func validateRecoveredResultArtifacts(
	checkpoints map[int]*persistedRootRecoveryArtifact,
	candidate *rootCandidate,
	validation *persistedRootRecoveryArtifact,
	canonical *persistedRootRecoveryArtifact,
	selected *integration.SelectedContract,
	evaluator *integration.AssertionEvaluator,
) error {
	if checkpoint := checkpoints[3]; checkpoint != nil {
		if candidate == nil {
			return persistenceIntegrityError("Candidate checkpoint exists without a durable raw candidate.", nil)
		}
		expectedPhase := rootCandidateCompleteStatus
		if candidate.source == integration.ResultSourceReducer {
			expectedPhase = rootReducerCompleteStatus
		}
		if strings.TrimSpace(stringFromAny(checkpoint.payload["phase"])) != expectedPhase || strings.TrimSpace(stringFromAny(checkpoint.payload["status"])) != "completed" {
			return persistenceIntegrityError("Persisted root candidate checkpoint phase or status is invalid.", nil)
		}
		if err := requireMatchingArtifactRef(mapFromAny(checkpoint.payload["raw_result_ref"]), candidate.rawResultRef, "raw result"); err != nil {
			return err
		}
	}
	if validation == nil {
		if checkpoints[4] != nil {
			return persistenceIntegrityError("Validation checkpoint exists without a durable validation artifact.", nil)
		}
		if canonical != nil {
			return validateRecoveredCanonical(candidate, canonical, selected, evaluator)
		}
		return nil
	}
	if candidate == nil {
		return persistenceIntegrityError("Durable result validation exists without a durable raw candidate.", nil)
	}
	status := strings.TrimSpace(stringFromAny(validation.payload["status"]))
	if status != "validated" && status != "not_required" && status != "failed" {
		return persistenceIntegrityError("Persisted root result validation status is invalid.", map[string]any{"status": status})
	}
	if err := requireMatchingArtifactRef(mapFromAny(validation.payload["raw_result_ref"]), candidate.rawResultRef, "result validation raw result"); err != nil {
		return err
	}
	if checkpoint := checkpoints[4]; checkpoint != nil {
		expectedCheckpointStatus := "completed"
		if status == "failed" {
			expectedCheckpointStatus = "failed"
		}
		if strings.TrimSpace(stringFromAny(checkpoint.payload["phase"])) != rootValidationCompletePhase || strings.TrimSpace(stringFromAny(checkpoint.payload["status"])) != expectedCheckpointStatus {
			return persistenceIntegrityError("Persisted root validation checkpoint phase or status is invalid.", nil)
		}
		if err := requireMatchingArtifactRef(mapFromAny(checkpoint.payload["result_validation_ref"]), validation.ref, "result validation"); err != nil {
			return err
		}
	}
	switch status {
	case "validated":
		if selected == nil || canonical == nil {
			return persistenceIntegrityError("Validated root result is missing its integration contract or canonical result.", nil)
		}
		return validateRecoveredCanonical(candidate, canonical, selected, evaluator)
	case "not_required":
		if selected != nil || canonical != nil {
			return persistenceIntegrityError("Contractless result validation unexpectedly has a canonical result or contract.", nil)
		}
	case "failed":
		if canonical != nil {
			return persistenceIntegrityError("Failed result validation unexpectedly has a canonical result.", nil)
		}
	}
	if checkpoint := checkpoints[5]; checkpoint != nil {
		if strings.TrimSpace(stringFromAny(checkpoint.payload["phase"])) != "cleanup_complete" || strings.TrimSpace(stringFromAny(checkpoint.payload["status"])) != "completed" || strings.TrimSpace(stringFromAny(checkpoint.payload["cleanup_status"])) != "completed" {
			return persistenceIntegrityError("Persisted root cleanup checkpoint phase or status is invalid.", nil)
		}
	}
	return nil
}

func validateRecoveredCanonical(candidate *rootCandidate, canonical *persistedRootRecoveryArtifact, selected *integration.SelectedContract, evaluator *integration.AssertionEvaluator) error {
	if candidate == nil || canonical == nil || selected == nil || evaluator == nil {
		return persistenceIntegrityError("Persisted canonical root result is missing required recovery state.", nil)
	}
	if err := requireMatchingArtifactRef(mapFromAny(canonical.payload["raw_result_ref"]), candidate.rawResultRef, "canonical raw result"); err != nil {
		return err
	}
	value, err := contracts.DecodeStrictJSONBytes([]byte(candidate.content))
	if err != nil {
		return persistenceIntegrityError("Persisted canonical result has an invalid raw JSON candidate.", map[string]any{"cause": err.Error()})
	}
	contract := selected.Contract()
	if contract == nil || contract.Result.Schema == nil {
		return persistenceIntegrityError("Persisted integration result schema is invalid during recovery.", nil)
	}
	if err := contract.Result.Schema.Validate(value); err != nil {
		return persistenceIntegrityError("Persisted raw candidate no longer satisfies its result schema.", map[string]any{"cause": err.Error()})
	}
	if err := evaluator.Validate(value); err != nil {
		return persistenceIntegrityError("Persisted raw candidate no longer satisfies its result assertions.", map[string]any{"cause": err.Error()})
	}
	bytes, err := contracts.CanonicalJSONBytes(value)
	if err != nil {
		return err
	}
	if string(bytes) != stringFromAny(canonical.payload["canonical_json"]) {
		return persistenceIntegrityError("Persisted canonical JSON differs from the raw candidate.", nil)
	}
	persistedValue, err := contracts.CanonicalJSONBytes(canonical.payload["value"])
	if err != nil || string(persistedValue) != string(bytes) {
		return persistenceIntegrityError("Persisted canonical value differs from the raw candidate.", nil)
	}
	return nil
}

func validateRootRecoveryMeta(meta model.SessionMeta, plan map[string]any, refs map[string]map[string]any) error {
	if strings.TrimSpace(meta.String("task")) == "" {
		return persistenceIntegrityError("Root recovery session metadata is missing its persisted task.", nil)
	}
	checks := []struct {
		field string
		value any
	}{
		{"participant_turns", plan["participant_turns"]},
		{"participant_schedule", plan["participant_schedule"]},
		{"result_source", plan["result_source"]},
		{"mode", plan["mode"]},
		{"lifecycle", plan["lifecycle"]},
	}
	for _, check := range checks {
		left, _ := contracts.CanonicalJSONBytes(meta.Get(check.field))
		right, _ := contracts.CanonicalJSONBytes(check.value)
		if string(left) != string(right) {
			return persistenceIntegrityError("Root recovery metadata differs from the persisted root plan.", map[string]any{"field": check.field})
		}
	}
	for _, key := range []string{"recipe_ref", "root_recipe_plan_ref", "runtime_config_ref", "integration_bundle_ref", "integration_contract_ref", "named_input_manifest_ref"} {
		metaRef, err := optionalArtifactRef(meta.Get(key), key)
		if err != nil {
			return err
		}
		if metaRef != nil && requireOptionalMatchingArtifactRef(metaRef, refs[key], "session "+key) != nil {
			return persistenceIntegrityError("Root recovery metadata ref differs from the participant checkpoint.", map[string]any{"field": key})
		}
	}
	return nil
}

func recoveredRootTimeouts(meta model.SessionMeta, opts ResumeOptions) (int, int) {
	timeout := positiveOrDefault(meta.Int("timeout_seconds", 0), defaultTimeoutSeconds)
	stall := meta.Int("stall_timeout_seconds", defaultTimeoutSeconds/2)
	if opts.ExplicitFields == nil {
		if opts.TimeoutSeconds > 0 {
			timeout = opts.TimeoutSeconds
		}
		if opts.StallTimeoutSeconds != 0 {
			stall = opts.StallTimeoutSeconds
		}
	} else {
		if opts.ExplicitFields["timeout"] {
			timeout = positiveOrDefault(opts.TimeoutSeconds, defaultTimeoutSeconds)
		}
		if opts.ExplicitFields["stall-timeout"] {
			stall = opts.StallTimeoutSeconds
		}
	}
	if stall < 0 {
		stall = 0
	}
	return timeout, stall
}

func artifactRefOrNil(artifact *persistedRootRecoveryArtifact) map[string]any {
	if artifact == nil {
		return nil
	}
	return artifact.ref
}

func persistedRootValidationError(payload map[string]any) error {
	message := strings.TrimSpace(stringFromAny(payload["error"]))
	if message == "" {
		message = "Root recipe result validation failed."
	}
	diagnostics := []contracts.Diagnostic{}
	for _, raw := range asSlice(payload["diagnostics"]) {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		details, _ := item["details"].(map[string]any)
		diagnostics = append(diagnostics, contracts.NewDiagnostic(
			stringFromAny(item["code"]),
			stringFromAny(item["phase"]),
			stringFromAny(item["path"]),
			stringFromAny(item["message"]),
			details,
		))
	}
	if len(diagnostics) == 0 {
		diagnostics = append(diagnostics, contracts.NewDiagnostic("result_validation_failed", contracts.DiagnosticPhaseResultValidation, "", message, nil))
	}
	return contracts.NewDiagnosticError(message, diagnostics...)
}
