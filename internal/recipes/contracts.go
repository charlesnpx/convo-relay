package recipes

import (
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func normalizeRecipePayload(data map[string]any) map[string]any {
	participants := cleanStringList(data["participants"], false)
	requiredCapabilities := cleanStringList(data["required_capabilities"], false)
	matchKeywords := cleanStringList(data["match_keywords"], true)
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
		"match_keywords":        matchKeywords,
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
			"max_graph_depth":         positiveInt(depthPolicy["max_graph_depth"], 1),
			"max_relay_backend_depth": positiveInt(depthPolicy["max_relay_backend_depth"], 1),
		},
	}, nil
}
