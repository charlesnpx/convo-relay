package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/provider"
	"github.com/charlesnpx/convo-relay/internal/recipes"
)

// These aliases retain runner's existing internal call sites while ownership
// of the provider contract lives in internal/provider.
type TurnOptions = provider.TurnOptions
type TurnResult = provider.TurnResult
type SlotConfig = provider.SlotConfig
type Backend = provider.Backend
type ProviderResult = provider.ProviderResult
type BackendRunError = provider.BackendRunError
type RetryableProviderError = provider.RetryableProviderError
type ProviderFailureError = provider.ProviderFailureError

// retryBackoff remains a runner test seam. Production delegates the retry
// policy and classification to provider with this unchanged default.
var retryBackoff = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func knownBackend(name string) bool {
	return provider.KnownBackend(name)
}

func backendLabel(name string) string {
	return provider.BackendLabel(name)
}

func facilitatorBackendAllowed(name string) bool {
	return provider.FacilitatorBackendAllowed(name)
}

func ParseAgents(rawAgents string) ([]string, bool, error) {
	return provider.ParseAgents(rawAgents)
}

func newBackend(backendName string, sessionRoot string, slotID string, label string, cwd string, config SlotConfig) (Backend, error) {
	backendName = strings.TrimSpace(backendName)
	if backendName == "relay" {
		if label == "" {
			label = backendLabel(backendName)
		}
		return newRelayBackend(sessionRoot, slotID, label, cwd, config), nil
	}
	return provider.NewBackend(backendName, sessionRoot, slotID, label, cwd, config)
}

func newRunnerRelayBackend(sessionRoot string, slotID string, label string, cwd string, config SlotConfig) (Backend, error) {
	return newRelayBackend(sessionRoot, slotID, label, cwd, config), nil
}

func buildSlots(agents []string, sessionRoot string, launchCWD string, configs []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int) ([]Backend, error) {
	return provider.BuildSlots(provider.SlotBuildInput{
		Agents:           agents,
		SessionRoot:      sessionRoot,
		LaunchCWD:        launchCWD,
		Configs:          configs,
		RuntimeConfig:    runtimeConfig,
		SettingsPath:     settingsPath,
		RelayDepth:       relayDepth,
		MaxRelayDepth:    maxRelayDepth,
		RelayConstructor: newRunnerRelayBackend,
	})
}

func restoreSlots(meta map[string]any, sessionRoot string, overrides []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int) ([]Backend, error) {
	result, err := restoreProviderSlots(meta, sessionRoot, overrides, runtimeConfig, settingsPath, relayDepth, maxRelayDepth, nil, 0)
	if err != nil {
		return nil, err
	}
	return result.Slots, nil
}

func restoreSlotsForResume(meta map[string]any, sessionRoot string, overrides []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int, replacements []string, appliesFromRound int) ([]Backend, []map[string]any, error) {
	result, err := restoreProviderSlots(meta, sessionRoot, overrides, runtimeConfig, settingsPath, relayDepth, maxRelayDepth, replacements, appliesFromRound)
	if err != nil {
		return nil, nil, err
	}
	events := make([]map[string]any, 0, len(result.ReplacementEvents))
	for _, event := range result.ReplacementEvents {
		events = append(events, event.ToMap())
	}
	return result.Slots, events, nil
}

func restoreProviderSlots(meta map[string]any, sessionRoot string, overrides []SlotConfig, runtimeConfig recipes.RuntimeConfig, settingsPath string, relayDepth int, maxRelayDepth int, replacements []string, appliesFromRound int) (provider.RestoreSlotsResult, error) {
	slots, err := decodePersistedProviderSlots(meta)
	if err != nil {
		return provider.RestoreSlotsResult{}, err
	}
	launchCWD := firstNonEmpty(stringFromAny(meta["launch_cwd"]), sessionRoot)
	return provider.RestoreSlots(provider.RestoreSlotsInput{
		Slots:            slots,
		SessionRoot:      sessionRoot,
		LaunchCWD:        launchCWD,
		Overrides:        overrides,
		RuntimeConfig:    runtimeConfig,
		SettingsPath:     settingsPath,
		RelayDepth:       relayDepth,
		MaxRelayDepth:    maxRelayDepth,
		Replacements:     replacements,
		AppliesFromRound: appliesFromRound,
		RelayConstructor: newRunnerRelayBackend,
	})
}

