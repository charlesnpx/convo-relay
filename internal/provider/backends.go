package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/gitexec"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/recipes"
)

type TurnOptions struct {
	TimeoutSeconds      int
	StallTimeoutSeconds int
}

type TurnResult struct {
	Content           string
	TimedOut          bool
	Stalled           bool
	Recovered         bool
	ProviderResult    ProviderResult
	ProviderResultRef Metadata
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

// providerState keeps the provider-owned persisted state wire-compatible with
// existing sessions while preventing the raw session metadata map from
// crossing the exported provider boundary.
type providerState = map[string]any

// SlotState is the persisted state of one provider slot.
type SlotState = providerState

// Metadata is provider-owned structured metadata retained for compatibility.
type Metadata = providerState

type Backend interface {
	Name() string
	SlotID() string
	Label() string
	RunTurn(context.Context, string, TurnOptions) (TurnResult, error)
	SessionState() SlotState
	RestoreState(SlotState, SlotConfig) error
	Cleanup() error
}

// RelayConstructor supplies only the runner-owned relay pseudo-backend. The
// provider package constructs every declared external provider itself.
type RelayConstructor func(sessionRoot string, slotID string, label string, cwd string, config SlotConfig) (Backend, error)

// SlotBuildInput contains the typed launch information needed to create the
// two participant slots.
type SlotBuildInput struct {
	Agents           []string
	SessionRoot      string
	LaunchCWD        string
	Configs          []SlotConfig
	RuntimeConfig    recipes.RuntimeConfig
	SettingsPath     string
	RelayDepth       int
	MaxRelayDepth    int
	RelayConstructor RelayConstructor
}

// PersistedSlot is the typed representation of one decoded session slot.
// Runner owns decoding it from legacy session metadata.
type PersistedSlot struct {
	Backend       string
	SlotID        string
	Label         string
	LogicalSlotID string
	Generation    int
	ProfileID     string
	State         SlotState
}

// RestoreSlotsInput contains typed restored-slot data and launch context.
type RestoreSlotsInput struct {
	Slots            []PersistedSlot
	SessionRoot      string
	LaunchCWD        string
	Overrides        []SlotConfig
	RuntimeConfig    recipes.RuntimeConfig
	SettingsPath     string
	RelayDepth       int
	MaxRelayDepth    int
	Replacements     []string
	AppliesFromRound int
	RelayConstructor RelayConstructor
}

// RestoreSlotsResult is the typed output of a slot restoration.
type RestoreSlotsResult struct {
	Slots             []Backend
	ReplacementEvents []model.SlotReplacementRecord
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
		return newEmbeddedClaudeBackend(sessionRoot, slotID, label, cwd, config), nil
	case "codex":
		return newEmbeddedCodexBackend(sessionRoot, slotID, label, cwd, config), nil
	case "gemini":
		return newGeminiBackend(sessionRoot, slotID, label, cwd, config), nil
	default:
		return nil, fmt.Errorf("unsupported backend %q for Go runner", backendName)
	}
}

// NewBackend constructs one declared external provider adapter. Relay is a
// runner pseudo-backend and is supplied only by the runner-side constructor.
func NewBackend(backendName string, sessionRoot string, slotID string, label string, cwd string, config SlotConfig) (Backend, error) {
	return newBackend(backendName, sessionRoot, slotID, label, cwd, config)
}

func KnownBackend(name string) bool {
	return knownBackend(name)
}

func BackendLabel(name string) string {
	return backendLabel(name)
}

func FacilitatorBackendAllowed(name string) bool {
	return facilitatorBackendAllowed(name)
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

// BuildSlots builds slots from typed launch configuration. The optional relay
// constructor retains the runner-owned relay pseudo-backend.
func BuildSlots(input SlotBuildInput) ([]Backend, error) {
	return buildSlotsWithConstructor(
		input.Agents,
		input.SessionRoot,
		input.LaunchCWD,
		input.Configs,
		input.RuntimeConfig,
		input.SettingsPath,
		input.RelayDepth,
		input.MaxRelayDepth,
		input.RelayConstructor,
	)
}

func buildSlotsWithConstructor(agents []string, sessionRoot string, launchCWD string, configs []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int, relayConstructor RelayConstructor) ([]Backend, error) {
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
		backend, err := newBackendForSlot(agent, sessionRoot, fmt.Sprintf("slot_%d", index), labels[index], cwd, config, relayConstructor)
		if err != nil {
			return nil, err
		}
		slots = append(slots, backend)
	}
	return slots, nil
}

