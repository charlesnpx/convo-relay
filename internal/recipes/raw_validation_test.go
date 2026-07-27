package recipes

import (
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestNormalizeRelayRecipeDefaultsAndExplicitRootFields(t *testing.T) {
	normalized := NormalizeRelayRecipes(map[string]any{
		"legacy": map[string]any{
			"participants": []any{"codex-deep", "codex-fast"},
			"max_rounds":   3,
			"max_depth":    1,
		},
		"explicit": map[string]any{
			"participants":         []any{"codex-deep", "codex-fast"},
			"max_rounds":           8,
			"participant_turns":    4,
			"result_source":        "reducer",
			"provider_retry":       "forbid",
			"integration_contract": "opaque contract/id",
			"max_depth":            1,
			"lifecycle": map[string]any{
				"resume":              "forbid",
				"steering":            "forbid",
				"dynamic":             "forbid",
				"workspace_isolation": "ephemeral",
			},
		},
	})

	legacy := normalized["legacy"]
	if legacy["participant_turns"] != 3 || legacy["result_source"] != "last_turn" {
		t.Fatalf("legacy root defaults = %#v", legacy)
	}
	legacyLifecycle := legacy["lifecycle"].(map[string]any)
	if legacyLifecycle["resume"] != "allow" || legacyLifecycle["steering"] != "allow" || legacyLifecycle["dynamic"] != "allow" || legacyLifecycle["workspace_isolation"] != "inherited" {
		t.Fatalf("legacy lifecycle defaults = %#v", legacyLifecycle)
	}

	explicit := normalized["explicit"]
	if explicit["schema_version"] != 2 || explicit["participant_turns"] != 4 || explicit["result_source"] != "reducer" || explicit["provider_retry"] != "forbid" || explicit["integration_contract"] != "opaque contract/id" {
		t.Fatalf("explicit root fields = %#v", explicit)
	}
	explicitLifecycle := explicit["lifecycle"].(map[string]any)
	if explicitLifecycle["resume"] != "forbid" || explicitLifecycle["workspace_isolation"] != "ephemeral" {
		t.Fatalf("explicit lifecycle = %#v", explicitLifecycle)
	}
}

func TestLoadRuntimeConfigRejectsRawRecipeErrorsWithTypedDiagnostics(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode string
		wantPath string
	}{
		{
			name: "unknown recipe field",
			body: `
[relay_recipes.invalid]
participants = ["codex-deep", "codex-fast"]
max_rounds = 2
max_depth = 1
mystery_policy = "allow"
`,
			wantCode: DiagnosticCodeUnknownRecipeField,
			wantPath: "/relay_recipes/invalid/mystery_policy",
		},
		{
			name: "v1 provider retry field",
			body: `
[relay_recipes.invalid]
schema_version = 1
participants = ["codex-deep", "codex-fast"]
provider_retry = "forbid"
`,
			wantCode: DiagnosticCodeInvalidRecipeField,
			wantPath: "/relay_recipes/invalid/provider_retry",
		},
		{
			name: "unknown lifecycle field",
			body: `
[relay_recipes.invalid]
participants = ["codex-deep", "codex-fast"]
max_rounds = 2
max_depth = 1

[relay_recipes.invalid.lifecycle]
restart = "allow"
`,
			wantCode: DiagnosticCodeUnknownLifecycleField,
			wantPath: "/relay_recipes/invalid/lifecycle/restart",
		},
		{
			name: "invalid recipe enum",
			body: `
[relay_recipes.invalid]
participants = ["codex-deep", "codex-fast"]
mode = "argumentative"
max_rounds = 2
max_depth = 1
`,
			wantCode: DiagnosticCodeInvalidRecipeEnum,
			wantPath: "/relay_recipes/invalid/mode",
		},
		{
			name: "invalid lifecycle enum",
			body: `
[relay_recipes.invalid]
participants = ["codex-deep", "codex-fast"]
max_rounds = 2
max_depth = 1

[relay_recipes.invalid.lifecycle]
workspace_isolation = "shared"
`,
			wantCode: DiagnosticCodeInvalidRecipeEnum,
			wantPath: "/relay_recipes/invalid/lifecycle/workspace_isolation",
		},
		{
			name: "fractional participant turns",
			body: `
[relay_recipes.invalid]
participants = ["codex-deep", "codex-fast"]
participant_turns = 1.5
max_rounds = 2
max_depth = 1
`,
			wantCode: DiagnosticCodeInvalidParticipantTurns,
			wantPath: "/relay_recipes/invalid/participant_turns",
		},
		{
			name: "nonpositive participant turns",
			body: `
[relay_recipes.invalid]
participants = ["codex-deep", "codex-fast"]
participant_turns = 0
max_rounds = 2
max_depth = 1
`,
			wantCode: DiagnosticCodeInvalidParticipantTurns,
			wantPath: "/relay_recipes/invalid/participant_turns",
		},
		{
			name: "used reducer is empty",
			body: `
[relay_recipes.invalid]
participants = ["codex-deep", "codex-fast"]
reducer = ""
result_source = "reducer"
max_rounds = 2
max_depth = 1
`,
			wantCode: DiagnosticCodeInvalidRecipeReducer,
			wantPath: "/relay_recipes/invalid/reducer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeSettings(t, test.body)
			_, err := LoadRuntimeConfig(path)
			diagnostic := requireRecipeDiagnostic(t, err, test.wantCode)
			if diagnostic.Path != test.wantPath {
				t.Fatalf("diagnostic path = %q, want %q", diagnostic.Path, test.wantPath)
			}
		})
	}
}

