package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/readiness"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const rootRecipeTestBundle = `{
  "schema_version": "relay-integration-bundle-v1",
  "id": "neutral/integration-v1",
  "contracts": {
    "neutral/contract-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Present the input."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Challenge the presentation."}
      ],
      "inputs": {
        "payload": {
          "required": true,
          "cardinality": "one",
          "media_type": "application/json",
          "max_bytes": 1024,
          "schema": {"type": "object"}
        }
      },
      "result": {
        "transport": "json",
        "schema": {"type": "object"}
      }
    }
  }
}`

const rootRecipeTestBundleWithoutInputs = `{
  "schema_version": "relay-integration-bundle-v1",
  "id": "neutral/integration-v1",
  "contracts": {
    "neutral/contract-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Present the task."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Challenge the presentation."}
      ],
      "inputs": {},
      "result": {
        "transport": "json",
        "schema": {"type": "object"}
      }
    }
  }
}`

func TestRunRecipeUsesExplicitRootTargetAndPersistsDirectContractlessSession(t *testing.T) {
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "context.md"), "context body\n")
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "skill.md"), "skill body\n")
	sessionDir := filepath.Join(t.TempDir(), "session")
	config := rootRecipeRuntimeConfig("")
	var compileTarget recipes.CompileTarget
	var readinessSessionExisted bool

	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:       sessionDir,
		Task:             "Frame this ordinary prose task",
		RecipeID:         "neutral-root",
		ContextFiles:     []string{"context.md"},
		SkillFiles:       []string{"skill.md"},
		SkillExplicit:    true,
		LaunchPlan:       map[string]any{"plan": []any{map[string]any{"step": "Inspect", "status": "pending"}}},
		TaskPlanExplicit: true,
		LaunchCWD:        launchCWD,
		RuntimeConfig:    config,
		ReadinessCheck: func(ctx context.Context, backends []string, options readiness.Options) ([]readiness.Record, error) {
			_, statErr := os.Stat(sessionDir)
			readinessSessionExisted = statErr == nil
			return readyRootRecipeBackends(backends), nil
		},
		compileRecipe: func(recipe map[string]any, profiles map[string]map[string]any, relayRecipes map[string]map[string]any, target recipes.CompileTarget, options recipes.CompileOptions) (map[string]any, error) {
			compileTarget = target
			return recipes.CompileRecipe(recipe, profiles, relayRecipes, target, options)
		},
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if compileTarget != recipes.CompileTargetRoot {
		t.Fatalf("compile target = %q, want root", compileTarget)
	}
	if readinessSessionExisted {
		t.Fatal("session existed during backend readiness preflight")
	}
	if result["execution_kind"] != "recipe" || result["status"] != "ready" || intFromAny(result["actual_participant_turns"], -1) != 0 {
		t.Fatalf("root result = %#v", result)
	}
	if result["integration_contract_ref"] != nil || result["named_input_manifest_ref"] != nil {
		t.Fatalf("contractless result invented integration artifacts: %#v", result)
	}

	st := store.New(sessionDir)
	meta, err := st.LoadMeta()
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	if meta.String("task") != "Frame this ordinary prose task" || meta.Get("launch_plan") == nil {
		t.Fatalf("contractless framing metadata = %#v", meta.ToMap())
	}
	contextRefs := meta.Slice("launch_context_refs")
	inputRefs := meta.Slice("input_bundle_refs")
	if len(contextRefs) != 1 || len(inputRefs) != 2 {
		t.Fatalf("context/input refs = %#v / %#v", contextRefs, inputRefs)
	}
	contextSource := contextRefs[0].(map[string]any)["source_path"]
	canonicalLaunchCWD, canonicalErr := filepath.EvalSymlinks(launchCWD)
	if canonicalErr != nil {
		t.Fatalf("canonical launch CWD: %v", canonicalErr)
	}
	if contextSource != filepath.Join(canonicalLaunchCWD, "context.md") {
		t.Fatalf("context source = %v", contextSource)
	}
	assertRootRecipeArtifact(t, st, meta.Get("root_recipe_plan_ref"), contracts.RootArtifactKindRootRecipePlan, 0)
	checkpoint := assertRootRecipeArtifact(t, st, meta.Get("latest_root_checkpoint_ref"), contracts.RootArtifactKindRootCheckpoint, 1)
	if checkpoint["phase"] != "workspace_ready" || checkpoint["preflight_complete"] != true || checkpoint["workspace_ready"] != true {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
	assertRootRecipeArtifact(t, st, meta.Get("execution_workspace_ref"), contracts.RootArtifactKindExecutionWorkspace, 0)

	graphPayload := st.LoadGraph()
	nodes := graphPayload["nodes"].(map[string]any)
	if len(nodes) != 1 || nodes["root"] == nil || len(graphPayload["edges"].([]any)) != 0 {
		t.Fatalf("direct root graph = %#v", graphPayload)
	}
	rootNode := nodes["root"].(map[string]any)
	if rootNode["execution_kind"] != "recipe" || rootNode["recipe_id"] != "neutral-root" || rootNode["parent_node_id"] != nil {
		t.Fatalf("root node = %#v", rootNode)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "relay.pid")); !os.IsNotExist(err) {
		t.Fatalf("recipe preflight launched a tracked provider process, err = %v", err)
	}
}

func TestRunRecipeBindsContractInputsAndPersistsExactArtifacts(t *testing.T) {
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"stable"}`)
	bundle := decodeRootRecipeTestBundle(t, rootRecipeTestBundle)
	sessionDir := filepath.Join(t.TempDir(), "session")
	var compileTarget recipes.CompileTarget

	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:        sessionDir,
		Task:              "Validate the supplied payload",
		RecipeID:          "neutral-root",
		InputBindings:     []string{"payload=payload.json"},
		LaunchCWD:         launchCWD,
		RuntimeConfig:     rootRecipeRuntimeConfig("neutral/contract-v1"),
		IntegrationBundle: bundle,
		ReadinessCheck:    readyRootRecipeCheck,
		compileRecipe: func(recipe map[string]any, profiles map[string]map[string]any, relayRecipes map[string]map[string]any, target recipes.CompileTarget, options recipes.CompileOptions) (map[string]any, error) {
			compileTarget = target
			return recipes.CompileRecipe(recipe, profiles, relayRecipes, target, options)
		},
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if compileTarget != recipes.CompileTargetRoot {
		t.Fatalf("compile target = %q, want root", compileTarget)
	}
	for _, field := range []string{"integration_bundle_ref", "integration_contract_ref", "named_input_manifest_ref", "execution_workspace_ref", "root_recipe_plan_ref"} {
		if result[field] == nil {
			t.Fatalf("result missing %s: %#v", field, result)
		}
	}
	st := store.New(sessionDir)
	assertRootRecipeArtifact(t, st, result["integration_bundle_ref"], contracts.RootArtifactKindIntegrationBundle, 0)
	assertRootRecipeArtifact(t, st, result["integration_contract_ref"], contracts.RootArtifactKindIntegrationContract, 0)
	manifest := assertRootRecipeArtifact(t, st, result["named_input_manifest_ref"], contracts.RootArtifactKindNamedInputManifest, 0)
	if manifest["contract_id"] != "neutral/contract-v1" || intFromAny(manifest["input_count"], 0) != 1 {
		t.Fatalf("input manifest = %#v", manifest)
	}
	providerInputs := result["provider_inputs"].(map[string]any)
	providerItem := providerInputs["inputs"].([]any)[0].(map[string]any)
	if _, leaked := providerItem["source_path"]; leaked {
		t.Fatalf("provider input leaked source path: %#v", providerItem)
	}
	materializedPath := stringFromAny(providerItem["materialized_path"])
	data, err := os.ReadFile(materializedPath)
	if err != nil || string(data) != `{"value":"stable"}` {
		t.Fatalf("materialized input = %q, %v", data, err)
	}
	if info, err := os.Stat(materializedPath); err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("materialized input mode = %v, %v", info, err)
	}
}

