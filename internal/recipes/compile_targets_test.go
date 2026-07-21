package recipes

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
)

const compileBundleJSON = `{
  "schema_version": "relay-integration-bundle-v1",
  "id": "compile-test-bundle",
  "contracts": {
    "test/contract-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Present."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Challenge."}
      ],
      "reducer": {"instructions": "Reduce."},
      "result": {
        "transport": "json",
        "schema": {"type": "object"}
      }
    }
  }
}`

func TestCompileRecipeRequiresExplicitKnownTarget(t *testing.T) {
	config := defaultCompileConfig(t)
	recipe := config.RelayRecipes["review-panel"]
	for _, target := range []CompileTarget{"", "sideways"} {
		t.Run(string(target), func(t *testing.T) {
			_, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, target, CompileOptions{})
			var validation contracts.ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("target %q error = %T %[2]v, want contracts.ValidationError", target, err)
			}
		})
	}
}

func TestContractlessRecipeCompilesForRootAndChildTargets(t *testing.T) {
	config := defaultCompileConfig(t)
	recipe := config.RelayRecipes["review-panel"]

	rootPlan, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetRoot, CompileOptions{})
	if err != nil {
		t.Fatalf("compile root: %v", err)
	}
	if rootPlan["kind"] != contracts.RootArtifactKindRootRecipePlan || rootPlan["schema_version"] != 1 {
		t.Fatalf("root plan identity = %#v", rootPlan)
	}
	if rootPlan["participant_turns"] != 6 || rootPlan["result_source"] != integration.ResultSourceLastTurn {
		t.Fatalf("root execution fields = %#v", rootPlan)
	}
	schedule := rootPlan["participant_schedule"].([]any)
	if len(schedule) != 6 || schedule[0].(map[string]any)["slot"] != "slot_0" || schedule[1].(map[string]any)["slot"] != "slot_1" {
		t.Fatalf("root schedule = %#v", schedule)
	}
	if _, exists := rootPlan["reducer"]; exists {
		t.Fatalf("last-turn root plan unexpectedly resolved unused reducer: %#v", rootPlan)
	}
	if rootPlan["workspace_isolation_minimum"] != "inherited" {
		t.Fatalf("root isolation minimum = %v", rootPlan["workspace_isolation_minimum"])
	}

	childPlan, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetChild, CompileOptions{})
	if err != nil {
		t.Fatalf("compile child: %v", err)
	}
	if childPlan["kind"] != "compiled_plan" || childPlan["schema_version"] != 1 {
		t.Fatalf("child plan identity = %#v", childPlan)
	}
	if _, exists := childPlan["participant_turns"]; exists {
		t.Fatalf("compiled_plan/v1 gained root execution fields: %#v", childPlan)
	}
}

func TestIntegrationBoundRecipeCompilesOnlyForRootWithMatchingBundle(t *testing.T) {
	config := defaultCompileConfig(t)
	bundle, err := integration.DecodeBundleBytes([]byte(compileBundleJSON))
	if err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	recipe := map[string]any{
		"id":                   "bound-review",
		"participants":         []any{"codex-deep", "codex-fast"},
		"facilitator":          "codex-fast",
		"reducer":              "codex-deep",
		"max_rounds":           9,
		"participant_turns":    2,
		"result_source":        "reducer",
		"integration_contract": "test/contract-v1",
		"max_depth":            1,
	}

	rootPlan, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetRoot, CompileOptions{IntegrationBundle: bundle})
	if err != nil {
		t.Fatalf("compile bound root: %v", err)
	}
	if rootPlan["participant_turns"] != 2 || rootPlan["integration_contract_id"] != "test/contract-v1" {
		t.Fatalf("bound root plan = %#v", rootPlan)
	}
	if rootPlan["integration_bundle_digest"] != bundle.Digest() || rootPlan["integration_contract_digest"] == "" {
		t.Fatalf("bound digests = %#v", rootPlan)
	}
	bundleRef := rootPlan["integration_bundle_ref"].(map[string]any)
	contractRef := rootPlan["integration_contract_ref"].(map[string]any)
	if bundleRef["id"] != "integration_bundle:selected" || contractRef["id"] != "integration_contract:selected" {
		t.Fatalf("safe binding refs = bundle %#v contract %#v", bundleRef, contractRef)
	}
	if rootPlan["reducer"].(map[string]any)["backend"] != "codex" {
		t.Fatalf("resolved reducer = %#v", rootPlan["reducer"])
	}

	_, err = CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetChild, CompileOptions{})
	var rootOnly *RootOnlyRecipeError
	if !errors.As(err, &rootOnly) || rootOnly.IntegrationContract != "test/contract-v1" {
		t.Fatalf("child bound error = %T %[1]v, want *RootOnlyRecipeError", err)
	}

	_, err = CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetRoot, CompileOptions{})
	var diagnostic *contracts.DiagnosticError
	if !errors.As(err, &diagnostic) || len(diagnostic.Diagnostics) == 0 || diagnostic.Diagnostics[0].Code != integration.DiagnosticCodeInvalidBundle {
		t.Fatalf("missing bundle error = %T %#v", err, err)
	}
}