func TestCompileRecipeValidatesDirectRawPayloadBeforeNormalization(t *testing.T) {
	config, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	recipe := map[string]any{
		"id":            "direct-invalid",
		"participants":  []any{"codex-deep", "codex-fast"},
		"max_rounds":    2,
		"max_depth":     1,
		"result_source": "invented",
	}
	_, err = CompileRecipe(recipe, config.BackendProfiles, config.RelayRecipes, CompileTargetRoot, CompileOptions{})
	diagnostic := requireRecipeDiagnostic(t, err, DiagnosticCodeInvalidRecipeEnum)
	if diagnostic.Path != "/recipe/result_source" {
		t.Fatalf("direct diagnostic path = %q", diagnostic.Path)
	}
}

func TestParticipantTurnIntegerValidationRejectsNativeOverflow(t *testing.T) {
	err := ValidateRawRelayRecipes(map[string]any{
		"overflow": map[string]any{
			"participant_turns": math.Exp2(63),
		},
	})
	requireRecipeDiagnostic(t, err, DiagnosticCodeInvalidParticipantTurns)
	if _, ok := parseInt(math.Exp2(63)); ok {
		t.Fatal("parseInt accepted a float outside int64 range")
	}
	if strconv.IntSize == 32 {
		if _, ok := parseInt(int64(math.MaxInt32) + 1); ok {
			t.Fatal("parseInt accepted an int64 outside native int range")
		}
	}
}

func TestRawRecipeDiagnosticPathsEscapeOpaqueRecipeIDs(t *testing.T) {
	err := ValidateRawRelayRecipes(map[string]any{
		"opaque/id~v1": map[string]any{"unknown": true},
	})
	diagnostic := requireRecipeDiagnostic(t, err, DiagnosticCodeUnknownRecipeField)
	if diagnostic.Path != "/relay_recipes/opaque~1id~0v1/unknown" {
		t.Fatalf("escaped path = %q", diagnostic.Path)
	}
}

func TestTransientRecipeDigestsTrackEachSourceMergePoint(t *testing.T) {
	first := transientSourceForTest(TransientRecipeSourceOrdinary, "first.toml", `
[relay_recipes.shared-review]
participants = ["codex-deep", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
`)
	second := transientSourceForTest(TransientRecipeSourceOrdinary, "second.toml", `
[relay_recipes.shared-review]
participants = ["codex-fast", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 2
max_depth = 1
`)
	config, sources, err := LoadRuntimeConfigWithTransientSources(
		filepath.Join(t.TempDir(), "missing.toml"),
		[]TransientRecipeSource{first, second},
	)
	if err != nil {
		t.Fatalf("load overriding transient sources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("source count = %d", len(sources))
	}
	firstDigest := sources[0].RecipeDigests["shared-review"]
	secondDigest := sources[1].RecipeDigests["shared-review"]
	if firstDigest == "" || secondDigest == "" || firstDigest == secondDigest {
		t.Fatalf("per-source recipe digests = first %q second %q", firstDigest, secondDigest)
	}
	wantFirst := normalizedRecipeDigestForTOML(t, string(first.RawTOML), "shared-review")
	if firstDigest != wantFirst {
		t.Fatalf("first source recipe digest = %s, want merge-point digest %s", firstDigest, wantFirst)
	}
	wantFinal, err := contracts.ContractDigest(ChildRecipeContractPayload(config.RelayRecipes["shared-review"]))
	if err != nil {
		t.Fatalf("final recipe digest: %v", err)
	}
	if secondDigest != wantFinal {
		t.Fatalf("second source recipe digest = %s, want final digest %s", secondDigest, wantFinal)
	}
	report, err := BuildCompileReport("shared-review", config, CompileTargetChild, CompileOptions{TransientSources: sources})
	if err != nil {
		t.Fatalf("compile final transient override: %v", err)
	}
	if report["source_digest"] != sources[1].SourceDigest || report["recipe_digest"] != secondDigest {
		t.Fatalf("compile trace did not select final source: %#v", report)
	}
}

func normalizedRecipeDigestForTOML(t *testing.T, content string, recipeID string) string {
	t.Helper()
	parsed, err := decodeTOMLBytes([]byte(content))
	if err != nil {
		t.Fatalf("decode recipe TOML: %v", err)
	}
	recipe := NormalizeRelayRecipes(asObject(parsed["relay_recipes"]))[recipeID]
	digest, err := contracts.ContractDigest(ChildRecipeContractPayload(recipe))
	if err != nil {
		t.Fatalf("digest normalized recipe: %v", err)
	}
	return digest
}

func requireRecipeDiagnostic(t *testing.T, err error, code string) contracts.Diagnostic {
	t.Helper()
	var typed *contracts.DiagnosticError
	if !errors.As(err, &typed) {
		t.Fatalf("error = %T %[1]v, want *contracts.DiagnosticError", err)
	}
	for _, diagnostic := range typed.Diagnostics {
		if diagnostic.Code == code {
			if diagnostic.Phase != contracts.DiagnosticPhasePreflight {
				t.Fatalf("diagnostic phase = %q", diagnostic.Phase)
			}
			return diagnostic
		}
	}
	t.Fatalf("diagnostic code %q missing from %#v", code, typed.Diagnostics)
	return contracts.Diagnostic{}
}
