package runner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/portable"
	"github.com/charlesnpx/convo-relay/internal/readiness"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
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
	  "reducer": {"instructions": "Return one final JSON object."},
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

	result, err := RunRecipe(nil, RecipeOptions{
		SessionDir:       sessionDir,
		Task:             "Frame this ordinary prose task",
		RecipeID:         "  neutral-root  ",
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
		backendFactory: successfulRootBackendFactory(),
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
	if result["execution_kind"] != "recipe" || result["status"] != "completed" || intFromAny(result["actual_participant_turns"], -1) != 2 {
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
	if meta.String("recipe_id") != "neutral-root" {
		t.Fatalf("normalized recipe id = %q", meta.String("recipe_id"))
	}
	if meta.String(workspace.WorkspaceContentSourceKey) != workspace.WorkspaceContentSourceWorkingTree ||
		meta.Get(workspace.WorkingTreeChangesIncludedKey) != true ||
		meta.Get(workspace.WorkspaceProvenanceInferredKey) != false {
		t.Fatalf("workspace provenance projection = %#v", meta.ToMap())
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
	checkpointRefs := meta.Slice("root_checkpoint_refs")
	if len(checkpointRefs) != 5 {
		t.Fatalf("root checkpoint refs = %#v", checkpointRefs)
	}
	workspaceCheckpoint := assertRootRecipeArtifact(t, st, checkpointRefs[0], contracts.RootArtifactKindRootCheckpoint, 1)
	if workspaceCheckpoint["phase"] != "workspace_ready" || workspaceCheckpoint["preflight_complete"] != true || workspaceCheckpoint["workspace_ready"] != true {
		t.Fatalf("workspace checkpoint = %#v", workspaceCheckpoint)
	}
	assertRootRecipeArtifact(
		t,
		st,
		workspaceCheckpoint["execution_workspace_ref"],
		contracts.RootArtifactKindExecutionWorkspace,
		0,
	)
	participantCheckpoint := assertRootRecipeArtifact(t, st, checkpointRefs[1], contracts.RootArtifactKindRootCheckpoint, 2)
	if participantCheckpoint["phase"] != "participant_turns_complete" || intFromAny(participantCheckpoint["participant_turns_completed"], 0) != 2 {
		t.Fatalf("participant checkpoint = %#v", participantCheckpoint)
	}
	cleanupCheckpoint := assertRootRecipeArtifact(t, st, checkpointRefs[4], contracts.RootArtifactKindRootCheckpoint, 5)
	if cleanupCheckpoint["phase"] != "cleanup_complete" || cleanupCheckpoint["status"] != "completed" {
		t.Fatalf("cleanup checkpoint = %#v", cleanupCheckpoint)
	}
	assertRootRecipeArtifact(t, st, meta.Get("raw_result_ref"), contracts.RootArtifactKindRawResult, 0)
	assertRootRecipeArtifact(t, st, meta.Get("result_validation_ref"), contracts.RootArtifactKindResultValidation, 0)
	if meta.Get("canonical_result_ref") != nil || meta.String("validation_status") != "not_required" {
		t.Fatalf("contractless result invented canonical validation: %#v", meta.ToMap())
	}
	assertRootRecipeArtifact(t, st, meta.Get("execution_workspace_ref"), contracts.RootArtifactKindExecutionWorkspace, 0)

	showReport, err := inspect.BuildShowTranscriptReport(sessionDir, 0, "")
	if err != nil {
		t.Fatalf("inspect root session: %v", err)
	}
	rootReport, _ := showReport["root"].(map[string]any)
	artifactValidation, _ := rootReport["artifact_validation"].(map[string]any)
	checkpointInspection, _ := rootReport["checkpoints"].(map[string]any)
	if artifactValidation["ok"] != true || checkpointInspection["ok"] != true {
		t.Fatalf("root inspection integrity = %#v / %#v", artifactValidation, checkpointInspection)
	}
	healthReport, err := inspect.BuildSessionHealthReport(sessionDir)
	if err != nil {
		t.Fatalf("inspect root health: %v", err)
	}
	if healthReport["status"] != "ok" || healthReport["root"] == nil {
		t.Fatalf("root health report = %#v", healthReport)
	}
	for _, rawCheck := range healthReport["checks"].([]any) {
		check := rawCheck.(map[string]any)
		if check["status"] != "ok" {
			t.Fatalf("root health check = %#v", check)
		}
	}

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
		t.Fatalf("recipe execution left a tracked provider process, err = %v", err)
	}
}

func TestRunRecipeContextOnlyUsesPersistedNamedInputAuthority(t *testing.T) {
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"stable"}`)
	config := rootRecipeRuntimeConfig("neutral/contract-v1")
	config.RelayRecipes["neutral-root"]["result_source"] = integration.ResultSourceReducer
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, `{"value":"stable"}`), nil
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:        sessionDir,
		Task:              "Use only the bound input",
		RecipeID:          "neutral-root",
		InputBindings:     []string{"payload=payload.json"},
		InvestigationMode: investigationModeContextOnly,
		LaunchCWD:         launchCWD,
		RuntimeConfig:     config,
		IntegrationBundle: decodeRootRecipeTestBundle(t, rootRecipeTestBundle),
		ReadinessCheck:    readyRootRecipeCheck,
		backendFactory:    recorder.factory(),
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if result["kind"] != "root_session_result" || intFromAny(result["schema_version"], 0) != 2 || result["prompt_policy_version"] != PromptPolicyVersionV2 {
		t.Fatalf("successor root result = %#v", result)
	}
	policy := result["prompt_policy"].(map[string]any)
	sources := policy["sources"].([]any)
	if policy["schema_version"] != PromptPolicyVersionV2 || len(sources) != 1 {
		t.Fatalf("prompt policy = %#v", policy)
	}
	source := sources[0].(map[string]any)
	manifestRef := result["named_input_manifest_ref"].(map[string]any)
	if source["kind"] != "named_input" || source["label"] != "payload" || len(source["content_refs"].([]any)) != 1 {
		t.Fatalf("authority source = %#v", source)
	}
	policyManifestRef := source["manifest_ref"].(map[string]any)
	if policyManifestRef["id"] != manifestRef["id"] || policyManifestRef["digest"] != manifestRef["digest"] {
		t.Fatalf("authority manifest ref = %#v, want %#v", policyManifestRef, manifestRef)
	}
	contentRef := source["content_refs"].([]any)[0].(map[string]any)
	for _, call := range recorder.snapshotCalls() {
		if call.SlotID == "facilitator" {
			continue
		}
		if !strings.Contains(call.Prompt, "[payload]") ||
			!strings.Contains(call.Prompt, stringFromAny(manifestRef["digest"])) ||
			!strings.Contains(call.Prompt, stringFromAny(contentRef["digest"])) {
			t.Fatalf("%s prompt does not cite stable named input authority:\n%s", call.SlotID, call.Prompt)
		}
	}
	report := inspect.BuildRootInspectionReport(sessionDir, result, false)
	if report["prompt_policy"].(map[string]any)["schema_version"] != PromptPolicyVersionV2 {
		t.Fatalf("inspection prompt policy = %#v", report["prompt_policy"])
	}
	isolation := report["isolation_report"].(map[string]any)
	if result["isolation_report_ref"] == nil || isolation["mechanism"] != "inherited" || isolation["filesystem_containment"] != "none" {
		t.Fatalf("successor isolation projection = %#v", isolation)
	}

	calls := recorder.snapshotCalls()
	invocationRefs := result["invocation_refs"].([]any)
	promptRefs := result["rendered_prompt_refs"].([]any)
	wantIDs := []string{"participant:000001", "facilitator:000001", "participant:000002", "facilitator:000002", "reducer:000001"}
	if len(calls) != len(wantIDs) || len(invocationRefs) != len(wantIDs) || len(promptRefs) != len(wantIDs) {
		t.Fatalf("provider accounting = calls %d invocations %d prompts %d", len(calls), len(invocationRefs), len(promptRefs))
	}
	st := store.New(sessionDir)
	for index, rawRef := range invocationRefs {
		payload := assertRootRecipeArtifact(t, st, rawRef, contracts.RootArtifactKindProviderInvocation, index+1)
		invocation, err := contracts.ValidateProviderInvocationRecord(payload["invocation"])
		if err != nil {
			t.Fatalf("invocation %d: %v", index+1, err)
		}
		promptPayload := assertRootRecipeArtifact(t, st, promptRefs[index], contracts.RootArtifactKindRenderedPrompt, index+1)
		if _, err := contracts.ValidateRenderedPromptRecord(promptPayload["rendered_prompt"]); err != nil {
			t.Fatalf("rendered prompt %d: %v", index+1, err)
		}
		if invocation["invocation_id"] != wantIDs[index] || invocation["runner_attempt"] != 1 ||
			invocation["provider_launch_attempted"] != true || invocation["provider_retry"] != recipes.ProviderRetryAllow ||
			invocation["outcome"] != "completed" || invocation["rendered_prompt_digest"] != contracts.RawBytesDigest([]byte(calls[index].Prompt)) {
			t.Fatalf("invocation %d = %#v", index+1, invocation)
		}
	}

	bundleDir := filepath.Join(t.TempDir(), "portable-bundle")
	exported, err := portable.Export(sessionDir, bundleDir, portable.Options{ConvoRelayVersion: "test"})
	if err != nil {
		t.Fatalf("portable export: %v", err)
	}
	for _, raw := range exported.Manifest["payload_inventory"].([]any) {
		entry := raw.(map[string]any)
		body, readErr := os.ReadFile(filepath.Join(exported.Directory, filepath.FromSlash(entry["path"].(string))))
		if readErr != nil || strings.Contains(string(body), sessionDir) || strings.Contains(string(body), launchCWD) {
			t.Fatalf("portable payload retained a required absolute path: %s, %v", entry["path"], readErr)
		}
	}
	relocated := filepath.Join(t.TempDir(), "relocated-bundle")
	if err := os.Rename(exported.Directory, relocated); err != nil {
		t.Fatalf("relocate portable export: %v", err)
	}
	if err := os.RemoveAll(sessionDir); err != nil {
		t.Fatalf("remove source session: %v", err)
	}
	verified, err := portable.VerifyDirectory(relocated)
	if err != nil || verified["status"] != "valid" || verified["terminal_status"] != "completed" {
		t.Fatalf("post-clean portable verification = %#v, %v", verified, err)
	}
	firstPayload := exported.Manifest["payload_inventory"].([]any)[0].(map[string]any)["path"].(string)
	if err := os.WriteFile(filepath.Join(relocated, filepath.FromSlash(firstPayload)), []byte("{}"), 0o644); err != nil {
		t.Fatalf("tamper portable payload: %v", err)
	}
	if _, err := portable.VerifyDirectory(relocated); err == nil {
		t.Fatal("tampered portable export verified")
	}
}

func TestRunRecipeContextOnlyWithoutBoundAuthorityFailsPurePreflight(t *testing.T) {
	object, err := contracts.DecodeStrictJSONObjectBytes([]byte(rootRecipeTestBundle))
	if err != nil {
		t.Fatalf("decode bundle object: %v", err)
	}
	input := object["contracts"].(map[string]any)["neutral/contract-v1"].(map[string]any)["inputs"].(map[string]any)["payload"].(map[string]any)
	input["required"] = false
	encoded, err := contracts.CanonicalJSONBytes(object)
	if err != nil {
		t.Fatalf("encode bundle: %v", err)
	}
	bundle, err := integration.DecodeBundleBytes(encoded)
	if err != nil {
		t.Fatalf("decode optional-input bundle: %v", err)
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	recorder := &rootBackendRecorder{}
	_, err = RunRecipe(context.Background(), RecipeOptions{
		SessionDir:        sessionDir,
		Task:              "Fail without authority",
		RecipeID:          "neutral-root",
		InvestigationMode: investigationModeContextOnly,
		LaunchCWD:         t.TempDir(),
		RuntimeConfig:     rootRecipeRuntimeConfig("neutral/contract-v1"),
		IntegrationBundle: bundle,
		ReadinessCheck:    readyRootRecipeCheck,
		backendFactory:    recorder.factory(),
	})
	assertRootRecipeDiagnostic(t, err, diagnosticCodePromptAuthority)
	if len(recorder.snapshotCalls()) != 0 {
		t.Fatalf("provider launched after pure preflight failure: %#v", recorder.snapshotCalls())
	}
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("session created after pure preflight failure: %v", statErr)
	}
}

func TestRootRecipeHealthCanonicalizesSessionPathAlias(t *testing.T) {
	fixtureRoot := t.TempDir()
	realParent := filepath.Join(fixtureRoot, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatalf("create real parent: %v", err)
	}
	aliasParent := filepath.Join(fixtureRoot, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("create session parent symlink: %v", err)
	}

	realLaunchCWD := filepath.Join(realParent, "launch")
	if err := os.Mkdir(realLaunchCWD, 0o755); err != nil {
		t.Fatalf("create launch CWD: %v", err)
	}
	writeRootRecipeTestFile(t, filepath.Join(realLaunchCWD, "payload.json"), `{"value":"stable"}`)
	aliasLaunchCWD := filepath.Join(aliasParent, "launch")
	aliasSessionDir := filepath.Join(aliasParent, "sessions", "alias-health-session")

	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, `{"value":"stable"}`), nil
	}
	if _, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:        aliasSessionDir,
		Task:              "Verify health through a session path alias",
		RecipeID:          "neutral-root",
		InputBindings:     []string{"payload=payload.json"},
		LaunchCWD:         aliasLaunchCWD,
		RuntimeConfig:     rootRecipeRuntimeConfig("neutral/contract-v1"),
		IntegrationBundle: decodeRootRecipeTestBundle(t, rootRecipeTestBundle),
		ReadinessCheck:    readyRootRecipeCheck,
		backendFactory:    recorder.factory(),
	}); err != nil {
		t.Fatalf("RunRecipe through alias: %v", err)
	}

	canonicalSessionDir, err := filepath.EvalSymlinks(aliasSessionDir)
	if err != nil {
		t.Fatalf("canonicalize session path: %v", err)
	}
	assertHealthy := func(requestedPath string) map[string]any {
		t.Helper()
		report, err := inspect.BuildSessionHealthReport(requestedPath)
		if err != nil {
			t.Fatalf("health report for %s: %v", requestedPath, err)
		}
		if report["status"] != "ok" || report["root"] == nil {
			t.Fatalf("health report for %s = %#v", requestedPath, report)
		}
		var namedInputCheck map[string]any
		for _, rawCheck := range report["checks"].([]any) {
			check := rawCheck.(map[string]any)
			if check["name"] == "root_named_input_digests" {
				namedInputCheck = check
				break
			}
		}
		if namedInputCheck == nil {
			t.Fatalf("health report for %s is missing root_named_input_digests: %#v", requestedPath, report)
		}
		if namedInputCheck["status"] != "ok" || namedInputCheck["retained_status"] != "ok" {
			t.Fatalf("named input health for %s = %#v", requestedPath, namedInputCheck)
		}
		return report
	}

	aliasReport := assertHealthy(aliasSessionDir)
	canonicalReport := assertHealthy(canonicalSessionDir)
	if aliasReport["session_dir"] != aliasSessionDir {
		t.Fatalf("alias report session_dir = %v, want %s", aliasReport["session_dir"], aliasSessionDir)
	}
	if canonicalReport["session_dir"] != canonicalSessionDir {
		t.Fatalf("canonical report session_dir = %v, want %s", canonicalReport["session_dir"], canonicalSessionDir)
	}
	aliasInfo, err := os.Stat(aliasSessionDir)
	if err != nil {
		t.Fatalf("stat alias session path: %v", err)
	}
	canonicalInfo, err := os.Stat(canonicalSessionDir)
	if err != nil {
		t.Fatalf("stat canonical session path: %v", err)
	}
	if !os.SameFile(aliasInfo, canonicalInfo) {
		t.Fatalf("session paths do not resolve to the same directory: alias=%s canonical=%s", aliasSessionDir, canonicalSessionDir)
	}
}

func TestRunRecipeBindsContractInputsAndPersistsExactArtifacts(t *testing.T) {
	launchCWD := t.TempDir()
	writeRootRecipeTestFile(t, filepath.Join(launchCWD, "payload.json"), `{"value":"stable"}`)
	bundle := decodeRootRecipeTestBundle(t, rootRecipeTestBundle)
	sessionDir := filepath.Join(t.TempDir(), "session")
	var compileTarget recipes.CompileTarget
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, `{"value":"stable"}`), nil
	}

	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:        sessionDir,
		Task:              "Validate the supplied payload",
		RecipeID:          "neutral-root",
		InputBindings:     []string{"payload=payload.json"},
		LaunchCWD:         launchCWD,
		RuntimeConfig:     rootRecipeRuntimeConfig("neutral/contract-v1"),
		IntegrationBundle: bundle,
		ReadinessCheck:    readyRootRecipeCheck,
		backendFactory:    recorder.factory(),
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
	inspection, err := inspect.BuildShowTranscriptReport(sessionDir, 0, "")
	if err != nil {
		t.Fatalf("inspect integration-bound session: %v", err)
	}
	rootInspection := inspection["root"].(map[string]any)
	integrationInspection := rootInspection["integration"].(map[string]any)
	inputInspection := rootInspection["named_inputs"].(map[string]any)
	if integrationInspection["bound"] != true || integrationInspection["contract_id"] != "neutral/contract-v1" ||
		inputInspection["status"] != "ok" || intFromAny(inputInspection["input_count"], 0) != 1 {
		t.Fatalf("integration/input inspection = %#v / %#v", integrationInspection, inputInspection)
	}
	rootJSON, err := contracts.CanonicalJSONBytes(rootInspection)
	if err != nil {
		t.Fatalf("encode root inspection: %v", err)
	}
	if strings.Contains(string(rootJSON), `"value":"stable"`) || strings.Contains(string(rootJSON), materializedPath) {
		t.Fatalf("ordinary inspection exposed named input content or provider path: %s", rootJSON)
	}
	missingManifestMeta := cloneMap(result)
	missingManifestMeta["named_input_manifest_ref"] = nil
	missingManifestInspection := inspect.BuildRootInspectionReport(sessionDir, missingManifestMeta, false)
	missingManifestArtifacts := missingManifestInspection["artifact_validation"].(map[string]any)
	missingManifestInputs := missingManifestInspection["named_inputs"].(map[string]any)
	if missingManifestArtifacts["ok"] != false || intFromAny(missingManifestArtifacts["required_missing"], 0) < 1 ||
		missingManifestInputs["status"] != "error" || missingManifestInputs["ok"] != false {
		t.Fatalf("missing manifest inspection = %#v / %#v", missingManifestArtifacts, missingManifestInputs)
	}
	missingManifestChecks := inspect.BuildRootHealthChecks(sessionDir, missingManifestMeta, missingManifestInspection)
	foundInputFailure := false
	for _, rawCheck := range missingManifestChecks {
		check := rawCheck.(map[string]any)
		if check["name"] == "root_named_input_digests" && check["status"] == "error" {
			foundInputFailure = true
		}
	}
	if !foundInputFailure {
		t.Fatalf("missing manifest health checks = %#v", missingManifestChecks)
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
			name: "programmatic task plan without CLI explicit marker",
			configure: func(options *RecipeOptions) {
				options.LaunchPlan = map[string]any{"plan": []any{}}
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
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, `{"settled":[],"contested":[],"withdrawn":[]}`), nil
		}
		return successfulRootTurn(call.Backend, `{"value":"context"}`), nil
	}
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:        sessionDir,
		Task:              "Context-compatible contract",
		RecipeID:          "neutral-root",
		ContextFiles:      []string{"context.md"},
		LaunchCWD:         launchCWD,
		RuntimeConfig:     rootRecipeRuntimeConfig("neutral/contract-v1"),
		IntegrationBundle: decodeRootRecipeTestBundle(t, rootRecipeTestBundleWithoutInputs),
		ReadinessCheck:    readyRootRecipeCheck,
		backendFactory:    recorder.factory(),
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	transcript, ok := result["transcript"].([]any)
	if !ok || len(transcript) != 2 {
		t.Fatalf("root transcript = %#v", result["transcript"])
	}
	if len(result["launch_context_refs"].([]any)) != 1 {
		t.Fatalf("launch context refs = %#v", result["launch_context_refs"])
	}
	manifest := assertRootRecipeArtifact(t, store.New(sessionDir), result["named_input_manifest_ref"], contracts.RootArtifactKindNamedInputManifest, 0)
	if intFromAny(manifest["input_count"], -1) != 0 {
		t.Fatalf("zero-input manifest = %#v", manifest)
	}
	participantCalls := 0
	for _, call := range recorder.snapshotCalls() {
		if call.SlotID == "facilitator" {
			continue
		}
		participantCalls++
		if !strings.Contains(call.Prompt, "--- Positional Context (Data Only) ---") ||
			!strings.Contains(call.Prompt, "ordinary positional context") ||
			strings.Contains(call.Prompt, "--- Named Inputs (Data Only) ---") {
			t.Fatalf("zero-input contract prompt did not preserve positional context:\n%s", call.Prompt)
		}
	}
	if participantCalls != 2 {
		t.Fatalf("participant calls = %d, want 2", participantCalls)
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
			name: "task plan contains unsupported value",
			configure: func(options *RecipeOptions) {
				options.LaunchPlan = map[string]any{"unsupported": make(chan int)}
			},
			wantCode: diagnosticCodeTaskPlanInvalid,
		},
		{
			name: "task plan contains non-finite number",
			configure: func(options *RecipeOptions) {
				options.LaunchPlan = map[string]any{"estimate": math.NaN()}
			},
			wantCode: diagnosticCodeTaskPlanInvalid,
		},
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

func TestRunRecipeRejectsEscapingRecipeArtifactIDsBeforeSessionCreation(t *testing.T) {
	t.Run("selected recipe", func(t *testing.T) {
		parent := t.TempDir()
		sessionDir := filepath.Join(parent, "session")
		unsafeID := "../../../outside-selected"
		config := rootRecipeRuntimeConfig("")
		recipe := config.RelayRecipes["neutral-root"]
		recipe["id"] = unsafeID
		config.RelayRecipes = map[string]map[string]any{unsafeID: recipe}

		_, err := RunRecipe(context.Background(), RecipeOptions{
			SessionDir:     sessionDir,
			Task:           "Reject unsafe selected recipe identity",
			RecipeID:       unsafeID,
			LaunchCWD:      t.TempDir(),
			RuntimeConfig:  config,
			ReadinessCheck: readyRootRecipeCheck,
		})
		assertRootRecipeDiagnostic(t, err, diagnosticCodeArtifactIDInvalid)
		assertNoUnsafeRootRecipePersistence(t, sessionDir, filepath.Join(parent, "outside-selected.json"))
	})

	t.Run("unselected transient recipe", func(t *testing.T) {
		parent := t.TempDir()
		sessionDir := filepath.Join(parent, "session")
		source := recipes.TransientRecipeSource{
			SourceType:  recipes.TransientRecipeSourceOrdinary,
			Path:        "unsafe-transient.toml",
			DisplayName: "unsafe-transient.toml",
			RawTOML: []byte(`
[relay_recipes."../../../outside-transient"]
purpose = "Must never become an artifact path."
participants = ["codex-deep", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "cooperative"
max_rounds = 1
max_depth = 1
`),
		}

		_, err := RunRecipe(context.Background(), RecipeOptions{
			SessionDir:       sessionDir,
			Task:             "Reject unsafe transient recipe identity",
			RecipeID:         "review-panel",
			LaunchCWD:        t.TempDir(),
			SettingsPath:     filepath.Join(t.TempDir(), "missing-settings.toml"),
			TransientSources: []recipes.TransientRecipeSource{source},
			ReadinessCheck:   readyRootRecipeCheck,
		})
		assertRootRecipeDiagnostic(t, err, diagnosticCodeArtifactIDInvalid)
		assertNoUnsafeRootRecipePersistence(t, sessionDir, filepath.Join(parent, "outside-transient.json"))
	})
}

func TestRunRecipeRejectsInvalidTransientRecipeRefsBeforeSessionCreation(t *testing.T) {
	tests := []struct {
		name     string
		recipeID string
	}{
		{name: "unsupported character", recipeID: "bad id"},
		{name: "overlength", recipeID: strings.Repeat("a", 300)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sessionDir := filepath.Join(t.TempDir(), "session")
			source := recipes.TransientRecipeSource{
				SourceType:  recipes.TransientRecipeSourceOrdinary,
				Path:        "invalid-ref.toml",
				DisplayName: "invalid-ref.toml",
				RawTOML: []byte(fmt.Sprintf(`
[relay_recipes.%q]
purpose = "Must be rejected before persistence."
participants = ["codex-deep", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "cooperative"
max_rounds = 1
max_depth = 1
`, test.recipeID)),
			}

			_, err := RunRecipe(context.Background(), RecipeOptions{
				SessionDir:       sessionDir,
				Task:             "Reject invalid transient recipe ref",
				RecipeID:         "review-panel",
				LaunchCWD:        t.TempDir(),
				SettingsPath:     filepath.Join(t.TempDir(), "missing-settings.toml"),
				TransientSources: []recipes.TransientRecipeSource{source},
				ReadinessCheck:   readyRootRecipeCheck,
			})
			assertRootRecipeDiagnostic(t, err, diagnosticCodeArtifactIDInvalid)
			if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
				t.Fatalf("invalid transient recipe ref created session, err = %v", statErr)
			}
		})
	}
}

func TestRunRecipeRejectsUnpersistableRuntimeConfigBeforeSessionCreation(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	config := rootRecipeRuntimeConfig("")
	config.BackendProfiles["unused-invalid-profile"] = map[string]any{
		"id":      "unused-invalid-profile",
		"backend": "codex",
		"ignored": make(chan int),
	}

	_, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     sessionDir,
		Task:           "Reject an unpersistable runtime snapshot",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
	})
	assertRootRecipeDiagnostic(t, err, diagnosticCodeRuntimeConfigInvalid)
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("unpersistable runtime config created session, err = %v", statErr)
	}
}

func assertNoUnsafeRootRecipePersistence(t *testing.T, sessionDir string, outsidePath string) {
	t.Helper()
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("unsafe recipe ID created session directory, err = %v", err)
	}
	if _, err := os.Stat(outsidePath); !os.IsNotExist(err) {
		t.Fatalf("unsafe recipe ID wrote outside session, err = %v", err)
	}
}

func TestRunRecipeRejectsIntegrationBoundNestedChildBeforeReadinessOrSession(t *testing.T) {
	config := rootRecipeRuntimeConfig("")
	config.BackendProfiles["bound-child-profile"] = map[string]any{
		"id":      "bound-child-profile",
		"backend": "relay",
		"model":   "bound-child",
		"effort":  1,
	}
	root := config.RelayRecipes["neutral-root"]
	root["participants"] = []any{"bound-child-profile", "participant-b"}
	root["max_depth"] = 3
	child := cloneMap(root)
	child["id"] = "bound-child"
	child["participants"] = []any{"participant-a", "participant-b"}
	child["max_depth"] = 1
	child["integration_contract"] = "neutral/contract-v1"
	config.RelayRecipes["bound-child"] = child

	sessionDir := filepath.Join(t.TempDir(), "session")
	readinessCalled := false
	_, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:    sessionDir,
		Task:          "Nested recipes must be preflighted",
		RecipeID:      "neutral-root",
		LaunchCWD:     t.TempDir(),
		RuntimeConfig: config,
		ReadinessCheck: func(context.Context, []string, readiness.Options) ([]readiness.Record, error) {
			readinessCalled = true
			return nil, nil
		},
	})
	var rootOnly *recipes.RootOnlyRecipeError
	if !errors.As(err, &rootOnly) || rootOnly.RecipeID != "bound-child" {
		t.Fatalf("nested child preflight error = %T %#v, want typed root-only error", err, err)
	}
	if readinessCalled {
		t.Fatal("backend readiness ran after nested child compilation failed")
	}
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("session created after nested child compilation failed, err = %v", statErr)
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
