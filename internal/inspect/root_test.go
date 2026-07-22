package inspect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestRootInspectionProjectionIsSharedDigestCheckedAndPayloadRedacted(t *testing.T) {
	sessionDir, meta := writeRootInspectionFixture(t)

	report := BuildRootInspectionReport(sessionDir, meta, false)
	if report == nil || report["execution_kind"] != "recipe" {
		t.Fatalf("root report = %#v", report)
	}
	validation := mapFromAny(report["artifact_validation"])
	if validation["ok"] != true || intFromAny(validation["invalid"], -1) != 0 || intFromAny(validation["required_missing"], -1) != 0 {
		t.Fatalf("artifact validation = %#v", validation)
	}
	checkpoints := mapFromAny(report["checkpoints"])
	if checkpoints["ok"] != true || checkpoints["chain_valid"] != true || checkpoints["latest_matches"] != true {
		t.Fatalf("checkpoint inspection = %#v", checkpoints)
	}
	providers := mapFromAny(report["providers"])
	participant := mapFromAny(asSlice(providers["participants"])[0])
	if participant["provider_state_recorded"] != true || participant["state"] != nil {
		t.Fatalf("participant summary = %#v", participant)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal root report: %v", err)
	}
	if strings.Contains(string(encoded), "opaque-provider-secret") || strings.Contains(string(encoded), "raw reducer output") || strings.Contains(string(encoded), "canonical_json") {
		t.Fatalf("ordinary root report exposed raw state or result payload: %s", encoded)
	}

	show, err := BuildShowTranscriptReport(sessionDir, 0, "")
	if err != nil {
		t.Fatalf("show report: %v", err)
	}
	if show["root"] == nil || strings.Contains(string(mustJSON(t, show["meta"])), "opaque-provider-secret") {
		t.Fatalf("show projection = %#v", show)
	}
	showMarkdown := FormatTranscriptMarkdown(show)
	if !strings.Contains(showMarkdown, "## Root Execution") || strings.Count(showMarkdown, "## Final Ledger") != 1 {
		t.Fatalf("show markdown root placement:\n%s", showMarkdown)
	}
	export, err := BuildExportReport(sessionDir, true)
	if err != nil || export["root"] == nil {
		t.Fatalf("export report = %#v, %v", export, err)
	}
	exportMarkdown := FormatExportMarkdown(export)
	if !strings.Contains(exportMarkdown, "## Root Execution") || strings.Count(exportMarkdown, "## Final Ledger") != 1 {
		t.Fatalf("export markdown root placement:\n%s", exportMarkdown)
	}
	contractsReport, err := BuildContractsReport(sessionDir, false, "", "")
	if err != nil || contractsReport["root"] == nil || strings.Contains(string(mustJSON(t, contractsReport)), "raw reducer output") {
		t.Fatalf("contracts report = %#v, %v", contractsReport, err)
	}
	rawContracts, err := BuildContractsReport(sessionDir, true, "", "")
	if err != nil || !strings.Contains(string(mustJSON(t, rawContracts["root"])), "raw reducer output") {
		t.Fatalf("raw contracts root report = %#v, %v", rawContracts["root"], err)
	}

	formatted := FormatRootSummary(report)
	for _, expected := range []string{"Root recipe: neutral-root", "Integration contract: none", "Participant turns: 2/2", "Result: reducer (validated)", "Workspace:", "Providers:", "Checkpoints:", "Cleanup:", "Root artifact refs:"} {
		if !strings.Contains(formatted, expected) {
			t.Fatalf("root summary missing %q:\n%s", expected, formatted)
		}
	}
}

