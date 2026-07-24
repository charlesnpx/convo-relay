package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/recipes"
)

type TurnOptions struct {
	TimeoutSeconds      int
	StallTimeoutSeconds int
}

type TurnResult struct {
	Content        string
	TimedOut       bool
	Stalled        bool
	Recovered      bool
	ProviderResult ProviderResult
}

type SlotConfig struct {
	ProfileID       string
	Model           string
	Effort          string
	SettingsPath    string
	CompositionPath string
	RuntimeConfig   recipes.RuntimeConfig
	Depth           int
	MaxDepth        int
}

type Backend interface {
	Name() string
	SlotID() string
	Label() string
	RunTurn(context.Context, string, TurnOptions) (TurnResult, error)
	SessionState() map[string]any
	RestoreState(map[string]any, SlotConfig) error
	Cleanup() error
}

type processResult struct {
	Stdout     string
	Stderr     string
	ReturnCode int
	TimedOut   bool
}

var backendLabels = map[string]string{
	"claude": "Claude Code",
	"codex":  "Codex",
	"gemini": "Gemini",
	"relay":  "Relay",
}

func knownBackend(name string) bool {
	_, ok := backendLabels[strings.TrimSpace(name)]
	return ok
}

func backendLabel(name string) string {
	name = strings.TrimSpace(name)
	if label, ok := backendLabels[name]; ok {
		return label
	}
	return name
}

func newBackend(backendName string, sessionRoot string, slotID string, label string, cwd string, config SlotConfig) (Backend, error) {
	backendName = strings.TrimSpace(backendName)
	if label == "" {
		label = backendLabel(backendName)
	}
	switch backendName {
	case "claude":
		return newClaudeBackend(sessionRoot, slotID, label, cwd, config), nil
	case "codex":
		return newCodexBackend(sessionRoot, slotID, label, cwd, config), nil
	case "gemini":
		return newGeminiBackend(sessionRoot, slotID, label, cwd, config), nil
	case "relay":
		return newRelayBackend(sessionRoot, slotID, label, cwd, config), nil
	default:
		return nil, fmt.Errorf("unsupported backend %q for Go runner", backendName)
	}
}

func facilitatorBackendAllowed(name string) bool {
	switch strings.TrimSpace(name) {
	case "claude", "codex", "gemini":
		return true
	default:
		return false
	}
}

func ParseAgents(rawAgents string) ([]string, bool, error) {
	parts := strings.Split(rawAgents, ",")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
	}
	for _, part := range parts {
		if part == "" {
			return nil, false, fmt.Errorf("--agents requires one backend or two known backends separated by a comma")
		}
	}
	if len(parts) == 1 {
		return []string{parts[0], parts[0]}, true, nil
	}
	if len(parts) != 2 {
		return nil, false, fmt.Errorf("--agents requires one backend or two known backends separated by a comma")
	}
	return parts, false, nil
}

func buildSlots(agents []string, sessionRoot string, launchCWD string, configs []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int) ([]Backend, error) {
	if len(agents) != 2 {
		return nil, fmt.Errorf("--agents requires exactly two backends")
	}
	resolved := make([]string, 0, len(agents))
	resolvedConfigs := make([]SlotConfig, 0, len(agents))
	for index, agent := range agents {
		config := SlotConfig{}
		if index < len(configs) {
			config = configs[index]
		}
		if config.SettingsPath == "" {
			config.SettingsPath = settingsPath
		}
		config.RuntimeConfig = cloneRuntimeConfig(runtimeConfig)
		if config.Depth == 0 {
			config.Depth = relayDepth
		}
		if config.MaxDepth == 0 {
			config.MaxDepth = maxRelayDepth
		}
		backendName, resolvedConfig, err := resolveAgent(agent, config, runtimeConfig.BackendProfiles)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, backendName)
		resolvedConfigs = append(resolvedConfigs, resolvedConfig)
	}
	labels := slotLabels(resolved)
	slots := make([]Backend, 0, len(agents))
	for index, agent := range resolved {
		config := resolvedConfigs[index]
		cwd := resolveBackendCWD(launchCWD, sessionRoot, agent)
		backend, err := newBackend(agent, sessionRoot, fmt.Sprintf("slot_%d", index), labels[index], cwd, config)
		if err != nil {
			return nil, err
		}
		slots = append(slots, backend)
	}
	return slots, nil
}

