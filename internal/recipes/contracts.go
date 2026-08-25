package recipes

import (
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

const (
	ProviderRetryAllow  = "allow"
	ProviderRetryForbid = "forbid"
)

func normalizeRecipePayload(data map[string]any) map[string]any {
	payload := normalizeLegacyRecipePayload(data)
	participantTurns := positiveInt(data["participant_turns"], intFromAny(payload["max_rounds"], 1))
	payload["participant_turns"] = participantTurns
	payload["result_source"] = normalizeResultSource(data["result_source"])
	payload["lifecycle"] = normalizeLifecyclePayload(data["lifecycle"])
	if integrationContract := stringValue(data["integration_contract"]); strings.TrimSpace(integrationContract) != "" {
		payload["integration_contract"] = integrationContract
	}
	if _, represented := data["provider_retry"]; represented {
		payload["schema_version"] = 2
		payload["provider_retry"] = EffectiveProviderRetry(data)
	}
	return payload
}

// normalizeLegacyRecipePayload is the stable recipe projection referenced by
// compiled_plan/v1. Root-only recipe fields deliberately do not enter this
// payload so existing child plans, refs, and persisted fixtures retain their
// established digests.
func normalizeLegacyRecipePayload(data map[string]any) map[string]any {
	participants := cleanStringList(data["participants"], false)
	requiredCapabilities := cleanStringList(data["required_capabilities"], false)
	payload := map[string]any{
		"kind":                  "recipe",
		"schema_version":        1,
		"id":                    strings.TrimSpace(stringValue(data["id"])),
		"purpose":               stringValue(data["purpose"]),
		"participants":          participants,
		"facilitator":           strings.TrimSpace(stringValue(data["facilitator"])),
		"reducer":               strings.TrimSpace(stringValue(data["reducer"])),
		"mode":                  normalizeMode(data["mode"]),
		"max_rounds":            positiveInt(data["max_rounds"], 1),
		"max_depth":             positiveInt(data["max_depth"], 1),
		"required_capabilities": requiredCapabilities,
		"auto_approval":         normalizeAutoApproval(data["auto_approval"]),
	}
	if strings.TrimSpace(stringValue(data["origin"])) == "generated" {
		payload["origin"] = "generated"
		if generatedFromRef := strings.TrimSpace(stringValue(data["generated_from_ref"])); generatedFromRef != "" {
			payload["generated_from_ref"] = generatedFromRef
		}
		if generatedSource := strings.TrimSpace(stringValue(data["generated_source"])); generatedSource != "" {
			payload["generated_source"] = generatedSource
		}
		if generatedRecipeID := strings.TrimSpace(stringValue(data["generated_recipe_id"])); generatedRecipeID != "" {
			payload["generated_recipe_id"] = generatedRecipeID
		}
	}
	return payload
}

func RecipeContractPayload(data map[string]any) map[string]any {
	return normalizeRecipePayload(data)
}

// ChildRecipeContractPayload returns the compatibility projection used by
// compiled_plan/v1 and its persisted recipe artifact.
func ChildRecipeContractPayload(data map[string]any) map[string]any {
	return normalizeLegacyRecipePayload(data)
}

func normalizeResultSource(value any) string {
	switch strings.TrimSpace(stringValue(value)) {
	case "reducer":
		return "reducer"
	default:
		return "last_turn"
	}
}

func EffectiveProviderRetry(recipe map[string]any) string {
	if strings.TrimSpace(stringValue(recipe["provider_retry"])) == ProviderRetryForbid {
		return ProviderRetryForbid
	}
	return ProviderRetryAllow
}

func normalizeLifecyclePayload(value any) map[string]any {
	lifecycle, _ := value.(map[string]any)
	return map[string]any{
		"resume":              normalizeAllowForbid(lifecycle["resume"]),
		"steering":            normalizeAllowForbid(lifecycle["steering"]),
		"dynamic":             normalizeAllowForbid(lifecycle["dynamic"]),
		"workspace_isolation": normalizeWorkspaceIsolation(lifecycle["workspace_isolation"]),
	}
}

func normalizeAllowForbid(value any) string {
	if strings.TrimSpace(stringValue(value)) == "forbid" {
		return "forbid"
	}
	return "allow"
}

func normalizeWorkspaceIsolation(value any) string {
	switch strings.TrimSpace(stringValue(value)) {
	case "read_only":
		return "read_only"
	case "ephemeral":
		return "ephemeral"
	default:
		return "inherited"
	}
}

func normalizeCompiledPlanPayload(data map[string]any) (map[string]any, error) {
	recipeRef, err := contracts.ValidateArtifactRef(data["recipe_ref"])
	if err != nil {
		return nil, err
	}
	participants, ok := data["participants"].([]any)
	if !ok || len(participants) != 2 {
		return nil, contracts.NewValidationError("compiled_plan.participants must contain exactly two entries")
	}
	facilitator, ok := data["facilitator"].(map[string]any)
	if !ok {
		return nil, contracts.NewValidationError("facilitator must be an object")
	}
	reducer, ok := data["reducer"].(map[string]any)
	if !ok {
		return nil, contracts.NewValidationError("reducer must be an object")
	}
	roundBounds, ok := data["round_bounds"].(map[string]any)
	if !ok {
		return nil, contracts.NewValidationError("round_bounds must be an object")
	}
	depthPolicy, ok := data["depth_policy"].(map[string]any)
	if !ok {
		return nil, contracts.NewValidationError("depth_policy must be an object")
	}
	return map[string]any{
		"kind":           "compiled_plan",
		"schema_version": 1,
		"recipe_ref":     recipeRef,
		"recipe_id":      strings.TrimSpace(stringValue(data["recipe_id"])),
		"participants":   participants,
		"facilitator":    facilitator,
		"reducer":        reducer,
		"mode":           normalizeMode(data["mode"]),
		"round_bounds": map[string]any{
			"max_rounds":     positiveInt(roundBounds["max_rounds"], 1),
			"default_rounds": positiveInt(roundBounds["default_rounds"], 1),
		},
		"depth_policy": map[string]any{
			"max_graph_depth": positiveInt(depthPolicy["max_graph_depth"], 1),
			"max_child_depth": positiveInt(depthPolicy["max_child_depth"], 1),
		},
	}, nil
}
