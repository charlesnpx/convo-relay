package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/namedinputs"
	"github.com/charlesnpx/convo-relay/internal/readiness"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const (
	diagnosticCodeRecipeRequired       = "root_recipe_required"
	diagnosticCodeTaskRequired         = "root_recipe_task_required"
	diagnosticCodeRecipeUnknown        = "root_recipe_unknown"
	diagnosticCodePolicyConflict       = "root_recipe_policy_conflict"
	diagnosticCodeBackendUnavailable   = "root_recipe_backend_unavailable"
	diagnosticCodeBackendCheckMissing  = "root_recipe_backend_check_missing"
	diagnosticCodeSessionPathInvalid   = "root_recipe_session_path_invalid"
	diagnosticCodePersistenceIntegrity = "root_recipe_persistence_integrity"
	diagnosticCodeArtifactIDInvalid    = "root_recipe_artifact_id_invalid"
	diagnosticCodeTaskPlanInvalid      = "root_recipe_task_plan_invalid"
	diagnosticCodeRuntimeConfigInvalid = "root_recipe_runtime_config_invalid"
)

// rootCheckpointAfterSave is a test-only interruption seam. The checkpoint
// artifact and its index/graph entry are already durable when it is invoked.
var rootCheckpointAfterSave func(int) error

// RecipeOptions describes a direct root-recipe run. Unlike Options, it does
// not accept caller-selected participant, facilitator, mode, round, or dynamic
// structure; those values come only from the compiled root plan.
type RecipeOptions struct {
	SessionDir            string
	SessionID             string
	RelayHome             string
	Task                  string
	RecipeID              string
	ContextFiles          []string
	SkillFiles            []string
	TransientSources      []recipes.TransientRecipeSource
	IntegrationBundle     *integration.Bundle
	IntegrationBundlePath string
	InputBindings         []string
	WorkspaceIsolation    string
	WorkspaceExplicit     bool
	AllowDirtySource      bool
	WarningCallback       RecipeWarningCallback
	SettingsPath          string
	LaunchCWD             string
	TimeoutSeconds        int
	StallTimeoutSeconds   int
	InvestigationMode     string
	LaunchPlan            any
	TaskPlanExplicit      bool
	SkillExplicit         bool
	RuntimeConfig         recipes.RuntimeConfig
	ReadinessOptions      readiness.Options
	ReadinessCheck        func(context.Context, []string, readiness.Options) ([]readiness.Record, error)

	// compileRecipe is an internal test seam that proves the caller-selected
	// target without creating a second compiler API.
	compileRecipe func(map[string]any, map[string]map[string]any, map[string]map[string]any, recipes.CompileTarget, recipes.CompileOptions) (map[string]any, error)

	// backendFactory is an internal test seam. Production root execution uses
	// the same low-level backend factory as ordinary relay execution.
	backendFactory rootBackendFactory

	retainedInputVerifier            rootRetainedInputVerifier
	retainedInputVerificationTimeout time.Duration
}

type recipePreflight struct {
	options            RecipeOptions
	sessionDir         string
	sessionID          string
	sessionPathSource  string
	launchCWD          string
	runtimeConfig      recipes.RuntimeConfig
	runtimeSnapshot    map[string]any
	transientFiles     []recipes.TransientRecipeFile
	transientRecipes   []preparedTransientRecipe
	recipe             map[string]any
	rootPlan           map[string]any
	bundle             *integration.Bundle
	selectedContract   *integration.SelectedContract
	assertionEvaluator *integration.AssertionEvaluator
	preparedInputs     *namedinputs.Prepared
	launchContexts     []LaunchContext
	launchSkills       []InputBundle
	promptPolicy       PromptPolicy
	backendReadiness   []readiness.Record
	workspace          *workspace.Snapshot
	warning            *RecipeWarning
}

type preparedTransientRecipe struct {
	recipeID    string
	payload     map[string]any
	expectedRef map[string]any
	selected    bool
}

type persistedRecipeRun struct {
	st                    *store.Store
	runtimeConfigRef      map[string]any
	transientRecipeRefs   []any
	transientContractRefs []any
	recipeRef             map[string]any
	rootPlanRef           map[string]any
	bundleRef             map[string]any
	contractRef           map[string]any
	launchContextRefs     []any
	launchSkillRefs       []any
	inputManifestRef      map[string]any
	retainedInputRef      map[string]any
	providerInputs        map[string]any
	workspaceArtifact     map[string]any
	workspaceRef          map[string]any
	executionCWD          string
	checkpointRef         map[string]any
}

// RunRecipe is the root-recipe entry point. It completes every pure preflight
// before creating a session, then durably records the direct root execution
// boundary. Participant execution is deliberately separate from ordinary Run.
func RunRecipe(ctx context.Context, opts RecipeOptions) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	preflight, err := preflightRecipe(ctx, opts)
	if err != nil {
		return nil, err
	}
	return startRecipeRun(ctx, preflight)
}