func TestRootCompilationResolvesReducerOnlyWhenUsed(t *testing.T) {
	config := defaultCompileConfig(t)
	lastTurn := map[string]any{
		"id":            "last-turn",
		"participants":  []any{"codex-deep", "codex-fast"},
		"facilitator":   "codex-fast",
		"reducer":       "does-not-exist",
		"max_rounds":    2,
		"result_source": "last_turn",
		"max_depth":     1,
	}
	plan, err := CompileRecipe(lastTurn, config.BackendProfiles, config.RelayRecipes, CompileTargetRoot, CompileOptions{})
	if err != nil {
		t.Fatalf("compile unused reducer: %v", err)
	}
	if _, exists := plan["reducer"]; exists {
		t.Fatalf("unused reducer entered root plan: %#v", plan)
	}

	relayProfiles := cloneNestedObject(config.BackendProfiles)
	relayProfiles["relay-reducer"] = map[string]any{
		"id":      "relay-reducer",
		"backend": "relay",
		"model":   "review-panel",
	}
	used := cloneObject(lastTurn)
	used["id"] = "reduced"
	used["result_source"] = "reducer"
	used["reducer"] = "relay-reducer"
	_, err = CompileRecipe(used, relayProfiles, config.RelayRecipes, CompileTargetRoot, CompileOptions{})
	var diagnostic *contracts.DiagnosticError
	if !errors.As(err, &diagnostic) || len(diagnostic.Diagnostics) != 1 || diagnostic.Diagnostics[0].Code != "invalid_root_reducer" {
		t.Fatalf("relay reducer error = %T %#v", err, err)
	}
}

func TestRootCompilationBoundsMaterializedParticipantSchedule(t *testing.T) {
	config := defaultCompileConfig(t)
	recipe := cloneObject(config.RelayRecipes["review-panel"])
	recipe["participant_turns"] = maxCompiledParticipantTurns
	plan, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetRoot, CompileOptions{})
	if err != nil {
		t.Fatalf("compile boundary schedule: %v", err)
	}
	if schedule := plan["participant_schedule"].([]any); len(schedule) != maxCompiledParticipantTurns {
		t.Fatalf("boundary schedule length = %d, want %d", len(schedule), maxCompiledParticipantTurns)
	}

	recipe["participant_turns"] = maxCompiledParticipantTurns + 1
	_, err = CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetRoot, CompileOptions{})
	diagnostic := requireRecipeDiagnostic(t, err, DiagnosticCodeInvalidParticipantTurns)
	if diagnostic.Path != "/participant_turns" {
		t.Fatalf("bounded schedule diagnostic path = %q", diagnostic.Path)
	}
	if _, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetChild, CompileOptions{}); err != nil {
		t.Fatalf("child target should ignore root-only participant_turns bound: %v", err)
	}
}

