package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const (
	defaultTimeoutSeconds         = 600
	defaultMaxRounds              = 50
	defaultMode                   = "adversarial"
	defaultDynamicMode            = "off"
	defaultFacilitatorBackend     = "codex"
	defaultCodexFacilitatorModel  = "gpt-5.5"
	defaultCodexFacilitatorEffort = "medium"
	defaultClaudeFacilitatorModel = "sonnet"
)

type Options struct {
	SessionDir             string
	SessionID              string
	RelayHome              string
	Task                   string
	TaskWithContext        string
	SkillsText             string
	ContextFiles           []string
	SkillFiles             []string
	RecipeFiles            []string
	GeneratedRecipeFiles   []string
	TransientRecipeSources []recipes.TransientRecipeSource
	Agents                 []string
	SlotConfigs            []SlotConfig
	Rounds                 int
	MaxRounds              int
	TimeoutSeconds         int
	StallTimeoutSeconds    int
	Mode                   string
	DynamicMode            string
	SettingsPath           string
	LaunchCWD              string
	FacilitatorBackend     string
	FacilitatorModel       string
	FacilitatorEffort      string
	LaunchPlan             any
	InvestigationMode      string
	RuntimeConfig          recipes.RuntimeConfig
	RelayBackendDepth      int
	MaxRelayDepth          int
}

type ResumeOptions struct {
	Prompt              string
	Mode                string
	ContextFiles        []string
	SkillFiles          []string
	ReplaceAgents       []string
	Rounds              int
	MaxRounds           int
	TimeoutSeconds      int
	StallTimeoutSeconds int
	SlotConfigs         []SlotConfig
	SettingsPath        string
	FacilitatorModel    string
	FacilitatorEffort   string
	RelayBackendDepth   int
	MaxRelayDepth       int
}

type StopOptions struct {
	ForceKill bool
}

type runState struct {
	st                  *store.Store
	sessionDir          string
	sessionID           string
	meta                model.SessionMeta
	transcript          model.Transcript
	slots               []Backend
	timeoutSeconds      int
	stallTimeoutSeconds int
	mode                string
	dynamicMode         string
	settingsPath        string
	runtimeConfig       recipes.RuntimeConfig
	startedAt           time.Time
	taskWithContext     string
	skillsText          string
	facilitatorName     string
	facilitator         SlotConfig
	promptPolicy        PromptPolicy
	relayHome           string
	relayDepth          int
	maxRelayDepth       int
}

