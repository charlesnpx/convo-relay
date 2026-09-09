package recipes

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/v2/internal/format"
)

type CompileTarget string

const (
	CompileTargetRoot  CompileTarget = "root"
	CompileTargetChild CompileTarget = "child"

	maxCompiledParticipantTurns = 10_000
)

type CompileOptions struct {
	CompositionPath      string
	RelayBackendDepth    int
	MaxRelayBackendDepth int
	ValidateExecutable   bool
	TransientSources     []TransientRecipeSource
}

func AlternatingSchedule(participantTurns int) ([]struct {
	ParticipantTurn int
	Slot            string
}, error) {
	if participantTurns < 1 {
		return nil, fmt.Errorf("participant turns must be positive")
	}
	turns := make([]struct {
		ParticipantTurn int
		Slot            string
	}, participantTurns)
	for index := range turns {
		turns[index] = struct {
			ParticipantTurn int
			Slot            string
		}{
			ParticipantTurn: index + 1,
			Slot:            fmt.Sprintf("slot_%d", index%2),
		}
	}
	return turns, nil
}

// CompileRecipe checks a recipe and returns a non-durable plan preview. The
// only durable plan is session.Plan, produced by plan.FromRecipe at launch.
func CompileRecipe(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	target CompileTarget,
	options CompileOptions,
) (map[string]any, error) {
	if err := validateCompileTarget(target); err != nil {
		return nil, err
	}
	if diagnostics := validateRecipeRecord(recipe, "/recipe"); len(diagnostics) > 0 {
		return nil, format.NewDiagnosticError("Relay recipe configuration is invalid.", diagnostics...)
	}
	payload := normalizeRecipePayload(recipe)
	var (
		compiled map[string]any
		err      error
	)
	if target == CompileTargetChild {
		compiled, err = compileChildPlan(payload, profiles, relayRecipes, options)
	} else {
		compiled, err = compileRootPlan(payload, profiles, relayRecipes, options)
	}
	if err != nil {
		return nil, err
	}
	if options.ValidateExecutable {
		closureOptions := options
		if closureOptions.MaxRelayBackendDepth <= 0 {
			closureOptions.MaxRelayBackendDepth = intFromAny(payload["max_depth"], 1)
		}
		visited := map[string]bool{strings.TrimSpace(stringValue(payload["id"])): true}
		if err := validateNestedChildCompileTargets(payload, profiles, relayRecipes, closureOptions, visited); err != nil {
			return nil, err
		}
	}
	return compiled, nil
}

func validateNestedChildCompileTargets(
	parent map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	options CompileOptions,
	visited map[string]bool,
) error {
	compositionPath := defaultCompositionPath(options.CompositionPath)
	for index, ref := range stringSlice(parent["participants"]) {
		profile, err := ResolveProfileRef(ref, profiles)
		if err != nil || stringValue(profile["backend"]) != "child" {
			continue
		}
		childID := strings.TrimSpace(stringValue(profile["model"]))
		childRecipe, exists := relayRecipes[childID]
		if !exists || childRecipe == nil || visited[childID] {
			continue
		}
		if diagnostics := validateRecipeRecord(childRecipe, "/recipe"); len(diagnostics) > 0 {
			return format.NewDiagnosticError("Relay recipe configuration is invalid.", diagnostics...)
		}
		childPayload := normalizeRecipePayload(childRecipe)
		childOptions := options
		childOptions.CompositionPath = fmt.Sprintf("%s.slot_%d", compositionPath, index)
		childOptions.RelayBackendDepth++
		if _, err := compileChildPlan(childPayload, profiles, relayRecipes, childOptions); err != nil {
			return err
		}
		visited[childID] = true
		if err := validateNestedChildCompileTargets(childPayload, profiles, relayRecipes, childOptions, visited); err != nil {
			return err
		}
	}
	return nil
}

func validateCompileTarget(target CompileTarget) error {
	switch target {
	case CompileTargetRoot, CompileTargetChild:
		return nil
	case "":
		return format.NewValidationError("recipe compile target is required")
	default:
		return format.NewValidationError("unknown recipe compile target %q", target)
	}
}

