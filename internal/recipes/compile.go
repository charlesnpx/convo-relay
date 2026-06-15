package recipes

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

type CompileOptions struct {
	CompositionPath      string
	RelayBackendDepth    int
	MaxRelayBackendDepth int
	ValidateExecutable   bool
	TransientSources     []TransientRecipeSource
}

func CompileRecipeToChildPlan(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	options CompileOptions,
) (map[string]any, error) {
	compositionPath := strings.TrimSpace(options.CompositionPath)
	if compositionPath == "" {
		compositionPath = "root"
	}
	recipePayload := normalizeRecipePayload(recipe)
	if options.ValidateExecutable {
		maxDepth := options.MaxRelayBackendDepth
		if maxDepth <= 0 {
			maxDepth = intFromAny(recipePayload["max_depth"], 1)
		}
		if issues := ExecutableIssues(recipePayload, profiles, relayRecipes, DepthPolicy{
			RelayBackendDepth:    options.RelayBackendDepth,
			MaxRelayBackendDepth: maxDepth,
		}, compositionPath); len(issues) > 0 {
			return nil, ChildRelayConfigError{
				Message: "Child relay recipe is not executable by the in-process runner.",
				Issues:  issues,
			}
		}
	}

	participants := stringSlice(recipePayload["participants"])
	participantProfiles := make([]any, 0, len(participants))
	for index, ref := range participants {
		profile, err := compiledProfile(
			ref,
			profiles,
			relayRecipes,
			fmt.Sprintf("slot_%d", index),
			fmt.Sprintf("%s.slot_%d", compositionPath, index),
		)
		if err != nil {
			return nil, err
		}
		participantProfiles = append(participantProfiles, profile)
	}
	facilitatorProfile, err := compiledProfile(
		stringValue(recipePayload["facilitator"]),
		profiles,
		relayRecipes,
		"facilitator",
		compositionPath+".facilitator",
	)
	if err != nil {
		return nil, err
	}
	reducerProfile, err := compiledProfile(
		stringValue(recipePayload["reducer"]),
		profiles,
		relayRecipes,
		"reducer",
		compositionPath+".reducer",
	)
	if err != nil {
		return nil, err
	}

	recipeDigest, err := contracts.ContractDigest(recipePayload)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"kind":           "compiled_plan",
		"schema_version": 1,
		"recipe_ref": map[string]any{
			"kind":           "artifact_ref",
			"schema_version": 1,
			"id":             "recipe:" + stringValue(recipePayload["id"]),
			"digest":         recipeDigest,
		},
		"recipe_id":    recipePayload["id"],
		"participants": participantProfiles,
		"facilitator":  facilitatorProfile,
		"reducer":      reducerProfile,
		"mode":         recipePayload["mode"],
		"round_bounds": map[string]any{
			"max_rounds":     recipePayload["max_rounds"],
			"default_rounds": recipePayload["max_rounds"],
		},
		"depth_policy": map[string]any{
			"max_graph_depth":         recipePayload["max_depth"],
			"max_relay_backend_depth": 1,
		},
	}
	return normalizeCompiledPlanPayload(payload)
}

func RecipeToChildLaunch(compiled map[string]any) (map[string]any, error) {
	participants, ok := compiled["participants"].([]any)
	if !ok {
		return nil, contracts.NewValidationError("compiled_plan.participants must be a list")
	}
	agents := make([]any, 0, len(participants))
	slotConfigs := make([]any, 0, len(participants))
	for _, rawProfile := range participants {
		profile, ok := rawProfile.(map[string]any)
		if !ok {
			return nil, contracts.NewValidationError("compiled_plan.participants items must be objects")
		}
		agents = append(agents, stringValue(profile["backend"]))
		slotConfigs = append(slotConfigs, map[string]any{
			"model":            profile["model"],
			"effort":           profile["effort"],
			"composition_path": profile["composition_path"],
		})
	}
	facilitatorProfile, _ := compiled["facilitator"].(map[string]any)
	facilitator := map[string]any{
		"backend": stringValue(facilitatorProfile["backend"]),
		"model":   facilitatorProfile["model"],
		"effort":  facilitatorProfile["effort"],
	}
	return map[string]any{
		"agents":       agents,
		"slot_configs": slotConfigs,
		"facilitator":  facilitator,
	}, nil
}