func resolveAgent(agent string, override SlotConfig, profiles map[string]map[string]any) (string, SlotConfig, error) {
	if knownBackend(agent) {
		return agent, override, nil
	}
	profile, ok := profiles[agent]
	if !ok {
		return "", SlotConfig{}, fmt.Errorf("unsupported backend or profile %q for Go runner", agent)
	}
	backendName := strings.TrimSpace(stringFromAny(profile["backend"]))
	if !knownBackend(backendName) {
		return "", SlotConfig{}, fmt.Errorf("profile %q uses unsupported backend %q for Go runner", agent, backendName)
	}
	config := SlotConfig{
		ProfileID:       agent,
		Model:           stringFromAny(profile["model"]),
		Effort:          stringFromAny(profile["effort"]),
		SettingsPath:    override.SettingsPath,
		CompositionPath: override.CompositionPath,
		RuntimeConfig:   cloneRuntimeConfig(override.RuntimeConfig),
		Depth:           override.Depth,
		MaxDepth:        override.MaxDepth,
	}
	if override.Model != "" {
		config.Model = override.Model
	}
	if override.Effort != "" {
		config.Effort = override.Effort
	}
	return backendName, config, nil
}

func restoreSlots(meta map[string]any, sessionRoot string, overrides []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int) ([]Backend, error) {
	slots, _, err := restoreSlotsWithReplacements(meta, sessionRoot, overrides, runtimeConfig, settingsPath, relayDepth, maxRelayDepth, nil, 0)
	return slots, err
}

func restoreSlotsForResume(meta map[string]any, sessionRoot string, overrides []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int, replacements []string, appliesFromRound int) ([]Backend, []map[string]any, error) {
	return restoreSlotsWithReplacements(meta, sessionRoot, overrides, runtimeConfig, settingsPath, relayDepth, maxRelayDepth, replacements, appliesFromRound)
}

func restoreSlotsWithReplacements(meta map[string]any, sessionRoot string, overrides []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int, replacements []string, appliesFromRound int) ([]Backend, []map[string]any, error) {
	rawSlots, ok := meta["slots"].([]any)
	if !ok || len(rawSlots) != 2 {
		return nil, nil, fmt.Errorf("session metadata must contain exactly two slots")
	}
	replacementAgents := normalizeReplacementAgents(replacements, len(rawSlots))
	slots := make([]Backend, 0, len(rawSlots))
	replacementEvents := []map[string]any{}
	for index, rawSlot := range rawSlots {
		slotEntry, ok := rawSlot.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("slot %d metadata must be an object", index)
		}
		backendName, _ := slotEntry["backend"].(string)
		slotID, _ := slotEntry["slot_id"].(string)
		label, _ := slotEntry["label"].(string)
		if backendName == "" || slotID == "" {
			return nil, nil, fmt.Errorf("slot %d metadata is missing backend or slot_id", index)
		}
		override := slotConfigForRestore(index, overrides, runtimeConfig, settingsPath, relayDepth, maxRelayDepth)
		if replacementAgent := replacementAgents[index]; replacementAgent != "" {
			backend, event, err := replacementSlot(meta, sessionRoot, slotEntry, index, replacementAgent, override, runtimeConfig, appliesFromRound)
			if err != nil {
				return nil, nil, err
			}
			slots = append(slots, backend)
			replacementEvents = append(replacementEvents, event)
			continue
		}
		if label == "" {
			label = backendLabel(backendName)
		}
		state, _ := slotEntry["state"].(map[string]any)
		backend, err := newBackend(backendName, sessionRoot, slotID, label, sessionRoot, override)
		if err != nil {
			return nil, nil, err
		}
		if err := backend.RestoreState(state, override); err != nil {
			return nil, nil, fmt.Errorf("slot %s has invalid %s state: %w", slotID, backendName, err)
		}
		slots = append(slots, backend)
	}
	return slots, replacementEvents, nil
}