func Run(ctx context.Context, opts Options) (map[string]any, error) {
	opts = normalizeOptions(opts)
	if strings.TrimSpace(opts.Task) == "" {
		return nil, fmt.Errorf("--task is required")
	}
	if len(opts.Agents) != 2 {
		return nil, fmt.Errorf("exactly two agents are required")
	}
	if err := validateFacilitatorBackend(opts.FacilitatorBackend); err != nil {
		return nil, err
	}
	if opts.SessionDir == "" {
		if opts.SessionID == "" {
			opts.SessionID = generateSessionID()
		}
		opts.SessionDir = sessionDirForID(opts.RelayHome, opts.SessionID)
	}
	opts.SessionID = sessionIDFromDir(opts.SessionDir)
	if opts.RelayHome == "" {
		opts.RelayHome = relayHomeForSessionDir(opts.SessionDir)
	}
	if opts.LaunchCWD == "" {
		cwd, err := filepath.Abs(".")
		if err == nil {
			opts.LaunchCWD = cwd
		}
	}
	launchContexts, err := PreflightLaunchContexts(opts.ContextFiles)
	if err != nil {
		return nil, err
	}
	skillBundles, err := PreflightSkillInputs(opts.SkillFiles)
	if err != nil {
		return nil, err
	}
	promptPolicy, err := BuildPromptPolicy(opts.InvestigationMode, len(launchContexts) > 0)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.TaskWithContext) == "" {
		opts.TaskWithContext = BuildTaskWithLaunchContext(opts.Task, launchContexts)
	}
	if strings.TrimSpace(opts.SkillsText) == "" {
		opts.SkillsText = BuildSkillsPromptText(skillBundles)
	}
	runtimeConfig, transientRecipeFiles, err := loadEffectiveRuntimeConfig(opts.SettingsPath, opts.RuntimeConfig, opts.RecipeFiles, opts.GeneratedRecipeFiles, opts.TransientRecipeSources)
	if err != nil {
		return nil, err
	}
	effectiveSettingsPath := runtimeConfig.SettingsPath
	if err := ensureSessionDir(opts.SessionDir); err != nil {
		return nil, err
	}
	st := store.New(opts.SessionDir)
	runtimeConfigRef, err := persistRuntimeConfigSnapshot(st, runtimeConfig)
	if err != nil {
		return nil, err
	}
	slots, err := buildSlots(opts.Agents, opts.SessionDir, opts.LaunchCWD, opts.SlotConfigs, runtimeConfig, effectiveSettingsPath, opts.RelayBackendDepth, opts.MaxRelayDepth)
	if err != nil {
		return nil, err
	}
	launchContextRefs, err := persistLaunchContextRefs(st, launchContexts)
	if err != nil {
		return nil, err
	}
	launchSkillRefs, err := persistInputBundleRefs(st, "launch", skillBundles)
	if err != nil {
		return nil, err
	}
	transientRecipeRefs, err := persistTransientRecipeFileRefs(st, transientRecipeFiles)
	if err != nil {
		return nil, err
	}
	transientRecipeContractRefs, err := persistTransientRecipeContractArtifacts(st, transientRecipeFiles, runtimeConfig)
	if err != nil {
		return nil, err
	}
	inputBundleRefs := append([]any{}, launchContextRefs...)
	inputBundleRefs = append(inputBundleRefs, launchSkillRefs...)
	effectiveMaxRounds := opts.MaxRounds
	roundLimitMode := "auto"
	var rounds any
	if opts.Rounds > 0 {
		effectiveMaxRounds = opts.Rounds
		roundLimitMode = "fixed"
		rounds = opts.Rounds
	}
	now := utcNow()
	metaPayload := map[string]any{
		"session_id":       opts.SessionID,
		"task":             opts.Task,
		"initial_prompt":   opts.Task,
		"title":            "",
		"round_limit_mode": roundLimitMode,
		"rounds":           rounds,
		"max_rounds":       effectiveMaxRounds,
		"mode":             opts.Mode,
		"mode_history": []any{map[string]any{
			"source":        "run",
			"status":        "applied",
			"mode":          opts.Mode,
			"applied_mode":  opts.Mode,
			"queued_round":  0,
			"applied_round": 1,
			"created_at":    now,
		}},
		"slots":                           slotEnvelopes(slots),
		"ledger":                          emptyLedger(),
		"facilitator_backend":             opts.FacilitatorBackend,
		"facilitator_model":               nil,
		"facilitator_effort":              nil,
		"dynamic_mode":                    opts.DynamicMode,
		"contested_lineages":              map[string]any{},
		"launch_plan":                     opts.LaunchPlan,
		"investigation_mode":              promptPolicy.InvestigationMode,
		"prompt_policy":                   promptPolicy.ToMap(),
		"prompt_policy_version":           promptPolicy.Version,
		"launch_context_refs":             launchContextRefs,
		"input_bundle_refs":               inputBundleRefs,
		"transient_recipe_refs":           transientRecipeRefs,
		"transient_recipe_contract_refs":  transientRecipeContractRefs,
		"runtime_config_ref":              runtimeConfigRef,
		"runtime_config_version":          RuntimeConfigSnapshotVersion,
		"launch_working_directory_policy": "backend_subprocess_cwd",
		"launch_cwd":                      opts.LaunchCWD,
		"settings_path":                   nil,
		"created_at":                      now,
		"status":                          "running",
	}
	if effectiveSettingsPath != "" {
		metaPayload["settings_path"] = effectiveSettingsPath
	}
	if opts.FacilitatorModel != "" {
		metaPayload["facilitator_model"] = opts.FacilitatorModel
	}
	if opts.FacilitatorEffort != "" {
		metaPayload["facilitator_effort"] = opts.FacilitatorEffort
	}
	meta := model.NewSessionMeta(metaPayload)
	state := &runState{
		st:                  st,
		sessionDir:          opts.SessionDir,
		sessionID:           opts.SessionID,
		meta:                meta,
		transcript:          model.EmptyTranscript(),
		slots:               slots,
		timeoutSeconds:      opts.TimeoutSeconds,
		stallTimeoutSeconds: opts.StallTimeoutSeconds,
		mode:                opts.Mode,
		dynamicMode:         opts.DynamicMode,
		settingsPath:        effectiveSettingsPath,
		runtimeConfig:       runtimeConfig,
		startedAt:           time.Now(),
		taskWithContext:     opts.TaskWithContext,
		skillsText:          opts.SkillsText,
		facilitatorName:     opts.FacilitatorBackend,
		facilitator:         SlotConfig{Model: opts.FacilitatorModel, Effort: opts.FacilitatorEffort},
		promptPolicy:        promptPolicy,
		relayHome:           opts.RelayHome,
		relayDepth:          opts.RelayBackendDepth,
		maxRelayDepth:       opts.MaxRelayDepth,
	}
	if err := state.saveProgress(); err != nil {
		return nil, err
	}
	if err := writePID(opts.SessionDir); err != nil {
		return nil, err
	}
	defer removePID(opts.SessionDir)

	if _, err := st.AppendSessionEventV1("node_started", graph.RootNodeID, "Root relay node started", map[string]any{
		"session_ref":                    opts.SessionID,
		"dynamic_mode":                   opts.DynamicMode,
		"investigation_mode":             promptPolicy.InvestigationMode,
		"prompt_policy_version":          promptPolicy.Version,
		"launch_context_refs":            launchContextRefs,
		"input_bundle_refs":              inputBundleRefs,
		"transient_recipe_refs":          transientRecipeRefs,
		"transient_recipe_contract_refs": transientRecipeContractRefs,
		"runtime_config_ref":             runtimeConfigRef,
		"task":                           opts.Task,
		"max_rounds":                     effectiveMaxRounds,
	}, store.EventOptions{}); err != nil {
		return nil, err
	}
	if err := state.saveGraph(); err != nil {
		return nil, err
	}

	result, err := state.runTurns(ctx, 1, effectiveMaxRounds, opts.Rounds > 0, "", 0)
	if err != nil {
		return result, err
	}
	return result, nil
}

