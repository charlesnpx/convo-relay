package recipes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestDefaultReviewPanelCompilesWithStablePythonDigest(t *testing.T) {
	config, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	report, err := BuildCompileReport("review-panel", config, CompileTargetChild, CompileOptions{
		CompositionPath:    "root",
		ValidateExecutable: true,
	})
	if err != nil {
		t.Fatalf("compile review-panel: %v", err)
	}
	if report["recipe_digest"] != "sha256:20740e0613d612f35eebfac9e81b9d14427bb704c541fab47dd09f2ee17dc5f5" {
		t.Fatalf("recipe digest = %v", report["recipe_digest"])
	}
	if report["compiled_plan_digest"] != "sha256:bd6f6295f9f32516562d8bc9156abcb328fb425301f648d34401855bfa8cef89" {
		t.Fatalf("compiled plan digest = %v", report["compiled_plan_digest"])
	}
	compiled := report["compiled_plan"].(map[string]any)
	participants := compiled["participants"].([]any)
	if participants[0].(map[string]any)["composition_path"] != "root.slot_0" {
		t.Fatalf("first participant path = %v", participants[0].(map[string]any)["composition_path"])
	}
}

func TestSettingsTOMLNestedRelayProfileCompilesWithChildRoundsAndPaths(t *testing.T) {
	settingsPath := writeSettings(t, `
[backend_profiles.child-panel]
backend = "relay"
model = "child-review"
description = "Nested child panel"
capabilities = ["composite"]

[relay_recipes.parent-review]
purpose = "Parent with a nested relay participant."
participants = ["child-panel", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "adversarial"
max_rounds = 2
max_depth = 2

[relay_recipes.child-review]
purpose = "Child relay."
participants = ["codex-deep", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "adversarial"
max_rounds = 4
max_depth = 1
`)
	config, err := LoadRuntimeConfig(settingsPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	report, err := BuildCompileReport("parent-review", config, CompileTargetChild, CompileOptions{
		CompositionPath:    "root.slot_1",
		ValidateExecutable: true,
	})
	if err != nil {
		t.Fatalf("compile parent-review: %v", err)
	}
	compiled := report["compiled_plan"].(map[string]any)
	first := compiled["participants"].([]any)[0].(map[string]any)
	if first["backend"] != "relay" {
		t.Fatalf("first participant backend = %v", first["backend"])
	}
	if first["effort"] != 4 {
		t.Fatalf("relay effort = %v, want child max_rounds 4", first["effort"])
	}
	if first["composition_path"] != "root.slot_1.slot_0" {
		t.Fatalf("composition path = %v", first["composition_path"])
	}
	launch := report["launch"].(map[string]any)
	agents := launch["agents"].([]any)
	if agents[0] != "relay" || agents[1] != "codex" {
		t.Fatalf("agents = %#v", agents)
	}
}

func TestLoadRuntimeConfigUsesEnvironmentSettings(t *testing.T) {
	settingsPath := writeSettings(t, `
[backend_profiles.local-codex]
backend = "codex"
model = "local-model"
effort = "low"

[relay_recipes.local-review]
participants = ["local-codex", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
`)
	t.Setenv(SettingsEnvVar, settingsPath)
	config, err := LoadRuntimeConfig("")
	if err != nil {
		t.Fatalf("load config from env: %v", err)
	}
	if config.SettingsPath != settingsPath {
		t.Fatalf("settings path = %s, want %s", config.SettingsPath, settingsPath)
	}
	if config.BackendProfiles["local-codex"]["model"] != "local-model" {
		t.Fatalf("env profile not loaded: %#v", config.BackendProfiles["local-codex"])
	}
}

func TestLoadRuntimeConfigFallsBackToDefaultSettingsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(SettingsEnvVar, "")

	config, err := LoadRuntimeConfig("")
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	wantPath := filepath.Join(home, ".convo-relay", "settings.toml")
	if config.SettingsPath != wantPath {
		t.Fatalf("settings path = %s, want %s", config.SettingsPath, wantPath)
	}
	if _, ok := config.BackendProfiles["codex-deep"]; !ok {
		t.Fatalf("default backend profiles were not loaded")
	}
	if _, ok := config.RelayRecipes["review-panel"]; !ok {
		t.Fatalf("default relay recipes were not loaded")
	}
}