func normalizeReplacementAgents(replacements []string, count int) []string {
	agents := make([]string, count)
	for index := 0; index < count && index < len(replacements); index++ {
		agents[index] = strings.TrimSpace(replacements[index])
	}
	return agents
}

func slotConfigForRestore(index int, overrides []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int) SlotConfig {
	override := SlotConfig{}
	if index < len(overrides) {
		override = overrides[index]
	}
	if override.SettingsPath == "" {
		override.SettingsPath = settingsPath
	}
	override.RuntimeConfig = cloneRuntimeConfig(runtimeConfig)
	if override.Depth == 0 {
		override.Depth = relayDepth
	}
	if override.MaxDepth == 0 {
		override.MaxDepth = maxRelayDepth
	}
	return override
}

func replacementSlot(meta map[string]any, sessionRoot string, oldSlot map[string]any, index int, requestedAgent string, override SlotConfig, runtimeConfig recipes.RuntimeConfig, appliesFromRound int) (Backend, map[string]any, error) {
	backendName, resolvedConfig, err := resolveReplacementAgent(requestedAgent, override, runtimeConfig.BackendProfiles)
	if err != nil {
		return nil, nil, err
	}
	logicalSlotID := slotEntryLogicalSlotID(oldSlot, index)
	previousGeneration := slotEntryGeneration(oldSlot)
	newGeneration := previousGeneration + 1
	newSlotID := physicalSlotID(logicalSlotID, newGeneration)
	label := replacementSlotLabel(backendName, index)
	launchCWD := firstNonEmpty(stringFromAny(meta["launch_cwd"]), sessionRoot)
	cwd := resolveBackendCWD(launchCWD, sessionRoot, backendName)
	backend, err := newBackend(backendName, sessionRoot, newSlotID, label, cwd, resolvedConfig)
	if err != nil {
		return nil, nil, err
	}
	return backend, slotReplacementPayload(sessionRoot, oldSlot, backend, index, requestedAgent, resolvedConfig.ProfileID, appliesFromRound), nil
}

func resolveReplacementAgent(agent string, override SlotConfig, profiles map[string]map[string]any) (string, SlotConfig, error) {
	agent = strings.TrimSpace(agent)
	if knownBackend(agent) {
		return agent, override, nil
	}
	backendName, resolvedConfig, err := resolveAgent(agent, override, profiles)
	if err != nil {
		return "", SlotConfig{}, err
	}
	return backendName, resolvedConfig, nil
}

func replacementSlotLabel(backendName string, index int) string {
	if index >= 0 && index < 26 {
		return fmt.Sprintf("%s (%c)", backendLabel(backendName), 'A'+rune(index))
	}
	return backendLabel(backendName)
}