func BuildCompileReport(
	recipeID string,
	config RuntimeConfig,
	options CompileOptions,
) (map[string]any, error) {
	recipe, ok := config.RelayRecipes[recipeID]
	if !ok {
		return nil, ChildRelayConfigError{
			Message: "Child relay recipe is not executable by the in-process runner.",
			Issues: []ChildRecipeIssue{{
				Category: "invalid_config",
				Code:     "unknown_recipe",
				Message:  fmt.Sprintf("Unknown relay recipe '%s'.", recipeID),
				Path:     "recipe",
				Detail:   map[string]any{"recipe_id": recipeID},
			}},
		}
	}
	compiled, err := CompileRecipeToChildPlan(recipe, config.BackendProfiles, config.RelayRecipes, options)
	if err != nil {
		return nil, err
	}
	launch, err := RecipeToChildLaunch(compiled)
	if err != nil {
		return nil, err
	}
	recipeDigest, err := contracts.ContractDigest(recipe)
	if err != nil {
		return nil, err
	}
	compiledDigest, err := contracts.ContractDigest(compiled)
	if err != nil {
		return nil, err
	}
	report := map[string]any{
		"recipe_id":            recipeID,
		"settings_path":        config.SettingsPath,
		"recipe":               recipe,
		"recipe_digest":        recipeDigest,
		"compiled_plan":        compiled,
		"compiled_plan_digest": compiledDigest,
		"launch":               launch,
	}
	if trace, ok := transientRecipeDigestTraces(options.TransientSources)[recipeID]; ok {
		if trace.RecipeDigest != "" && trace.RecipeDigest != recipeDigest {
			return nil, fmt.Errorf("transient recipe digest mismatch for %q: source metadata %s, compiled recipe %s", recipeID, trace.RecipeDigest, recipeDigest)
		}
		report["source_digest"] = trace.SourceDigest
		report["source_type"] = trace.SourceType
		report["source_path"] = trace.Path
		report["source_display_name"] = trace.DisplayName
	}
	return report, nil
}

func compiledProfile(
	ref string,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	slotID string,
	compositionPath string,
) (map[string]any, error) {
	profile, err := ResolveProfileRef(ref, profiles)
	if err != nil {
		return nil, err
	}
	backend := stringValue(profile["backend"])
	model := profile["model"]
	effort := profile["effort"]
	if backend == "relay" {
		childRecipeID := stringValue(model)
		childRecipe, ok := relayRecipes[childRecipeID]
		if effort == nil && ok {
			effort = intFromAny(childRecipe["max_rounds"], 1)
		}
	}
	return map[string]any{
		"slot_id":          slotID,
		"profile_id":       fallbackString(profile["id"], ref),
		"backend":          backend,
		"model":            model,
		"effort":           effort,
		"composition_path": compositionPath,
		"capabilities":     cleanStringList(profile["capabilities"], false),
	}, nil
}

func ResolveProfileRef(ref string, profiles map[string]map[string]any) (map[string]any, error) {
	profileRef := strings.TrimSpace(ref)
	if profile, ok := profiles[profileRef]; ok {
		return cloneObject(profile), nil
	}
	if backendRegistry[profileRef] {
		return map[string]any{
			"id":           profileRef,
			"backend":      profileRef,
			"model":        nil,
			"effort":       nil,
			"description":  fmt.Sprintf("Direct %s backend", profileRef),
			"capabilities": []any{},
		}, nil
	}
	return nil, fmt.Errorf("Unknown backend profile or backend '%s'", profileRef)
}