func Resume(ctx context.Context, sessionDir string, opts ResumeOptions) (map[string]any, error) {
	if strings.TrimSpace(sessionDir) == "" {
		return nil, fmt.Errorf("--session-dir is required")
	}
	opts = normalizeResumeOptions(opts)
	meta, err := loadSessionMeta(sessionDir)
	if err != nil {
		return nil, err
	}
	transcript, err := loadSessionTranscript(sessionDir)
	if err != nil {
		return nil, err
	}
	if transcript.Len() == 0 {
		return nil, fmt.Errorf("session has no transcript to resume")
	}
	st := store.New(sessionDir)
	resumeContexts, err := PreflightLaunchContexts(opts.ContextFiles)
	if err != nil {
		return nil, err
	}
	resumeSkills, err := PreflightSkillInputs(opts.SkillFiles)
	if err != nil {
		return nil, err
	}
	runtimeConfig, effectiveSettingsPath, err := effectiveRuntimeConfigForResume(st, meta.ToMap(), opts)
	if err != nil {
		return nil, err
	}
	lastEntry, ok := transcript.LastBackendEntry()
	if !ok {
		return nil, fmt.Errorf("session has no backend transcript entry to resume")
	}
	lastRound := lastEntry.Round
	if lastRound == 0 {
		lastRound = transcript.Len()
	}
	slots, slotReplacementEvents, err := restoreSlotsForResume(meta.ToMap(), sessionDir, opts.SlotConfigs, runtimeConfig, effectiveSettingsPath, opts.RelayBackendDepth, opts.MaxRelayDepth, opts.ReplaceAgents, lastRound+1)
	if err != nil {
		return nil, err
	}
	lastLogicalSlotID := lastEntry.LogicalSlotID
	nextIndex := -1
	for index, slot := range slots {
		if logicalSlotIDForSlotID(slot.SlotID()) == lastLogicalSlotID {
			nextIndex = 1 - index
			break
		}
	}
	if nextIndex < 0 {
		return nil, fmt.Errorf("transcript references unknown logical slot_id %q", lastLogicalSlotID)
	}
	currentMode := normalizeMode(meta.String("mode"))
	modeControl := map[string]any(nil)
	if strings.TrimSpace(opts.Mode) != "" {
		requestedMode := strings.TrimSpace(opts.Mode)
		appliedMode, err := validateRelayMode(requestedMode)
		if err != nil {
			return rejectModeControl(st, sessionDir, meta, transcript, requestedMode, currentMode, lastRound, lastRound+1, err)
		}
		transcript = transcript.StampMissingModes(currentMode)
		modeControl = modeControlPayload("resume", "applied", currentMode, requestedMode, appliedMode, lastRound, lastRound+1, "")
		meta = meta.WithMode(appliedMode)
		meta = meta.With("mode_history", appendAny(ensureModeHistory(meta.ToMap(), currentMode), modeControl))
		currentMode = appliedMode
	}
	resumeInputRefs, err := persistResumeInputBundleRefs(st, lastRound, resumeContexts, resumeSkills)
	if err != nil {
		return nil, err
	}
	additionalRounds := opts.MaxRounds
	fixedRounds := opts.Rounds > 0
	if fixedRounds {
		additionalRounds = opts.Rounds
	}
	effectiveMaxRounds := lastRound + additionalRounds
	meta = meta.With("round_limit_mode", map[bool]string{true: "fixed", false: "auto"}[fixedRounds])
	if fixedRounds {
		meta = meta.With("rounds", opts.Rounds)
	} else {
		meta = meta.With("rounds", nil)
	}
	meta = meta.With("max_rounds", effectiveMaxRounds)
	meta = meta.WithStatus("running")
	meta = meta.With("dynamic_mode", normalizeDynamicMode(meta.Get("dynamic_mode")))
	if opts.Prompt != "" {
		meta = meta.AppendToSlice("resume_prompts", map[string]any{
			"prompt":     opts.Prompt,
			"created_at": utcNow(),
		})
	}
	if len(resumeInputRefs) > 0 {
		meta = meta.With("input_bundle_refs", append(meta.Slice("input_bundle_refs"), resumeInputRefs...))
		meta = meta.With("resume_input_bundle_refs", append(meta.Slice("resume_input_bundle_refs"), resumeInputRefs...))
	}
	if len(slotReplacementEvents) > 0 {
		meta = meta.With("slot_replacement_history", append(meta.Slice("slot_replacement_history"), mapSliceAny(slotReplacementEvents)...))
	}
	if effectiveSettingsPath != "" {
		meta = meta.With("settings_path", effectiveSettingsPath)
	}
	slotOverrideEvents := slotRuntimeOverrideEvents(meta.ToMap(), opts.SlotConfigs, replacedSlotIndexes(slotReplacementEvents))
	facilitatorOverrideEvents := facilitatorRuntimeOverrideEvents(meta.ToMap(), opts.FacilitatorModel, opts.FacilitatorEffort)
	meta = meta.Without("error")
	facilitatorName := normalizeString(meta.Get("facilitator_backend"), defaultFacilitatorBackend)
	if err := validateFacilitatorBackend(facilitatorName); err != nil {
		return nil, err
	}
	state := &runState{
		st:                  st,
		sessionDir:          sessionDir,
		sessionID:           sessionIDFromDir(sessionDir),
		meta:                meta,
		transcript:          transcript,
		slots:               slots,
		timeoutSeconds:      opts.TimeoutSeconds,
		stallTimeoutSeconds: opts.StallTimeoutSeconds,
		mode:                currentMode,
		dynamicMode:         normalizeDynamicMode(meta.Get("dynamic_mode")),
		settingsPath:        effectiveSettingsPath,
		runtimeConfig:       runtimeConfig,
		startedAt:           time.Now(),
		facilitatorName:     facilitatorName,
		facilitator: SlotConfig{
			Model:  firstNonEmpty(opts.FacilitatorModel, meta.String("facilitator_model")),
			Effort: firstNonEmpty(opts.FacilitatorEffort, meta.String("facilitator_effort")),
		},
		promptPolicy:  promptPolicyFromMeta(meta.ToMap()),
		relayHome:     relayHomeForSessionDir(sessionDir),
		relayDepth:    opts.RelayBackendDepth,
		maxRelayDepth: opts.MaxRelayDepth,
	}
	if err := state.saveProgress(); err != nil {
		return nil, err
	}
	if err := writePID(sessionDir); err != nil {
		return nil, err
	}
	defer removePID(sessionDir)
	if _, err := st.AppendSessionEventV1("node_resumed", graph.RootNodeID, "Root relay resumed", map[string]any{
		"start_round":           lastRound + 1,
		"effective_max_rounds":  effectiveMaxRounds,
		"investigation_mode":    meta.String("investigation_mode"),
		"prompt_policy_version": meta.String("prompt_policy_version"),
	}, store.EventOptions{}); err != nil {
		return nil, err
	}
	if err := state.saveGraph(); err != nil {
		return nil, err
	}
	for _, payload := range slotReplacementEvents {
		if _, err := st.AppendSessionEventV1("slot_replaced", graph.RootNodeID, "Slot replacement applied", payload, store.EventOptions{}); err != nil {
			return nil, err
		}
	}
	for _, payload := range slotOverrideEvents {
		if _, err := st.AppendSessionEventV1("slot_runtime_override", graph.RootNodeID, "Slot runtime override applied", payload, store.EventOptions{}); err != nil {
			return nil, err
		}
	}
	for _, payload := range facilitatorOverrideEvents {
		if _, err := st.AppendSessionEventV1("facilitator_runtime_override", graph.RootNodeID, "Facilitator runtime override applied", payload, store.EventOptions{}); err != nil {
			return nil, err
		}
	}
	if modeControl != nil {
		if _, err := st.AppendSessionEventV1("mode_control_applied", graph.RootNodeID, "Relay mode control applied", modeControl, store.EventOptions{}); err != nil {
			return nil, err
		}
	}
	resumePrompt := BuildResumeInputDirection(opts.Prompt, resumeContexts, resumeSkills)
	return state.runTurns(ctx, lastRound+1, effectiveMaxRounds, fixedRounds, resumePrompt, nextIndex)
}

