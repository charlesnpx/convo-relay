package recipes

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
)

type CompileTarget string

const (
	CompileTargetRoot  CompileTarget = "root"
	CompileTargetChild CompileTarget = "child"

	// A root plan materializes one schedule record per participant turn. This
	// bound keeps malformed or impractical configuration from allocating an
	// unbounded plan before execution can reject it.
	maxCompiledParticipantTurns = 10_000
)

type CompileOptions struct {
	CompositionPath      string
	RelayBackendDepth    int
	MaxRelayBackendDepth int
	ValidateExecutable   bool
	TransientSources     []TransientRecipeSource
	IntegrationBundle    *integration.Bundle
}

// RootOnlyRecipeError reports an integration-bound recipe that cannot be
// compiled for nested or dynamic child execution.
type RootOnlyRecipeError struct {
	RecipeID            string `json:"recipe_id"`
	IntegrationContract string `json:"integration_contract"`
}

func (e *RootOnlyRecipeError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("recipe %q declares integration contract %q and can only compile for the root target", e.RecipeID, e.IntegrationContract)
}

func (e *RootOnlyRecipeError) ToMap() map[string]any {
	if e == nil {
		return map[string]any{}
	}
	return map[string]any{
		"code":                 "root_only_recipe",
		"message":              e.Error(),
		"recipe_id":            e.RecipeID,
		"integration_contract": e.IntegrationContract,
	}
}

// CompileRecipe is the sole canonical recipe compiler. Callers must choose a
// target explicitly; integration_contract never selects a target implicitly.
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
		return nil, contracts.NewDiagnosticError("Relay recipe configuration is invalid.", diagnostics...)
	}
	recipePayload := normalizeRecipePayload(recipe)
	var compiled map[string]any
	var err error
	if target == CompileTargetChild {
		if contractID := stringValue(recipePayload["integration_contract"]); strings.TrimSpace(contractID) != "" {
			return nil, &RootOnlyRecipeError{
				RecipeID:            stringValue(recipePayload["id"]),
				IntegrationContract: contractID,
			}
		}
		compiled, err = compileChildPlan(recipePayload, profiles, relayRecipes, options)
	} else {
		compiled, err = compileRootPlan(recipePayload, profiles, relayRecipes, options)
	}
	if err != nil {
		return nil, err
	}
	if options.ValidateExecutable {
		closureOptions := options
		if closureOptions.MaxRelayBackendDepth <= 0 {
			closureOptions.MaxRelayBackendDepth = intFromAny(recipePayload["max_depth"], 1)
		}
		visited := map[string]bool{strings.TrimSpace(stringValue(recipePayload["id"])): true}
		if err := validateNestedChildCompileTargets(recipePayload, profiles, relayRecipes, closureOptions, visited); err != nil {
			return nil, err
		}
	}
	return compiled, nil
}

// validateNestedChildCompileTargets closes the gap between validating relay
// linkage and validating the recipes reached through that linkage. Every
// reachable relay participant is compiled under child rules before the parent
// plan can cross a persistence boundary.
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
		if err != nil || stringValue(profile["backend"]) != "relay" {
			continue
		}
		childRecipeID := strings.TrimSpace(stringValue(profile["model"]))
		childRecipe, exists := relayRecipes[childRecipeID]
		if !exists || childRecipe == nil || visited[childRecipeID] {
			continue
		}
		if diagnostics := validateRecipeRecord(childRecipe, "/recipe"); len(diagnostics) > 0 {
			return contracts.NewDiagnosticError("Relay recipe configuration is invalid.", diagnostics...)
		}
		childPayload := normalizeRecipePayload(childRecipe)
		if contractID := strings.TrimSpace(stringValue(childPayload["integration_contract"])); contractID != "" {
			return &RootOnlyRecipeError{
				RecipeID:            stringValue(childPayload["id"]),
				IntegrationContract: contractID,
			}
		}

		childOptions := options
		childOptions.CompositionPath = fmt.Sprintf("%s.slot_%d", compositionPath, index)
		childOptions.RelayBackendDepth = options.RelayBackendDepth + 1
		childOptions.IntegrationBundle = nil
		if _, err := compileChildPlan(childPayload, profiles, relayRecipes, childOptions); err != nil {
			return err
		}
		visited[childRecipeID] = true
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
		return contracts.NewValidationError("recipe compile target is required")
	default:
		return contracts.NewValidationError("unknown recipe compile target %q", target)
	}
}