func compileChildPlan(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	options CompileOptions,
) (map[string]any, error) {
	compositionPath := defaultCompositionPath(options.CompositionPath)
	if options.ValidateExecutable {
		maxDepth := options.MaxRelayBackendDepth
		if maxDepth <= 0 {
			maxDepth = intFromAny(recipe["max_depth"], 1)
		}
		if issues := ExecutableIssues(recipe, profiles, relayRecipes, DepthPolicy{RelayBackendDepth: options.RelayBackendDepth, MaxRelayBackendDepth: maxDepth}, compositionPath); len(issues) > 0 {
			return nil, ChildRelayConfigError{Message: "Child relay recipe is not executable by the in-process runner.", Issues: issues}
		}
	}
	participants, err := compileParticipants(recipe, profiles, relayRecipes, compositionPath)
	if err != nil {
		return nil, err
	}
	facilitator, err := compiledProfile(stringValue(recipe["facilitator"]), profiles, relayRecipes, "facilitator", compositionPath+".facilitator")
	if err != nil {
		return nil, err
	}
	reducer, err := compiledProfile(stringValue(recipe["reducer"]), profiles, relayRecipes, "reducer", compositionPath+".reducer")
	if err != nil {
		return nil, err
	}
	return preview(recipe, participants, facilitator, reducer), nil
}

func compileRootPlan(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	options CompileOptions,
) (map[string]any, error) {
	compositionPath := defaultCompositionPath(options.CompositionPath)
	if options.ValidateExecutable {
		if issues := rootExecutableIssues(recipe, profiles, relayRecipes, DepthPolicy{RelayBackendDepth: options.RelayBackendDepth, MaxRelayBackendDepth: options.MaxRelayBackendDepth}, compositionPath); len(issues) > 0 {
			return nil, ChildRelayConfigError{Message: "Root recipe is not executable by the in-process runner.", Issues: issues}
		}
	}
	participants, err := compileParticipants(recipe, profiles, relayRecipes, compositionPath)
	if err != nil {
		return nil, rootCompileDiagnostic("invalid_root_participant", "/participants", "Root recipe participant profile could not be resolved.", map[string]any{"cause": err.Error()})
	}
	facilitator, err := compiledProfile(stringValue(recipe["facilitator"]), profiles, relayRecipes, "facilitator", compositionPath+".facilitator")
	if err != nil {
		return nil, rootCompileDiagnostic("invalid_root_facilitator", "/facilitator", "Root recipe facilitator profile could not be resolved.", map[string]any{"cause": err.Error()})
	}
	if stringValue(facilitator["kind"]) == "child_step" {
		return nil, rootCompileDiagnostic("invalid_root_facilitator", "/facilitator", "Root recipe facilitator must resolve to a provider, not a child step.", nil)
	}
	turns, err := validatedRootParticipantTurns(recipe)
	if err != nil {
		return nil, err
	}
	schedule, err := AlternatingSchedule(turns)
	if err != nil {
		return nil, format.NewValidationError("root recipe participant schedule: %v", err)
	}
	participantSchedule := make([]any, 0, len(schedule))
	for _, turn := range schedule {
		participantSchedule = append(participantSchedule, map[string]any{"participant_turn": turn.ParticipantTurn, "slot": turn.Slot})
	}
	resultSource := normalizeResultSource(recipe["result_source"])
	var reducer map[string]any
	if resultSource == ResultSourceReducer {
		reducer, err = compiledProfile(stringValue(recipe["reducer"]), profiles, relayRecipes, "reducer", compositionPath+".reducer")
		if err != nil {
			return nil, rootCompileDiagnostic("invalid_root_reducer", "/reducer", "Root recipe reducer profile could not be resolved.", map[string]any{"cause": err.Error()})
		}
		if stringValue(reducer["kind"]) == "child_step" {
			return nil, rootCompileDiagnostic("invalid_root_reducer", "/reducer", "Root recipe reducer must resolve to a provider, not a child step.", nil)
		}
	}
	result := preview(recipe, participants, facilitator, reducer)
	result["participant_turns"] = turns
	result["participant_schedule"] = participantSchedule
	result["result_source"] = resultSource
	lifecycle := normalizeLifecyclePayload(recipe["lifecycle"])
	result["lifecycle"] = lifecycle
	result["workspace_mode"] = "current"
	if lifecycle["workspace_isolation"] == "ephemeral" {
		result["workspace_mode"] = "head-copy"
	}
	return result, nil
}

func compileParticipants(recipe map[string]any, profiles map[string]map[string]any, relayRecipes map[string]map[string]any, compositionPath string) ([]any, error) {
	participants := stringSlice(recipe["participants"])
	if len(participants) != 2 {
		return nil, format.NewValidationError("recipe participants must contain exactly two entries")
	}
	compiled := make([]any, 0, len(participants))
	for index, ref := range participants {
		profile, err := compiledProfile(ref, profiles, relayRecipes, fmt.Sprintf("slot_%d", index), fmt.Sprintf("%s.slot_%d", compositionPath, index))
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, profile)
	}
	return compiled, nil
}

