//go:build !convo_relay_acceptance_no_optional_defaults

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/integration"
)

const witnessV1OnlyBundleJSON = `{
  "schema_version": "relay-integration-bundle-v2",
  "id": "catalog-cli-v1-only-mismatch",
  "contracts": {
    "witnessed-review/witness-falsification-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Provide the first analysis."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Provide the second analysis."},
        {"participant_turn": 3, "slot": "slot_0", "instructions": "Provide the third analysis."},
        {"participant_turn": 4, "slot": "slot_1", "instructions": "Provide the fourth analysis."}
      ],
      "reducer": {"instructions": "Return one JSON object."},
      "result": {"transport": "json", "schema": {"type": "object"}},
      "prompt_context": {
        "participant_transcript": "complete",
        "facilitator_ledger": "trace_only"
      }
    }
  }
}`

func TestWitnessV2DefaultRecipeCLIContracts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake readiness executables use POSIX shell")
	}
	env := setupCatalogCLIEnv(t)
	v2BundlePath := filepath.Join(env.repoRoot, "testdata", "contracts", "relay-integration-bundle-neutral-v2.json")
	v2RecipeIDs := []string{
		"witness-falsify-v2",
		"witness-falsify-v2-codex",
		"witness-falsify-v2-claude",
		"economy-equivalence-v2",
		"economy-equivalence-v2-codex",
		"economy-equivalence-v2-claude",
	}

	unbound := env.run(t, "recipes", "list", "--settings", env.settingsPath, "--json")
	unbound.requireExit(t, 0)
	unboundReport := decodeCLIJSON(t, unbound.stdout)
	for _, recipeID := range v2RecipeIDs {
		requireRecipeCLIStatus(t, unboundReport, recipeID, "requires_integration")
	}

	bound := env.run(t, "recipes", "list", "--settings", env.settingsPath, "--integration-bundle", v2BundlePath, "--json")
	bound.requireExit(t, 0)
	boundReport := decodeCLIJSON(t, bound.stdout)
	for _, recipeID := range v2RecipeIDs {
		requireRecipeCLIStatus(t, boundReport, recipeID, "usable")
	}

	show := env.run(t, "recipes", "show", "witness-falsify-v2", "--settings", env.settingsPath, "--integration-bundle", v2BundlePath)
	show.requireExit(t, 0)
	for _, want := range []string{
		"Recipe: witness-falsify-v2",
		"Status: usable",
		"Integration: bound contract=witnessed-review/witness-falsification-v2",
	} {
		if !strings.Contains(show.stdout, want) {
			t.Fatalf("human v2 recipe show omitted %q:\n%s", want, show.stdout)
		}
	}

	compiled := env.run(t, "compile-recipe", "--recipe", "witness-falsify-v2", "--target", "root", "--settings", env.settingsPath, "--integration-bundle", v2BundlePath, "--json")
	compiled.requireExit(t, 0)
	plan := decodeCLIJSON(t, compiled.stdout)["compiled_plan"].(map[string]any)
	projection, _ := plan["prompt_context"].(map[string]any)
	if plan["schema_version"] != float64(2) || plan["integration_contract_id"] != "witnessed-review/witness-falsification-v2" || projection["participant_transcript"] != "complete" || projection["facilitator_ledger"] != "trace_only" {
		t.Fatalf("compiled v2 default root plan = %#v", plan)
	}

	v1OnlyBundlePath := filepath.Join(t.TempDir(), "witness-v1-only.json")
	if err := os.WriteFile(v1OnlyBundlePath, []byte(witnessV1OnlyBundleJSON), 0o644); err != nil {
		t.Fatalf("write v1-only mismatch bundle: %v", err)
	}
	mismatch := env.run(t, "compile-recipe", "--recipe", "witness-falsify-v2", "--target", "root", "--settings", env.settingsPath, "--integration-bundle", v1OnlyBundlePath, "--json")
	mismatch.requireExit(t, 1)
	if !cliReportHasDiagnosticCode(decodeCLIJSON(t, mismatch.stdout), integration.DiagnosticCodeContractNotFound) {
		t.Fatalf("v1-only mismatch omitted contract-not-found diagnostic: %s", mismatch.stdout)
	}
}

func cliReportHasDiagnosticCode(report map[string]any, code string) bool {
	diagnostics, _ := report["diagnostics"].([]any)
	for _, raw := range diagnostics {
		diagnostic, _ := raw.(map[string]any)
		if diagnostic["code"] == code {
			return true
		}
	}
	return false
}
