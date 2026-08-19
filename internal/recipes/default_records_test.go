package recipes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
)

func TestEveryDefaultRecipeRecordPassesGenericRegistryChecks(t *testing.T) {
	expectedFamilies := map[string][]string{
		"witnessed-review/witness-falsification-v1": {
			"witness-falsify",
			"witness-falsify-codex",
			"witness-falsify-claude",
		},
		"witnessed-review/witness-falsification-v2": {
			"witness-falsify-v2",
			"witness-falsify-v2-codex",
			"witness-falsify-v2-claude",
		},
		"witnessed-review/economy-equivalence-v1": {
			"economy-equivalence",
			"economy-equivalence-codex",
			"economy-equivalence-claude",
		},
		"witnessed-review/economy-equivalence-v2": {
			"economy-equivalence-v2",
			"economy-equivalence-v2-codex",
			"economy-equivalence-v2-claude",
		},
	}
	expectedContracts := make(map[string]string, 12)
	for contractID, recipeIDs := range expectedFamilies {
		for _, recipeID := range recipeIDs {
			expectedContracts[recipeID] = contractID
		}
	}
	v2Bundle := loadDefaultRecordV2Bundle(t)
	if v2Bundle.SchemaVersion() != integration.BundleSchemaVersionV2 || len(v2Bundle.Contracts()) != 2 {
		t.Fatalf("neutral v2 bundle = %#v", v2Bundle.ToMap())
	}

	rawDefaults := make(map[string]any, len(defaultRelayRecipeRecords))
	for recipeID, record := range defaultRelayRecipeRecords {
		rawDefaults[recipeID] = cloneObject(record)
	}
	if err := ValidateRawRelayRecipes(rawDefaults); err != nil {
		t.Fatalf("validate default registry: %v", err)
	}

	config, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load default registry: %v", err)
	}
	allRecipes := normalizeRelayRecipesWithDefaults(nil, defaultRelayRecipeRecords)
	integrationBound := make(map[string]map[string]any)
	familyAssignments := make(map[string]map[string]bool)
	for recipeID, rawRecord := range defaultRelayRecipeRecords {
		contractID := stringValue(rawRecord["integration_contract"])
		if contractID == "" {
			continue
		}
		wantContractID, expected := expectedContracts[recipeID]
		if !expected {
			t.Fatalf("unexpected integration-bound default recipe %q", recipeID)
		}
		if contractID != wantContractID {
			t.Fatalf("default recipe %q contract = %q, want %q", recipeID, contractID, wantContractID)
		}
		if strings.HasSuffix(contractID, "-v2") {
			wantPurpose := "Use the consumer-owned " + contractID + " contract."
			if rawRecord["purpose"] != wantPurpose {
				t.Fatalf("default recipe %q purpose = %q, want %q", recipeID, rawRecord["purpose"], wantPurpose)
			}
		}
		integrationBound[recipeID] = rawRecord

		recipe := config.RelayRecipes[recipeID]
		if recipe == nil {
			t.Fatalf("default recipe %q was not normalized", recipeID)
		}
		if recipe["mode"] != "adversarial" || recipe["participant_turns"] != 4 || recipe["result_source"] != integration.ResultSourceReducer || recipe["provider_retry"] != ProviderRetryForbid || recipe["max_depth"] != 1 || recipe["auto_approval"] != "never" {
			t.Fatalf("default recipe %q execution fields = %#v", recipeID, recipe)
		}
		lifecycle, _ := recipe["lifecycle"].(map[string]any)
		if lifecycle["resume"] != "forbid" || lifecycle["steering"] != "forbid" || lifecycle["dynamic"] != "forbid" || lifecycle["workspace_isolation"] != "ephemeral" {
			t.Fatalf("default recipe %q lifecycle = %#v", recipeID, lifecycle)
		}

		bundle := v2Bundle
		if !strings.HasSuffix(contractID, "-v2") {
			bundle = defaultRecordBundle(t, contractID, intFromAny(recipe["participant_turns"], 0))
		}
		rootPlan, err := CompileRecipe(recipe, config.BackendProfiles, allRecipes, CompileTargetRoot, CompileOptions{
			IntegrationBundle:  bundle,
			ValidateExecutable: true,
		})
		if err != nil {
			t.Fatalf("compile default recipe %q for root: %v", recipeID, err)
		}
		if rootPlan["kind"] != contracts.RootArtifactKindRootRecipePlan || rootPlan["schema_version"] != 2 || rootPlan["provider_retry"] != ProviderRetryForbid || rootPlan["integration_contract_id"] != contractID || rootPlan["participant_turns"] != recipe["participant_turns"] {
			t.Fatalf("default recipe %q root plan = %#v", recipeID, rootPlan)
		}
		if strings.HasSuffix(contractID, "-v2") {
			projection, ok := rootPlan["prompt_context"].(map[string]any)
			if !ok || projection["participant_transcript"] != integration.ParticipantTranscriptComplete || projection["facilitator_ledger"] != integration.FacilitatorLedgerTraceOnly {
				t.Fatalf("default recipe %q v2 prompt context = %#v", recipeID, rootPlan["prompt_context"])
			}
		} else if _, exists := rootPlan["prompt_context"]; exists {
			t.Fatalf("default recipe %q v1 root plan gained prompt context: %#v", recipeID, rootPlan)
		}

		_, err = CompileRecipe(recipe, config.BackendProfiles, allRecipes, CompileTargetChild, CompileOptions{})
		var rootOnly *RootOnlyRecipeError
		if !errors.As(err, &rootOnly) || rootOnly.RecipeID != recipeID || rootOnly.IntegrationContract != contractID {
			t.Fatalf("default recipe %q child error = %T %#v", recipeID, err, err)
		}

		assignmentKey, err := contracts.CanonicalJSONText(map[string]any{
			"participants": rawRecord["participants"],
			"facilitator":  rawRecord["facilitator"],
			"reducer":      rawRecord["reducer"],
		})
		if err != nil {
			t.Fatalf("canonicalize default recipe %q assignment: %v", recipeID, err)
		}
		if familyAssignments[contractID] == nil {
			familyAssignments[contractID] = map[string]bool{}
		}
		familyAssignments[contractID][assignmentKey] = true
	}

	if len(integrationBound) != 12 {
		t.Fatalf("integration-bound default recipe count = %d, want 12", len(integrationBound))
	}
	if len(familyAssignments) != 4 {
		t.Fatalf("default integration family count = %d, want 4", len(familyAssignments))
	}
	expectedAssignments := []map[string]any{
		{
			"participants": []any{"claude-code", "codex-deep"},
			"facilitator":  "codex-fast",
			"reducer":      "codex-deep",
		},
		{
			"participants": []any{"codex-deep", "codex-deep"},
			"facilitator":  "codex-fast",
			"reducer":      "codex-deep",
		},
		{
			"participants": []any{"claude-code", "claude-code"},
			"facilitator":  "claude-code",
			"reducer":      "claude-code",
		},
	}
	for contractID := range expectedFamilies {
		assignments := familyAssignments[contractID]
		if len(assignments) != 3 {
			t.Fatalf("default integration family %s has %d backend assignments, want 3", contractID, len(assignments))
		}
		for _, assignment := range expectedAssignments {
			key, err := contracts.CanonicalJSONText(assignment)
			if err != nil {
				t.Fatalf("canonicalize expected assignment: %v", err)
			}
			if !assignments[key] {
				t.Fatalf("default integration family %s is missing assignment %s", contractID, key)
			}
		}
	}

	counterparts := [][2]string{
		{"witness-falsify", "witness-falsify-v2"},
		{"witness-falsify-codex", "witness-falsify-v2-codex"},
		{"witness-falsify-claude", "witness-falsify-v2-claude"},
		{"economy-equivalence", "economy-equivalence-v2"},
		{"economy-equivalence-codex", "economy-equivalence-v2-codex"},
		{"economy-equivalence-claude", "economy-equivalence-v2-claude"},
	}
	for _, pair := range counterparts {
		v1 := defaultRecordCounterpartShape(allRecipes[pair[0]])
		v2 := defaultRecordCounterpartShape(allRecipes[pair[1]])
		if !reflect.DeepEqual(v1, v2) {
			t.Fatalf("normalized default counterparts %q and %q differ: v1=%#v v2=%#v", pair[0], pair[1], v1, v2)
		}
	}

	mismatches := []struct {
		recipeID     string
		v1ContractID string
	}{
		{recipeID: "witness-falsify-v2", v1ContractID: "witnessed-review/witness-falsification-v1"},
		{recipeID: "economy-equivalence-v2", v1ContractID: "witnessed-review/economy-equivalence-v1"},
	}
	for _, mismatch := range mismatches {
		t.Run(mismatch.recipeID+" rejects v1 counterpart", func(t *testing.T) {
			bundle := defaultRecordBundleV2(t, mismatch.v1ContractID, 4)
			_, err := CompileRecipe(allRecipes[mismatch.recipeID], config.BackendProfiles, allRecipes, CompileTargetRoot, CompileOptions{
				IntegrationBundle:  bundle,
				ValidateExecutable: true,
			})
			var diagnostic *contracts.DiagnosticError
			if !errors.As(err, &diagnostic) || !defaultRecordHasDiagnosticCode(diagnostic, integration.DiagnosticCodeContractNotFound) {
				t.Fatalf("default recipe %q mismatch error = %T %#v", mismatch.recipeID, err, err)
			}
		})
	}
}

