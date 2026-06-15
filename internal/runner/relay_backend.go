package runner

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const (
	defaultRelayBackendRecipe       = "review-panel"
	defaultRelayBackendMaxDepth     = 1
	relayBackendDepthEnv            = "CONVO_RELAY_BACKEND_DEPTH"
	relayBackendMaxDepthEnv         = "CONVO_RELAY_BACKEND_MAX_DEPTH"
	relayBackendMaxDepthInternalEnv = "CONVO_RELAY_BACKEND_MAX_DEPTH_INTERNAL"
)

type relayBackend struct {
	sessionRoot           string
	slotID                string
	label                 string
	cwd                   string
	profileID             string
	recipeID              string
	rounds                int
	settingsPath          string
	runtimeConfig         recipes.RuntimeConfig
	compositionPath       string
	started               bool
	childSessionIDs       []string
	childContractRefs     []map[string]any
	depth                 int
	maxDepth              int
	maxDepthEnvOverride   string
	maxDepthEnvIsInternal bool
}

func newRelayBackend(sessionRoot string, slotID string, label string, cwd string, config SlotConfig) *relayBackend {
	if cwd == "" {
		cwd = sessionRoot
	}
	recipeID := strings.TrimSpace(config.Model)
	if recipeID == "" {
		recipeID = defaultRelayBackendRecipe
	}
	compositionPath := strings.TrimSpace(config.CompositionPath)
	if compositionPath == "" {
		compositionPath = "root." + slotID
	}
	depth := config.Depth
	if depth == 0 {
		depth = parsePositiveInt(os.Getenv(relayBackendDepthEnv), 0, true)
	}
	maxDepthOverride := os.Getenv(relayBackendMaxDepthEnv)
	maxDepth := config.MaxDepth
	if maxDepth == 0 {
		maxDepth = parsePositiveInt(maxDepthOverride, defaultRelayBackendMaxDepth, true)
	}
	return &relayBackend{
		sessionRoot:           sessionRoot,
		slotID:                slotID,
		label:                 label,
		cwd:                   cwd,
		profileID:             config.ProfileID,
		recipeID:              recipeID,
		rounds:                parsePositiveInt(config.Effort, 0, true),
		settingsPath:          config.SettingsPath,
		runtimeConfig:         cloneRuntimeConfig(config.RuntimeConfig),
		compositionPath:       compositionPath,
		childSessionIDs:       []string{},
		childContractRefs:     []map[string]any{},
		depth:                 depth,
		maxDepth:              maxDepth,
		maxDepthEnvOverride:   maxDepthOverride,
		maxDepthEnvIsInternal: os.Getenv(relayBackendMaxDepthInternalEnv) != "",
	}
}

func (b *relayBackend) Name() string {
	return "relay"
}

func (b *relayBackend) SlotID() string {
	return b.slotID
}

func (b *relayBackend) Label() string {
	return b.label
}

func (b *relayBackend) RunTurn(ctx context.Context, prompt string, options TurnOptions) (TurnResult, error) {
	config := cloneRuntimeConfig(b.runtimeConfig)
	if !runtimeConfigProvided(config) {
		var err error
		config, err = recipes.LoadRuntimeConfig(b.settingsPath)
		if err != nil {
			return TurnResult{}, err
		}
		b.runtimeConfig = cloneRuntimeConfig(config)
	}
	if b.settingsPath == "" {
		b.settingsPath = config.SettingsPath
	}
	recipe, ok := config.RelayRecipes[b.recipeID]
	if !ok {
		return TurnResult{}, fmt.Errorf("%s unknown relay recipe %q", b.label, b.recipeID)
	}
	b.maxDepth = b.effectiveMaxDepth(recipe)
	if b.depth >= b.maxDepth {
		return TurnResult{}, fmt.Errorf("%s recursion depth %d reached max depth %d", b.label, b.depth, b.maxDepth)
	}
	rounds := b.rounds
	if rounds <= 0 {
		rounds = intFromAny(recipe["max_rounds"], 1)
	}
	childTask := buildBackendChildTask(prompt)
	depthPolicy := map[string]any{
		"graph_depth":             0,
		"max_graph_depth":         nil,
		"relay_backend_depth":     b.depth,
		"max_relay_backend_depth": b.maxDepth,
	}
	runContext := map[string]any{
		"origin":               "relay-backend",
		"composition_path":     b.compositionPath,
		"parent_session_id":    nil,
		"parent_node_id":       nil,
		"child_node_id":        nil,
		"proposal_id":          nil,
		"contested_lineage_id": nil,
		"delegated_question":   nil,
		"admitted_plan_id":     nil,
		"parent_slot_id":       b.slotID,
		"parent_backend":       "relay",
	}
	result, runErr := runChildRelay(ctx, childRelaySpec{
		Task:               childTask,
		Recipe:             recipe,
		Profiles:           config.BackendProfiles,
		Recipes:            config.RelayRecipes,
		AdmittedRounds:     rounds,
		Origin:             "relay-backend",
		RunContext:         runContext,
		DepthPolicy:        depthPolicy,
		TimeoutSeconds:     options.TimeoutSeconds,
		StallTimeoutSecond: options.StallTimeoutSeconds,
		SettingsPath:       b.settingsPath,
		RuntimeConfig:      config,
		LaunchCWD:          b.cwd,
		RelayHome:          relayHomeForSessionDir(b.sessionRoot),
	})
	if result != nil {
		b.started = true
		b.childSessionIDs = append(b.childSessionIDs, result.SessionID)
		refs, persistErr := b.persistChildResult(result, recipe)
		if persistErr != nil {
			return TurnResult{}, persistErr
		}
		if len(refs) > 0 {
			b.childContractRefs = append(b.childContractRefs, refs)
		}
	}
	if runErr != nil {
		return TurnResult{}, fmt.Errorf("%s child relay failed contract checks: %w", b.label, runErr)
	}
	if result == nil {
		return TurnResult{}, fmt.Errorf("%s child relay returned no result", b.label)
	}
	return TurnResult{Content: collapseChildResultText(result, b.recipeID)}, nil
}

