package recipes

import "strings"

const (
	ProviderRetryAllow  = "allow"
	ProviderRetryForbid = "forbid"

	ResultSourceLastTurn = "last_turn"
	ResultSourceReducer  = "reducer"
)

// normalizeRecipePayload is the one configuration projection consumed by the
// typed plan compiler. It deliberately has no independent version envelope:
// recipes are launch configuration, while session.Plan is the durable format.
func normalizeRecipePayload(data map[string]any) map[string]any {
	payload := map[string]any{
		"id":                strings.TrimSpace(stringValue(data["id"])),
		"purpose":           stringValue(data["purpose"]),
		"participants":      cleanStringList(data["participants"], false),
		"facilitator":       strings.TrimSpace(stringValue(data["facilitator"])),
		"reducer":           strings.TrimSpace(stringValue(data["reducer"])),
		"mode":              normalizeMode(data["mode"]),
		"max_rounds":        positiveInt(data["max_rounds"], 1),
		"max_depth":         positiveInt(data["max_depth"], 1),
		"auto_approval":     normalizeAutoApproval(data["auto_approval"]),
		"provider_retry":    EffectiveProviderRetry(data),
		"participant_turns": positiveInt(data["participant_turns"], positiveInt(data["max_rounds"], 1)),
		"result_source":     normalizeResultSource(data["result_source"]),
		"lifecycle":         normalizeLifecyclePayload(data["lifecycle"]),
	}
	if strings.TrimSpace(stringValue(data["origin"])) == "generated" {
		payload["origin"] = "generated"
		if value := strings.TrimSpace(stringValue(data["generated_from_ref"])); value != "" {
			payload["generated_from_ref"] = value
		}
		if value := strings.TrimSpace(stringValue(data["generated_source"])); value != "" {
			payload["generated_source"] = value
		}
		if value := strings.TrimSpace(stringValue(data["generated_recipe_id"])); value != "" {
			payload["generated_recipe_id"] = value
		}
	}
	return payload
}

func RecipePayload(data map[string]any) map[string]any { return normalizeRecipePayload(data) }

func normalizeResultSource(value any) string {
	if strings.TrimSpace(stringValue(value)) == ResultSourceReducer {
		return ResultSourceReducer
	}
	return ResultSourceLastTurn
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
	case "ephemeral":
		return "ephemeral"
	default:
		return "inherited"
	}
}