func preflightRecipe(ctx context.Context, opts RecipeOptions) (*recipePreflight, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(opts.RecipeID) == "" {
		return nil, rootRecipeDiagnostic(diagnosticCodeRecipeRequired, contracts.DiagnosticPhasePolicy, "/recipe", "--recipe is required for a root recipe run.", nil)
	}
	if strings.TrimSpace(opts.Task) == "" {
		return nil, rootRecipeDiagnostic(diagnosticCodeTaskRequired, contracts.DiagnosticPhasePolicy, "/task", "--task or positional task text is required for a root recipe run.", nil)
	}
	if opts.IntegrationBundle != nil && strings.TrimSpace(opts.IntegrationBundlePath) != "" {
		return nil, rootRecipeDiagnostic(diagnosticCodePolicyConflict, contracts.DiagnosticPhasePolicy, "/integration_bundle", "Provide either an integration bundle value or an integration bundle path, not both.", nil)
	}

	launchCWD, err := resolveRecipeLaunchCWD(opts.LaunchCWD)
	if err != nil {
		return nil, rootRecipeDiagnostic(diagnosticCodePolicyConflict, contracts.DiagnosticPhasePreflight, "/launch_cwd", "Launch CWD must resolve to a readable directory.", map[string]any{"cause": err.Error()})
	}
	settingsPath := resolveRecipeSourcePath(launchCWD, opts.SettingsPath)
	integrationBundlePath := resolveRecipeSourcePath(launchCWD, opts.IntegrationBundlePath)

	launchContexts, err := preflightLaunchContextsAt(opts.ContextFiles, launchCWD)
	if err != nil {
		return nil, err
	}
	launchSkills, err := preflightSkillInputsAt(opts.SkillFiles, launchCWD)
	if err != nil {
		return nil, err
	}
	runtimeConfig, transientFiles, err := loadEffectiveRuntimeConfig(settingsPath, opts.RuntimeConfig, nil, nil, opts.TransientSources)
	if err != nil {
		return nil, err
	}
	runtimeSnapshot, err := prepareRuntimeConfigSnapshot(runtimeConfig)
	if err != nil {
		return nil, rootRecipeDiagnostic(
			diagnosticCodeRuntimeConfigInvalid,
			contracts.DiagnosticPhasePreflight,
			"/runtime_config",
			"The effective runtime configuration must be persistable JSON.",
			map[string]any{"cause": err.Error()},
		)
	}
	recipeID := strings.TrimSpace(opts.RecipeID)
	recipe, exists := runtimeConfig.RelayRecipes[recipeID]
	if !exists || recipe == nil {
		return nil, rootRecipeDiagnostic(diagnosticCodeRecipeUnknown, contracts.DiagnosticPhasePreflight, "/recipe", "The requested root recipe is not present in the effective runtime configuration.", map[string]any{"recipe_id": recipeID})
	}
	if err := validateRootRecipeArtifactLocations(recipeID, transientFiles); err != nil {
		return nil, err
	}

	bundle := opts.IntegrationBundle
	if bundle == nil && strings.TrimSpace(integrationBundlePath) != "" {
		bundle, err = integration.LoadBundleFile(integrationBundlePath, runtimeConfig.EffectiveLimits().IntegrationBundleMaxBytes)
		if err != nil {
			return nil, err
		}
	}
	compile := opts.compileRecipe
	if compile == nil {
		compile = recipes.CompileRecipe
	}
	rootPlan, err := compile(
		recipe,
		runtimeConfig.BackendProfiles,
		runtimeConfig.RelayRecipes,
		recipes.CompileTargetRoot,
		recipes.CompileOptions{
			CompositionPath:    "root",
			ValidateExecutable: true,
			TransientSources:   transientFiles,
			IntegrationBundle:  bundle,
		},
	)
	if err != nil {
		return nil, err
	}
	if _, err := contracts.ValidateRootArtifact(rootPlan, contracts.RootArtifactKindRootRecipePlan); err != nil {
		return nil, err
	}
	transientRecipes, err := prepareRootTransientRecipes(transientFiles, runtimeConfig, recipeID, rootPlan)
	if err != nil {
		return nil, err
	}

	selectedContract, err := selectedContractForRootPlan(bundle, rootPlan)
	if err != nil {
		return nil, err
	}
	if selectedContract != nil && (opts.SkillExplicit || len(opts.SkillFiles) > 0) {
		return nil, rootRecipeDiagnostic(diagnosticCodePolicyConflict, contracts.DiagnosticPhasePolicy, "/skill", "Explicit skill inputs are not represented by an integration-bound root plan.", nil)
	}
	if selectedContract != nil && (opts.TaskPlanExplicit || opts.LaunchPlan != nil) {
		return nil, rootRecipeDiagnostic(diagnosticCodePolicyConflict, contracts.DiagnosticPhasePolicy, "/task_plan", "An explicit task plan is not represented by an integration-bound root plan.", nil)
	}
	normalizedLaunchPlan, err := normalizeRootRecipeLaunchPlan(opts.LaunchPlan)
	if err != nil {
		return nil, err
	}
	opts.LaunchPlan = normalizedLaunchPlan
	preparedInputs, err := namedinputs.Prepare(namedinputs.Options{
		Contract:                selectedContract,
		Bindings:                append([]string{}, opts.InputBindings...),
		SourceAnchor:            launchCWD,
		PositionalContextCount:  len(launchContexts),
		Context:                 ctx,
		NamedInputMaxBytes:      runtimeConfig.EffectiveLimits().NamedInputMaxBytes,
		NamedInputTotalMaxBytes: runtimeConfig.EffectiveLimits().NamedInputTotalMaxBytes,
	})
	if err != nil {
		return nil, err
	}
	declaresNamedInputs := false
	if selectedContract != nil {
		contract := selectedContract.Contract()
		declaresNamedInputs = contract != nil && len(contract.Inputs) > 0
	}
	promptPolicy, err := BuildRootPromptPolicy(
		opts.InvestigationMode,
		len(launchContexts) > 0,
		declaresNamedInputs,
		preparedInputNames(preparedInputs),
	)
	if err != nil {
		return nil, err
	}
	var assertionEvaluator *integration.AssertionEvaluator
	if selectedContract != nil {
		assertionEvaluator, err = integration.PrepareAssertionEvaluator(selectedContract, preparedInputs.AssertionInputs())
		if err != nil {
			return nil, err
		}
	}

	backendNames, err := readiness.ResolveBackendClosure(recipe, runtimeConfig.BackendProfiles, runtimeConfig.RelayRecipes, readiness.ClosureOptions{
		IncludeReducer:        stringFromAny(rootPlan["result_source"]) == integration.ResultSourceReducer,
		IncludeNestedReducers: true,
	})
	if err != nil {
		return nil, err
	}
	check := opts.ReadinessCheck
	if check == nil {
		check = readiness.Check
	}
	readinessOptions := opts.ReadinessOptions
	readinessOptions.ProbeAuth = false
	backendReadiness, err := check(ctx, backendNames, readinessOptions)
	if err != nil {
		return nil, fmt.Errorf("check root recipe backend readiness: %w", err)
	}
	if err := validateRootBackendReadiness(backendNames, backendReadiness); err != nil {
		return nil, err
	}

	sessionDir, sessionID, sessionPathSource, err := resolveNewRecipeSession(opts)
	if err != nil {
		return nil, err
	}
	workspaceSnapshot, err := workspace.Preflight(ctx, workspace.Options{
		LaunchCWD:         launchCWD,
		SessionDir:        sessionDir,
		SessionPathSource: sessionPathSource,
		MinimumPolicy:     stringFromAny(rootPlan["workspace_isolation_minimum"]),
		RequestedPolicy:   opts.WorkspaceIsolation,
		RequestedExplicit: opts.WorkspaceExplicit,
		AllowDirtySource:  opts.AllowDirtySource,
		InventoryMaxFiles: runtimeConfig.EffectiveLimits().RepositoryInventoryMaxFiles,
		InventoryMaxBytes: runtimeConfig.EffectiveLimits().RepositoryInventoryMaxBytes,
	})
	if err != nil {
		return nil, err
	}
	sessionDir = workspaceSnapshot.SessionDir()
	sessionID = sessionIDFromDir(sessionDir)
	if err := validateNewRecipeSessionDestination(sessionDir); err != nil {
		return nil, err
	}
	var warning *RecipeWarning
	if changes := workspaceSnapshot.SourceChanges(); workspaceSnapshot.AllowDirtySource() && changes.Dirty() {
		value := RecipeWarning{
			Code:                       RecipeWarningCodeDirtySourceCommittedHead,
			Message:                    "Dirty source changes are excluded; isolated execution uses committed HEAD.",
			StagedChanges:              changes.Staged,
			UnstagedChanges:            changes.Unstaged,
			UntrackedChanges:           changes.Untracked,
			WorkspaceContentSource:     workspace.WorkspaceContentSourceCommittedHead,
			WorkingTreeChangesIncluded: false,
		}
		warning = &value
	}

	opts.RecipeID = recipeID
	opts.SettingsPath = settingsPath
	opts.IntegrationBundlePath = integrationBundlePath
	opts.LaunchCWD = launchCWD
	opts.TimeoutSeconds = positiveOrDefault(opts.TimeoutSeconds, defaultTimeoutSeconds)
	opts.StallTimeoutSeconds = positiveOrDefault(opts.StallTimeoutSeconds, defaultTimeoutSeconds/2)
	return &recipePreflight{
		options:            opts,
		sessionDir:         sessionDir,
		sessionID:          sessionID,
		sessionPathSource:  sessionPathSource,
		launchCWD:          launchCWD,
		runtimeConfig:      runtimeConfig,
		runtimeSnapshot:    runtimeSnapshot,
		transientFiles:     transientFiles,
		transientRecipes:   transientRecipes,
		recipe:             contracts.Materialize(recipe).(map[string]any),
		rootPlan:           contracts.Materialize(rootPlan).(map[string]any),
		bundle:             bundle,
		selectedContract:   selectedContract,
		assertionEvaluator: assertionEvaluator,
		preparedInputs:     preparedInputs,
		launchContexts:     launchContexts,
		launchSkills:       launchSkills,
		promptPolicy:       promptPolicy,
		backendReadiness:   append([]readiness.Record{}, backendReadiness...),
		workspace:          workspaceSnapshot,
		warning:            warning,
	}, nil
}