func (b *relayBackend) SessionState() map[string]any {
	refs := make([]any, 0, len(b.childContractRefs))
	for _, ref := range b.childContractRefs {
		refs = append(refs, ref)
	}
	childIDs := make([]any, 0, len(b.childSessionIDs))
	for _, childID := range b.childSessionIDs {
		childIDs = append(childIDs, childID)
	}
	return map[string]any{
		"started":             b.started,
		"child_session_ids":   childIDs,
		"cwd":                 b.cwd,
		"profile_id":          emptyStringAsNil(b.profileID),
		"recipe_id":           b.recipeID,
		"rounds":              b.rounds,
		"settings_path":       b.settingsPath,
		"composition_path":    b.compositionPath,
		"depth":               b.depth,
		"max_depth":           b.maxDepth,
		"child_contract_refs": refs,
	}
}

func (b *relayBackend) RestoreState(state map[string]any, override SlotConfig) error {
	if state == nil {
		state = map[string]any{}
	}
	if started, ok := state["started"].(bool); ok {
		b.started = started
	} else if state["started"] != nil {
		return fmt.Errorf("started must be a bool")
	}
	b.childSessionIDs = stringListFromAny(state["child_session_ids"])
	b.childContractRefs = objectListFromAny(state["child_contract_refs"])
	if cwd := stringFromAny(state["cwd"]); cwd != "" {
		b.cwd = cwd
	}
	if override.ProfileID != "" {
		b.profileID = override.ProfileID
	} else if profileID := stringFromAny(state["profile_id"]); profileID != "" {
		b.profileID = profileID
	}
	if override.Model != "" {
		b.recipeID = override.Model
	} else if recipeID := stringFromAny(state["recipe_id"]); recipeID != "" {
		b.recipeID = recipeID
	}
	if override.Effort != "" {
		b.rounds = parsePositiveInt(override.Effort, b.rounds, true)
	} else {
		b.rounds = intFromAny(state["rounds"], b.rounds)
	}
	if override.SettingsPath != "" {
		b.settingsPath = override.SettingsPath
	} else if settingsPath := stringFromAny(state["settings_path"]); settingsPath != "" {
		b.settingsPath = settingsPath
	}
	if runtimeConfigProvided(override.RuntimeConfig) {
		b.runtimeConfig = cloneRuntimeConfig(override.RuntimeConfig)
	}
	if override.CompositionPath != "" {
		b.compositionPath = override.CompositionPath
	} else if compositionPath := stringFromAny(state["composition_path"]); compositionPath != "" {
		b.compositionPath = compositionPath
	}
	if override.Depth != 0 {
		b.depth = override.Depth
	} else {
		b.depth = intFromAny(state["depth"], b.depth)
	}
	if b.maxDepthEnvOverride != "" && !b.maxDepthEnvIsInternal {
		b.maxDepth = parsePositiveInt(b.maxDepthEnvOverride, defaultRelayBackendMaxDepth, true)
	} else if override.MaxDepth != 0 {
		b.maxDepth = override.MaxDepth
	} else {
		b.maxDepth = intFromAny(state["max_depth"], b.maxDepth)
	}
	return nil
}

func (b *relayBackend) Cleanup() error {
	return nil
}

