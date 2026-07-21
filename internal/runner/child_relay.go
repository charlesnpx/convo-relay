package runner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

type childRelaySpec struct {
	Task               string
	Recipe             map[string]any
	Profiles           map[string]map[string]any
	Recipes            map[string]map[string]any
	AdmittedRounds     int
	Origin             string
	RunContext         map[string]any
	DepthPolicy        map[string]any
	TimeoutSeconds     int
	StallTimeoutSecond int
	SettingsPath       string
	RuntimeConfig      recipes.RuntimeConfig
	LaunchCWD          string
	RelayHome          string
	CompiledPlan       map[string]any
	CompiledPlanRef    map[string]any
}

type childRelayResult struct {
	SessionID               string
	Status                  string
	StopReason              any
	Ledger                  map[string]any
	Transcript              []map[string]any
	ActualRounds            int
	ElapsedSeconds          any
	Error                   any
	Origin                  string
	RecipeID                string
	RunContext              map[string]any
	DepthPolicy             map[string]any
	LastContent             string
	TracePayload            map[string]any
	CompiledPlanRef         map[string]any
	CompiledPlanContract    map[string]any
	ChildInvocationRef      map[string]any
	ChildInvocationContract map[string]any
	ChildResultRef          map[string]any
	ChildResultContract     map[string]any
}