func TestRootDisplaySeparatesReducerAndCanonicalResultsThroughValidatedRefs(t *testing.T) {
	sessionDir, meta := writeRootInspectionFixture(t)
	html, err := BuildDisplayHTML(sessionDir)
	if err != nil {
		t.Fatalf("display HTML: %v", err)
	}
	for _, expected := range []string{"Reducer Output", "raw reducer output", "Canonical Result", `{&#34;items&#34;:[&#34;one&#34;]}`} {
		if !strings.Contains(html, expected) {
			t.Fatalf("display missing %q:\n%s", expected, html)
		}
	}

	tampered := cloneObject(meta)
	rawRef := cloneObject(mapFromAny(meta["raw_result_ref"]))
	digest := stringFromAny(rawRef["digest"])
	rawRef["digest"] = digest[:len(digest)-1] + map[bool]string{true: "1", false: "0"}[strings.HasSuffix(digest, "0")]
	tampered["raw_result_ref"] = rawRef
	if err := store.New(sessionDir).SaveMetaMap(tampered); err != nil {
		t.Fatalf("save tampered meta: %v", err)
	}
	if _, err := BuildDisplayHTML(sessionDir); err == nil || !strings.Contains(err.Error(), "resolve root result output") {
		t.Fatalf("tampered display error = %v", err)
	}
}

func TestOrdinaryInspectionCompatibilityOmitsRootProjection(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "ordinary")
	st := store.New(sessionDir)
	meta := model.NewSessionMeta(map[string]any{
		"task": "ordinary", "status": "completed", "mode": "adversarial",
		"actual_rounds": 1, "max_rounds": 1,
		"ledger": map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}},
	})
	if err := st.SaveMeta(meta); err != nil {
		t.Fatalf("save ordinary meta: %v", err)
	}
	if err := st.SaveTranscriptItems([]any{map[string]any{
		"round": 1, "from": "Codex", "content": "ordinary body",
		"ledger": map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}},
	}}); err != nil {
		t.Fatalf("save ordinary transcript: %v", err)
	}
	report, err := BuildShowTranscriptReport(sessionDir, 0, "")
	if err != nil {
		t.Fatalf("ordinary show: %v", err)
	}
	if report["root"] != nil || BuildRootInspectionReport(sessionDir, meta.ToMap(), false) != nil {
		t.Fatalf("ordinary report gained root state: %#v", report)
	}
	if !strings.Contains(FormatTranscriptMarkdown(report), "ordinary body") {
		t.Fatalf("ordinary transcript formatting changed: %s", FormatTranscriptMarkdown(report))
	}
}

func TestRootInspectionLifecycleStateProjection(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		phase     string
		recovered bool
		cleanup   map[string]any
	}{
		{name: "invalid result", status: "invalid_result", phase: "root_validation_failed"},
		{name: "reducer failed", status: "reducer_failed", phase: "reducer_failed"},
		{name: "interrupted", status: "interrupted", phase: "reducer_interrupted"},
		{name: "recovered", status: "completed", phase: "result_complete", recovered: true},
		{name: "cleanup failed", status: "completed", phase: "result_complete", cleanup: map[string]any{"attempt": 2, "status": "failed", "stage": "execution_workspace", "error": "retry cleanup"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sessionDir, meta := writeRootInspectionFixture(t)
			meta["status"] = test.status
			meta["execution_phase"] = test.phase
			if test.recovered {
				meta["recovered_at"] = "2026-07-21T00:00:00Z"
			}
			if test.cleanup != nil {
				meta["workspace_cleanup"] = test.cleanup
			}
			if err := store.New(sessionDir).SaveMetaMap(meta); err != nil {
				t.Fatalf("save lifecycle meta: %v", err)
			}
			report := BuildRootInspectionReport(sessionDir, meta, false)
			if report["status"] != test.status || report["phase"] != test.phase || report["recovered"] != test.recovered {
				t.Fatalf("lifecycle projection = %#v", report)
			}
			if !strings.Contains(FormatRootSummary(report), "Root status: "+test.status+" ("+test.phase+")") {
				t.Fatalf("lifecycle summary =\n%s", FormatRootSummary(report))
			}
			if test.cleanup != nil {
				cleanup := mapFromAny(report["cleanup"])
				if cleanup["administrative_status"] != "failed" || cleanup["retryable"] != true || cleanup["stage"] != "execution_workspace" {
					t.Fatalf("cleanup failure projection = %#v", cleanup)
				}
			}
		})
	}
}