func TestTransientRecipeSourcesSupportStdinFileAndGeneratedOverrides(t *testing.T) {
	toml := `
[backend_profiles.local-codex]
backend = "codex"
model = "local-model"

[relay_recipes.local-review]
participants = ["local-codex", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
auto_approval = "auto-safe"
`
	stdinSources, err := ReadTransientRecipeSources([]string{"-"}, nil, strings.NewReader(toml))
	if err != nil {
		t.Fatalf("read stdin source: %v", err)
	}
	if len(stdinSources) != 1 || stdinSources[0].Path != "-" || stdinSources[0].SourceType != TransientRecipeSourceOrdinary {
		t.Fatalf("stdin sources = %#v", stdinSources)
	}
	stdinConfig, stdinRefs, err := LoadRuntimeConfigWithTransientSources(filepath.Join(t.TempDir(), "missing.toml"), stdinSources)
	if err != nil {
		t.Fatalf("load stdin config: %v", err)
	}
	if stdinConfig.RelayRecipes["local-review"]["auto_approval"] != "auto-safe" {
		t.Fatalf("ordinary auto_approval = %#v", stdinConfig.RelayRecipes["local-review"])
	}
	if len(stdinRefs) != 1 || stdinRefs[0].Digest == "" || stdinRefs[0].SourceDigest == "" || stdinRefs[0].RecipeIDs[0] != "local-review" || stdinRefs[0].RecipeDigests["local-review"] == "" {
		t.Fatalf("stdin refs = %#v", stdinRefs)
	}

	recipePath := filepath.Join(t.TempDir(), "recipes.toml")
	if err := os.WriteFile(recipePath, []byte(toml), 0o644); err != nil {
		t.Fatalf("write recipe file: %v", err)
	}
	fileSources, err := ReadTransientRecipeSources([]string{recipePath}, nil, nil)
	if err != nil {
		t.Fatalf("read file source: %v", err)
	}
	fileConfig, _, err := LoadRuntimeConfigWithTransientSources(filepath.Join(t.TempDir(), "missing.toml"), fileSources)
	if err != nil {
		t.Fatalf("load file config: %v", err)
	}
	if fileConfig.RelayRecipes["local-review"]["auto_approval"] != stdinConfig.RelayRecipes["local-review"]["auto_approval"] {
		t.Fatalf("file/stdin auto_approval mismatch: %#v %#v", fileConfig.RelayRecipes["local-review"], stdinConfig.RelayRecipes["local-review"])
	}

	generatedSources, err := ReadTransientRecipeSources(nil, []string{"-"}, strings.NewReader(toml))
	if err != nil {
		t.Fatalf("read generated stdin source: %v", err)
	}
	generatedConfig, generatedRefs, err := LoadRuntimeConfigWithTransientSources(filepath.Join(t.TempDir(), "missing.toml"), generatedSources)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	generatedRecipe := generatedConfig.RelayRecipes["local-review"]
	if generatedRecipe["auto_approval"] != "never" {
		t.Fatalf("generated auto_approval = %#v", generatedRecipe)
	}
	if len(generatedRefs) != 1 || generatedRefs[0].SourceType != TransientRecipeSourceGenerated {
		t.Fatalf("generated refs = %#v", generatedRefs)
	}
}

func TestTransientRecipeSourceSizeCaps(t *testing.T) {
	oversized := strings.Repeat("x", int(TransientRecipeSourceMaxBytes)+1)
	if _, err := ReadTransientRecipeSources([]string{"-"}, nil, strings.NewReader(oversized)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("stdin size cap error = %v", err)
	}

	recipePath := filepath.Join(t.TempDir(), "oversized.toml")
	if err := os.WriteFile(recipePath, []byte(oversized), 0o644); err != nil {
		t.Fatalf("write oversized file: %v", err)
	}
	if _, err := ReadTransientRecipeSources([]string{recipePath}, nil, nil); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("file size cap error = %v", err)
	}
}