func Stop(sessionDir string, opts StopOptions) (map[string]any, error) {
	if strings.TrimSpace(sessionDir) == "" {
		return nil, fmt.Errorf("--session-dir is required")
	}
	meta, err := loadSessionMeta(sessionDir)
	if err != nil {
		return nil, err
	}
	transcript, err := loadSessionTranscript(sessionDir)
	if err != nil {
		return nil, err
	}
	pid, err := readPID(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("no tracked relay process for session %s", sessionIDFromDir(sessionDir))
	}
	st := store.New(sessionDir)
	if !processAlive(pid) {
		meta = meta.WithStatus("orphaned").
			WithActualRounds(transcript.Len()).
			With("orphaned_at", utcNow())
		if err := st.SaveMeta(meta); err != nil {
			return nil, err
		}
		removePID(sessionDir)
		return map[string]any{"session_id": sessionIDFromDir(sessionDir), "status": "orphaned", "pid": pid}, nil
	}
	sig := syscall.SIGTERM
	if opts.ForceKill {
		sig = syscall.SIGKILL
	}
	if err := signalProcess(pid, sig); err != nil {
		return nil, err
	}
	if opts.ForceKill {
		meta = meta.WithStatus("killed").
			WithActualRounds(transcript.Len()).
			With("killed_at", utcNow()).
			With("stop_reason", "killed")
		if err := st.SaveMeta(meta); err != nil {
			return nil, err
		}
		removePID(sessionDir)
		return map[string]any{"session_id": sessionIDFromDir(sessionDir), "status": "killed", "pid": pid}, nil
	}
	return map[string]any{"session_id": sessionIDFromDir(sessionDir), "status": "signaled", "pid": pid}, nil
}

func (s *runState) runTurns(ctx context.Context, startRound int, effectiveMaxRounds int, fixedRounds bool, resumePrompt string, nextIndex int) (map[string]any, error) {
	stopReason := "max_rounds"
	if fixedRounds {
		stopReason = "fixed_rounds"
	}
	for roundNum := startRound; roundNum <= effectiveMaxRounds; roundNum++ {
		if !fixedRounds && roundNum > startRound {
			if reason, ok := s.autoStopReason(); ok {
				stopReason = reason
				break
			}
		}
		slotIndex := nextIndex
		if slotIndex < 0 {
			slotIndex = (roundNum - 1) % len(s.slots)
		}
		slot := s.slots[slotIndex]
		prompt := s.promptForRound(roundNum, effectiveMaxRounds, slotIndex, resumePrompt)
		var err error
		prompt, err = s.consumeOperatorSteering(prompt)
		if err != nil {
			return s.markFailed(err)
		}
		if err := s.finalizeTurn(ctx, roundNum, slot, prompt); err != nil {
			if errors.Is(err, context.Canceled) {
				return s.markInterrupted("context canceled")
			}
			return s.markFailed(err)
		}
		if nextIndex >= 0 {
			nextIndex = 1 - nextIndex
		}
		if !fixedRounds {
			if reason, ok := s.autoStopReason(); ok {
				stopReason = reason
				break
			}
		}
	}
	if !fixedRounds {
		if reason, ok := s.autoStopReason(); ok {
			stopReason = reason
		}
	}
	return s.markCompleted(stopReason)
}

func (s *runState) autoStopReason() (string, bool) {
	if relayHasConverged(s.transcript, s.meta.Ledger()) {
		return "converged", true
	}
	if relayHasNoLedgerSignal(s.transcript, s.meta.Ledger()) {
		return "stalled_no_ledger_signal", true
	}
	return "", false
}

func (s *runState) promptForRound(roundNum int, effectiveMaxRounds int, slotIndex int, resumePrompt string) string {
	task := s.meta.String("initial_prompt")
	if task == "" {
		task = s.meta.String("task")
	}
	if roundNum == 1 && s.transcript.Len() == 0 {
		return frameInitial(task, s.slots[1].Label(), effectiveMaxRounds, s.mode, true, s.taskWithContext, s.skillsText, s.promptPolicy)
	}
	last, _ := s.transcript.Last()
	if resumePrompt != "" && last.Round == roundNum-1 {
		return frameResumeDirection(last, roundNum, effectiveMaxRounds, task, resumePrompt, s.mode, s.meta.Ledger(), s.promptPolicy)
	}
	if roundNum == 2 && s.transcript.Len() == 1 {
		intro := frameInitial(task, s.slots[0].Label(), effectiveMaxRounds, s.mode, false, s.taskWithContext, s.skillsText, s.promptPolicy)
		return intro + "\n\n" + frameRelay(last.Content, last.From, roundNum, effectiveMaxRounds, task, s.mode, s.meta.Ledger(), s.promptPolicy)
	}
	return frameRelay(last.Content, last.From, roundNum, effectiveMaxRounds, task, s.mode, s.meta.Ledger(), s.promptPolicy)
}

func (s *runState) consumeOperatorSteering(prompt string) (string, error) {
	steeringPrompts, err := consumeSteeringPrompts(s.sessionDir)
	if err != nil {
		return "", err
	}
	if len(steeringPrompts) == 0 {
		return prompt, nil
	}
	for _, item := range steeringPrompts {
		s.meta = s.meta.AppendToSlice("steering_history", item)
	}
	if err := s.saveProgress(); err != nil {
		return "", err
	}
	return appendSteeringBlock(prompt, steeringPrompts), nil
}