func compileChildPlan(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	options CompileOptions,
) (map[string]any, error) {
	compositionPath := strings.TrimSpace(options.CompositionPath)
	if compositionPath == "" {
		compositionPath = "root"
	}
	// compiled_plan/v1 deliberately references the legacy child projection.
	// The full normalized recipe remains available to root compilation.
	recipePayload := normalizeLegacyRecipePayload(recipe)
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

func compileRootPlan(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	options CompileOptions,
) (map[string]any, error) {
	compositionPath := defaultCompositionPath(options.CompositionPath)
	if options.ValidateExecutable {
		if issues := rootExecutableIssues(recipe, profiles, relayRecipes, DepthPolicy{
			RelayBackendDepth:    options.RelayBackendDepth,
			MaxRelayBackendDepth: options.MaxRelayBackendDepth,
		}, compositionPath); len(issues) > 0 {
			return nil, ChildRelayConfigError{
				Message: "Root recipe is not executable by the in-process runner.",
				Issues:  issues,
			}
		}
	}
	participants := stringSlice(recipe["participants"])
	if len(participants) != 2 {
		return nil, contracts.NewValidationError("root recipe participants must contain exactly two entries")
	}
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
			return nil, rootCompileDiagnostic(
				"invalid_root_participant",
				fmt.Sprintf("/participants/%d", index),
				"Root recipe participant profile could not be resolved.",
				map[string]any{"profile_ref": ref, "cause": err.Error()},
			)
		}
		participantProfiles = append(participantProfiles, profile)
	}

	facilitatorRef := strings.TrimSpace(stringValue(recipe["facilitator"]))
	facilitatorProfile, err := compiledProfile(
		facilitatorRef,
		profiles,
		relayRecipes,
		"facilitator",
		compositionPath+".facilitator",
	)
	if err != nil {
		return nil, rootCompileDiagnostic(
			"invalid_root_facilitator",
			"/facilitator",
			"Root recipe facilitator profile could not be resolved.",
			map[string]any{"profile_ref": facilitatorRef, "cause": err.Error()},
		)
	}
	if stringValue(facilitatorProfile["backend"]) == "relay" {
		return nil, rootCompileDiagnostic(
			"invalid_root_facilitator",
			"/facilitator",
			"Root recipe facilitator must resolve to a non-relay provider.",
			map[string]any{"profile_ref": facilitatorRef},
		)
	}

	participantTurns, err := validatedRootParticipantTurns(recipe)
	if err != nil {
		return nil, err
	}
	scheduledTurns, err := integration.AlternatingSchedule(participantTurns)
	if err != nil {
		return nil, contracts.NewValidationError("root recipe participant schedule: %v", err)
	}
	participantSchedule := make([]any, 0, len(scheduledTurns))
	for _, turn := range scheduledTurns {
		participantSchedule = append(participantSchedule, map[string]any{
			"participant_turn": turn.ParticipantTurn,
			"slot":             turn.Slot,
		})
	}

	resultSource := normalizeResultSource(recipe["result_source"])
	var reducerProfile map[string]any
	if resultSource == integration.ResultSourceReducer {
		reducerRef := strings.TrimSpace(stringValue(recipe["reducer"]))
		reducerProfile, err = compiledProfile(
			reducerRef,
			profiles,
			relayRecipes,
			"reducer",
			compositionPath+".reducer",
		)
		if err != nil {
			return nil, rootCompileDiagnostic(
				"invalid_root_reducer",
				"/reducer",
				"Root recipe reducer profile could not be resolved.",
				map[string]any{"profile_ref": reducerRef, "cause": err.Error()},
			)
		}
		if stringValue(reducerProfile["backend"]) == "relay" {
			return nil, rootCompileDiagnostic(
				"invalid_root_reducer",
				"/reducer",
				"Root recipe reducer must resolve to a non-relay provider.",
				map[string]any{"profile_ref": reducerRef},
			)
		}
	}

	recipePayload := normalizeRecipePayload(recipe)
	recipeRef, err := contracts.ArtifactRefForPayload("recipe:"+stringValue(recipePayload["id"]), recipePayload)
	if err != nil {
		return nil, err
	}
	lifecycle := normalizeLifecyclePayload(recipePayload["lifecycle"])
	planFields := map[string]any{
		"recipe_id":                   recipePayload["id"],
		"recipe_ref":                  recipeRef,
		"participant_turns":           participantTurns,
		"participant_schedule":        participantSchedule,
		"participants":                participantProfiles,
		"facilitator":                 facilitatorProfile,
		"result_source":               resultSource,
		"lifecycle":                   lifecycle,
		"workspace_isolation_minimum": lifecycle["workspace_isolation"],
		"mode":                        recipePayload["mode"],
		"auto_approval":               recipePayload["auto_approval"],
		"required_capabilities":       recipePayload["required_capabilities"],
		"depth_policy": map[string]any{
			"max_graph_depth": recipePayload["max_depth"],
		},
	}
	_, providerRetryRepresented := recipePayload["provider_retry"]
	if providerRetryRepresented {
		planFields["provider_retry"] = EffectiveProviderRetry(recipePayload)
	}
	if reducerProfile != nil {
		planFields["reducer"] = reducerProfile
	}

	contractID := stringValue(recipePayload["integration_contract"])
	if strings.TrimSpace(contractID) != "" {
		selected, err := integration.SelectContract(options.IntegrationBundle, contractID, integration.ScheduleRequirement{
			Turns:        scheduledTurns,
			ResultSource: resultSource,
		})
		if err != nil {
			return nil, err
		}
		bundleArtifact, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindIntegrationBundle, map[string]any{
			"bundle_id":     options.IntegrationBundle.ID(),
			"bundle_digest": options.IntegrationBundle.Digest(),
			"bundle":        options.IntegrationBundle.ToMap(),
		})
		if err != nil {
			return nil, err
		}
		bundleRef, err := contracts.RootArtifactRefForPayload(contracts.RootArtifactKindIntegrationBundle, 0, bundleArtifact)
		if err != nil {
			return nil, err
		}
		contractArtifact, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindIntegrationContract, map[string]any{
			"contract_id":     selected.ID(),
			"contract_digest": selected.Digest(),
			"contract":        selected.ToMap(),
		})
		if err != nil {
			return nil, err
		}
		contractRef, err := contracts.RootArtifactRefForPayload(contracts.RootArtifactKindIntegrationContract, 0, contractArtifact)
		if err != nil {
			return nil, err
		}
		planFields["integration_bundle_ref"] = bundleRef
		planFields["integration_bundle_digest"] = options.IntegrationBundle.Digest()
		planFields["integration_contract_ref"] = contractRef
		planFields["integration_contract_id"] = selected.ID()
		planFields["integration_contract_digest"] = selected.Digest()
	}
	if providerRetryRepresented {
		return contracts.NormalizeRootArtifactVersion(contracts.RootArtifactKindRootRecipePlan, contracts.RootArtifactSchemaVersionV2, planFields)
	}
	return contracts.NormalizeRootArtifact(contracts.RootArtifactKindRootRecipePlan, planFields)
}