func TestRecipeDoctorAndCompileUseTransientSourceIngestion(t *testing.T) {
	toml := `
[relay_recipes.stdin-review]
participants = ["codex-fast", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
`
	sources, err := ReadTransientRecipeSources([]string{"-"}, nil, strings.NewReader(toml))
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	report, err := BuildRecipeCatalogReportWithOptions(filepath.Join(t.TempDir(), "missing.toml"), RecipeCatalogOptions{
		TransientSources: sources,
		ReadinessCheck:   catalogReadinessCheck(nil),
	})
	if err != nil {
		t.Fatalf("doctor report: %v", err)
	}
	record, ok := FindRecipeRecord(report.Recipes, "stdin-review")
	if !ok || record.Source != "transient" || record.Status != RecipeStatusUsable {
		t.Fatalf("transient doctor record = %#v ok=%v", record, ok)
	}
	config, transientRefs, err := LoadRuntimeConfigWithTransientSources(filepath.Join(t.TempDir(), "missing.toml"), sources)
	if err != nil {
		t.Fatalf("load transient config: %v", err)
	}
	if len(transientRefs) != 1 {
		t.Fatalf("transient refs = %#v", transientRefs)
	}
	sourceDigest := transientRefs[0].SourceDigest
	recipeDigest := transientRefs[0].RecipeDigests["stdin-review"]
	if record.SourceDigest != sourceDigest || record.RecipeDigest != recipeDigest {
		t.Fatalf("doctor digest pair = source %q recipe %q, want %q %q", record.SourceDigest, record.RecipeDigest, sourceDigest, recipeDigest)
	}
	compile, err := BuildCompileReport("stdin-review", config, CompileTargetChild, CompileOptions{
		ValidateExecutable: true,
		TransientSources:   transientRefs,
	})
	if err != nil {
		t.Fatalf("compile transient recipe: %v", err)
	}
	if compile["recipe_id"] != "stdin-review" {
		t.Fatalf("compile report = %#v", compile)
	}
	if compile["source_digest"] != sourceDigest || compile["recipe_digest"] != recipeDigest {
		t.Fatalf("compile digest pair = source %v recipe %v, want %s %s", compile["source_digest"], compile["recipe_digest"], sourceDigest, recipeDigest)
	}
}