func runChildRelay(ctx context.Context, spec childRelaySpec) (*childRelayResult, error) {
	if spec.AdmittedRounds <= 0 {
		spec.AdmittedRounds = 1
	}
	if spec.TimeoutSeconds <= 0 {
		spec.TimeoutSeconds = defaultTimeoutSeconds
	}
	spec.DepthPolicy = normalizeChildDepthPolicy(spec.DepthPolicy)
	if err := validateChildDepthPolicy(spec.DepthPolicy, spec.Origin); err != nil {
		return nil, err
	}
	compositionPath := stringFromAny(spec.RunContext["composition_path"])
	if compositionPath == "" {
		compositionPath = "root"
		spec.RunContext["composition_path"] = compositionPath
	}
	compiledPlan := spec.CompiledPlan
	if compiledPlan == nil {
		var err error
		compiledPlan, err = recipes.CompileRecipeToChildPlan(spec.Recipe, spec.Profiles, spec.Recipes, recipes.CompileOptions{
			CompositionPath:      compositionPath,
			RelayBackendDepth:    intFromAny(spec.DepthPolicy["relay_backend_depth"], 0),
			MaxRelayBackendDepth: intFromAny(spec.DepthPolicy["max_relay_backend_depth"], 1),
			ValidateExecutable:   true,
		})
		if err != nil {
			return nil, childRelayConfigError(err)
		}
	}
	compiledPlanRef := spec.CompiledPlanRef
	if compiledPlanRef == nil {
		var err error
		compiledPlanRef, err = refForPayload("compiled_plan", compiledPlan)
		if err != nil {
			return nil, err
		}
	}
	childInvocation := map[string]any{
		"kind":                  "child_invocation",
		"schema_version":        1,
		"compiled_plan_ref":     compiledPlanRef,
		"task":                  spec.Task,
		"origin_kind":           spec.Origin,
		"admitted_rounds":       spec.AdmittedRounds,
		"depth_policy":          spec.DepthPolicy,
		"timeout_seconds":       spec.TimeoutSeconds,
		"stall_timeout_seconds": maxInt(spec.StallTimeoutSecond, 0),
	}
	childInvocationRef, err := refForPayload("child_invocation", childInvocation)
	if err != nil {
		return nil, err
	}
	launch, err := recipes.RecipeToChildLaunch(compiledPlan)
	if err != nil {
		return nil, err
	}
	childAgents, childSlotConfigs, err := childLaunchSlots(launch)
	if err != nil {
		return nil, err
	}
	facilitator := childLaunchFacilitator(launch)
	raw, err := Run(ctx, Options{
		RelayHome:           spec.RelayHome,
		Task:                spec.Task,
		Agents:              childAgents,
		SlotConfigs:         childSlotConfigs,
		Rounds:              spec.AdmittedRounds,
		MaxRounds:           spec.AdmittedRounds,
		TimeoutSeconds:      spec.TimeoutSeconds,
		StallTimeoutSeconds: spec.StallTimeoutSecond,
		Mode:                stringFromAny(spec.Recipe["mode"]),
		DynamicMode:         defaultDynamicMode,
		SettingsPath:        spec.SettingsPath,
		RuntimeConfig:       runtimeConfigForChildSpec(spec),
		LaunchCWD:           spec.LaunchCWD,
		FacilitatorBackend:  stringFromAny(facilitator["backend"]),
		FacilitatorModel:    stringFromAny(facilitator["model"]),
		FacilitatorEffort:   stringFromAny(facilitator["effort"]),
		RelayBackendDepth:   intFromAny(spec.DepthPolicy["relay_backend_depth"], 0) + 1,
		MaxRelayDepth:       intFromAny(spec.DepthPolicy["max_relay_backend_depth"], 1),
	})
	if err != nil {
		if raw == nil {
			return nil, err
		}
	}
	result, normalizeErr := normalizeChildRelayResult(raw, spec, compiledPlanRef, compiledPlan, childInvocationRef, childInvocation)
	if normalizeErr != nil {
		return nil, normalizeErr
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result, err
}

func runtimeConfigForChildSpec(spec childRelaySpec) recipes.RuntimeConfig {
	if runtimeConfigProvided(spec.RuntimeConfig) {
		return cloneRuntimeConfig(spec.RuntimeConfig)
	}
	return recipes.RuntimeConfig{
		BackendProfiles: mapStringObjectMap(spec.Profiles),
		RelayRecipes:    mapStringObjectMap(spec.Recipes),
		Limits:          recipes.DefaultRuntimeLimits(),
		SettingsPath:    spec.SettingsPath,
	}
}

func childRelayConfigError(err error) error {
	var configErr recipes.ChildRelayConfigError
	if !errors.As(err, &configErr) || len(configErr.Issues) == 0 {
		return err
	}
	codes := make([]string, 0, len(configErr.Issues))
	for _, issue := range configErr.Issues {
		codes = append(codes, issue.Code)
	}
	return fmt.Errorf("%s: %s", configErr.Message, strings.Join(codes, ", "))
}

func normalizeChildRelayResult(raw map[string]any, spec childRelaySpec, compiledPlanRef map[string]any, compiledPlan map[string]any, childInvocationRef map[string]any, childInvocation map[string]any) (*childRelayResult, error) {
	if raw == nil {
		return nil, fmt.Errorf("child relay returned no result")
	}
	sessionID := strings.TrimSpace(stringFromAny(raw["session_id"]))
	if sessionID == "" {
		return nil, fmt.Errorf("child relay result is missing session_id")
	}
	status := strings.TrimSpace(stringFromAny(raw["status"]))
	if status == "" {
		return nil, fmt.Errorf("child relay result is missing status")
	}
	transcript := childTranscript(raw["transcript"])
	actualRounds := intFromAny(raw["actual_rounds"], backendTranscriptCount(transcript))
	ledger := normalizeLedger(raw["ledger"])
	lastContent := lastChildBackendContent(transcript)
	childResult := map[string]any{
		"kind":                 "child_result",
		"schema_version":       1,
		"child_invocation_ref": childInvocationRef,
		"child_session_id":     sessionID,
		"status":               status,
		"stop_reason":          optionalString(raw["stop_reason"]),
		"actual_rounds":        actualRounds,
		"elapsed_seconds":      optionalFloat(raw["elapsed_seconds"]),
		"error":                optionalString(raw["error"]),
		"ledger":               ledger,
		"transcript":           transcriptContracts(transcript),
		"last_content":         lastContent,
	}
	childResultRef, err := refForPayload("child_result", childResult)
	if err != nil {
		return nil, err
	}
	tracePayload := cloneMap(raw)
	tracePayload["child_run_context"] = cloneMap(spec.RunContext)
	tracePayload["child_depth_policy"] = cloneMap(spec.DepthPolicy)
	tracePayload["child_origin"] = spec.Origin
	tracePayload["child_recipe_id"] = stringFromAny(spec.Recipe["id"])
	tracePayload["compiled_plan_ref"] = compiledPlanRef
	tracePayload["compiled_plan_contract"] = compiledPlan
	tracePayload["child_invocation_ref"] = childInvocationRef
	tracePayload["child_invocation_contract"] = childInvocation
	tracePayload["child_result_contract"] = childResult
	tracePayload["child_result_ref"] = childResultRef
	return &childRelayResult{
		SessionID:               sessionID,
		Status:                  status,
		StopReason:              raw["stop_reason"],
		Ledger:                  ledger,
		Transcript:              transcript,
		ActualRounds:            actualRounds,
		ElapsedSeconds:          optionalFloat(raw["elapsed_seconds"]),
		Error:                   raw["error"],
		Origin:                  spec.Origin,
		RecipeID:                stringFromAny(spec.Recipe["id"]),
		RunContext:              cloneMap(spec.RunContext),
		DepthPolicy:             cloneMap(spec.DepthPolicy),
		LastContent:             lastContent,
		TracePayload:            tracePayload,
		CompiledPlanRef:         compiledPlanRef,
		CompiledPlanContract:    compiledPlan,
		ChildInvocationRef:      childInvocationRef,
		ChildInvocationContract: childInvocation,
		ChildResultRef:          childResultRef,
		ChildResultContract:     childResult,
	}, nil
}

func childLaunchSlots(launch map[string]any) ([]string, []SlotConfig, error) {
	rawAgents, ok := launch["agents"].([]any)
	if !ok || len(rawAgents) != 2 {
		return nil, nil, fmt.Errorf("compiled child launch must contain exactly two agents")
	}
	agents := make([]string, 0, len(rawAgents))
	for _, rawAgent := range rawAgents {
		agent := strings.TrimSpace(stringFromAny(rawAgent))
		if agent == "" {
			return nil, nil, fmt.Errorf("compiled child launch contains an empty agent")
		}
		agents = append(agents, agent)
	}
	rawConfigs, _ := launch["slot_configs"].([]any)
	configs := make([]SlotConfig, 0, len(rawConfigs))
	for _, rawConfig := range rawConfigs {
		configMap, _ := rawConfig.(map[string]any)
		configs = append(configs, SlotConfig{
			Model:           stringFromAny(configMap["model"]),
			Effort:          stringFromAny(configMap["effort"]),
			CompositionPath: stringFromAny(configMap["composition_path"]),
		})
	}
	return agents, configs, nil
}

func childLaunchFacilitator(launch map[string]any) map[string]any {
	facilitator, _ := launch["facilitator"].(map[string]any)
	if facilitator == nil {
		facilitator = map[string]any{}
	}
	if strings.TrimSpace(stringFromAny(facilitator["backend"])) == "" {
		facilitator["backend"] = "codex"
	}
	return facilitator
}

func saveChildContractArtifacts(st *store.Store, artifactID string, result *childRelayResult, recipe map[string]any) (map[string]any, error) {
	refs := map[string]any{}
	if result == nil {
		return refs, nil
	}
	recipePayload := recipes.RecipeContractPayload(recipe)
	if recipeRef, ok := result.CompiledPlanContract["recipe_ref"].(map[string]any); ok {
		ref, err := st.SaveContractArtifact("recipes", stringFromAny(recipePayload["id"]), recipePayload, stringFromAny(recipeRef["id"]))
		if err != nil {
			return nil, err
		}
		refs["recipe_ref"] = ref
	}
	if result.CompiledPlanContract != nil && result.CompiledPlanRef != nil {
		ref, err := st.SaveContractArtifact("compiled_plans", artifactID, result.CompiledPlanContract, stringFromAny(result.CompiledPlanRef["id"]))
		if err != nil {
			return nil, err
		}
		refs["compiled_plan_ref"] = ref
	}
	if result.ChildInvocationContract != nil && result.ChildInvocationRef != nil {
		ref, err := st.SaveContractArtifact("child_invocations", artifactID, result.ChildInvocationContract, stringFromAny(result.ChildInvocationRef["id"]))
		if err != nil {
			return nil, err
		}
		refs["child_invocation_ref"] = ref
	}
	if result.ChildResultContract != nil && result.ChildResultRef != nil {
		ref, err := st.SaveContractArtifact("child_results", artifactID, result.ChildResultContract, stringFromAny(result.ChildResultRef["id"]))
		if err != nil {
			return nil, err
		}
		refs["child_result_ref"] = ref
	}
	return refs, nil
}

func buildBackendChildTask(prompt string) string {
	return "You are running as a composite convo-relay backend. Answer the parent turn by resolving " +
		"the prompt below, then collapse the child relay into one parent-facing result.\n\n" +
		"Parent prompt:\n" + prompt
}

func collapseChildResultText(result *childRelayResult, recipeID string) string {
	return fmt.Sprintf(
		"Relay backend result (%s)\n\nChild session: %s\nStatus: %s / %s\n\nParent-facing answer:\n%s\n\nProjected settled:\n%s\n\nProjected contested:\n%s\n\nProjected withdrawn:\n%s",
		recipeID,
		result.SessionID,
		result.Status,
		firstNonEmpty(stringFromAny(result.StopReason), "?"),
		firstNonEmpty(result.LastContent, "No child relay answer was produced."),
		bulletBlock(result.Ledger["settled"]),
		bulletBlock(result.Ledger["contested"]),
		bulletBlock(result.Ledger["withdrawn"]),
	)
}

func transcriptContracts(transcript []map[string]any) []any {
	items := make([]any, 0, len(transcript))
	for _, entry := range transcript {
		items = append(items, map[string]any{
			"kind":           "transcript_entry",
			"schema_version": 1,
			"round":          intFromAny(entry["round"], 0),
			"slot_id":        strings.TrimSpace(stringFromAny(entry["slot_id"])),
			"speaker":        strings.TrimSpace(firstNonEmpty(stringFromAny(entry["speaker"]), stringFromAny(entry["from"]))),
			"content":        stringFromAny(entry["content"]),
			"ledger_after":   normalizeLedger(firstNonNil(entry["ledger_after"], entry["ledger"])),
		})
	}
	return items
}

func childTranscript(value any) []map[string]any {
	rawEntries, _ := value.([]any)
	entries := make([]map[string]any, 0, len(rawEntries))
	for _, rawEntry := range rawEntries {
		if entry, ok := rawEntry.(map[string]any); ok {
			entries = append(entries, cloneMap(entry))
		}
	}
	return entries
}

func lastChildBackendContent(transcript []map[string]any) string {
	for index := len(transcript) - 1; index >= 0; index-- {
		entry := transcript[index]
		if isSyntheticChildEntry(entry) {
			continue
		}
		return stringFromAny(entry["content"])
	}
	return ""
}

func backendTranscriptCount(transcript []map[string]any) int {
	count := 0
	for _, entry := range transcript {
		if !isSyntheticChildEntry(entry) {
			count++
		}
	}
	return count
}

func isSyntheticChildEntry(entry map[string]any) bool {
	return entry["synthetic"] == true && entry["source_type"] == "child_result"
}

func normalizeChildDepthPolicy(policy map[string]any) map[string]any {
	result := cloneMap(policy)
	if result == nil {
		result = map[string]any{}
	}
	result["graph_depth"] = intFromAny(result["graph_depth"], 0)
	if result["max_graph_depth"] != nil {
		result["max_graph_depth"] = intFromAny(result["max_graph_depth"], 1)
	} else {
		result["max_graph_depth"] = nil
	}
	result["relay_backend_depth"] = intFromAny(result["relay_backend_depth"], 0)
	result["max_relay_backend_depth"] = intFromAny(result["max_relay_backend_depth"], 1)
	return result
}

func validateChildDepthPolicy(policy map[string]any, origin string) error {
	if maxGraph := policy["max_graph_depth"]; maxGraph != nil && intFromAny(policy["graph_depth"], 0) > intFromAny(maxGraph, 1) {
		return fmt.Errorf("child graph depth %d exceeds max depth %d for %s", intFromAny(policy["graph_depth"], 0), intFromAny(maxGraph, 1), origin)
	}
	if intFromAny(policy["relay_backend_depth"], 0) >= intFromAny(policy["max_relay_backend_depth"], 1) {
		return fmt.Errorf("relay backend depth %d reached max depth %d for %s", intFromAny(policy["relay_backend_depth"], 0), intFromAny(policy["max_relay_backend_depth"], 1), origin)
	}
	return nil
}

func refForPayload(prefix string, payload map[string]any) (map[string]any, error) {
	digest, err := contracts.ContractDigest(payload)
	if err != nil {
		return nil, err
	}
	return contracts.ArtifactRefForPayload(prefix+":"+strings.TrimPrefix(digest, contracts.DigestPrefix)[:16], payload)
}

func optionalString(value any) any {
	if value == nil {
		return nil
	}
	return stringFromAny(value)
}

func optionalFloat(value any) any {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nil
		}
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		text := strings.TrimSpace(stringFromAny(value))
		if text == "" {
			return nil
		}
		return value
	}
}

func bulletBlock(value any) string {
	items, _ := value.([]any)
	if len(items) == 0 {
		return "- None"
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, "- "+stringFromAny(item))
	}
	return strings.Join(parts, "\n")
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func maxInt(value int, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	cloned, _ := contracts.Materialize(value).(map[string]any)
	return cloned
}