func validatedRootParticipantTurns(recipe map[string]any) (int, error) {
	participantTurns := intFromAny(recipe["participant_turns"], intFromAny(recipe["max_rounds"], 1))
	if participantTurns > maxCompiledParticipantTurns {
		return 0, rootCompileDiagnostic(
			DiagnosticCodeInvalidParticipantTurns,
			"/participant_turns",
			fmt.Sprintf("Root recipe participant_turns must not exceed %d.", maxCompiledParticipantTurns),
			map[string]any{"participant_turns": participantTurns, "maximum": maxCompiledParticipantTurns},
		)
	}
	return participantTurns, nil
}

func rootCompileDiagnostic(code string, path string, message string, details map[string]any) error {
	diagnostic := contracts.NewDiagnostic(code, contracts.DiagnosticPhasePreflight, path, message, details)
	return contracts.NewDiagnosticError(message, diagnostic)
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
	target CompileTarget,
	options CompileOptions,
) (map[string]any, error) {
	if err := validateCompileTarget(target); err != nil {
		return nil, err
	}
	recipe, ok := config.RelayRecipes[recipeID]
	if !ok {
		return nil, ChildRelayConfigError{
			Message: "Relay recipe is not available for compilation.",
			Issues: []ChildRecipeIssue{{
				Category: "invalid_config",
				Code:     "unknown_recipe",
				Message:  fmt.Sprintf("Unknown relay recipe '%s'.", recipeID),
				Path:     "recipe",
				Detail:   map[string]any{"recipe_id": recipeID},
			}},
		}
	}
	compiled, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, target, options)
	if err != nil {
		return nil, err
	}
	var recipePayload map[string]any
	if target == CompileTargetChild {
		recipePayload = ChildRecipeContractPayload(recipe)
	} else {
		recipePayload = RecipeContractPayload(recipe)
	}
	recipeDigest, err := contracts.ContractDigest(recipePayload)
	if err != nil {
		return nil, err
	}
	compiledDigest, err := contracts.ContractDigest(compiled)
	if err != nil {
		return nil, err
	}
	report := map[string]any{
		"recipe_id":            recipeID,
		"target":               string(target),
		"settings_path":        config.SettingsPath,
		"recipe":               recipe,
		"recipe_digest":        recipeDigest,
		"compiled_plan":        compiled,
		"compiled_plan_digest": compiledDigest,
	}
	if target == CompileTargetChild {
		launch, err := RecipeToChildLaunch(compiled)
		if err != nil {
			return nil, err
		}
		report["launch"] = launch
	}
	if trace, ok := transientRecipeDigestTraces(options.TransientSources)[recipeID]; ok {
		if target == CompileTargetChild && trace.RecipeDigest != "" && trace.RecipeDigest != recipeDigest {
			return nil, fmt.Errorf("transient recipe digest mismatch for %q: source metadata %s, compiled recipe %s", recipeID, trace.RecipeDigest, recipeDigest)
		}
		report["source_digest"] = trace.SourceDigest
		report["source_type"] = trace.SourceType
		report["source_path"] = trace.Path
		report["source_display_name"] = trace.DisplayName
		if target == CompileTargetRoot && trace.RecipeDigest != "" && trace.RecipeDigest != recipeDigest {
			report["source_recipe_digest"] = trace.RecipeDigest
		}
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