func TestRootAndChildDigestsRetainTargetSpecificFields(t *testing.T) {
	config := defaultCompileConfig(t)
	base := cloneObject(config.RelayRecipes["review-panel"])
	base["participant_turns"] = 2
	base["lifecycle"] = map[string]any{"steering": "forbid"}
	changed := cloneObject(base)
	changed["participant_turns"] = 4

	rootFirst := mustCompileTarget(t, base, config, CompileTargetRoot)
	rootSecond := mustCompileTarget(t, changed, config, CompileTargetRoot)
	if mustContractDigest(t, rootFirst) == mustContractDigest(t, rootSecond) {
		t.Fatal("root plan digest ignored participant_turns")
	}
	lifecycleChanged := cloneObject(base)
	lifecycleChanged["lifecycle"] = map[string]any{"steering": "allow"}
	if mustContractDigest(t, rootFirst) == mustContractDigest(t, mustCompileTarget(t, lifecycleChanged, config, CompileTargetRoot)) {
		t.Fatal("root plan digest ignored lifecycle policy")
	}
	resultChanged := cloneObject(base)
	resultChanged["result_source"] = "reducer"
	if mustContractDigest(t, rootFirst) == mustContractDigest(t, mustCompileTarget(t, resultChanged, config, CompileTargetRoot)) {
		t.Fatal("root plan digest ignored result_source and used reducer")
	}
	rootRecipeRef := rootFirst["recipe_ref"].(map[string]any)
	if rootRecipeRef["digest"] != mustContractDigest(t, RecipeContractPayload(base)) {
		t.Fatalf("root recipe ref does not bind the full normalized payload: %#v", rootRecipeRef)
	}
	childFirst := mustCompileTarget(t, base, config, CompileTargetChild)
	childSecond := mustCompileTarget(t, changed, config, CompileTargetChild)
	if mustContractDigest(t, childFirst) != mustContractDigest(t, childSecond) {
		t.Fatal("compiled_plan/v1 digest changed for root-only participant_turns")
	}

	childRounds := cloneObject(changed)
	childRounds["max_rounds"] = 7
	if mustContractDigest(t, childSecond) == mustContractDigest(t, mustCompileTarget(t, childRounds, config, CompileTargetChild)) {
		t.Fatal("child plan digest ignored max_rounds")
	}
}

func TestExistingChildPlanPayloadAndDigestsRemainCompatible(t *testing.T) {
	config := defaultCompileConfig(t)
	report, err := BuildCompileReport("review-panel", config, CompileOptions{CompositionPath: "root", ValidateExecutable: true})
	if err != nil {
		t.Fatalf("compile report: %v", err)
	}
	if report["recipe_digest"] != "sha256:20740e0613d612f35eebfac9e81b9d14427bb704c541fab47dd09f2ee17dc5f5" {
		t.Fatalf("legacy child recipe digest = %v", report["recipe_digest"])
	}
	if report["compiled_plan_digest"] != "sha256:bd6f6295f9f32516562d8bc9156abcb328fb425301f648d34401855bfa8cef89" {
		t.Fatalf("legacy child plan digest = %v", report["compiled_plan_digest"])
	}
	normalized := RecipeContractPayload(config.RelayRecipes["review-panel"])
	for _, field := range []string{"participant_turns", "result_source", "lifecycle"} {
		if _, exists := normalized[field]; !exists {
			t.Fatalf("full normalized recipe missing %s: %#v", field, normalized)
		}
	}
}

func TestCompileRecipeIsOnlyExportedCanonicalCompiler(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate recipes package")
	}
	directory := filepath.Dir(sourceFile)
	packages, err := parser.ParseDir(token.NewFileSet(), directory, func(info fs.FileInfo) bool {
		return filepath.Ext(info.Name()) == ".go" && !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse recipes package: %v", err)
	}
	compilerCount := 0
	for _, parsed := range packages {
		for _, file := range parsed.Files {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || !function.Name.IsExported() {
					continue
				}
				name := function.Name.Name
				if strings.HasPrefix(name, "Compile") && strings.Contains(name, "Recipe") {
					if name != "CompileRecipe" {
						t.Fatalf("parallel exported compiler %s remains", name)
					}
					compilerCount++
				}
			}
		}
	}
	if compilerCount != 1 {
		t.Fatalf("exported canonical compiler count = %d, want exactly one CompileRecipe", compilerCount)
	}
}

func defaultCompileConfig(t *testing.T) RuntimeConfig {
	t.Helper()
	config, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	return config
}

func mustCompileTarget(t *testing.T, recipe map[string]any, config RuntimeConfig, target CompileTarget) map[string]any {
	t.Helper()
	plan, err := CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, target, CompileOptions{})
	if err != nil {
		t.Fatalf("compile %s: %v", target, err)
	}
	return plan
}

func mustContractDigest(t *testing.T, value any) string {
	t.Helper()
	digest, err := contracts.ContractDigest(value)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest
}