func (s *runState) finalizeTurn(ctx context.Context, roundNum int, slot Backend, prompt string) error {
	result, err := runWithRetryableProviderErrors(ctx, slot.Label(), func() (TurnResult, error) {
		return slot.RunTurn(ctx, prompt, TurnOptions{TimeoutSeconds: s.timeoutSeconds, StallTimeoutSeconds: s.stallTimeoutSeconds})
	})
	if err != nil {
		_ = s.recordProviderFailureEvent("turn", slot.Label(), slot.Name(), err, providerResultForTurn(slot.Name(), result))
		return err
	}
	previousLedger := s.meta.Ledger()
	var parseReport LedgerParseReport
	ledger, err := runWithRetryableProviderErrors(ctx, facilitatorLabel(s.facilitatorName), func() (model.Ledger, error) {
		parsedLedger, report, facilitatorErr := s.runFacilitator(ctx, result.Content, slot.Label())
		parseReport = report
		return parsedLedger, facilitatorErr
	})
	if err != nil {
		_ = s.recordProviderFailureEvent("facilitator", facilitatorLabel(s.facilitatorName), s.facilitatorName, err, ProviderResult{Backend: s.facilitatorName, ReturnCodeKnown: false})
		return err
	}
	providerResult := providerResultForTurn(slot.Name(), result)
	providerResultPayload := providerResultMap(providerResult)
	logicalSlotID := logicalSlotIDForSlotID(slot.SlotID())
	slotGeneration := slotGenerationForSlotID(slot.SlotID())
	entry := model.TranscriptEntry{
		Round:          roundNum,
		SlotID:         slot.SlotID(),
		LogicalSlotID:  logicalSlotID,
		SlotGeneration: slotGeneration,
		From:           slot.Label(),
		Mode:           s.mode,
		Content:        result.Content,
		ContentPresent: true,
		Ledger:         ledger,
		LedgerPresent:  true,
		Timestamp:      utcNow(),
		Extra:          map[string]any{},
	}
	if typedProviderResult, ok := model.ParseProviderResult(providerResultPayload); ok {
		entry.ProviderResult = &typedProviderResult
	}
	s.transcript = s.transcript.Append(entry)
	s.meta = s.meta.WithLedger(ledger).WithActualRounds(s.transcript.Len())
	s.meta = s.meta.WithTitleIfEmpty(makeTitle(result.Content))
	if err := s.saveProgress(); err != nil {
		return err
	}
	event, err := s.st.AppendSessionEventV1("turn_completed", graph.RootNodeID, fmt.Sprintf("Round %d completed by %s", roundNum, slot.Label()), map[string]any{
		"round":           roundNum,
		"slot_id":         slot.SlotID(),
		"logical_slot_id": logicalSlotID,
		"slot_generation": slotGeneration,
		"speaker":         slot.Label(),
		"mode":            s.mode,
		"ledger_counts":   ledger.CountsMap(),
		"parse_status":    string(parseReport.Status),
		"provider_result": providerResultPayload,
	}, store.EventOptions{})
	if err != nil {
		return err
	}
	if parseReport.Status == LedgerParseFallback {
		if err := s.recordFacilitatorLedgerUnparsed(roundNum, slot.Label(), parseReport); err != nil {
			return err
		}
	}
	lineages := updateContestedLineages(s.meta.Get("contested_lineages"), previousLedger.ToMap(), ledger.ToMap(), roundNum, stringFromAny(event["event_id"]))
	if normalizeDynamicMode(s.dynamicMode) != defaultDynamicMode {
		if _, err := maybeCreateSpawnProposals(s.st, s.dynamicMode, lineages, previousLedger.ToMap(), ledger.ToMap(), roundNum, stringFromAny(event["event_id"]), s.runtimeConfig.BackendProfiles, s.runtimeConfig.RelayRecipes); err != nil {
			return err
		}
	}
	s.meta = s.meta.With("contested_lineages", lineages)
	if err := s.saveProgress(); err != nil {
		return err
	}
	return s.saveGraph()
}

func (s *runState) recordFacilitatorLedgerUnparsed(roundNum int, speaker string, report LedgerParseReport) error {
	_, err := s.st.AppendSessionEventV1("facilitator_ledger_unparsed", graph.RootNodeID, fmt.Sprintf("Facilitator ledger parse fallback after round %d", roundNum), map[string]any{
		"round":                  roundNum,
		"speaker":                speaker,
		"parse_status":           string(report.Status),
		"fallback_ledger_counts": report.FallbackLedgerCounts,
		"raw_digest":             report.RawDigest,
		"raw_excerpt":            report.RawExcerpt,
		"raw_bytes":              report.RawBytes,
		"excerpt_truncated":      report.ExcerptTruncated,
	}, store.EventOptions{})
	return err
}

func (s *runState) recordProviderFailureEvent(phase string, actor string, backend string, err error, result ProviderResult) error {
	if s == nil || s.st == nil || err == nil {
		return nil
	}
	payload := providerFailurePayload(phase, actor, backend, err, result)
	if _, appendErr := s.st.AppendSessionEventV1("provider_failure", graph.RootNodeID, "Provider failure: "+actor, payload, store.EventOptions{}); appendErr != nil {
		return appendErr
	}
	s.meta = s.meta.AppendToSlice("provider_failures", payload)
	return s.saveProgress()
}

func facilitatorLabel(backend string) string {
	switch strings.TrimSpace(backend) {
	case "claude":
		return backendLabel("claude") + " Facilitator"
	case "gemini":
		return backendLabel("gemini") + " Facilitator"
	default:
		return backendLabel("codex") + " Facilitator"
	}
}

func (s *runState) runFacilitator(ctx context.Context, latestResponse string, speaker string) (model.Ledger, LedgerParseReport, error) {
	if s.facilitatorName == "" {
		s.facilitatorName = defaultFacilitatorBackend
	}
	if err := validateFacilitatorBackend(s.facilitatorName); err != nil {
		return model.EmptyLedger(), LedgerParseReport{}, err
	}
	config := resolveFacilitatorConfig(s.facilitatorName, s.facilitator)
	s.facilitator = config
	label := facilitatorLabel(s.facilitatorName)
	facilitator, err := newBackend(s.facilitatorName, s.sessionDir, "facilitator", label, s.sessionDir, config)
	if err != nil {
		return model.EmptyLedger(), LedgerParseReport{}, err
	}
	prompt := fmt.Sprintf(
		"%s\n\nCurrent ledger:\n%s\n\nRecent context:\n%s\n\nLatest response from %s:\n---\n%s\n---\n\nReturn the updated ledger as JSON.",
		facilitatorSystem,
		mustJSON(s.meta.Ledger().ToMap()),
		summarizeRecentTranscript(s.transcript),
		speaker,
		latestResponse,
	)
	result, err := facilitator.RunTurn(ctx, prompt, TurnOptions{TimeoutSeconds: s.timeoutSeconds, StallTimeoutSeconds: s.stallTimeoutSeconds})
	if err != nil {
		return model.EmptyLedger(), LedgerParseReport{}, err
	}
	ledger, report := parseLedgerFromText(result.Content, s.meta.Ledger())
	return ledger, report, nil
}