func validateRootRecipeArtifactLocations(recipeID string, transientFiles []recipes.TransientRecipeFile) error {
	ids := append([]string{recipeID}, effectiveTransientRecipeIDs(transientFiles)...)
	for _, artifactID := range ids {
		if err := store.ValidateArtifactLocation("recipes", artifactID); err != nil {
			return rootRecipeDiagnostic(
				diagnosticCodeArtifactIDInvalid,
				contracts.DiagnosticPhasePreflight,
				"/recipe",
				"Recipe IDs must resolve inside the session recipe artifact directory.",
				map[string]any{"recipe_id": artifactID, "cause": err.Error()},
			)
		}
	}
	return nil
}

func normalizeRootRecipeLaunchPlan(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	canonical, err := contracts.CanonicalJSONBytes(value)
	if err != nil {
		return nil, rootRecipeDiagnostic(
			diagnosticCodeTaskPlanInvalid,
			contracts.DiagnosticPhasePreflight,
			"/task_plan",
			"Task plan data must be persistable JSON.",
			map[string]any{"cause": err.Error()},
		)
	}
	normalized, err := contracts.DecodeStrictJSONBytes(canonical)
	if err != nil {
		return nil, rootRecipeDiagnostic(
			diagnosticCodeTaskPlanInvalid,
			contracts.DiagnosticPhasePreflight,
			"/task_plan",
			"Task plan data must be persistable JSON.",
			map[string]any{"cause": err.Error()},
		)
	}
	return normalized, nil
}

