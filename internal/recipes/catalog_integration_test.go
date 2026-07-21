package recipes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/readiness"
)

func TestRecipeCatalogClassificationPrecedenceAndFilters(t *testing.T) {
	settingsPath := writeSettings(t, `
[relay_recipes]
skipped-review = "not a table"

[relay_recipes.usable-review]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
max_depth = 1

[relay_recipes.requires-review]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
participant_turns = 2
result_source = "reducer"
integration_contract = "test/contract-v1"
max_depth = 1

[relay_recipes.requires-unavailable-review]
participants = ["claude", "codex"]
facilitator = "codex"
reducer = "codex"
participant_turns = 2
result_source = "reducer"
integration_contract = "test/contract-v1"
max_depth = 1

[relay_recipes.unavailable-review]
participants = ["claude", "codex"]
facilitator = "codex"
reducer = "codex"
max_depth = 1

[relay_recipes.invalid-bound-review]
participants = ["codex"]
integration_contract = "test/contract-v1"
max_depth = 1
`)
	report, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{
		ReadinessCheck: catalogReadinessCheck(map[string]string{"claude": readiness.StatusNotInstalled}),
	})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	wantStatuses := map[string]string{
		"usable-review":               RecipeStatusUsable,
		"requires-review":             RecipeStatusRequiresIntegration,
		"requires-unavailable-review": RecipeStatusRequiresIntegration,
		"unavailable-review":          RecipeStatusUnavailable,
		"invalid-bound-review":        RecipeStatusInvalid,
		"skipped-review":              RecipeStatusSkipped,
	}
	for recipeID, want := range wantStatuses {
		record, ok := FindRecipeRecord(report.Recipes, recipeID)
		if !ok || record.Status != want {
			t.Fatalf("%s record = %#v, want status %s", recipeID, record, want)
		}
	}
	invalid, _ := FindRecipeRecord(report.Recipes, "invalid-bound-review")
	if invalid.Integration != nil {
		t.Fatalf("invalid recipe proceeded to integration binding: %#v", invalid)
	}
	requiresUnavailable, _ := FindRecipeRecord(report.Recipes, "requires-unavailable-review")
	if issueCodes(requiresUnavailable.Diagnostics)["backend_unavailable"] {
		t.Fatalf("unresolved integration proceeded to readiness: %#v", requiresUnavailable)
	}

	for _, status := range []string{
		RecipeStatusUsable,
		RecipeStatusRequiresIntegration,
		RecipeStatusUnavailable,
		RecipeStatusInvalid,
		RecipeStatusSkipped,
	} {
		filtered := FilterRecipeRecords(report.Recipes, status)
		if len(filtered) == 0 {
			t.Fatalf("status filter %s returned no records", status)
		}
		for _, record := range filtered {
			if record.Status != status {
				t.Fatalf("status filter %s returned %#v", status, record)
			}
		}
	}
	if got := FilterRecipeRecords(report.Recipes, "all"); len(got) != len(report.Recipes) {
		t.Fatalf("all filter returned %d of %d records", len(got), len(report.Recipes))
	}
	defaultHuman := FilterRecipeRecordsByStatuses(report.Recipes, RecipeStatusUsable, RecipeStatusRequiresIntegration)
	for _, record := range defaultHuman {
		if record.Status != RecipeStatusUsable && record.Status != RecipeStatusRequiresIntegration {
			t.Fatalf("default human records include %#v", record)
		}
	}
}