func (s *runState) saveProgress() error {
	s.meta = s.meta.WithMode(s.mode)
	s.meta = s.meta.WithSlots(slotEnvelopes(s.slots))
	s.meta = s.meta.With("facilitator_backend", s.facilitatorName)
	if s.facilitator.Model != "" {
		s.meta = s.meta.With("facilitator_model", s.facilitator.Model)
	}
	if s.facilitator.Effort != "" {
		s.meta = s.meta.With("facilitator_effort", s.facilitator.Effort)
	}
	if err := s.st.SaveMeta(s.meta); err != nil {
		return err
	}
	return saveSessionTranscript(s.st, s.transcript)
}

func (s *runState) saveGraph() error {
	repaired, _, err := graph.RepairAndSaveFromEvents(s.st)
	if err != nil {
		return err
	}
	repaired["backend_profiles"] = s.runtimeConfig.BackendProfiles
	repaired["relay_recipes"] = s.runtimeConfig.RelayRecipes
	repaired["runtime_limits"] = recipes.RuntimeLimitsMap(s.runtimeConfig.EffectiveLimits())
	repaired["runtime_config_ref"] = s.meta.Get("runtime_config_ref")
	nodes, _ := repaired["nodes"].(map[string]any)
	if root, ok := nodes[graph.RootNodeID].(map[string]any); ok {
		root["task"] = s.meta.Get("task")
		root["max_rounds"] = s.meta.Get("max_rounds")
		root["actual_rounds"] = s.meta.Get("actual_rounds")
		root["dynamic_mode"] = s.dynamicMode
		root["session_ref"] = s.sessionID
	}
	return s.st.SaveGraph(repaired)
}

func (s *runState) markCompleted(stopReason string) (map[string]any, error) {
	s.meta = s.meta.WithCompleted(s.transcript.Len(), roundElapsed(s.startedAt), utcNow(), stopReason)
	if err := s.saveProgress(); err != nil {
		return nil, err
	}
	if _, err := s.st.AppendSessionEventV1("node_completed", graph.RootNodeID, "Root relay completed: "+stopReason, map[string]any{
		"actual_rounds": s.transcript.Len(),
		"stop_reason":   stopReason,
	}, store.EventOptions{}); err != nil {
		return nil, err
	}
	if err := s.saveGraph(); err != nil {
		return nil, err
	}
	return s.result(), nil
}

func (s *runState) markInterrupted(reason string) (map[string]any, error) {
	s.meta = s.meta.WithInterrupted(s.transcript.Len(), utcNow(), reason)
	if err := s.saveProgress(); err != nil {
		return nil, err
	}
	_, _ = s.st.AppendSessionEventV1("node_interrupted", graph.RootNodeID, "Root relay interrupted", map[string]any{
		"actual_rounds": s.transcript.Len(),
		"reason":        reason,
	}, store.EventOptions{})
	_ = s.saveGraph()
	return s.result(), context.Canceled
}

func (s *runState) markFailed(err error) (map[string]any, error) {
	s.meta = s.meta.WithFailed(s.transcript.Len(), utcNow(), err)
	if saveErr := s.saveProgress(); saveErr != nil {
		return nil, saveErr
	}
	_, _ = s.st.AppendSessionEventV1("node_failed", graph.RootNodeID, "Root relay failed: "+err.Error(), map[string]any{
		"actual_rounds": s.transcript.Len(),
		"error":         err.Error(),
	}, store.EventOptions{})
	_ = s.saveGraph()
	return s.result(), err
}

func (s *runState) result() map[string]any {
	result := s.meta.ToMap()
	result["session_id"] = s.sessionID
	result["session_dir"] = s.sessionDir
	result["transcript"] = s.transcript.ToSlice()
	return result
}

func persistLaunchContextRefs(st *store.Store, contexts []LaunchContext) ([]any, error) {
	return persistInputBundleRefsWithCategory(st, "launch", "launch_context", contexts)
}

func persistInputBundleRefs(st *store.Store, phase string, bundles []InputBundle) ([]any, error) {
	return persistInputBundleRefsWithCategory(st, phase, "input_bundles", bundles)
}

func persistResumeInputBundleRefs(st *store.Store, lastRound int, contexts []InputBundle, skills []InputBundle) ([]any, error) {
	bundles := append([]InputBundle{}, contexts...)
	bundles = append(bundles, skills...)
	if len(bundles) == 0 {
		return nil, nil
	}
	phase := fmt.Sprintf("resume_round_%d", lastRound+1)
	return persistInputBundleRefs(st, phase, bundles)
}

func persistInputBundleRefsWithCategory(st *store.Store, phase string, category string, bundles []InputBundle) ([]any, error) {
	refs := make([]any, 0, len(bundles))
	for _, context := range bundles {
		artifactID := context.Label
		if phase != "" && phase != "launch" {
			artifactID = phase + "_" + context.Label
		}
		ref, err := st.SaveArtifact(category, artifactID, launchContextArtifactPayload(context))
		if err != nil {
			return nil, err
		}
		metadata := launchContextMetadata(context)
		metadata["phase"] = firstNonEmpty(phase, "launch")
		metadata["artifact_ref"] = ref
		refs = append(refs, metadata)
	}
	return refs, nil
}