func startRecipeRun(ctx context.Context, preflight *recipePreflight) (map[string]any, error) {
	if preflight == nil {
		return nil, errors.New("root recipe preflight is required")
	}
	if preflight.warning != nil {
		deliverRecipeWarning(preflight.options.WarningCallback, *preflight.warning)
	}
	transaction, err := beginRootInitialization(preflight)
	if err != nil {
		return nil, err
	}
	persisted, err := persistRecipePreflight(ctx, preflight, transaction)
	if err != nil {
		return nil, transaction.Fail(err)
	}
	meta, err := rootRecipeMeta(preflight, persisted)
	if err != nil {
		return nil, transaction.Fail(err)
	}
	transcript := model.EmptyTranscript()
	if err := persisted.st.SaveTranscript(transcript); err != nil {
		return nil, transaction.Fail(err)
	}
	if err := runRootInitializationAfterMutation("transcript_persisted"); err != nil {
		return nil, transaction.Fail(err)
	}
	if err := transaction.advance("transcript_persisted"); err != nil {
		return nil, transaction.Fail(err)
	}
	if _, err := persisted.st.AppendSessionEventV1("node_started", graph.RootNodeID, "Root recipe execution is ready", rootRecipeStartEvent(preflight, persisted), store.EventOptions{}); err != nil {
		return nil, transaction.Fail(err)
	}
	if err := runRootInitializationAfterMutation("start_event_persisted"); err != nil {
		return nil, transaction.Fail(err)
	}
	if err := transaction.advance("start_event_persisted"); err != nil {
		return nil, transaction.Fail(err)
	}
	if _, _, err := graph.RepairAndSaveFromEvents(persisted.st); err != nil {
		return nil, transaction.Fail(err)
	}
	if err := runRootInitializationAfterMutation("graph_persisted"); err != nil {
		return nil, transaction.Fail(err)
	}
	if err := transaction.advance("graph_persisted"); err != nil {
		return nil, transaction.Fail(err)
	}
	meta = meta.
		With("initialization_state", RootInitializationStateReady).
		With("initialization_token", transaction.token).
		With("initialization_phase", "ready").
		With("initialization_ready_at", utcNow())
	if err := transaction.markReady(meta.ToMap()); err != nil {
		return nil, transaction.Fail(err)
	}
	return runRootParticipants(ctx, preflight, persisted, meta, transcript)
}

