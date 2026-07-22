//go:build convo_relay_acceptance_no_optional_defaults

package recipes

import "testing"

func TestGenericRecipeExecutionWorksWithoutIntegrationBoundDefaults(t *testing.T) {
	rawRecipes := map[string]any{
		"neutral-root": map[string]any{
			"participants":      []any{"codex-deep", "codex-fast"},
			"facilitator":       "codex-fast",
			"reducer":           "codex-deep",
			"participant_turns": 2,
			"result_source":     "last_turn",
			"max_depth":         1,
		},
	}
	if err := ValidateRawRelayRecipes(rawRecipes); err != nil {
		t.Fatalf("validate neutral recipe: %v", err)
	}
	recipes := NormalizeRelayRecipes(rawRecipes)
	for recipeID, recipe := range recipes {
		if stringValue(recipe["integration_contract"]) != "" {
			t.Fatalf("defaults-disabled runtime registry retained integration-bound recipe %q", recipeID)
		}
	}
	profiles := NormalizeBackendProfiles(nil)
	for _, target := range []CompileTarget{CompileTargetRoot, CompileTargetChild} {
		plan, err := CompileRecipe(recipes["neutral-root"], profiles, recipes, target, CompileOptions{ValidateExecutable: true})
		if err != nil {
			t.Fatalf("compile neutral recipe for %s with optional defaults disabled: %v", target, err)
		}
		if plan == nil {
			t.Fatalf("compile neutral recipe for %s returned no plan", target)
		}
	}
}