func decodePersistedProviderSlots(meta map[string]any) ([]provider.PersistedSlot, error) {
	rawSlots, ok := meta["slots"].([]any)
	if !ok || len(rawSlots) != 2 {
		return nil, fmt.Errorf("session metadata must contain exactly two slots")
	}
	slots := make([]provider.PersistedSlot, 0, len(rawSlots))
	for index, rawSlot := range rawSlots {
		slotEntry, ok := rawSlot.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("slot %d metadata must be an object", index)
		}
		backendName, _ := slotEntry["backend"].(string)
		slotID, _ := slotEntry["slot_id"].(string)
		if backendName == "" || slotID == "" {
			return nil, fmt.Errorf("slot %d metadata is missing backend or slot_id", index)
		}
		label, _ := slotEntry["label"].(string)
		state, _ := slotEntry["state"].(map[string]any)
		slots = append(slots, provider.PersistedSlot{
			Backend:       backendName,
			SlotID:        slotID,
			Label:         label,
			LogicalSlotID: stringFromAny(slotEntry["logical_slot_id"]),
			Generation:    intFromAny(slotEntry["generation"], provider.SlotGenerationForSlotID(slotID)),
			ProfileID:     stringFromAny(slotEntry["profile_id"]),
			State:         state,
		})
	}
	return slots, nil
}

func slotEnvelope(slot Backend) map[string]any {
	return provider.SlotEnvelope(slot).ToMap()
}

func slotLabels(agents []string) []string {
	return provider.SlotLabels(agents)
}

func logicalSlotIDForSlotID(slotID string) string {
	return provider.LogicalSlotIDForSlotID(slotID)
}

func slotGenerationForSlotID(slotID string) int {
	return provider.SlotGenerationForSlotID(slotID)
}

func emptyStringAsNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.TrimSpace(value)
}

func collapseWhitespace(value string) string {
	return provider.CollapseWhitespace(value)
}

func runWithRetryableProviderErrors[T any](ctx context.Context, label string, operation func() (T, error)) (T, error) {
	return runWithProviderRetryPolicy(ctx, label, recipes.ProviderRetryAllow, operation)
}

func runWithProviderRetryPolicy[T any](ctx context.Context, label string, policy string, operation func() (T, error)) (T, error) {
	return provider.RunWithProviderRetryPolicyWithBackoff(ctx, label, policy, provider.RetryBackoff(retryBackoff), operation)
}

func providerResultForTurn(backend string, result TurnResult) ProviderResult {
	return provider.ProviderResultForTurn(backend, result)
}

func providerResultMap(result ProviderResult) map[string]any {
	return result.ToMap()
}

func providerFailurePayload(phase string, actor string, backend string, err error, result ProviderResult) map[string]any {
	failure := provider.NewProviderFailure(phase, actor, backend, err, result)
	return map[string]any{
		"phase":             failure.Phase,
		"actor":             failure.Actor,
		"backend":           failure.Backend,
		"category":          failure.Category,
		"retryable":         failure.Retryable,
		"attempts":          failure.Attempts,
		"timed_out":         failure.TimedOut,
		"stalled":           failure.Stalled,
		"return_code":       failure.ReturnCode,
		"remediation_code":  failure.RemediationCode,
		"remediation":       failure.Remediation,
		"sanitized_detail":  failure.SanitizedDetail,
		"raw_detail_hidden": failure.RawDetailHidden,
	}
}

func providerFailureDetail(err error, result ProviderResult) string {
	return provider.ProviderFailureDetail(err, result)
}

func providerFailureCategory(text string) string {
	return provider.ProviderFailureCategory(text)
}

func sanitizeProviderFailureDetail(detail string) string {
	return provider.SanitizeProviderFailureDetail(detail)
}