func TestRecipeCatalogIntegrationBindingAndDoctorSemantics(t *testing.T) {
	settingsPath := writeSettings(t, `
[relay_recipes.bound-review]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
participant_turns = 2
result_source = "reducer"
integration_contract = "test/contract-v1"
max_depth = 1
`)
	ready := catalogReadinessCheck(nil)

	missing, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{ReadinessCheck: ready})
	if err != nil {
		t.Fatalf("missing-bundle catalog: %v", err)
	}
	record, _ := FindRecipeRecord(missing.Recipes, "bound-review")
	if record.Status != RecipeStatusRequiresIntegration || missing.Status != "ok" {
		t.Fatalf("missing integration should be non-degrading: record=%#v report=%#v", record, missing)
	}
	if !strings.Contains(FormatRecipeDoctor(missing), "integration_required") {
		t.Fatalf("doctor omitted integration diagnostic:\n%s", FormatRecipeDoctor(missing))
	}

	bundle, err := integration.DecodeBundleBytes([]byte(compileBundleJSON))
	if err != nil {
		t.Fatalf("decode matching bundle: %v", err)
	}
	bound, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{
		IntegrationBundle: bundle,
		ReadinessCheck:    ready,
	})
	if err != nil {
		t.Fatalf("bound catalog: %v", err)
	}
	record, _ = FindRecipeRecord(bound.Recipes, "bound-review")
	if record.Status != RecipeStatusUsable || record.Integration == nil || record.Integration.Status != RecipeIntegrationStatusBound {
		t.Fatalf("bound record = %#v", record)
	}
	if record.Integration.BundleDigest != bundle.Digest() || record.Integration.ContractDigest == "" {
		t.Fatalf("bound integration provenance = %#v", record.Integration)
	}

	otherBundleJSON := strings.ReplaceAll(compileBundleJSON, "test/contract-v1", "other/contract-v1")
	otherBundle, err := integration.DecodeBundleBytes([]byte(otherBundleJSON))
	if err != nil {
		t.Fatalf("decode nonmatching bundle: %v", err)
	}
	nonmatching, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{
		IntegrationBundle: otherBundle,
		ReadinessCheck:    ready,
	})
	if err != nil {
		t.Fatalf("nonmatching catalog: %v", err)
	}
	record, _ = FindRecipeRecord(nonmatching.Recipes, "bound-review")
	if record.Status != RecipeStatusRequiresIntegration || !issueCodes(record.Diagnostics)[integration.DiagnosticCodeContractNotFound] || nonmatching.Status != "ok" {
		t.Fatalf("nonmatching record = %#v report status=%s", record, nonmatching.Status)
	}

	mismatchedSettings := writeSettings(t, `
[relay_recipes.bound-review]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
participant_turns = 4
result_source = "reducer"
integration_contract = "test/contract-v1"
max_depth = 1
`)
	scheduleMismatch, err := BuildRecipeCatalogReportWithOptions(mismatchedSettings, RecipeCatalogOptions{
		IntegrationBundle: bundle,
		ReadinessCheck:    ready,
	})
	if err != nil {
		t.Fatalf("schedule-mismatch catalog: %v", err)
	}
	record, _ = FindRecipeRecord(scheduleMismatch.Recipes, "bound-review")
	if record.Status != RecipeStatusRequiresIntegration || !issueCodes(record.Diagnostics)[integration.DiagnosticCodeScheduleMismatch] {
		t.Fatalf("schedule-mismatch record = %#v", record)
	}
}

func TestLoadIntegrationBundleDistinguishesOmissionAndMalformedInput(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "missing-settings.toml")
	bundle, err := LoadIntegrationBundle(settingsPath, "")
	if err != nil || bundle != nil {
		t.Fatalf("omitted bundle = %#v, %v", bundle, err)
	}
	malformedPath := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(malformedPath, []byte(`{"schema_version":`), 0o644); err != nil {
		t.Fatalf("write malformed bundle: %v", err)
	}
	if _, err := LoadIntegrationBundle(settingsPath, malformedPath); err == nil {
		t.Fatal("malformed bundle was accepted")
	}
	if _, err := LoadIntegrationBundle(settingsPath, filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing bundle path was accepted")
	}
}

