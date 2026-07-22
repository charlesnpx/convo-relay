package recipes

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
)

func TestEveryDefaultRecipeRecordPassesGenericRegistryChecks(t *testing.T) {
	rawDefaults := make(map[string]any, len(defaultRelayRecipes))
	for recipeID, record := range defaultRelayRecipes {
		rawDefaults[recipeID] = cloneObject(record)
	}
	if err := ValidateRawRelayRecipes(rawDefaults); err != nil {
		t.Fatalf("validate default registry: %v", err)
	}

	config, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load default registry: %v", err)
	}
	integrationBound := make(map[string]map[string]any)
	variantGroups := make(map[string]map[string]bool)
	for recipeID, rawRecord := range defaultRelayRecipes {
		contractID := stringValue(rawRecord["integration_contract"])
		if contractID == "" {
			continue
		}
		integrationBound[recipeID] = rawRecord

		recipe := config.RelayRecipes[recipeID]
		if recipe == nil {
			t.Fatalf("default recipe %q was not normalized", recipeID)
		}
		if recipe["participant_turns"] != 4 || recipe["result_source"] != integration.ResultSourceReducer || recipe["max_depth"] != 1 || recipe["auto_approval"] != "never" {
			t.Fatalf("default recipe %q execution fields = %#v", recipeID, recipe)
		}
		lifecycle, _ := recipe["lifecycle"].(map[string]any)
		if lifecycle["resume"] != "forbid" || lifecycle["steering"] != "forbid" || lifecycle["dynamic"] != "forbid" || lifecycle["workspace_isolation"] != "ephemeral" {
			t.Fatalf("default recipe %q lifecycle = %#v", recipeID, lifecycle)
		}

		bundle := defaultRecordBundle(t, contractID, intFromAny(recipe["participant_turns"], 0))
		rootPlan, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetRoot, CompileOptions{
			IntegrationBundle:  bundle,
			ValidateExecutable: true,
		})
		if err != nil {
			t.Fatalf("compile default recipe %q for root: %v", recipeID, err)
		}
		if rootPlan["kind"] != contracts.RootArtifactKindRootRecipePlan || rootPlan["integration_contract_id"] != contractID || rootPlan["participant_turns"] != recipe["participant_turns"] {
			t.Fatalf("default recipe %q root plan = %#v", recipeID, rootPlan)
		}

		_, err = CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetChild, CompileOptions{})
		var rootOnly *RootOnlyRecipeError
		if !errors.As(err, &rootOnly) || rootOnly.RecipeID != recipeID || rootOnly.IntegrationContract != contractID {
			t.Fatalf("default recipe %q child error = %T %#v", recipeID, err, err)
		}

		shape := cloneObject(rawRecord)
		delete(shape, "participants")
		delete(shape, "facilitator")
		delete(shape, "reducer")
		shapeKey, err := contracts.CanonicalJSONText(shape)
		if err != nil {
			t.Fatalf("canonicalize default recipe %q shape: %v", recipeID, err)
		}
		assignmentKey, err := contracts.CanonicalJSONText(map[string]any{
			"participants": rawRecord["participants"],
			"facilitator":  rawRecord["facilitator"],
			"reducer":      rawRecord["reducer"],
		})
		if err != nil {
			t.Fatalf("canonicalize default recipe %q assignment: %v", recipeID, err)
		}
		if variantGroups[shapeKey] == nil {
			variantGroups[shapeKey] = map[string]bool{}
		}
		variantGroups[shapeKey][assignmentKey] = true
	}

	if len(integrationBound) != 6 {
		t.Fatalf("integration-bound default recipe count = %d, want 6", len(integrationBound))
	}
	if len(variantGroups) != 2 {
		t.Fatalf("default protocol-shape count = %d, want 2", len(variantGroups))
	}
	for shape, assignments := range variantGroups {
		if len(assignments) != 3 {
			t.Fatalf("default protocol shape %s has %d backend assignments, want 3", shape, len(assignments))
		}
	}
}

func TestGenericRecipeExecutionWorksWithoutIntegrationBoundDefaults(t *testing.T) {
	baseDefaults := make(map[string]map[string]any)
	for recipeID, record := range defaultRelayRecipes {
		if stringValue(record["integration_contract"]) == "" {
			baseDefaults[recipeID] = cloneObject(record)
		}
	}
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
	recipes := normalizeRelayRecipesWithDefaults(rawRecipes, baseDefaults)
	for recipeID, recipe := range recipes {
		if stringValue(recipe["integration_contract"]) != "" {
			t.Fatalf("registry without optional defaults retained integration-bound recipe %q", recipeID)
		}
	}
	profiles := NormalizeBackendProfiles(nil)
	for _, target := range []CompileTarget{CompileTargetRoot, CompileTargetChild} {
		plan, err := CompileRecipe(recipes["neutral-root"], profiles, recipes, target, CompileOptions{ValidateExecutable: true})
		if err != nil {
			t.Fatalf("compile neutral recipe for %s without optional defaults: %v", target, err)
		}
		if plan == nil {
			t.Fatalf("compile neutral recipe for %s returned no plan", target)
		}
	}
}

func defaultRecordBundle(t *testing.T, contractID string, participantTurns int) *integration.Bundle {
	t.Helper()
	turns := make([]any, 0, participantTurns)
	for turn := 1; turn <= participantTurns; turn++ {
		turns = append(turns, map[string]any{
			"participant_turn": turn,
			"slot":             fmt.Sprintf("slot_%d", (turn-1)%2),
			"instructions":     fmt.Sprintf("Follow the declared instructions for turn %d.", turn),
		})
	}
	payload := map[string]any{
		"schema_version": "relay-integration-bundle-v1",
		"id":             "default-registry-test-bundle",
		"contracts": map[string]any{
			contractID: map[string]any{
				"turns":   turns,
				"reducer": map[string]any{"instructions": "Return one JSON object."},
				"result": map[string]any{
					"transport": "json",
					"schema":    map[string]any{"type": "object"},
				},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal default-record bundle: %v", err)
	}
	bundle, err := integration.DecodeBundleBytes(data)
	if err != nil {
		t.Fatalf("decode default-record bundle: %v", err)
	}
	return bundle
}