func slotReplacementPayload(sessionRoot string, oldSlot map[string]any, backend Backend, index int, requestedAgent string, newProfileID string, appliesFromRound int) map[string]any {
	oldState, _ := oldSlot["state"].(map[string]any)
	previousSlotID := stringFromAny(oldSlot["slot_id"])
	previousBackend := stringFromAny(oldSlot["backend"])
	logicalSlotID := slotEntryLogicalSlotID(oldSlot, index)
	previousGeneration := slotEntryGeneration(oldSlot)
	newState := backend.SessionState()
	newSlotID := backend.SlotID()
	return map[string]any{
		"kind":                        "slot_replacement",
		"schema_version":              1,
		"source":                      "resume",
		"slot_index":                  index,
		"logical_slot_id":             logicalSlotID,
		"requested_agent":             requestedAgent,
		"applies_from_round":          appliesFromRound,
		"created_at":                  utcNow(),
		"previous_slot_id":            previousSlotID,
		"previous_generation":         previousGeneration,
		"previous_backend":            previousBackend,
		"previous_profile_id":         emptyStringAsNil(slotEntryProfileID(oldSlot)),
		"previous_model":              emptyStringAsNil(slotStateModel(oldState)),
		"previous_effort":             emptyStringAsNil(slotStateEffort(oldState)),
		"new_slot_id":                 newSlotID,
		"new_generation":              slotGenerationForSlotID(newSlotID),
		"new_backend":                 backend.Name(),
		"new_profile_id":              emptyStringAsNil(newProfileID),
		"new_model":                   emptyStringAsNil(slotStateModel(newState)),
		"new_effort":                  emptyStringAsNil(slotStateEffort(newState)),
		"cleanup_preserves_artifacts": true,
		"cleanup_hint":                fmt.Sprintf("Historical provider artifacts for %s are preserved; delete the session to remove them or inspect before manual cleanup.", previousSlotID),
		"preserved_artifact_roots":    preservedProviderArtifactRoots(sessionRoot, previousBackend, previousSlotID),
		"previous": map[string]any{
			"slot_id":    previousSlotID,
			"generation": previousGeneration,
			"backend":    previousBackend,
			"profile_id": emptyStringAsNil(slotEntryProfileID(oldSlot)),
			"model":      emptyStringAsNil(slotStateModel(oldState)),
			"effort":     emptyStringAsNil(slotStateEffort(oldState)),
		},
		"new": map[string]any{
			"slot_id":    newSlotID,
			"generation": slotGenerationForSlotID(newSlotID),
			"backend":    backend.Name(),
			"profile_id": emptyStringAsNil(newProfileID),
			"model":      emptyStringAsNil(slotStateModel(newState)),
			"effort":     emptyStringAsNil(slotStateEffort(newState)),
		},
	}
}

func slotEntryLogicalSlotID(slotEntry map[string]any, index int) string {
	return firstNonEmpty(stringFromAny(slotEntry["logical_slot_id"]), logicalSlotIDForSlotID(stringFromAny(slotEntry["slot_id"])), logicalSlotIDForIndex(index))
}

func slotEntryGeneration(slotEntry map[string]any) int {
	generation := intFromAny(slotEntry["generation"], slotGenerationForSlotID(stringFromAny(slotEntry["slot_id"])))
	if generation < 1 {
		return 1
	}
	return generation
}

func slotEntryProfileID(slotEntry map[string]any) string {
	if profileID := stringFromAny(slotEntry["profile_id"]); profileID != "" {
		return profileID
	}
	state, _ := slotEntry["state"].(map[string]any)
	return stringFromAny(state["profile_id"])
}

func slotStateModel(state map[string]any) string {
	return firstNonEmpty(stringFromAny(state["model"]), stringFromAny(state["recipe_id"]))
}

func slotStateEffort(state map[string]any) string {
	if effort := stringFromAny(state["effort"]); effort != "" {
		return effort
	}
	if rounds := intFromAny(state["rounds"], 0); rounds > 0 {
		return fmt.Sprint(rounds)
	}
	return ""
}

func preservedProviderArtifactRoots(sessionRoot string, backendName string, slotID string) []any {
	roots := []any{}
	switch strings.TrimSpace(backendName) {
	case "codex":
		roots = append(roots, filepath.ToSlash(filepath.Join("codex", slotID)))
	case "gemini":
		roots = append(roots, filepath.ToSlash(filepath.Join("gemini", slotID)))
	case "claude":
		roots = append(roots, "claude provider transcript referenced by previous session state")
	case "relay":
		roots = append(roots, "relay child artifacts referenced by previous slot state")
	}
	if len(roots) == 0 && sessionRoot != "" {
		roots = append(roots, "session-local provider artifacts")
	}
	return roots
}

