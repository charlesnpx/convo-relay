package recipes

import (
	"errors"
	"path/filepath"
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
	if explicit["participant_turns"] != 4 || explicit["result_source"] != "reducer" || explicit["integration_contract"] != "opaque contract/id" {
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

func TestRawRecipeDiagnosticPathsEscapeOpaqueRecipeIDs(t *testing.T) {
	err := ValidateRawRelayRecipes(map[string]any{
		"opaque/id~v1": map[string]any{"unknown": true},
	})
	diagnostic := requireRecipeDiagnostic(t, err, DiagnosticCodeUnknownRecipeField)
	if diagnostic.Path != "/relay_recipes/opaque~1id~0v1/unknown" {
		t.Fatalf("escaped path = %q", diagnostic.Path)
	}
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