func TestGeneratedTransientSourcesRejectTrustedIDCollisions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "builtin recipe",
			content: `
[relay_recipes.review-panel]
participants = ["codex-fast", "codex-deep"]
max_rounds = 1
max_depth = 1
`,
			want: `relay recipe "review-panel"`,
		},
		{
			name: "builtin profile",
			content: `
[backend_profiles.codex-deep]
backend = "codex"
model = "local"

[relay_recipes.gen-review]
participants = ["codex-deep", "codex-fast"]
max_rounds = 1
max_depth = 1
`,
			want: `backend profile "codex-deep"`,
		},
		{
			name: "backend alias profile",
			content: `
[backend_profiles.codex]
backend = "codex"
model = "local"

[relay_recipes.gen-review]
participants = ["codex", "codex-fast"]
max_rounds = 1
max_depth = 1
`,
			want: `backend profile "codex"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := transientSourceForTest(TransientRecipeSourceGenerated, tt.name+".toml", tt.content)
			_, _, err := LoadRuntimeConfigWithTransientSources(filepath.Join(t.TempDir(), "missing.toml"), []TransientRecipeSource{source})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("collision error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestGeneratedTransientSourceRejectsSettingsCollision(t *testing.T) {
	settingsPath := writeSettings(t, `
[relay_recipes.settings-review]
participants = ["codex-fast", "codex-deep"]
max_rounds = 1
max_depth = 1
`)
	source := transientSourceForTest(TransientRecipeSourceGenerated, "generated.toml", `
[relay_recipes.settings-review]
participants = ["codex-fast", "codex-deep"]
max_rounds = 1
max_depth = 1
`)
	_, _, err := LoadRuntimeConfigWithTransientSources(settingsPath, []TransientRecipeSource{source})
	if err == nil || !strings.Contains(err.Error(), `relay recipe "settings-review"`) {
		t.Fatalf("settings collision error = %v", err)
	}
}

func TestGeneratedTransientSourceCollisionsAreOrderIndependentAndAtomic(t *testing.T) {
	ordinary := transientSourceForTest(TransientRecipeSourceOrdinary, "ordinary.toml", `
[relay_recipes.shared-review]
participants = ["codex-fast", "codex-deep"]
max_rounds = 1
max_depth = 1
`)
	generated := transientSourceForTest(TransientRecipeSourceGenerated, "generated.toml", `
[relay_recipes.shared-review]
participants = ["codex-fast", "codex-deep"]
max_rounds = 1
max_depth = 1
`)
	for _, sources := range [][]TransientRecipeSource{
		{ordinary, generated},
		{generated, ordinary},
	} {
		_, _, err := LoadRuntimeConfigWithTransientSources(filepath.Join(t.TempDir(), "missing.toml"), sources)
		if err == nil || !strings.Contains(err.Error(), `relay recipe "shared-review"`) {
			t.Fatalf("order-independent collision error = %v", err)
		}
	}

	mixed := transientSourceForTest(TransientRecipeSourceGenerated, "mixed.toml", `
[relay_recipes.review-panel]
participants = ["codex-fast", "codex-deep"]
max_rounds = 1
max_depth = 1

[relay_recipes.gen-fresh-review]
participants = ["codex-fast", "codex-deep"]
max_rounds = 1
max_depth = 1
`)
	_, _, err := LoadRuntimeConfigWithTransientSources(filepath.Join(t.TempDir(), "missing.toml"), []TransientRecipeSource{mixed})
	if err == nil || !strings.Contains(err.Error(), `relay recipe "review-panel"`) {
		t.Fatalf("mixed generated collision error = %v", err)
	}
}

func TestOrdinaryTransientSourceCanStillOverrideExistingRecipe(t *testing.T) {
	source := transientSourceForTest(TransientRecipeSourceOrdinary, "ordinary.toml", `
[relay_recipes.review-panel]
purpose = "Ordinary override."
participants = ["codex-fast", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "steelman"
max_rounds = 1
max_depth = 1
`)
	config, _, err := LoadRuntimeConfigWithTransientSources(filepath.Join(t.TempDir(), "missing.toml"), []TransientRecipeSource{source})
	if err != nil {
		t.Fatalf("ordinary override load: %v", err)
	}
	recipe := config.RelayRecipes["review-panel"]
	if recipe["purpose"] != "Ordinary override." || recipe["mode"] != "steelman" {
		t.Fatalf("ordinary override recipe = %#v", recipe)
	}
}

func TestGeneratedTransientSourceAllowsExistingProfileReferencesAndFreshProfiles(t *testing.T) {
	source := transientSourceForTest(TransientRecipeSourceGenerated, "generated.toml", `
[backend_profiles.gen-review-slot-a]
backend = "codex"
model = "generated-model"

[relay_recipes.gen-review]
participants = ["gen-review-slot-a", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
auto_approval = "auto-safe"
`)
	config, transientRefs, err := LoadRuntimeConfigWithTransientSources(filepath.Join(t.TempDir(), "missing.toml"), []TransientRecipeSource{source})
	if err != nil {
		t.Fatalf("generated source load: %v", err)
	}
	profile := config.BackendProfiles["gen-review-slot-a"]
	if profile["origin"] != "generated" || profile["generated_from_ref"] != "generated.toml" {
		t.Fatalf("generated profile provenance = %#v", profile)
	}
	recipe := config.RelayRecipes["gen-review"]
	if recipe["origin"] != "generated" || recipe["auto_approval"] != "never" || recipe["generated_from_ref"] != "generated.toml" {
		t.Fatalf("generated recipe provenance = %#v", recipe)
	}
	contract := RecipeContractPayload(recipe)
	if contract["origin"] != "generated" || contract["generated_from_ref"] != "generated.toml" {
		t.Fatalf("contract provenance = %#v", contract)
	}
	compile, err := BuildCompileReport("gen-review", config, CompileTargetChild, CompileOptions{
		ValidateExecutable: true,
		TransientSources:   transientRefs,
	})
	if err != nil {
		t.Fatalf("compile generated recipe: %v", err)
	}
	compiledRecipe := compile["recipe"].(map[string]any)
	if compiledRecipe["origin"] != "generated" {
		t.Fatalf("compile recipe provenance = %#v", compiledRecipe)
	}
	if len(transientRefs) != 1 || compile["source_digest"] != transientRefs[0].SourceDigest || compile["recipe_digest"] != transientRefs[0].RecipeDigests["gen-review"] {
		t.Fatalf("generated compile digest pair = %#v refs=%#v", compile, transientRefs)
	}
}

func transientSourceForTest(sourceType string, path string, content string) TransientRecipeSource {
	return TransientRecipeSource{
		SourceType:  sourceType,
		Path:        path,
		DisplayName: filepath.Base(path),
		RawTOML:     []byte(content),
	}
}

func TestRecipeCatalogIncludesBuiltinsUserSettingsJSONAndHumanOutput(t *testing.T) {
	settingsPath := writeSettings(t, `
[backend_profiles.local-codex]
backend = "codex"
model = "local-model"
effort = "low"

[relay_recipes.local-review]
purpose = "Local custom review."
participants = ["local-codex", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 2
max_depth = 1
`)
	report, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{ReadinessCheck: catalogReadinessCheck(nil)})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, recipeID := range []string{"review-panel", "vision-review", "one-pass-review", "local-review"} {
		record, ok := FindRecipeRecord(report.Recipes, recipeID)
		if !ok {
			t.Fatalf("missing recipe %s in %#v", recipeID, report.Recipes)
		}
		if record.Status != RecipeStatusUsable {
			t.Fatalf("%s status = %s diagnostics=%#v", recipeID, record.Status, record.Diagnostics)
		}
	}
	local, _ := FindRecipeRecord(report.Recipes, "local-review")
	if local.Source != "settings" {
		t.Fatalf("source = %s", local.Source)
	}
	renderedList := FormatRecipeList(FilterRecipeRecords(report.Recipes, RecipeStatusUsable))
	if !strings.Contains(renderedList, "review-panel") || !strings.Contains(renderedList, "local-review") {
		t.Fatalf("list output:\n%s", renderedList)
	}
	renderedShow := FormatRecipeShow(local, "all")
	if !strings.Contains(renderedShow, "Declared:") || !strings.Contains(renderedShow, "Resolved:") || !strings.Contains(renderedShow, "backend=codex") {
		t.Fatalf("show output:\n%s", renderedShow)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if !strings.Contains(string(data), `"recipes"`) || !strings.Contains(string(data), `"local-review"`) {
		t.Fatalf("json report: %s", data)
	}
}

func TestRecipeCatalogReportsInvalidParseableAndSkippedRecipes(t *testing.T) {
	settingsPath := writeSettings(t, `
[relay_recipes]
not-table = "bad"
"review-panel" = "bad override"

[relay_recipes.broken-review]
purpose = "Broken but parseable."
participants = ["codex-fast"]
max_rounds = "many"
`)
	report, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{ReadinessCheck: catalogReadinessCheck(nil)})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	broken, ok := FindRecipeRecord(report.Recipes, "broken-review")
	if !ok {
		t.Fatalf("broken recipe missing: %#v", report.Recipes)
	}
	if broken.Status != RecipeStatusInvalid || !issueCodes(broken.Diagnostics)["invalid_participants"] || !issueCodes(broken.Diagnostics)["invalid_max_rounds"] {
		t.Fatalf("broken record = %#v", broken)
	}
	skipped, ok := FindRecipeRecord(report.Recipes, "not-table")
	if !ok {
		t.Fatalf("skipped recipe missing: %#v", report.Recipes)
	}
	if skipped.Status != RecipeStatusSkipped || !issueCodes(skipped.Diagnostics)["recipe_record_not_table"] {
		t.Fatalf("skipped record = %#v", skipped)
	}
	overridden, ok := FindRecipeRecord(report.Recipes, "review-panel")
	if !ok {
		t.Fatalf("overridden built-in missing: %#v", report.Recipes)
	}
	if overridden.Status != RecipeStatusSkipped || !issueCodes(overridden.Diagnostics)["recipe_record_not_table"] {
		t.Fatalf("overridden built-in record = %#v", overridden)
	}
}

func TestRecipeCatalogReportsUnknownProfilesAndNestedRelayDoctorGroups(t *testing.T) {
	settingsPath := writeSettings(t, `
[backend_profiles.child-panel]
backend = "relay"
model = "child-review"
effort = 0

[relay_recipes.unknown-profile-review]
participants = ["does-not-exist", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_depth = 2

[relay_recipes.parent-review]
participants = ["child-panel", "codex-fast"]
facilitator = "child-panel"
reducer = "codex-deep"
max_rounds = 2
max_depth = 1

[relay_recipes.child-review]
participants = ["codex-deep", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
`)
	report, err := BuildRecipeCatalogReportWithOptions(settingsPath, RecipeCatalogOptions{ReadinessCheck: catalogReadinessCheck(nil)})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if report.Status != "degraded" {
		t.Fatalf("status = %s", report.Status)
	}
	unknown, _ := FindRecipeRecord(report.Recipes, "unknown-profile-review")
	if unknown.Status != RecipeStatusInvalid || !issueCodes(unknown.Diagnostics)["unknown_participant_profile"] {
		t.Fatalf("unknown profile record = %#v", unknown)
	}
	parent, _ := FindRecipeRecord(report.Recipes, "parent-review")
	codes := issueCodes(parent.Diagnostics)
	if parent.Status != RecipeStatusInvalid || !codes["invalid_relay_profile_effort"] || !codes["relay_backend_role_unsupported"] {
		t.Fatalf("parent diagnostics = %#v", parent)
	}
	doctor := FormatRecipeDoctor(report)
	if !strings.Contains(doctor, "invalid_relay_profile_effort") || !strings.Contains(doctor, "relay_backend_role_unsupported") {
		t.Fatalf("doctor output:\n%s", doctor)
	}
}

func TestRecipeCatalogRejectsUnparseableSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.toml")
	if err := os.WriteFile(path, []byte("[relay_recipes\nbad"), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if _, err := BuildRecipeCatalogReport(path); err == nil {
		t.Fatalf("expected unparseable settings error")
	}
}

func TestCompileRejectsRelayFacilitatorUnknownChildRecipeInvalidEffortAndDepth(t *testing.T) {
	settingsPath := writeSettings(t, `
[backend_profiles.child-panel]
backend = "relay"
model = "child-review"
effort = 0

[backend_profiles.missing-panel]
backend = "relay"
model = "missing-review"

[relay_recipes.parent-review]
participants = ["child-panel", "codex-fast"]
facilitator = "child-panel"
reducer = "codex-deep"
max_rounds = 2
max_depth = 1

[relay_recipes.child-review]
participants = ["missing-panel", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
`)
	config, err := LoadRuntimeConfig(settingsPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	_, err = BuildCompileReport("parent-review", config, CompileTargetChild, CompileOptions{
		CompositionPath:    "root",
		ValidateExecutable: true,
	})
	configErr, ok := err.(ChildRelayConfigError)
	if !ok {
		t.Fatalf("error = %T %[1]v, want ChildRelayConfigError", err)
	}
	codes := issueCodes(configErr.Issues)
	for _, want := range []string{
		"invalid_relay_profile_effort",
		"relay_backend_role_unsupported",
	} {
		if !codes[want] {
			t.Fatalf("missing issue %s in %#v", want, configErr.Issues)
		}
	}
}

func TestCompileRejectsUnknownProfilesAndUnknownRelayProfileRecipes(t *testing.T) {
	settingsPath := writeSettings(t, `
[backend_profiles.missing-panel]
backend = "relay"
model = "missing-review"

[relay_recipes.unknown-profile-review]
participants = ["does-not-exist", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_depth = 2

[relay_recipes.unknown-child-review]
participants = ["missing-panel", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_depth = 2
`)
	config, err := LoadRuntimeConfig(settingsPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	_, err = BuildCompileReport("unknown-profile-review", config, CompileTargetChild, CompileOptions{ValidateExecutable: true})
	configErr, ok := err.(ChildRelayConfigError)
	if !ok {
		t.Fatalf("unknown profile error = %T %[1]v, want ChildRelayConfigError", err)
	}
	if !issueCodes(configErr.Issues)["unknown_participant_profile"] {
		t.Fatalf("missing unknown profile issue: %#v", configErr.Issues)
	}

	_, err = BuildCompileReport("unknown-child-review", config, CompileTargetChild, CompileOptions{ValidateExecutable: true})
	configErr, ok = err.(ChildRelayConfigError)
	if !ok {
		t.Fatalf("unknown child recipe error = %T %[1]v, want ChildRelayConfigError", err)
	}
	if !issueCodes(configErr.Issues)["unknown_relay_profile_recipe"] {
		t.Fatalf("missing unknown relay recipe issue: %#v", configErr.Issues)
	}
}

func TestCompileRejectsRelayRecipeCyclesAndDepthExceeded(t *testing.T) {
	settingsPath := writeSettings(t, `
[backend_profiles.self-panel]
backend = "relay"
model = "self-review"

[backend_profiles.child-panel]
backend = "relay"
model = "child-review"

[backend_profiles.parent-panel]
backend = "relay"
model = "parent-review"

[relay_recipes.self-review]
participants = ["self-panel", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_depth = 3

[relay_recipes.parent-review]
participants = ["child-panel", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_depth = 3

[relay_recipes.child-review]
participants = ["parent-panel", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_depth = 3
`)
	config, err := LoadRuntimeConfig(settingsPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	for _, recipeID := range []string{"self-review", "parent-review"} {
		_, err := BuildCompileReport(recipeID, config, CompileTargetChild, CompileOptions{
			CompositionPath:    "root",
			ValidateExecutable: true,
		})
		configErr, ok := err.(ChildRelayConfigError)
		if !ok {
			t.Fatalf("%s error = %T %[2]v, want ChildRelayConfigError", recipeID, err)
		}
		if !issueCodes(configErr.Issues)["relay_recipe_cycle"] {
			t.Fatalf("%s missing cycle issue: %#v", recipeID, configErr.Issues)
		}
	}

	_, err = BuildCompileReport("parent-review", config, CompileTargetChild, CompileOptions{
		CompositionPath:      "root",
		MaxRelayBackendDepth: 1,
		ValidateExecutable:   true,
	})
	configErr, ok := err.(ChildRelayConfigError)
	if !ok {
		t.Fatalf("depth error = %T %[1]v, want ChildRelayConfigError", err)
	}
	if !issueCodes(configErr.Issues)["relay_backend_depth_exceeded"] {
		t.Fatalf("missing depth issue: %#v", configErr.Issues)
	}
}

func TestCompiledPlanDigestMatchesContractDigest(t *testing.T) {
	config, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	report, err := BuildCompileReport("review-panel", config, CompileTargetChild, CompileOptions{ValidateExecutable: true})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	digest, err := contracts.ContractDigest(report["compiled_plan"])
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if digest != report["compiled_plan_digest"] {
		t.Fatalf("digest = %s, report has %s", digest, report["compiled_plan_digest"])
	}
}

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

func issueCodes(issues []ChildRecipeIssue) map[string]bool {
	codes := map[string]bool{}
	for _, issue := range issues {
		codes[issue.Code] = true
	}
	return codes
}