func slotLabels(agents []string) []string {
	counts := map[string]int{}
	for _, agent := range agents {
		counts[agent]++
	}
	seen := map[string]int{}
	labels := make([]string, 0, len(agents))
	for _, agent := range agents {
		seen[agent]++
		label := backendLabels[agent]
		if counts[agent] > 1 {
			label = fmt.Sprintf("%s (%c)", label, 'A'+rune(seen[agent]-1))
		}
		labels = append(labels, label)
	}
	return labels
}

func slotEnvelope(slot Backend) map[string]any {
	logicalSlotID := logicalSlotIDForSlotID(slot.SlotID())
	generation := slotGenerationForSlotID(slot.SlotID())
	state := slot.SessionState()
	profileID := stringFromAny(state["profile_id"])
	return map[string]any{
		"backend":         slot.Name(),
		"profile_id":      emptyStringAsNil(profileID),
		"slot_id":         slot.SlotID(),
		"logical_slot_id": logicalSlotID,
		"generation":      generation,
		"label":           slot.Label(),
		"state":           state,
	}
}

func logicalSlotIDForSlotID(slotID string) string {
	slotID = strings.TrimSpace(slotID)
	if slotID == "" {
		return ""
	}
	if base, _, ok := strings.Cut(slotID, "_gen"); ok && strings.HasPrefix(base, "slot_") {
		return base
	}
	return slotID
}

func slotGenerationForSlotID(slotID string) int {
	slotID = strings.TrimSpace(slotID)
	if slotID == "" {
		return 1
	}
	_, rawGeneration, ok := strings.Cut(slotID, "_gen")
	if !ok {
		return 1
	}
	generation, err := strconv.Atoi(rawGeneration)
	if err != nil || generation < 1 {
		return 1
	}
	return generation
}

func physicalSlotID(logicalSlotID string, generation int) string {
	logicalSlotID = strings.TrimSpace(logicalSlotID)
	if logicalSlotID == "" {
		logicalSlotID = "slot_0"
	}
	if generation <= 1 {
		return logicalSlotID
	}
	return fmt.Sprintf("%s_gen%d", logicalSlotID, generation)
}

func logicalSlotIDForIndex(index int) string {
	return fmt.Sprintf("slot_%d", index)
}

func emptyStringAsNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.TrimSpace(value)
}

func resolveBackendCWD(launchCWD string, sessionRoot string, backendName string) string {
	if launchCWD == "" {
		return sessionRoot
	}
	info, err := os.Stat(launchCWD)
	if err != nil || !info.IsDir() {
		return sessionRoot
	}
	if backendName == "codex" && !pathWithinGitRepo(launchCWD) {
		return sessionRoot
	}
	return launchCWD
}

type codexBackend struct {
	sessionRoot    string
	slotID         string
	label          string
	cwd            string
	profileID      string
	model          string
	effort         string
	threadID       string
	started        bool
	codexHome      string
	codexHomeReady bool
}

func newCodexBackend(sessionRoot string, slotID string, label string, cwd string, config SlotConfig) *codexBackend {
	if cwd == "" {
		cwd = sessionRoot
	}
	return &codexBackend{
		sessionRoot: sessionRoot,
		slotID:      slotID,
		label:       label,
		cwd:         cwd,
		profileID:   config.ProfileID,
		model:       config.Model,
		effort:      config.Effort,
		codexHome:   filepath.Join(sessionRoot, "codex", slotID),
	}
}

func (b *codexBackend) Name() string {
	return "codex"
}

func (b *codexBackend) SlotID() string {
	return b.slotID
}

func (b *codexBackend) Label() string {
	return b.label
}