func (b *relayBackend) persistChildResult(result *childRelayResult, recipe map[string]any) (map[string]any, error) {
	st := store.New(b.sessionRoot)
	artifactID := b.slotID + "-" + result.SessionID
	refs, err := saveChildContractArtifacts(st, artifactID, result, recipe)
	if err != nil {
		return nil, err
	}
	tracePayload := sanitizeChildTracePayload(result.TracePayload)
	tracePayload["composition_path"] = b.compositionPath
	if len(refs) > 0 {
		tracePayload["portable_contract_refs"] = refs
	}
	traceRef, err := st.SaveArtifact("child_traces", artifactID, tracePayload)
	if err != nil {
		return nil, err
	}
	nodeID := relayBackendChildNodeID(b.slotID, result.SessionID)
	if _, err := st.AppendSessionEventV1(
		"relay_backend_child_completed",
		nodeID,
		fmt.Sprintf("%s completed child relay %s", b.label, shortID(result.SessionID)),
		map[string]any{
			"parent_node_id":   graph.RootNodeID,
			"slot_id":          b.slotID,
			"composition_path": b.compositionPath,
			"recipe_id":        b.recipeID,
			"child_session_id": result.SessionID,
			"child_node_id":    nodeID,
			"trace_ref":        traceRef,
			"contract_refs":    refs,
		},
		store.EventOptions{},
	); err != nil {
		return nil, err
	}
	return refs, nil
}

func (b *relayBackend) effectiveMaxDepth(recipe map[string]any) int {
	recipeDepth := intFromAny(recipe["max_depth"], defaultRelayBackendMaxDepth)
	if recipeDepth <= 0 {
		recipeDepth = defaultRelayBackendMaxDepth
	}
	if b.maxDepthEnvOverride != "" && !b.maxDepthEnvIsInternal {
		return parsePositiveInt(b.maxDepthEnvOverride, defaultRelayBackendMaxDepth, true)
	}
	if b.maxDepth > recipeDepth {
		return b.maxDepth
	}
	return recipeDepth
}

func parsePositiveInt(value any, fallback int, allowZero bool) int {
	parsed := intFromAny(value, fallback)
	if text, ok := value.(string); ok {
		if integer, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
			parsed = integer
		}
	}
	if allowZero && parsed == 0 {
		return 0
	}
	if parsed > 0 {
		return parsed
	}
	return fallback
}

func relayBackendChildNodeID(slotID string, childSessionID string) string {
	return "relay_backend_child_" + sanitizeGraphIDSuffix(slotID, "slot") + "_" + truncateString(sanitizeGraphIDSuffix(childSessionID, "unknown"), 16)
}

func sanitizeGraphIDSuffix(value string, fallback string) string {
	var builder strings.Builder
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			builder.WriteRune(ch)
		} else {
			builder.WriteByte('_')
		}
	}
	text := builder.String()
	if text == "" {
		return fallback
	}
	return text
}

func sanitizeChildTracePayload(value map[string]any) map[string]any {
	sanitized, _ := sanitizeChildTraceValue(value, "").(map[string]any)
	if sanitized == nil {
		return map[string]any{}
	}
	return sanitized
}

func sanitizeChildTraceValue(value any, key string) any {
	if key == "child_contract_refs" {
		rawItems, _ := value.([]any)
		items := make([]any, 0, len(rawItems))
		for _, item := range rawItems {
			items = append(items, externalContractRefSummary(item))
		}
		return items
	}
	switch typed := value.(type) {
	case map[string]any:
		result := map[string]any{}
		for itemKey, itemValue := range typed {
			result[itemKey] = sanitizeChildTraceValue(itemValue, itemKey)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, sanitizeChildTraceValue(item, ""))
		}
		return result
	default:
		return typed
	}
}

func externalContractRefSummary(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	summary := map[string]any{}
	for key, ref := range object {
		if refObject, ok := ref.(map[string]any); ok && refObject["kind"] == "artifact_ref" {
			summary[key] = map[string]any{
				"id":       refObject["id"],
				"digest":   refObject["digest"],
				"external": true,
			}
		} else {
			summary[key] = sanitizeChildTraceValue(ref, "")
		}
	}
	return summary
}

func stringListFromAny(value any) []string {
	rawItems, _ := value.([]any)
	items := make([]string, 0, len(rawItems))
	for _, item := range rawItems {
		if text := stringFromAny(item); text != "" {
			items = append(items, text)
		}
	}
	return items
}

func objectListFromAny(value any) []map[string]any {
	rawItems, _ := value.([]any)
	items := make([]map[string]any, 0, len(rawItems))
	for _, item := range rawItems {
		if object, ok := item.(map[string]any); ok {
			items = append(items, cloneMap(object))
		}
	}
	return items
}

func shortID(value string) string {
	return truncateString(value, 8)
}

func truncateString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