func persistRecipePreflight(
	ctx context.Context,
	preflight *recipePreflight,
	transaction *rootInitializationTransaction,
) (*persistedRecipeRun, error) {
	if transaction == nil {
		return nil, errors.New("root recipe persistence requires an initialization transaction")
	}
	st := transaction.st
	runtimeConfigRef, err := persistPreparedRuntimeConfigSnapshot(st, preflight.runtimeSnapshot)
	if err != nil {
		return nil, err
	}
	transientRecipeRefs, err := persistTransientRecipeFileRefs(st, preflight.transientFiles)
	if err != nil {
		return nil, err
	}

	recipePayload := recipes.RecipeContractPayload(preflight.recipe)
	expectedRecipeRef, ok := preflight.rootPlan["recipe_ref"].(map[string]any)
	if !ok {
		return nil, persistenceIntegrityError("Compiled root plan is missing its recipe ref.", nil)
	}
	recipeRef, err := st.SaveContractArtifact("recipes", strings.TrimSpace(preflight.options.RecipeID), recipePayload, stringFromAny(expectedRecipeRef["id"]))
	if err != nil {
		return nil, err
	}
	if err := requireMatchingArtifactRef(expectedRecipeRef, recipeRef, "recipe"); err != nil {
		return nil, err
	}
	transientContractRefs, err := persistRootTransientRecipeContractArtifacts(st, preflight.transientRecipes, recipeRef)
	if err != nil {
		return nil, err
	}
	rootPlanRef, err := saveRootArtifact(st, contracts.RootArtifactKindRootRecipePlan, 0, preflight.rootPlan)
	if err != nil {
		return nil, err
	}

	var bundleRef map[string]any
	var contractRef map[string]any
	if preflight.selectedContract != nil {
		bundleArtifact, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindIntegrationBundle, map[string]any{
			"bundle_id":     preflight.bundle.ID(),
			"bundle_digest": preflight.bundle.Digest(),
			"bundle":        preflight.bundle.ToMap(),
		})
		if err != nil {
			return nil, err
		}
		bundleRef, err = saveRootArtifact(st, contracts.RootArtifactKindIntegrationBundle, 0, bundleArtifact)
		if err != nil {
			return nil, err
		}
		if err := requireMatchingArtifactRef(mapFromAny(preflight.rootPlan["integration_bundle_ref"]), bundleRef, "integration bundle"); err != nil {
			return nil, err
		}
		contractArtifact, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindIntegrationContract, map[string]any{
			"contract_id":     preflight.selectedContract.ID(),
			"contract_digest": preflight.selectedContract.Digest(),
			"contract":        preflight.selectedContract.ToMap(),
		})
		if err != nil {
			return nil, err
		}
		contractRef, err = saveRootArtifact(st, contracts.RootArtifactKindIntegrationContract, 0, contractArtifact)
		if err != nil {
			return nil, err
		}
		if err := requireMatchingArtifactRef(mapFromAny(preflight.rootPlan["integration_contract_ref"]), contractRef, "integration contract"); err != nil {
			return nil, err
		}
	}

	launchContextRefs, err := persistLaunchContextRefs(st, preflight.launchContexts)
	if err != nil {
		return nil, err
	}
	launchSkillRefs, err := persistInputBundleRefs(st, "launch", preflight.launchSkills)
	if err != nil {
		return nil, err
	}

	var inputManifestRef map[string]any
	var retainedInputRef map[string]any
	var providerInputs map[string]any
	if preflight.selectedContract != nil {
		persistedInputs, err := namedinputs.Persist(st, preflight.preparedInputs)
		if err != nil {
			return nil, err
		}
		inputManifestRef = persistedInputs.ManifestRef
		preflight.promptPolicy, err = finalizeNamedInputPromptPolicy(
			preflight.promptPolicy,
			persistedInputs.ManifestRef,
			persistedInputs.Manifest,
		)
		if err != nil {
			return nil, err
		}
	}
	materializedWorkspace, err := transaction.materializeWorkspace(ctx, preflight.workspace)
	if err != nil {
		return nil, err
	}
	if err := runRootInitializationAfterMutation("workspace_materialized"); err != nil {
		return nil, err
	}
	if err := transaction.advance("workspace_materialized"); err != nil {
		return nil, err
	}
	if inputManifestRef != nil {
		if err := transaction.beginOwnedScope("retained_input_materialization", filepath.Join("execution", "inputs")); err != nil {
			return nil, err
		}
		retained, materializeErr := namedinputs.MaterializeRetained(
			ctx,
			st,
			inputManifestRef,
			filepath.Join(preflight.sessionDir, "execution", "inputs"),
		)
		err = materializeErr
		if err != nil {
			return nil, errors.Join(err, transaction.abortOwnedScope())
		}
		if err := namedinputs.VerifyRetained(
			ctx,
			st,
			retained.DescriptorRef,
			"initialization_scope",
			namedinputs.IntegrityBoundaryInitialization,
			nil,
		); err != nil {
			return nil, err
		}
		if err := transaction.completeOwnedScope(); err != nil {
			return nil, err
		}
		providerInputs = retained.ProviderInputs
		retainedInputRef = retained.DescriptorRef
		if err := runRootInitializationAfterMutation("inputs_materialized"); err != nil {
			return nil, err
		}
		if err := transaction.advance("inputs_materialized"); err != nil {
			return nil, err
		}
		if err := namedinputs.VerifyRetained(
			ctx,
			st,
			retainedInputRef,
			"initialization",
			namedinputs.IntegrityBoundaryInitialization,
			nil,
		); err != nil {
			return nil, err
		}
	}
	if err := runRootInitializationAfterMutation("retained_inputs_verified"); err != nil {
		return nil, err
	}
	if err := transaction.advance("retained_inputs_verified"); err != nil {
		return nil, err
	}

	persisted := &persistedRecipeRun{
		st:                    st,
		runtimeConfigRef:      runtimeConfigRef,
		transientRecipeRefs:   transientRecipeRefs,
		transientContractRefs: transientContractRefs,
		recipeRef:             recipeRef,
		rootPlanRef:           rootPlanRef,
		bundleRef:             bundleRef,
		contractRef:           contractRef,
		launchContextRefs:     launchContextRefs,
		launchSkillRefs:       launchSkillRefs,
		inputManifestRef:      inputManifestRef,
		retainedInputRef:      retainedInputRef,
		providerInputs:        providerInputs,
		workspaceArtifact:     cloneMap(materializedWorkspace.Artifact),
		workspaceRef:          materializedWorkspace.ArtifactRef,
		executionCWD:          materializedWorkspace.ExecutionCWD,
	}
	checkpoint, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindRootCheckpoint, rootCheckpointFields(preflight, persisted))
	if err != nil {
		return nil, err
	}
	persisted.checkpointRef, err = saveRootArtifact(st, contracts.RootArtifactKindRootCheckpoint, 1, checkpoint)
	if err != nil {
		return nil, err
	}
	if err := runRootInitializationAfterMutation("checkpoint_1_persisted"); err != nil {
		return nil, err
	}
	if err := transaction.advance("checkpoint_1_persisted"); err != nil {
		return nil, err
	}
	return persisted, nil
}