func TestRecipeCatalogReadinessIncludesUnavailableTransitiveBackends(t *testing.T) {
	settingsPath := writeSettings(t, `
[backend_profiles.child-panel]
backend = "relay"
model = "child-review"
effort = 1

[relay_recipes.parent-review]
participants = ["child-panel", "codex"]
facilitator = "codex"
reducer = "codex"
max_depth = 3

[relay_recipes.child-review]
participants = ["gemini", "codex"]
facilitator = "codex"
reducer = "codex"
max_depth = 1
`)
	var checked []string
	report, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{
		ReadinessCheck: func(ctx context.Context, backends []string, options readiness.Options) ([]readiness.Record, error) {
			checked = append([]string{}, backends...)
			return catalogReadinessCheck(map[string]string{"gemini": readiness.StatusNotInstalled})(ctx, backends, options)
		},
	})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if !containsString(checked, "gemini") || !containsString(checked, "relay") {
		t.Fatalf("checked backend closure = %#v", checked)
	}
	parent, _ := FindRecipeRecord(report.Recipes, "parent-review")
	if parent.Status != RecipeStatusUnavailable || !issueCodes(parent.Diagnostics)["backend_unavailable"] {
		t.Fatalf("parent transitive readiness = %#v", parent)
	}
	var readinessBackends []string
	for _, record := range parent.BackendReadiness {
		readinessBackends = append(readinessBackends, record.Backend)
	}
	if !containsString(readinessBackends, "gemini") {
		t.Fatalf("parent readiness records = %#v", parent.BackendReadiness)
	}
}

func TestRecipeCatalogIgnoresUnusedRootReducers(t *testing.T) {
	settingsPath := writeSettings(t, `
[backend_profiles.unused-relay-reducer]
backend = "relay"
model = "unused-child"
effort = 1

[relay_recipes.unknown-unused-reducer]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "does-not-exist"
participant_turns = 2
result_source = "last_turn"
max_depth = 1

[relay_recipes.relay-unused-reducer]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "unused-relay-reducer"
participant_turns = 2
result_source = "last_turn"
max_depth = 1

[relay_recipes.unused-child]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
max_depth = 1
`)
	report, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{
		ReadinessCheck: catalogReadinessCheck(nil),
	})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	config, err := LoadRuntimeConfig(settingsPath)
	if err != nil {
		t.Fatalf("load runtime config: %v", err)
	}
	for _, recipeID := range []string{"unknown-unused-reducer", "relay-unused-reducer"} {
		record, ok := FindRecipeRecord(report.Recipes, recipeID)
		if !ok || record.Status != RecipeStatusUsable || len(record.Diagnostics) != 0 {
			t.Fatalf("%s catalog record = %#v, want usable", recipeID, record)
		}
		for _, backend := range record.BackendReadiness {
			if backend.Backend == "relay" {
				t.Fatalf("%s checked unused reducer backend: %#v", recipeID, record.BackendReadiness)
			}
		}
		plan, err := CompileRecipe(
			config.RelayRecipes[recipeID],
			config.BackendProfiles,
			config.RelayRecipes,
			CompileTargetRoot,
			CompileOptions{ValidateExecutable: true},
		)
		if err != nil {
			t.Fatalf("compile %s as root: %v", recipeID, err)
		}
		if _, exists := plan["reducer"]; exists {
			t.Fatalf("%s root plan resolved unused reducer: %#v", recipeID, plan)
		}
	}
}

func TestRecipeCatalogSharesRootParticipantTurnAllocationBound(t *testing.T) {
	settingsPath := writeSettings(t, `
[relay_recipes.boundary-contractless]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
participant_turns = 10000
max_depth = 1

[relay_recipes.overflow-contractless]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
participant_turns = 10001
max_depth = 1

[relay_recipes.boundary-bound]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
participant_turns = 10000
integration_contract = "test/contract-v1"
max_depth = 1

[relay_recipes.overflow-bound]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
participant_turns = 10001
integration_contract = "test/contract-v1"
max_depth = 1
`)
	assertStatuses := func(t *testing.T, report RecipeCatalogReport) {
		t.Helper()
		for recipeID, want := range map[string]string{
			"boundary-contractless": RecipeStatusUsable,
			"overflow-contractless": RecipeStatusInvalid,
			"boundary-bound":        RecipeStatusRequiresIntegration,
			"overflow-bound":        RecipeStatusInvalid,
		} {
			record, ok := FindRecipeRecord(report.Recipes, recipeID)
			if !ok || record.Status != want {
				t.Fatalf("%s record = %#v, want %s", recipeID, record, want)
			}
			if strings.HasPrefix(recipeID, "overflow-") && !issueCodes(record.Diagnostics)[DiagnosticCodeInvalidParticipantTurns] {
				t.Fatalf("%s diagnostics = %#v", recipeID, record.Diagnostics)
			}
		}
	}

	withoutBundle, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{ReadinessCheck: catalogReadinessCheck(nil)})
	if err != nil {
		t.Fatalf("catalog without bundle: %v", err)
	}
	assertStatuses(t, withoutBundle)

	bundle, err := integration.DecodeBundleBytes([]byte(compileBundleJSON))
	if err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	withBundle, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{
		IntegrationBundle: bundle,
		ReadinessCheck:    catalogReadinessCheck(nil),
	})
	if err != nil {
		t.Fatalf("catalog with bundle: %v", err)
	}
	assertStatuses(t, withBundle)
}