func (b *codexBackend) RunTurn(ctx context.Context, prompt string, options TurnOptions) (TurnResult, error) {
	if err := b.ensureCodexHome(); err != nil {
		return TurnResult{}, err
	}
	result, err := runSubprocess(ctx, b.buildCommand(), prompt, b.cwd, b.env(), options.TimeoutSeconds)
	providerResult := newProviderResult("codex", result)
	text, startedThreadID := parseCodexEvents(result.Stdout)
	if b.threadID == "" {
		b.threadID = startedThreadID
	}
	if errors.Is(err, context.Canceled) {
		return TurnResult{}, err
	}
	if err != nil {
		return TurnResult{}, err
	}
	if result.TimedOut {
		if text == "" {
			providerResult.Warnings = append(providerResult.Warnings, fmt.Sprintf("codex timed out after %ds with no recoverable response", options.TimeoutSeconds))
			text = fmt.Sprintf("[%s timed out after %ds]", b.label, options.TimeoutSeconds)
		} else if retryableError := classifyRetryableProviderError(text); retryableError != "" {
			providerResult.RetryableError = retryableError
			return TurnResult{ProviderResult: providerResult}, RetryableProviderError{Label: b.label, Detail: retryableError}
		} else if providerFailureCategory(text) == "auth" {
			return TurnResult{ProviderResult: providerResult}, BackendRunError{Label: b.label, Detail: text}
		} else {
			providerResult.Recovered = true
			providerResult.RecoverySource = "event_buffer"
			providerResult.Warnings = append(providerResult.Warnings, fmt.Sprintf("codex timed out after %ds but response recovered from event buffer", options.TimeoutSeconds))
		}
	} else if result.ReturnCode != 0 {
		if text == "" {
			detail := strings.TrimSpace(result.Stderr)
			if detail == "" {
				detail = strings.TrimSpace(result.Stdout)
			}
			if detail == "" {
				detail = fmt.Sprintf("process exited %d", result.ReturnCode)
			}
			detail = collapseWhitespace(detail)
			if retryableError := classifyRetryableProviderError(detail); retryableError != "" {
				providerResult.RetryableError = retryableError
				return TurnResult{ProviderResult: providerResult}, RetryableProviderError{Label: b.label, Detail: retryableError}
			}
			return TurnResult{ProviderResult: providerResult}, BackendRunError{Label: b.label, Detail: detail}
		}
		if retryableError := classifyRetryableProviderError(text); retryableError != "" {
			providerResult.RetryableError = retryableError
			return TurnResult{ProviderResult: providerResult}, RetryableProviderError{Label: b.label, Detail: retryableError}
		}
		if providerFailureCategory(text) == "auth" {
			return TurnResult{ProviderResult: providerResult}, BackendRunError{Label: b.label, Detail: text}
		}
		providerResult.Recovered = true
		providerResult.RecoverySource = "event_buffer"
		providerResult.Warnings = append(providerResult.Warnings, fmt.Sprintf("codex exited %d", result.ReturnCode))
		if result.Stderr != "" {
			providerResult.Warnings = append(providerResult.Warnings, truncateString(result.Stderr, 500))
		}
	}
	if text == "" {
		text = fmt.Sprintf("[No response from %s]", b.label)
	}
	if b.threadID != "" {
		b.started = true
	}
	return TurnResult{
		Content:        text,
		TimedOut:       providerResult.TimedOut,
		Stalled:        providerResult.Stalled,
		Recovered:      providerResult.Recovered,
		ProviderResult: providerResult,
	}, nil
}

func (b *codexBackend) SessionState() map[string]any {
	state := map[string]any{
		"thread_id":  b.threadID,
		"started":    b.started,
		"cwd":        b.cwd,
		"profile_id": nil,
		"model":      nil,
		"effort":     nil,
	}
	if b.profileID != "" {
		state["profile_id"] = b.profileID
	}
	if b.model != "" {
		state["model"] = b.model
	}
	if b.effort != "" {
		state["effort"] = b.effort
	}
	return state
}