func persistTransientRecipeFileRefs(st *store.Store, files []recipes.TransientRecipeFile) ([]any, error) {
	refs := make([]any, 0, len(files))
	for index, file := range files {
		label := fmt.Sprintf("recipe_file%d", index+1)
		sourceDigest := strings.TrimSpace(file.SourceDigest)
		if sourceDigest == "" {
			sourceDigest = strings.TrimSpace(file.Digest)
		}
		recipeDigests := stringMapAny(file.RecipeDigests)
		payload := map[string]any{
			"kind":           "transient_recipe_file",
			"schema_version": 1,
			"label":          label,
			"source_type":    file.SourceType,
			"display_name":   file.DisplayName,
			"source_path":    file.Path,
			"digest":         file.Digest,
			"source_digest":  sourceDigest,
			"recipe_digests": recipeDigests,
			"size_bytes":     file.SizeBytes,
			"recipe_ids":     stringSliceAny(file.RecipeIDs),
			"profile_ids":    stringSliceAny(file.ProfileIDs),
			"content":        file.Content,
		}
		ref, err := st.SaveArtifact("transient_recipes", label, payload)
		if err != nil {
			return nil, err
		}
		refs = append(refs, map[string]any{
			"label":          label,
			"source_type":    file.SourceType,
			"display_name":   file.DisplayName,
			"source_path":    file.Path,
			"digest":         file.Digest,
			"source_digest":  sourceDigest,
			"recipe_digests": recipeDigests,
			"size_bytes":     file.SizeBytes,
			"recipe_ids":     stringSliceAny(file.RecipeIDs),
			"profile_ids":    stringSliceAny(file.ProfileIDs),
			"artifact_ref":   ref,
		})
	}
	return refs, nil
}

func persistTransientRecipeContractArtifacts(st *store.Store, files []recipes.TransientRecipeFile, runtimeConfig recipes.RuntimeConfig) ([]any, error) {
	recipeIDs := effectiveTransientRecipeIDs(files)
	refs := make([]any, 0, len(recipeIDs))
	for _, recipeID := range recipeIDs {
		recipe := runtimeConfig.RelayRecipes[recipeID]
		if recipe == nil {
			return nil, fmt.Errorf("transient recipe %q is missing from effective runtime config", recipeID)
		}
		ref, err := st.SaveContractArtifact("recipes", recipeID, recipes.ChildRecipeContractPayload(recipe), "recipe:"+recipeID)
		if err != nil {
			return nil, err
		}
		refs = append(refs, map[string]any{
			"recipe_id":     recipeID,
			"recipe_ref":    ref,
			"recipe_digest": ref["digest"],
		})
	}
	return refs, nil
}