func rootRecipeMeta(preflight *recipePreflight, persisted *persistedRecipeRun) (model.SessionMeta, error) {
	participantTurns := intFromAny(preflight.rootPlan["participant_turns"], 0)
	provenance, err := workspace.ProvenanceProjection(persisted.workspaceArtifact)
	if err != nil {
		return model.EmptySessionMeta(), fmt.Errorf("project execution workspace provenance: %w", err)
	}
	meta := map[string]any{
		"session_id":                         preflight.sessionID,
		"execution_kind":                     "recipe",
		"task":                               preflight.options.Task,
		"initial_prompt":                     preflight.options.Task,
		"title":                              makeTitle(preflight.options.Task),
		"status":                             "ready",
		"recipe_id":                          preflight.options.RecipeID,
		"recipe_ref":                         persisted.recipeRef,
		"root_recipe_plan_ref":               persisted.rootPlanRef,
		"runtime_config_ref":                 persisted.runtimeConfigRef,
		"runtime_config_version":             RuntimeConfigSnapshotVersion,
		"transient_recipe_refs":              persisted.transientRecipeRefs,
		"transient_recipe_contract_refs":     persisted.transientContractRefs,
		"integration_bundle_ref":             nil,
		"integration_contract_ref":           nil,
		"named_input_manifest_ref":           nil,
		"retained_input_materialization_ref": nil,
		"provider_inputs":                    nil,
		"execution_workspace_ref":            persisted.workspaceRef,
		"root_checkpoint_refs":               []any{persisted.checkpointRef},
		"latest_root_checkpoint_ref":         persisted.checkpointRef,
		"participant_turns":                  participantTurns,
		"actual_participant_turns":           0,
		"participant_turns_completed":        0,
		"next_unsealed_participant_turn":     1,
		"sealed_participant_turns":           []any{},
		"actual_rounds":                      0,
		"max_rounds":                         participantTurns,
		"round_limit_mode":                   "fixed",
		"rounds":                             participantTurns,
		"participant_schedule":               preflight.rootPlan["participant_schedule"],
		"slots":                              rootParticipantEnvelopes(preflight.rootPlan),
		"facilitator":                        preflight.rootPlan["facilitator"],
		"reducer":                            preflight.rootPlan["reducer"],
		"result_source":                      preflight.rootPlan["result_source"],
		"mode":                               preflight.rootPlan["mode"],
		"ledger":                             emptyLedger(),
		"lifecycle":                          preflight.rootPlan["lifecycle"],
		"launch_plan":                        preflight.options.LaunchPlan,
		"launch_context_refs":                persisted.launchContextRefs,
		"input_bundle_refs":                  append(append([]any{}, persisted.launchContextRefs...), persisted.launchSkillRefs...),
		"investigation_mode":                 preflight.promptPolicy.InvestigationMode,
		"prompt_policy":                      preflight.promptPolicy.ToMap(),
		"prompt_policy_version":              preflight.promptPolicy.Version,
		"source_launch_cwd":                  preflight.launchCWD,
		"launch_cwd":                         persisted.executionCWD,
		"execution_cwd":                      persisted.executionCWD,
		"launch_working_directory_policy":    "root_recipe_execution_workspace",
		"workspace_isolation":                preflight.workspace.Policy().Effective,
		"backend_readiness":                  preflight.backendReadiness,
		"timeout_seconds":                    preflight.options.TimeoutSeconds,
		"stall_timeout_seconds":              preflight.options.StallTimeoutSeconds,
		"created_at":                         utcNow(),
	}
	for key, value := range provenance {
		meta[key] = value
	}
	launchFacts, err := workspace.LaunchFactsFromArtifact(persisted.workspaceArtifact)
	if err != nil {
		return model.EmptySessionMeta(), fmt.Errorf("project execution workspace launch facts: %w", err)
	}
	for key, value := range launchFacts.Projection() {
		meta[key] = value
	}
	if strings.TrimSpace(preflight.runtimeConfig.SettingsPath) != "" {
		meta["settings_path"] = preflight.runtimeConfig.SettingsPath
	}
	if persisted.bundleRef != nil {
		meta["integration_bundle_ref"] = persisted.bundleRef
	}
	if persisted.contractRef != nil {
		meta["integration_contract_ref"] = persisted.contractRef
		meta["integration_contract_id"] = preflight.selectedContract.ID()
	}
	if persisted.inputManifestRef != nil {
		meta["named_input_manifest_ref"] = persisted.inputManifestRef
		meta["retained_input_materialization_ref"] = persisted.retainedInputRef
		meta["provider_inputs"] = persisted.providerInputs
	}
	if providerRetry, represented := preflight.rootPlan["provider_retry"]; represented {
		meta["provider_retry"] = providerRetry
	}
	return model.NewSessionMeta(meta), nil
}

func rootRecipeStartEvent(preflight *recipePreflight, persisted *persistedRecipeRun) map[string]any {
	return map[string]any{
		"session_ref":                        preflight.sessionID,
		"execution_kind":                     "recipe",
		"recipe_id":                          preflight.options.RecipeID,
		"recipe_ref":                         persisted.recipeRef,
		"root_recipe_plan_ref":               persisted.rootPlanRef,
		"runtime_config_ref":                 persisted.runtimeConfigRef,
		"integration_bundle_ref":             persisted.bundleRef,
		"integration_contract_ref":           persisted.contractRef,
		"named_input_manifest_ref":           persisted.inputManifestRef,
		"retained_input_materialization_ref": persisted.retainedInputRef,
		"execution_workspace_ref":            persisted.workspaceRef,
		"root_checkpoint_ref":                persisted.checkpointRef,
		"participant_turns":                  preflight.rootPlan["participant_turns"],
		"actual_participant_turns":           0,
		"participant_schedule":               preflight.rootPlan["participant_schedule"],
		"result_source":                      preflight.rootPlan["result_source"],
		"dynamic_mode":                       "off",
		"task":                               preflight.options.Task,
	}
}

func rootCheckpointFields(preflight *recipePreflight, persisted *persistedRecipeRun) map[string]any {
	return map[string]any{
		"ordinal":                            1,
		"phase":                              "workspace_ready",
		"status":                             "completed",
		"preflight_complete":                 true,
		"workspace_ready":                    true,
		"participant_turns_completed":        0,
		"recipe_ref":                         persisted.recipeRef,
		"root_recipe_plan_ref":               persisted.rootPlanRef,
		"runtime_config_ref":                 persisted.runtimeConfigRef,
		"integration_bundle_ref":             persisted.bundleRef,
		"integration_contract_ref":           persisted.contractRef,
		"named_input_manifest_ref":           persisted.inputManifestRef,
		"retained_input_materialization_ref": persisted.retainedInputRef,
		"execution_workspace_ref":            persisted.workspaceRef,
		"created_at":                         utcNow(),
	}
}