func TestBuildCompileReportUsesExplicitTargetSpecificPayloads(t *testing.T) {
	config := defaultCompileConfig(t)
	child, err := BuildCompileReport("review-panel", config, CompileTargetChild, CompileOptions{
		CompositionPath:    "root",
		ValidateExecutable: true,
	})
	if err != nil {
		t.Fatalf("child report: %v", err)
	}
	root, err := BuildCompileReport("review-panel", config, CompileTargetRoot, CompileOptions{
		CompositionPath:    "root",
		ValidateExecutable: true,
	})
	if err != nil {
		t.Fatalf("root report: %v", err)
	}
	if child["target"] != "child" || child["compiled_plan"].(map[string]any)["kind"] != "compiled_plan" || child["launch"] == nil {
		t.Fatalf("child report = %#v", child)
	}
	if root["target"] != "root" || root["compiled_plan"].(map[string]any)["kind"] != contracts.RootArtifactKindRootRecipePlan {
		t.Fatalf("root report = %#v", root)
	}
	if _, exists := root["launch"]; exists {
		t.Fatalf("root report derived a child launch: %#v", root)
	}
	rootRecipeRef := root["compiled_plan"].(map[string]any)["recipe_ref"].(map[string]any)
	if rootRecipeRef["digest"] != root["recipe_digest"] {
		t.Fatalf("root recipe digest = %v, plan ref = %#v", root["recipe_digest"], rootRecipeRef)
	}
	if child["recipe_digest"] != "sha256:20740e0613d612f35eebfac9e81b9d14427bb704c541fab47dd09f2ee17dc5f5" ||
		child["compiled_plan_digest"] != "sha256:bd6f6295f9f32516562d8bc9156abcb328fb425301f648d34401855bfa8cef89" {
		t.Fatalf("child compatibility digests changed: %#v", child)
	}
	if reflect.DeepEqual(root["compiled_plan_digest"], child["compiled_plan_digest"]) {
		t.Fatal("root and child plan digests unexpectedly match")
	}

	_, err = BuildCompileReport("review-panel", config, "", CompileOptions{})
	var validation contracts.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("missing report target error = %T %[1]v", err)
	}
}

func catalogReadinessCheck(overrides map[string]string) func(context.Context, []string, readiness.Options) ([]readiness.Record, error) {
	return func(_ context.Context, backends []string, options readiness.Options) ([]readiness.Record, error) {
		if options.ProbeAuth {
			return nil, errors.New("catalog unexpectedly requested authentication probes")
		}
		records := make([]readiness.Record, 0, len(backends))
		for _, backend := range backends {
			status := readiness.StatusInstalledAuthUnknown
			if backend == "relay" {
				status = readiness.StatusReady
			}
			if override := strings.TrimSpace(overrides[backend]); override != "" {
				status = override
			}
			records = append(records, readiness.Record{
				Backend:              backend,
				ExecutablePath:       "/fake/bin/" + backend,
				Version:              backend + " 1.0",
				AuthenticationStatus: readiness.AuthenticationUnknown,
				Status:               status,
			})
		}
		return records, nil
	}
}