func TestRootHealthClassifiesSourceMutationRecoveryAndCleanupFailures(t *testing.T) {
	sessionDir, meta := writeRootInspectionFixture(t)
	meta["status"] = "failed"
	root := BuildRootInspectionReport(sessionDir, meta, false)
	workspaceState := mapFromAny(root["workspace"])
	workspaceState["source_check"] = "complete"
	workspaceState["source_changed"] = true
	workspaceState["source_mutated"] = true
	cleanup := mapFromAny(root["cleanup"])
	cleanup["recovery_pending"] = true
	cleanup["administrative_status"] = "failed"
	cleanup["stage"] = "execution_workspace"
	cleanup["retryable"] = true

	checks := BuildRootHealthChecks(sessionDir, meta, root)
	for _, name := range []string{"root_workspace_registration", "root_source_integrity", "root_cleanup_state"} {
		check := findRootHealthCheck(t, checks, name)
		if check["status"] != "error" {
			t.Fatalf("%s = %#v", name, check)
		}
	}
}

func writeRootInspectionFixture(t *testing.T) (string, map[string]any) {
	t.Helper()
	sessionDir := filepath.Join(t.TempDir(), "root-session")
	st := store.New(sessionDir)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session: %v", err)
	}
	recipeRef := saveInspectionContractArtifact(t, st, "recipes", "neutral-root", "recipe:neutral-root", map[string]any{
		"kind": "recipe_contract", "schema_version": 1, "id": "neutral-root",
	})
	runtimeRef := saveInspectionContractArtifact(t, st, "runtime_config", "selected", "runtime_config:selected", map[string]any{
		"kind": "runtime_config", "schema_version": 1, "recipes": map[string]any{},
	})
	rootPlanRef := saveInspectionRootArtifact(t, st, contracts.RootArtifactKindRootRecipePlan, 0, map[string]any{
		"recipe_id": "neutral-root", "participant_turns": 2, "result_source": "reducer",
	})
	workspaceRef := saveInspectionRootArtifact(t, st, contracts.RootArtifactKindExecutionWorkspace, 0, map[string]any{
		"policy":       map[string]any{"requested": "read_only", "effective": "read_only", "achieved": "ephemeral"},
		"source_check": "complete", "source_changed": false, "source_mutated": false,
	})

	checkpointRefs := []any{}
	var previous map[string]any
	for ordinal, phase := range []string{"workspace_ready", "participant_turns_complete", "reducer_complete", "root_validation_complete", "cleanup_complete"} {
		fields := map[string]any{
			"ordinal": ordinal + 1, "phase": phase, "status": "completed",
			"participant_turns_completed": map[bool]int{true: 0, false: 2}[ordinal == 0],
		}
		if ordinal > 1 {
			fields["previous_checkpoint_ref"] = previous
		}
		previous = saveInspectionRootArtifact(t, st, contracts.RootArtifactKindRootCheckpoint, ordinal+1, fields)
		checkpointRefs = append(checkpointRefs, previous)
	}
	reducerAttemptRef := saveInspectionRootArtifact(t, st, contracts.RootArtifactKindReducerAttempt, 1, map[string]any{
		"ordinal": 1, "status": "completed", "backend": "codex", "content": "raw reducer output",
	})
	rawResultRef := saveInspectionRootArtifact(t, st, contracts.RootArtifactKindRawResult, 0, map[string]any{
		"result_source": "reducer", "reducer_attempt_ref": reducerAttemptRef,
		"content": "raw reducer output", "content_bytes": len("raw reducer output"),
	})
	canonicalRef := saveInspectionRootArtifact(t, st, contracts.RootArtifactKindCanonicalResult, 0, map[string]any{
		"transport": "json", "raw_result_ref": rawResultRef,
		"canonical_json": `{"items":["one"]}`, "value": map[string]any{"items": []any{"one"}},
	})
	validationRef := saveInspectionRootArtifact(t, st, contracts.RootArtifactKindResultValidation, 0, map[string]any{
		"status": "validated", "result_source": "reducer", "raw_result_ref": rawResultRef,
		"canonical_result_ref": canonicalRef, "diagnostics": []any{},
	})

	meta := map[string]any{
		"session_id": "root-session", "execution_kind": "recipe", "recipe_id": "neutral-root",
		"task": "Inspect root output", "title": "Inspect root output", "status": "completed",
		"mode": "cooperative", "participant_turns": 2, "participant_turns_completed": 2,
		"actual_participant_turns": 2, "actual_rounds": 2, "max_rounds": 2,
		"result_source": "reducer", "validation_status": "validated", "cleanup_status": "completed",
		"workspace_isolation": "read_only", "workspace_effective_policy": "read_only", "workspace_achieved_policy": "ephemeral",
		"source_changed": false, "source_mutated": false,
		"recipe_ref": recipeRef, "runtime_config_ref": runtimeRef, "root_recipe_plan_ref": rootPlanRef,
		"execution_workspace_ref": workspaceRef, "root_checkpoint_refs": checkpointRefs, "latest_root_checkpoint_ref": previous,
		"reducer_attempt_refs": []any{reducerAttemptRef}, "latest_reducer_attempt_ref": reducerAttemptRef,
		"raw_result_ref": rawResultRef, "result_validation_ref": validationRef, "canonical_result_ref": canonicalRef,
		"slots": []any{
			map[string]any{"backend": "codex", "slot_id": "slot_0", "state": map[string]any{"token": "opaque-provider-secret"}},
			map[string]any{"backend": "codex", "slot_id": "slot_1", "state": map[string]any{"token": "opaque-provider-secret"}},
		},
		"facilitator_provider_state": map[string]any{"backend": "codex", "slot_id": "facilitator", "state": map[string]any{"token": "opaque-provider-secret"}},
		"reducer_provider_state":     map[string]any{"backend": "codex", "slot_id": "reducer", "state": map[string]any{"token": "opaque-provider-secret"}},
		"ledger":                     map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}},
	}
	if err := st.SaveMetaMap(meta); err != nil {
		t.Fatalf("save root meta: %v", err)
	}
	if err := st.SaveTranscriptItems([]any{
		map[string]any{"round": 1, "from": "Participant A", "content": "first", "ledger": map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}}},
		map[string]any{"round": 2, "from": "Participant B", "content": "second", "ledger": map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}}},
	}); err != nil {
		t.Fatalf("save root transcript: %v", err)
	}
	return sessionDir, meta
}

func saveInspectionRootArtifact(t *testing.T, st *store.Store, kind string, ordinal int, fields map[string]any) map[string]any {
	t.Helper()
	payload, err := contracts.NormalizeRootArtifact(kind, fields)
	if err != nil {
		t.Fatalf("normalize %s: %v", kind, err)
	}
	identity, err := contracts.RootArtifactIdentityFor(kind, ordinal)
	if err != nil {
		t.Fatalf("identity %s: %v", kind, err)
	}
	return saveInspectionContractArtifact(t, st, kind, identity.ArtifactID, identity.RefID, payload)
}

func saveInspectionContractArtifact(t *testing.T, st *store.Store, category string, artifactID string, refID string, payload map[string]any) map[string]any {
	t.Helper()
	ref, err := st.SaveContractArtifact(category, artifactID, payload, refID)
	if err != nil {
		t.Fatalf("save %s/%s: %v", category, artifactID, err)
	}
	return ref
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return body
}

func findRootHealthCheck(t *testing.T, checks []any, name string) map[string]any {
	t.Helper()
	for _, raw := range checks {
		check, _ := raw.(map[string]any)
		if check["name"] == name {
			return check
		}
	}
	t.Fatalf("missing health check %q: %#v", name, checks)
	return nil
}