func preparedInputNames(prepared *namedinputs.Prepared) []string {
	items := prepared.Items()
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}
	return names
}

func selectedContractForRootPlan(bundle *integration.Bundle, rootPlan map[string]any) (*integration.SelectedContract, error) {
	contractID := strings.TrimSpace(stringFromAny(rootPlan["integration_contract_id"]))
	if contractID == "" {
		return nil, nil
	}
	if bundle == nil {
		return nil, rootRecipeDiagnostic(diagnosticCodePersistenceIntegrity, contracts.DiagnosticPhasePreflight, "/integration_bundle", "Compiled root plan selected a contract without an integration bundle.", nil)
	}
	rawSchedule, ok := rootPlan["participant_schedule"].([]any)
	if !ok {
		return nil, persistenceIntegrityError("Compiled root plan participant schedule is invalid.", nil)
	}
	schedule := make([]integration.ScheduledTurn, 0, len(rawSchedule))
	for index, raw := range rawSchedule {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, persistenceIntegrityError("Compiled root plan participant schedule entry is invalid.", map[string]any{"index": index})
		}
		schedule = append(schedule, integration.ScheduledTurn{
			ParticipantTurn: intFromAny(item["participant_turn"], 0),
			Slot:            stringFromAny(item["slot"]),
		})
	}
	return integration.SelectContract(bundle, contractID, integration.ScheduleRequirement{
		Turns:        schedule,
		ResultSource: stringFromAny(rootPlan["result_source"]),
	})
}

func validateRootBackendReadiness(expected []string, records []readiness.Record) error {
	byBackend := make(map[string]readiness.Record, len(records))
	for _, record := range records {
		byBackend[record.Backend] = record
	}
	diagnostics := []contracts.Diagnostic{}
	for _, backend := range expected {
		record, ok := byBackend[backend]
		if !ok {
			diagnostics = append(diagnostics, contracts.NewDiagnostic(
				diagnosticCodeBackendCheckMissing,
				contracts.DiagnosticPhasePreflight,
				"/backends/"+backend,
				"Required backend readiness was not reported.",
				map[string]any{"backend": backend},
			))
			continue
		}
		if record.Status != readiness.StatusInstalledAuthUnknown && record.Status != readiness.StatusReady {
			diagnostics = append(diagnostics, contracts.NewDiagnostic(
				diagnosticCodeBackendUnavailable,
				contracts.DiagnosticPhasePreflight,
				"/backends/"+backend,
				"Required backend is unavailable under the installation readiness policy.",
				map[string]any{"backend": backend, "status": record.Status, "authentication_status": record.AuthenticationStatus},
			))
		}
	}
	if len(diagnostics) == 0 {
		return nil
	}
	return contracts.NewDiagnosticError("Root recipe backend readiness failed.", diagnostics...)
}

func resolveNewRecipeSession(opts RecipeOptions) (string, string, string, error) {
	if strings.TrimSpace(opts.SessionDir) != "" {
		return opts.SessionDir, filepath.Base(filepath.Clean(opts.SessionDir)), workspace.SessionPathExplicit, nil
	}
	sessionID := strings.TrimSpace(opts.SessionID)
	if sessionID == "" {
		sessionID = generateSessionID()
	}
	if sessionID == "." || sessionID == ".." || filepath.Base(sessionID) != sessionID || strings.ContainsAny(sessionID, `/\\`) {
		return "", "", "", rootRecipeDiagnostic(diagnosticCodeSessionPathInvalid, contracts.DiagnosticPhasePolicy, "/session_id", "A new root recipe session id must be a single path-safe name.", map[string]any{"session_id": sessionID})
	}
	return sessionDirForID(opts.RelayHome, sessionID), sessionID, workspace.SessionPathRelayHome, nil
}

func validateNewRecipeSessionDestination(sessionDir string) error {
	info, err := os.Lstat(sessionDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return rootRecipeDiagnostic(diagnosticCodeSessionPathInvalid, contracts.DiagnosticPhasePreflight, "/session_dir", "The root recipe session destination could not be inspected.", map[string]any{"cause": err.Error()})
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return rootRecipeDiagnostic(diagnosticCodeSessionPathInvalid, contracts.DiagnosticPhasePolicy, "/session_dir", "The root recipe session destination must be a new directory path.", nil)
	}
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return rootRecipeDiagnostic(diagnosticCodeSessionPathInvalid, contracts.DiagnosticPhasePolicy, "/session_dir", "The root recipe session destination already exists and is not empty.", map[string]any{"session_dir": sessionDir})
	}
	return nil
}

func resolveRecipeLaunchCWD(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		value = "."
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func resolveRecipeSourcePath(sourceAnchor string, value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "-" || filepath.IsAbs(value) {
		return value
	}
	if value == "~" || strings.HasPrefix(value, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil {
			if value == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(value, "~"+string(filepath.Separator)))
		}
	}
	return filepath.Join(sourceAnchor, value)
}

func saveRootArtifact(st *store.Store, kind string, ordinal int, payload map[string]any) (map[string]any, error) {
	identity, err := contracts.RootArtifactIdentityFor(kind, ordinal)
	if err != nil {
		return nil, err
	}
	ref, err := st.SaveContractArtifact(kind, identity.ArtifactID, payload, identity.RefID)
	if err != nil {
		return nil, err
	}
	persisted, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, err
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, kind, ordinal, persisted); err != nil {
		return nil, err
	}
	if kind == contracts.RootArtifactKindRootCheckpoint && rootCheckpointAfterSave != nil {
		if err := rootCheckpointAfterSave(ordinal); err != nil {
			return ref, err
		}
	}
	return ref, nil
}