func newBackendForSlot(backendName string, sessionRoot string, slotID string, label string, cwd string, config SlotConfig, relayConstructor RelayConstructor) (Backend, error) {
	if strings.TrimSpace(backendName) == "relay" && relayConstructor != nil {
		return relayConstructor(sessionRoot, slotID, label, cwd, config)
	}
	return newBackend(backendName, sessionRoot, slotID, label, cwd, config)
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

// RestoreSlots restores provider slots from runner-decoded typed metadata.
func RestoreSlots(input RestoreSlotsInput) (RestoreSlotsResult, error) {
	if len(input.Slots) != 2 {
		return RestoreSlotsResult{}, fmt.Errorf("session metadata must contain exactly two slots")
	}
	replacementAgents := normalizeReplacementAgents(input.Replacements, len(input.Slots))
	slots := make([]Backend, 0, len(input.Slots))
	replacementEvents := make([]model.SlotReplacementRecord, 0)
	for index, slotEntry := range input.Slots {
		backendName := slotEntry.Backend
		slotID := slotEntry.SlotID
		label := slotEntry.Label
		if backendName == "" || slotID == "" {
			return RestoreSlotsResult{}, fmt.Errorf("slot %d metadata is missing backend or slot_id", index)
		}
		override := slotConfigForRestore(index, input.Overrides, input.RuntimeConfig, input.SettingsPath, input.RelayDepth, input.MaxRelayDepth)
		if replacementAgent := replacementAgents[index]; replacementAgent != "" {
			backend, event, err := replacementSlot(input.SessionRoot, input.LaunchCWD, slotEntry, index, replacementAgent, override, input.RuntimeConfig, input.AppliesFromRound, input.RelayConstructor)
			if err != nil {
				return RestoreSlotsResult{}, err
			}
			slots = append(slots, backend)
			replacementEvents = append(replacementEvents, event)
			continue
		}
		if label == "" {
			label = backendLabel(backendName)
		}
		backend, err := newBackendForSlot(backendName, input.SessionRoot, slotID, label, input.SessionRoot, override, input.RelayConstructor)
		if err != nil {
			return RestoreSlotsResult{}, err
		}
		if err := backend.RestoreState(slotEntry.State, override); err != nil {
			return RestoreSlotsResult{}, fmt.Errorf("slot %s has invalid %s state: %w", slotID, backendName, err)
		}
		slots = append(slots, backend)
	}
	return RestoreSlotsResult{Slots: slots, ReplacementEvents: replacementEvents}, nil
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

func replacementSlot(sessionRoot string, launchCWD string, oldSlot PersistedSlot, index int, requestedAgent string, override SlotConfig, runtimeConfig recipes.RuntimeConfig, appliesFromRound int, relayConstructor RelayConstructor) (Backend, model.SlotReplacementRecord, error) {
	backendName, resolvedConfig, err := resolveReplacementAgent(requestedAgent, override, runtimeConfig.BackendProfiles)
	if err != nil {
		return nil, model.SlotReplacementRecord{}, err
	}
	logicalSlotID := slotEntryLogicalSlotID(oldSlot, index)
	previousGeneration := slotEntryGeneration(oldSlot)
	newGeneration := previousGeneration + 1
	newSlotID := physicalSlotID(logicalSlotID, newGeneration)
	label := replacementSlotLabel(backendName, index)
	cwd := resolveBackendCWD(firstNonEmpty(launchCWD, sessionRoot), sessionRoot, backendName)
	backend, err := newBackendForSlot(backendName, sessionRoot, newSlotID, label, cwd, resolvedConfig, relayConstructor)
	if err != nil {
		return nil, model.SlotReplacementRecord{}, err
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

func slotReplacementPayload(sessionRoot string, oldSlot PersistedSlot, backend Backend, index int, requestedAgent string, newProfileID string, appliesFromRound int) model.SlotReplacementRecord {
	oldState := oldSlot.State
	previousSlotID := oldSlot.SlotID
	previousBackend := oldSlot.Backend
	logicalSlotID := slotEntryLogicalSlotID(oldSlot, index)
	previousGeneration := slotEntryGeneration(oldSlot)
	newState := backend.SessionState()
	newSlotID := backend.SlotID()
	return model.NewSlotReplacementRecord(map[string]any{
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
	})
}

func slotEntryLogicalSlotID(slotEntry PersistedSlot, index int) string {
	return firstNonEmpty(slotEntry.LogicalSlotID, logicalSlotIDForSlotID(slotEntry.SlotID), logicalSlotIDForIndex(index))
}

func slotEntryGeneration(slotEntry PersistedSlot) int {
	generation := slotEntry.Generation
	if generation == 0 {
		generation = slotGenerationForSlotID(slotEntry.SlotID)
	}
	if generation < 1 {
		return 1
	}
	return generation
}

func slotEntryProfileID(slotEntry PersistedSlot) string {
	if slotEntry.ProfileID != "" {
		return slotEntry.ProfileID
	}
	return stringFromAny(slotEntry.State["profile_id"])
}

func slotStateModel(state SlotState) string {
	return firstNonEmpty(stringFromAny(state["model"]), stringFromAny(state["recipe_id"]))
}

func slotStateEffort(state SlotState) string {
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

func SlotLabels(agents []string) []string {
	return slotLabels(agents)
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

// SlotEnvelope converts provider state to the project slot record used by
// runner-owned session persistence.
func SlotEnvelope(slot Backend) model.SlotEnvelope {
	return model.NewSlotEnvelope(slotEnvelope(slot))
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

func LogicalSlotIDForSlotID(slotID string) string {
	return logicalSlotIDForSlotID(slotID)
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

func SlotGenerationForSlotID(slotID string) int {
	return slotGenerationForSlotID(slotID)
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

func CollapseWhitespace(value string) string {
	return collapseWhitespace(value)
}

func cloneRuntimeConfig(config recipes.RuntimeConfig) recipes.RuntimeConfig {
	return recipes.RuntimeConfig{
		BackendProfiles: mapStringObjectMap(config.BackendProfiles),
		RelayRecipes:    mapStringObjectMap(config.RelayRecipes),
		Limits:          config.EffectiveLimits(),
		SettingsPath:    strings.TrimSpace(config.SettingsPath),
	}
}

func mapStringObjectMap(value any) map[string]map[string]any {
	result := map[string]map[string]any{}
	switch typed := value.(type) {
	case map[string]map[string]any:
		for key, child := range typed {
			result[key] = cloneMap(child)
		}
	case map[string]any:
		for key, raw := range typed {
			if child, ok := raw.(map[string]any); ok {
				result[key] = cloneMap(child)
			}
		}
	}
	return result
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	cloned, _ := contracts.Materialize(value).(map[string]any)
	return cloned
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func utcNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

func pathWithinGitRepo(path string) bool {
	_, ok := gitRootForPath(path)
	return ok
}

func gitRootForPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	output, err := gitexec.Run(context.Background(), "git", path, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(output))
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", false
	}
	return filepath.Clean(root), true
}