func effectiveTransientRecipeIDs(files []recipes.TransientRecipeFile) []string {
	seen := map[string]bool{}
	for _, file := range files {
		for _, recipeID := range file.RecipeIDs {
			seen[recipeID] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for recipeID := range seen {
		ids = append(ids, recipeID)
	}
	sort.Strings(ids)
	return ids
}

func stringMapAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func stringSliceAny(values []string) []any {
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	return items
}

func slotRuntimeOverrideEvents(meta map[string]any, configs []SlotConfig, replacedIndexes map[int]bool) []map[string]any {
	rawSlots := asSlice(meta["slots"])
	events := []map[string]any{}
	for index, config := range configs {
		if replacedIndexes[index] {
			continue
		}
		if config.Model == "" && config.Effort == "" {
			continue
		}
		slot := map[string]any{}
		if index < len(rawSlots) {
			slot, _ = rawSlots[index].(map[string]any)
		}
		state, _ := slot["state"].(map[string]any)
		payload := map[string]any{
			"slot_index": index,
			"slot_id":    firstNonEmpty(stringFromAny(slot["slot_id"]), fmt.Sprintf("slot_%d", index)),
			"label":      slot["label"],
			"backend":    slot["backend"],
			"source":     "resume",
			"overrides":  map[string]any{},
		}
		overrides := payload["overrides"].(map[string]any)
		if config.Model != "" {
			overrides["model"] = map[string]any{
				"previous":  state["model"],
				"requested": config.Model,
			}
		}
		if config.Effort != "" {
			overrides["effort"] = map[string]any{
				"previous":  state["effort"],
				"requested": config.Effort,
			}
		}
		events = append(events, payload)
	}
	return events
}

func replacedSlotIndexes(events []map[string]any) map[int]bool {
	indexes := map[int]bool{}
	for _, event := range events {
		indexes[intFromAny(event["slot_index"], -1)] = true
	}
	delete(indexes, -1)
	return indexes
}

func mapSliceAny(items []map[string]any) []any {
	result := make([]any, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	return result
}

func facilitatorRuntimeOverrideEvents(meta map[string]any, model string, effort string) []map[string]any {
	if model == "" && effort == "" {
		return nil
	}
	payload := map[string]any{
		"backend":   normalizeString(meta["facilitator_backend"], defaultFacilitatorBackend),
		"source":    "resume",
		"overrides": map[string]any{},
	}
	overrides := payload["overrides"].(map[string]any)
	if model != "" {
		overrides["model"] = map[string]any{
			"previous":  meta["facilitator_model"],
			"requested": model,
		}
	}
	if effort != "" {
		overrides["effort"] = map[string]any{
			"previous":  meta["facilitator_effort"],
			"requested": effort,
		}
	}
	return []map[string]any{payload}
}

func normalizeOptions(opts Options) Options {
	if opts.MaxRounds <= 0 {
		opts.MaxRounds = defaultMaxRounds
	}
	if opts.TimeoutSeconds <= 0 {
		opts.TimeoutSeconds = defaultTimeoutSeconds
	}
	if opts.StallTimeoutSeconds < 0 {
		opts.StallTimeoutSeconds = 0
	}
	opts.Mode = normalizeMode(opts.Mode)
	opts.DynamicMode = normalizeDynamicMode(opts.DynamicMode)
	opts.FacilitatorBackend = strings.TrimSpace(opts.FacilitatorBackend)
	if opts.FacilitatorBackend == "" {
		opts.FacilitatorBackend = defaultFacilitatorBackend
	}
	facilitatorConfig := resolveFacilitatorConfig(opts.FacilitatorBackend, SlotConfig{
		Model:  opts.FacilitatorModel,
		Effort: opts.FacilitatorEffort,
	})
	opts.FacilitatorModel = facilitatorConfig.Model
	opts.FacilitatorEffort = facilitatorConfig.Effort
	return opts
}

func normalizeResumeOptions(opts ResumeOptions) ResumeOptions {
	if opts.MaxRounds <= 0 {
		opts.MaxRounds = defaultMaxRounds
	}
	if opts.TimeoutSeconds <= 0 {
		opts.TimeoutSeconds = defaultTimeoutSeconds
	}
	if opts.StallTimeoutSeconds < 0 {
		opts.StallTimeoutSeconds = 0
	}
	return opts
}

func validateFacilitatorBackend(name string) error {
	backendName := strings.TrimSpace(name)
	if !knownBackend(backendName) {
		return fmt.Errorf("unsupported facilitator backend %q for Go runner", backendName)
	}
	if !facilitatorBackendAllowed(backendName) {
		return fmt.Errorf("backend %q cannot be used as a facilitator", backendName)
	}
	return nil
}

func resolveFacilitatorConfig(backendName string, config SlotConfig) SlotConfig {
	switch strings.TrimSpace(backendName) {
	case "codex":
		if config.Model == "" {
			config.Model = defaultCodexFacilitatorModel
		}
		if config.Effort == "" {
			config.Effort = defaultCodexFacilitatorEffort
		}
	case "claude":
		if config.Model == "" {
			config.Model = defaultClaudeFacilitatorModel
		}
	}
	return config
}

func normalizeMode(mode string) string {
	valid, err := validateRelayMode(mode)
	if err == nil {
		return valid
	}
	return defaultMode
}

func validateRelayMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "cooperative", "adversarial", "steelman":
		return strings.TrimSpace(mode), nil
	default:
		return "", fmt.Errorf("unsupported relay mode %q; expected one of: adversarial, cooperative, steelman", strings.TrimSpace(mode))
	}
}

func rejectModeControl(st *store.Store, sessionDir string, meta model.SessionMeta, transcript model.Transcript, requestedMode string, currentMode string, queuedRound int, appliedRound int, validationErr error) (map[string]any, error) {
	payload := modeControlPayload("resume", "rejected", currentMode, requestedMode, "", queuedRound, appliedRound, validationErr.Error())
	meta = meta.WithAttentionRequired(transcript.Len(), utcNow(), validationErr, payload)
	if err := st.SaveMeta(meta); err != nil {
		return nil, err
	}
	if err := saveSessionTranscript(st, transcript); err != nil {
		return nil, err
	}
	if _, err := st.AppendSessionEventV1("mode_control_rejected", graph.RootNodeID, "Relay mode control rejected", payload, store.EventOptions{}); err != nil {
		return nil, err
	}
	return sessionResult(sessionDir, meta, transcript), validationErr
}

func modeControlPayload(source string, status string, previousMode string, requestedMode string, appliedMode string, queuedRound int, appliedRound int, reason string) map[string]any {
	payload := map[string]any{
		"source":         source,
		"status":         status,
		"previous_mode":  previousMode,
		"requested_mode": requestedMode,
		"queued_round":   queuedRound,
		"applied_round":  appliedRound,
		"created_at":     utcNow(),
	}
	if appliedMode != "" {
		payload["applied_mode"] = appliedMode
		payload["mode"] = appliedMode
	}
	if reason != "" {
		payload["reason"] = reason
	}
	return payload
}

func ensureModeHistory(meta map[string]any, currentMode string) []any {
	history := asSlice(meta["mode_history"])
	if len(history) > 0 {
		return history
	}
	return []any{map[string]any{
		"source":        "session",
		"status":        "applied",
		"mode":          currentMode,
		"applied_mode":  currentMode,
		"queued_round":  0,
		"applied_round": 1,
	}}
}

func stampMissingTranscriptModes(transcript []map[string]any, mode string) {
	for _, entry := range transcript {
		if strings.TrimSpace(stringFromAny(entry["mode"])) == "" {
			entry["mode"] = mode
		}
	}
}

func sessionResult(sessionDir string, meta model.SessionMeta, transcript model.Transcript) map[string]any {
	result := meta.ToMap()
	result["session_id"] = sessionIDFromDir(sessionDir)
	result["session_dir"] = sessionDir
	result["transcript"] = transcript.ToSlice()
	return result
}

func slotEnvelopes(slots []Backend) []any {
	entries := make([]any, 0, len(slots))
	for _, slot := range slots {
		entries = append(entries, slotEnvelope(slot))
	}
	return entries
}

func transcriptAny(transcript []map[string]any) []any {
	items := make([]any, 0, len(transcript))
	for _, entry := range transcript {
		items = append(items, entry)
	}
	return items
}

func lastBackendEntry(transcript []map[string]any) map[string]any {
	for index := len(transcript) - 1; index >= 0; index-- {
		if isSyntheticChildTranscriptEntry(transcript[index]) {
			continue
		}
		if slotID, ok := transcript[index]["slot_id"].(string); ok && slotID != "" {
			return transcript[index]
		}
	}
	return nil
}

func logicalSlotIDForTranscriptEntry(entry map[string]any) string {
	return firstNonEmpty(stringFromAny(entry["logical_slot_id"]), logicalSlotIDForSlotID(stringFromAny(entry["slot_id"])))
}

func isSyntheticChildTranscriptEntry(entry map[string]any) bool {
	if synthetic, _ := entry["synthetic"].(bool); synthetic {
		return true
	}
	if sourceType, _ := entry["source_type"].(string); sourceType == "child_result" {
		return true
	}
	if slotID, _ := entry["slot_id"].(string); strings.HasPrefix(slotID, "child:") {
		return true
	}
	return false
}

func intFromAny(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case jsonNumber:
		if value, err := typed.Int64(); err == nil {
			return int(value)
		}
	}
	return fallback
}

type jsonNumber interface {
	Int64() (int64, error)
}

func stringFromAny(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func normalizeString(value any, fallback string) string {
	text := strings.TrimSpace(stringFromAny(value))
	if text == "" {
		return fallback
	}
	return text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func asSlice(value any) []any {
	items, _ := value.([]any)
	return items
}

func appendAny(items []any, item any) []any {
	return append(items, item)
}

func roundElapsed(start time.Time) float64 {
	return float64(time.Since(start).Round(100*time.Millisecond)) / float64(time.Second)
}