func TestRunRecipeContractPoliciesRejectBeforeSessionCreation(t *testing.T) {
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "context.md"), "context\n")
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "skill.md"), "skill\n")
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{}`)
	bundle := decodeRootRecipeTestBundle(t, rootRecipeTestBundle)

	tests := []struct {
		name      string
		configure func(*RecipeOptions)
		wantCode  string
	}{
		{
			name: "explicit skill",
			configure: func(options *RecipeOptions) {
				options.SkillFiles = []string{"skill.md"}
				options.SkillExplicit = true
				options.InputBindings = []string{"payload=payload.json"}
			},
			wantCode: diagnosticCodePolicyConflict,
		},
		{
			name: "explicit task plan",
			configure: func(options *RecipeOptions) {
				options.LaunchPlan = map[string]any{"plan": []any{}}
				options.TaskPlanExplicit = true
				options.InputBindings = []string{"payload=payload.json"}
			},
			wantCode: diagnosticCodePolicyConflict,
		},
		{
			name: "context with declared named inputs",
			configure: func(options *RecipeOptions) {
				options.ContextFiles = []string{"context.md"}
				options.InputBindings = []string{"payload=payload.json"}
			},
			wantCode: namedinputsDiagnosticContextConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sessionDir := filepath.Join(t.TempDir(), "session")
			options := RecipeOptions{
				SessionDir:        sessionDir,
				Task:              "Policy task",
				RecipeID:          "neutral-root",
				LaunchCWD:         launchCWD,
				RuntimeConfig:     rootRecipeRuntimeConfig("neutral/contract-v1"),
				IntegrationBundle: bundle,
				ReadinessCheck:    readyRootRecipeCheck,
			}
			test.configure(&options)
			_, err := RunRecipe(context.Background(), options)
			assertRootRecipeDiagnostic(t, err, test.wantCode)
			if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
				t.Fatalf("session created after policy failure, err = %v", statErr)
			}
		})
	}
}

func TestRunRecipeContractWithoutNamedInputsAllowsContext(t *testing.T) {
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "context.md"), "ordinary positional context\n")
	sessionDir := filepath.Join(t.TempDir(), "session")
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:        sessionDir,
		Task:              "Context-compatible contract",
		RecipeID:          "neutral-root",
		ContextFiles:      []string{"context.md"},
		LaunchCWD:         launchCWD,
		RuntimeConfig:     rootRecipeRuntimeConfig("neutral/contract-v1"),
		IntegrationBundle: decodeRootRecipeTestBundle(t, rootRecipeTestBundleWithoutInputs),
		ReadinessCheck:    readyRootRecipeCheck,
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if len(result["launch_context_refs"].([]any)) != 1 {
		t.Fatalf("launch context refs = %#v", result["launch_context_refs"])
	}
	manifest := assertRootRecipeArtifact(t, store.New(sessionDir), result["named_input_manifest_ref"], contracts.RootArtifactKindNamedInputManifest, 0)
	if intFromAny(manifest["input_count"], -1) != 0 {
		t.Fatalf("zero-input manifest = %#v", manifest)
	}
}

func TestRunRecipePurePreflightFailuresLeaveNoSession(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*RecipeOptions)
		wantCode  string
	}{
		{name: "missing recipe", configure: func(options *RecipeOptions) { options.RecipeID = "" }, wantCode: diagnosticCodeRecipeRequired},
		{name: "missing task", configure: func(options *RecipeOptions) { options.Task = "" }, wantCode: diagnosticCodeTaskRequired},
		{name: "unknown recipe", configure: func(options *RecipeOptions) { options.RecipeID = "absent" }, wantCode: diagnosticCodeRecipeUnknown},
		{
			name: "missing integration bundle",
			configure: func(options *RecipeOptions) {
				options.RuntimeConfig = rootRecipeRuntimeConfig("neutral/contract-v1")
			},
			wantCode: integration.DiagnosticCodeInvalidBundle,
		},
		{
			name: "backend unavailable",
			configure: func(options *RecipeOptions) {
				options.ReadinessCheck = func(ctx context.Context, backends []string, readinessOptions readiness.Options) ([]readiness.Record, error) {
					return []readiness.Record{{Backend: "codex", Status: readiness.StatusNotInstalled}}, nil
				}
			},
			wantCode: diagnosticCodeBackendUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sessionDir := filepath.Join(t.TempDir(), "session")
			options := RecipeOptions{
				SessionDir:     sessionDir,
				Task:           "Preflight task",
				RecipeID:       "neutral-root",
				LaunchCWD:      t.TempDir(),
				RuntimeConfig:  rootRecipeRuntimeConfig(""),
				ReadinessCheck: readyRootRecipeCheck,
			}
			test.configure(&options)
			_, err := RunRecipe(context.Background(), options)
			assertRootRecipeDiagnostic(t, err, test.wantCode)
			if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
				t.Fatalf("session created after pure preflight failure, err = %v", statErr)
			}
		})
	}
}

func TestRunRecipeWorkspaceFeasibilityFailsBeforeSessionCreation(t *testing.T) {
	launchCWD := t.TempDir()
	sessionDir := filepath.Join(t.TempDir(), "session")
	config := rootRecipeRuntimeConfig("")
	recipe := config.RelayRecipes["neutral-root"]
	recipe["lifecycle"] = map[string]any{"workspace_isolation": "read_only"}
	_, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Needs isolation",
		RecipeID:       "neutral-root",
		LaunchCWD:      launchCWD,
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
	})
	assertRootRecipeDiagnostic(t, err, "workspace_git_repository_required")
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("session created after workspace preflight failure, err = %v", statErr)
	}
}

func rootRecipeRuntimeConfig(contractID string) recipes.RuntimeConfig {
	recipe := map[string]any{
		"id":                    "neutral-root",
		"purpose":               "Neutral root test recipe",
		"participants":          []any{"participant-a", "participant-b"},
		"facilitator":           "facilitator",
		"reducer":               "reducer",
		"mode":                  "cooperative",
		"max_rounds":            2,
		"participant_turns":     2,
		"result_source":         "last_turn",
		"max_depth":             1,
		"required_capabilities": []any{},
		"auto_approval":         "never",
		"match_keywords":        []any{},
		"lifecycle": map[string]any{
			"resume":              "allow",
			"steering":            "allow",
			"dynamic":             "forbid",
			"workspace_isolation": "inherited",
		},
	}
	if contractID != "" {
		recipe["integration_contract"] = contractID
	}
	profile := func(id string, model string) map[string]any {
		return map[string]any{
			"id":           id,
			"backend":      "codex",
			"model":        model,
			"effort":       "medium",
			"capabilities": []any{},
		}
	}
	return recipes.RuntimeConfig{
		BackendProfiles: map[string]map[string]any{
			"participant-a": profile("participant-a", "model-a"),
			"participant-b": profile("participant-b", "model-b"),
			"facilitator":   profile("facilitator", "model-f"),
			"reducer":       profile("reducer", "model-r"),
		},
		RelayRecipes: map[string]map[string]any{"neutral-root": recipe},
		Limits:       recipes.DefaultRuntimeLimits(),
	}
}

func readyRootRecipeCheck(ctx context.Context, backends []string, options readiness.Options) ([]readiness.Record, error) {
	return readyRootRecipeBackends(backends), nil
}

func readyRootRecipeBackends(backends []string) []readiness.Record {
	records := make([]readiness.Record, 0, len(backends))
	for _, backend := range backends {
		records = append(records, readiness.Record{Backend: backend, Status: readiness.StatusInstalledAuthUnknown})
	}
	return records
}

func decodeRootRecipeTestBundle(t *testing.T, data string) *integration.Bundle {
	t.Helper()
	bundle, err := integration.DecodeBundleBytes([]byte(data))
	if err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	return bundle
}

func writeRootRecipeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertRootRecipeArtifact(t *testing.T, st *store.Store, rawRef any, kind string, ordinal int) map[string]any {
	t.Helper()
	ref, ok := rawRef.(map[string]any)
	if !ok {
		t.Fatalf("%s ref = %#v", kind, rawRef)
	}
	payload, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		t.Fatalf("load %s: %v", kind, err)
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, kind, ordinal, payload); err != nil {
		t.Fatalf("validate %s: %v", kind, err)
	}
	return payload
}

func assertRootRecipeDiagnostic(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected diagnostic %q", code)
	}
	var diagnosticError *contracts.DiagnosticError
	if !errors.As(err, &diagnosticError) {
		t.Fatalf("error = %T %v, want diagnostic %q", err, err, code)
	}
	for _, diagnostic := range diagnosticError.Diagnostics {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("diagnostics = %#v, want %q", diagnosticError.Diagnostics, code)
}

// Keep this local alias explicit so the policy test remains coupled to the
// public diagnostic contract without importing unexported namedinputs state.
const namedinputsDiagnosticContextConflict = "named_input_context_conflict"

func TestRootRecipeReadinessCheckErrorsRemainPreflightFailures(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	_, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:    sessionDir,
		Task:          "Readiness error",
		RecipeID:      "neutral-root",
		LaunchCWD:     t.TempDir(),
		RuntimeConfig: rootRecipeRuntimeConfig(""),
		ReadinessCheck: func(context.Context, []string, readiness.Options) ([]readiness.Record, error) {
			return nil, errors.New("probe failed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("readiness error = %v", err)
	}
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("session created after readiness error, err = %v", statErr)
	}
}