func defaultRecordBundle(t *testing.T, contractID string, participantTurns int) *integration.Bundle {
	t.Helper()
	return defaultRecordBundleVersion(t, integration.BundleSchemaVersionV1, contractID, participantTurns)
}

func defaultRecordBundleV2(t *testing.T, contractID string, participantTurns int) *integration.Bundle {
	t.Helper()
	return defaultRecordBundleVersion(t, integration.BundleSchemaVersionV2, contractID, participantTurns)
}

func defaultRecordBundleVersion(t *testing.T, schemaVersion string, contractID string, participantTurns int) *integration.Bundle {
	t.Helper()
	turns := make([]any, 0, participantTurns)
	for turn := 1; turn <= participantTurns; turn++ {
		turns = append(turns, map[string]any{
			"participant_turn": turn,
			"slot":             fmt.Sprintf("slot_%d", (turn-1)%2),
			"instructions":     fmt.Sprintf("Follow the declared instructions for turn %d.", turn),
		})
	}
	contract := map[string]any{
		"turns":   turns,
		"reducer": map[string]any{"instructions": "Return one JSON object."},
		"result": map[string]any{
			"transport": "json",
			"schema":    map[string]any{"type": "object"},
		},
	}
	if schemaVersion == integration.BundleSchemaVersionV2 {
		contract["prompt_context"] = map[string]any{
			"participant_transcript": integration.ParticipantTranscriptComplete,
			"facilitator_ledger":     integration.FacilitatorLedgerTraceOnly,
		}
	}
	payload := map[string]any{
		"schema_version": schemaVersion,
		"id":             "default-registry-test-bundle",
		"contracts": map[string]any{
			contractID: contract,
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

func loadDefaultRecordV2Bundle(t *testing.T) *integration.Bundle {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "contracts", "relay-integration-bundle-neutral-v2.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read neutral v2 bundle: %v", err)
	}
	bundle, err := integration.DecodeBundleBytes(data)
	if err != nil {
		t.Fatalf("decode neutral v2 bundle: %v", err)
	}
	return bundle
}

func defaultRecordCounterpartShape(recipe map[string]any) map[string]any {
	shape := cloneObject(recipe)
	delete(shape, "id")
	delete(shape, "integration_contract")
	delete(shape, "purpose")
	return shape
}

func defaultRecordHasDiagnosticCode(err *contracts.DiagnosticError, code string) bool {
	for _, diagnostic := range err.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