func (b *codexBackend) RestoreState(state map[string]any, override SlotConfig) error {
	if state == nil {
		state = map[string]any{}
	}
	started, ok := state["started"].(bool)
	if state["started"] != nil && !ok {
		return fmt.Errorf("started must be a bool")
	}
	threadID, _ := state["thread_id"].(string)
	if started && threadID == "" {
		return fmt.Errorf("started codex slots require thread_id")
	}
	cwd, _ := state["cwd"].(string)
	profileID, _ := state["profile_id"].(string)
	model, _ := state["model"].(string)
	effort, _ := state["effort"].(string)
	b.threadID = threadID
	b.started = started || threadID != ""
	if cwd != "" {
		b.cwd = cwd
	}
	if override.ProfileID != "" {
		b.profileID = override.ProfileID
	} else if profileID != "" {
		b.profileID = profileID
	}
	if override.Model != "" {
		b.model = override.Model
	} else if model != "" {
		b.model = model
	}
	if override.Effort != "" {
		b.effort = override.Effort
	} else if effort != "" {
		b.effort = effort
	}
	return nil
}

func (b *codexBackend) Cleanup() error {
	return nil
}

func (b *codexBackend) buildCommand() []string {
	options := []string{}
	if b.model != "" {
		options = append(options, "-m", b.model)
	}
	if b.effort != "" {
		options = append(options, "-c", fmt.Sprintf("model_reasoning_effort=%q", b.effort))
	}
	if b.threadID != "" {
		command := append([]string{"codex", "exec", "resume"}, options...)
		return append(command, "--json", b.threadID, "-")
	}
	command := append([]string{"codex", "exec"}, options...)
	return append(command, "--json", "-")
}

func (b *codexBackend) env() []string {
	env := os.Environ()
	env = append(env, "CODEX_HOME="+b.codexHome)
	return env
}

func (b *codexBackend) ensureCodexHome() error {
	if b.codexHomeReady {
		return nil
	}
	if err := os.MkdirAll(b.codexHome, 0o755); err != nil {
		return err
	}
	realCodexHome, err := os.UserHomeDir()
	if err == nil {
		realCodexHome = filepath.Join(realCodexHome, ".codex")
		for _, name := range []string{"auth.json", "config.toml"} {
			src := filepath.Join(realCodexHome, name)
			dst := filepath.Join(b.codexHome, name)
			if _, err := os.Stat(src); err == nil {
				if _, err := os.Lstat(dst); os.IsNotExist(err) {
					_ = os.Symlink(src, dst)
				}
			}
		}
	}
	b.codexHomeReady = true
	return nil
}

func parseCodexEvents(stdout string) (string, string) {
	var parts []string
	threadID := ""
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		eventType, _ := event["type"].(string)
		switch eventType {
		case "thread.started":
			if value, ok := event["thread_id"].(string); ok {
				threadID = value
			}
		case "item.completed":
			item, _ := event["item"].(map[string]any)
			if text, ok := item["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
			if content, ok := item["content"].([]any); ok {
				for _, rawBlock := range content {
					block, _ := rawBlock.(map[string]any)
					if block["type"] == "output_text" {
						if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
							parts = append(parts, text)
						}
					}
				}
			}
		case "message.completed":
			if text, ok := event["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n\n"), threadID
}

func runSubprocess(ctx context.Context, command []string, prompt string, cwd string, env []string, timeoutSeconds int) (processResult, error) {
	if len(command) == 0 {
		return processResult{ReturnCode: -1}, fmt.Errorf("empty command")
	}
	runCtx := ctx
	cancel := func() {}
	if timeoutSeconds > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = cwd
	cmd.Env = env
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := processResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ReturnCode: 0,
		TimedOut:   errors.Is(runCtx.Err(), context.DeadlineExceeded),
	}
	if cmd.ProcessState != nil {
		result.ReturnCode = cmd.ProcessState.ExitCode()
	} else if err != nil {
		result.ReturnCode = -1
	}
	if errors.Is(ctx.Err(), context.Canceled) && !result.TimedOut {
		return result, ctx.Err()
	}
	if result.TimedOut {
		return result, nil
	}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if result.ReturnCode == 0 {
			result.ReturnCode = exitErr.ExitCode()
		}
		return result, nil
	}
	return result, err
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