func preview(recipe map[string]any, participants []any, facilitator map[string]any, reducer map[string]any) map[string]any {
	result := map[string]any{
		"recipe_id":    recipe["id"],
		"participants": participants,
		"facilitator":  facilitator,
		"mode":         recipe["mode"],
		"round_bounds": map[string]any{
			"max_rounds":     recipe["max_rounds"],
			"default_rounds": recipe["max_rounds"],
		},
		"depth_policy":   map[string]any{"max_graph_depth": recipe["max_depth"], "max_child_depth": recipe["max_depth"]},
		"provider_retry": recipe["provider_retry"],
	}
	if reducer != nil {
		result["reducer"] = reducer
	}
	return result
}

func validatedRootParticipantTurns(recipe map[string]any) (int, error) {
	turns := intFromAny(recipe["participant_turns"], intFromAny(recipe["max_rounds"], 1))
	if turns > maxCompiledParticipantTurns {
		return 0, rootCompileDiagnostic(DiagnosticCodeInvalidParticipantTurns, "/participant_turns", fmt.Sprintf("Root recipe participant_turns must not exceed %d.", maxCompiledParticipantTurns), map[string]any{"participant_turns": turns, "maximum": maxCompiledParticipantTurns})
	}
	return turns, nil
}

func rootCompileDiagnostic(code string, path string, message string, details map[string]any) error {
	diagnostic := format.NewDiagnostic(code, format.DiagnosticPhasePreflight, path, message, details)
	return format.NewDiagnosticError(message, diagnostic)
}

func BuildCompileReport(recipeID string, config RuntimeConfig, target CompileTarget, options CompileOptions) (map[string]any, error) {
	if err := validateCompileTarget(target); err != nil {
		return nil, err
	}
	recipe, ok := config.RelayRecipes[recipeID]
	if !ok {
		return nil, ChildRelayConfigError{Message: "Relay recipe is not available for compilation.", Issues: []ChildRecipeIssue{{Category: "invalid_config", Code: "unknown_recipe", Message: fmt.Sprintf("Unknown relay recipe '%s'.", recipeID), Path: "recipe", Detail: map[string]any{"recipe_id": recipeID}}}}
	}
	compiled, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, target, options)
	if err != nil {
		return nil, err
	}
	payload := RecipePayload(recipe)
	recipeDigest, err := format.SemanticJSONDigest(payload)
	if err != nil {
		return nil, err
	}
	compiledDigest, err := format.SemanticJSONDigest(compiled)
	if err != nil {
		return nil, err
	}
	report := map[string]any{
		"recipe_id":            recipeID,
		"target":               string(target),
		"settings_path":        config.SettingsPath,
		"recipe":               payload,
		"recipe_digest":        recipeDigest,
		"compiled_plan":        compiled,
		"compiled_plan_digest": compiledDigest,
	}
	if trace, ok := transientRecipeDigestTraces(options.TransientSources)[recipeID]; ok {
		report["source_digest"] = trace.SourceDigest
		report["source_type"] = trace.SourceType
		report["source_path"] = trace.Path
		report["source_display_name"] = trace.DisplayName
	}
	return report, nil
}

func compiledProfile(ref string, profiles map[string]map[string]any, relayRecipes map[string]map[string]any, slotID string, compositionPath string) (map[string]any, error) {
	profile, err := ResolveProfileRef(ref, profiles)
	if err != nil {
		return nil, err
	}
	backend := stringValue(profile["backend"])
	model := profile["model"]
	effort := profile["effort"]
	if backend == "child" {
		childID := stringValue(model)
		if effort == nil {
			if childRecipe, ok := relayRecipes[childID]; ok {
				effort = intFromAny(childRecipe["max_rounds"], 1)
			}
		}
		return map[string]any{"kind": "child_step", "slot_id": slotID, "profile_id": fallbackString(profile["id"], ref), "recipe_id": childID, "turns": effort, "composition_path": compositionPath}, nil
	}
	return map[string]any{"slot_id": slotID, "profile_id": fallbackString(profile["id"], ref), "backend": backend, "model": model, "effort": effort, "composition_path": compositionPath}, nil
}

func ResolveProfileRef(ref string, profiles map[string]map[string]any) (map[string]any, error) {
	profileRef := strings.TrimSpace(ref)
	if profile, ok := profiles[profileRef]; ok {
		return cloneObject(profile), nil
	}
	if backendRegistry[profileRef] {
		return map[string]any{"id": profileRef, "backend": profileRef, "model": nil, "effort": nil, "description": fmt.Sprintf("Direct %s backend", profileRef)}, nil
	}
	return nil, fmt.Errorf("Unknown backend profile or backend '%s'", profileRef)
}