func withRootCheckpointRef(meta model.SessionMeta, ref map[string]any) model.SessionMeta {
	if ref == nil {
		return meta
	}
	wantID := strings.TrimSpace(stringFromAny(ref["id"]))
	refs := append([]any{}, meta.Slice("root_checkpoint_refs")...)
	replaced := false
	for index, raw := range refs {
		candidate, _ := raw.(map[string]any)
		if strings.TrimSpace(stringFromAny(candidate["id"])) == wantID {
			refs[index] = cloneMap(ref)
			replaced = true
		}
	}
	if !replaced {
		refs = append(refs, cloneMap(ref))
	}
	sort.SliceStable(refs, func(left int, right int) bool {
		leftRef, _ := refs[left].(map[string]any)
		rightRef, _ := refs[right].(map[string]any)
		return stringFromAny(leftRef["id"]) < stringFromAny(rightRef["id"])
	})
	return meta.With("root_checkpoint_refs", refs).With("latest_root_checkpoint_ref", cloneMap(ref))
}

func prepareRootTransientRecipes(files []recipes.TransientRecipeFile, runtimeConfig recipes.RuntimeConfig, rootRecipeID string, rootPlan map[string]any) ([]preparedTransientRecipe, error) {
	ids := effectiveTransientRecipeIDs(files)
	prepared := make([]preparedTransientRecipe, 0, len(ids))
	for _, recipeID := range ids {
		recipe := runtimeConfig.RelayRecipes[recipeID]
		if recipe == nil {
			return nil, fmt.Errorf("transient recipe %q is missing from effective runtime config", recipeID)
		}
		selected := recipeID == strings.TrimSpace(rootRecipeID)
		payload := recipes.ChildRecipeContractPayload(recipe)
		if selected {
			payload = recipes.RecipeContractPayload(recipe)
		}
		expectedRef, err := contracts.ArtifactRefForPayload("recipe:"+recipeID, payload)
		if err != nil {
			return nil, rootRecipeDiagnostic(
				diagnosticCodeArtifactIDInvalid,
				contracts.DiagnosticPhasePreflight,
				"/recipe",
				"Transient recipe IDs must form valid recipe artifact references.",
				map[string]any{"recipe_id": recipeID, "cause": err.Error()},
			)
		}
		if selected {
			if err := requireMatchingArtifactRef(mapFromAny(rootPlan["recipe_ref"]), expectedRef, "recipe"); err != nil {
				return nil, err
			}
		}
		prepared = append(prepared, preparedTransientRecipe{
			recipeID:    recipeID,
			payload:     payload,
			expectedRef: expectedRef,
			selected:    selected,
		})
	}
	return prepared, nil
}

func persistRootTransientRecipeContractArtifacts(st *store.Store, prepared []preparedTransientRecipe, rootRecipeRef map[string]any) ([]any, error) {
	refs := make([]any, 0, len(prepared))
	for _, recipe := range prepared {
		ref := rootRecipeRef
		if !recipe.selected {
			var err error
			ref, err = st.SaveContractArtifact("recipes", recipe.recipeID, recipe.payload, stringFromAny(recipe.expectedRef["id"]))
			if err != nil {
				return nil, err
			}
		}
		if err := requireMatchingArtifactRef(recipe.expectedRef, ref, "transient recipe"); err != nil {
			return nil, err
		}
		refs = append(refs, map[string]any{
			"recipe_id":     recipe.recipeID,
			"recipe_ref":    ref,
			"recipe_digest": ref["digest"],
		})
	}
	return refs, nil
}

func rootParticipantEnvelopes(rootPlan map[string]any) []any {
	rawParticipants, _ := rootPlan["participants"].([]any)
	result := make([]any, 0, len(rawParticipants))
	for index, raw := range rawParticipants {
		profile, _ := raw.(map[string]any)
		result = append(result, map[string]any{
			"slot_id":          firstNonEmpty(stringFromAny(profile["slot_id"]), fmt.Sprintf("slot_%d", index)),
			"logical_slot_id":  fmt.Sprintf("slot_%d", index),
			"generation":       1,
			"backend":          profile["backend"],
			"profile_id":       profile["profile_id"],
			"model":            profile["model"],
			"effort":           profile["effort"],
			"composition_path": profile["composition_path"],
			"state":            map[string]any{},
		})
	}
	return result
}

func requireMatchingArtifactRef(expected map[string]any, actual map[string]any, label string) error {
	want, err := contracts.ValidateArtifactRef(expected)
	if err != nil {
		return persistenceIntegrityError("Compiled "+label+" ref is invalid.", map[string]any{"cause": err.Error()})
	}
	got, err := contracts.ValidateArtifactRef(actual)
	if err != nil {
		return persistenceIntegrityError("Persisted "+label+" ref is invalid.", map[string]any{"cause": err.Error()})
	}
	if want["id"] != got["id"] || want["digest"] != got["digest"] {
		return persistenceIntegrityError("Persisted "+label+" ref does not match the compiled root plan.", map[string]any{"expected": want, "actual": got})
	}
	return nil
}

func mapFromAny(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func persistenceIntegrityError(message string, details map[string]any) error {
	return rootRecipeDiagnostic(diagnosticCodePersistenceIntegrity, contracts.DiagnosticPhasePreflight, "", message, details)
}

func rootRecipeDiagnostic(code string, phase string, path string, message string, details map[string]any) error {
	return contracts.NewDiagnosticError(message, contracts.NewDiagnostic(code, phase, path, message, details))
}

func positiveOrDefault(value int, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
