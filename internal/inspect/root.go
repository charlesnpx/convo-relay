package inspect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/namedinputs"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const rootExecutionKind = "recipe"

type inspectedRootRef struct {
	status  map[string]any
	payload map[string]any
}

// IsRootSession reports whether metadata belongs to a direct recipe execution.
func IsRootSession(meta map[string]any) bool {
	return strings.TrimSpace(stringFromAny(meta["execution_kind"])) == rootExecutionKind
}

// SanitizeMetaForInspection retains the established metadata shape while
// removing opaque provider state from direct recipe inspection responses.
func SanitizeMetaForInspection(meta map[string]any) map[string]any {
	result := cloneObject(meta)
	if !IsRootSession(meta) {
		return result
	}
	if slots, ok := result["slots"].([]any); ok {
		sanitized := make([]any, 0, len(slots))
		for _, raw := range slots {
			slot, _ := raw.(map[string]any)
			sanitized = append(sanitized, sanitizeProviderEnvelope(slot))
		}
		result["slots"] = sanitized
	}
	for _, key := range []string{
		"facilitator_provider_state", "reducer_provider_state",
		"facilitator_provider", "reducer_provider",
	} {
		if envelope, ok := result[key].(map[string]any); ok && !contracts.IsArtifactRef(envelope) {
			result[key] = sanitizeProviderEnvelope(envelope)
		}
	}
	if states, ok := result["provider_states"].(map[string]any); ok {
		sanitized := map[string]any{}
		for role, raw := range states {
			if envelope, ok := raw.(map[string]any); ok && !contracts.IsArtifactRef(envelope) {
				sanitized[role] = sanitizeProviderEnvelope(envelope)
			} else {
				sanitized[role] = raw
			}
		}
		result["provider_states"] = sanitized
	}
	for _, key := range []string{"facilitator_state", "reducer_state", "claude_session_id"} {
		if result[key] != nil {
			result[key+"_recorded"] = true
			delete(result, key)
		}
	}
	return result
}

// BuildRootInspectionReport creates the shared, payload-redacted projection
// used by every generic inspection surface. contracts --raw is the only caller
// that requests artifact payloads.
func BuildRootInspectionReport(sessionDir string, meta map[string]any, includeRaw bool) map[string]any {
	if !IsRootSession(meta) {
		return nil
	}
	st := store.New(sessionDir)
	contractID := strings.TrimSpace(stringFromAny(meta["integration_contract_id"]))
	validationStatus := strings.TrimSpace(stringFromAny(meta["validation_status"]))

	refs := map[string]any{}
	inspected := make([]inspectedRootRef, 0, 16)
	addGeneric := func(name string, ref any, required bool) inspectedRootRef {
		item := inspectGenericRootRef(st, name, ref, required, includeRaw)
		refs[name] = item.status
		inspected = append(inspected, item)
		return item
	}
	addRoot := func(name string, ref any, kind string, ordinal int, required bool) inspectedRootRef {
		item := inspectTypedRootRef(st, name, ref, kind, ordinal, required, includeRaw)
		refs[name] = item.status
		inspected = append(inspected, item)
		return item
	}

	recipeRef := addGeneric("recipe_ref", meta["recipe_ref"], true)
	runtimeConfigRef := addGeneric("runtime_config_ref", meta["runtime_config_ref"], true)
	rootPlanRef := addRoot("root_recipe_plan_ref", meta["root_recipe_plan_ref"], contracts.RootArtifactKindRootRecipePlan, 0, true)
	bundleRef := addRoot("integration_bundle_ref", meta["integration_bundle_ref"], contracts.RootArtifactKindIntegrationBundle, 0, contractID != "")
	contractRef := addRoot("integration_contract_ref", meta["integration_contract_ref"], contracts.RootArtifactKindIntegrationContract, 0, contractID != "")
	manifestRef := addRoot("named_input_manifest_ref", meta["named_input_manifest_ref"], contracts.RootArtifactKindNamedInputManifest, 0, false)
	workspaceRef := addRoot("execution_workspace_ref", meta["execution_workspace_ref"], contracts.RootArtifactKindExecutionWorkspace, 0, true)
	rawResultRef := addRoot("raw_result_ref", meta["raw_result_ref"], contracts.RootArtifactKindRawResult, 0, false)
	validationRef := addRoot("result_validation_ref", meta["result_validation_ref"], contracts.RootArtifactKindResultValidation, 0, validationStatus != "")
	canonicalRef := addRoot("canonical_result_ref", meta["canonical_result_ref"], contracts.RootArtifactKindCanonicalResult, 0, validationStatus == "validated")

	checkpointItems, checkpointInspected := inspectRootRefList(st, "root_checkpoint_ref", meta["root_checkpoint_refs"], contracts.RootArtifactKindRootCheckpoint, includeRaw)
	inspected = append(inspected, checkpointInspected...)
	latestCheckpoint := inspectTypedRootRef(st, "latest_root_checkpoint_ref", meta["latest_root_checkpoint_ref"], contracts.RootArtifactKindRootCheckpoint, ordinalFromRootRef(meta["latest_root_checkpoint_ref"], contracts.RootArtifactKindRootCheckpoint), true, includeRaw)
	inspected = append(inspected, latestCheckpoint)
	refs["latest_root_checkpoint_ref"] = latestCheckpoint.status

	reducerAttemptItems, reducerAttemptInspected := inspectRootRefList(st, "reducer_attempt_ref", meta["reducer_attempt_refs"], contracts.RootArtifactKindReducerAttempt, includeRaw)
	inspected = append(inspected, reducerAttemptInspected...)
	latestReducerAttempt := inspectTypedRootRef(st, "latest_reducer_attempt_ref", meta["latest_reducer_attempt_ref"], contracts.RootArtifactKindReducerAttempt, ordinalFromRootRef(meta["latest_reducer_attempt_ref"], contracts.RootArtifactKindReducerAttempt), false, includeRaw)
	inspected = append(inspected, latestReducerAttempt)
	refs["latest_reducer_attempt_ref"] = latestReducerAttempt.status

	checkpoints := rootCheckpointSummary(checkpointItems, checkpointInspected, latestCheckpoint)
	inputs := namedInputIntegrity(st, manifestRef, contractID)
	workspaceSummary := rootWorkspaceSummary(meta, workspaceRef)
	providers := rootProviderSummaries(meta)
	cleanup := rootCleanupSummary(meta)

	result := map[string]any{
		"execution_kind": rootExecutionKind,
		"status":         valueOr(meta["status"], "unknown"),
		"phase":          meta["execution_phase"],
		"recovered":      meta["recovered_at"] != nil,
		"recipe": map[string]any{
			"id":                   meta["recipe_id"],
			"recipe_ref":           recipeRef.status,
			"root_recipe_plan_ref": rootPlanRef.status,
			"runtime_config_ref":   runtimeConfigRef.status,
		},
		"integration": map[string]any{
			"bound":                    contractID != "",
			"contract_id":              emptyStringAsNil(contractID),
			"integration_bundle_ref":   bundleRef.status,
			"integration_contract_ref": contractRef.status,
		},
		"turns": map[string]any{
			"configured": valueOr(meta["participant_turns"], meta["rounds"]),
			"completed":  valueOr(meta["participant_turns_completed"], meta["actual_participant_turns"]),
		},
		"result": map[string]any{
			"source":                meta["result_source"],
			"validation_status":     valueOr(meta["validation_status"], "pending"),
			"raw_result_ref":        rawResultRef.status,
			"result_validation_ref": validationRef.status,
			"canonical_result_ref":  canonicalRef.status,
		},
		"workspace":           workspaceSummary,
		"providers":           providers,
		"checkpoints":         checkpoints,
		"reducer_attempts":    map[string]any{"count": len(reducerAttemptItems), "refs": reducerAttemptItems, "latest_ref": latestReducerAttempt.status},
		"cleanup":             cleanup,
		"named_inputs":        inputs,
		"artifact_refs":       refs,
		"artifact_validation": rootArtifactValidationSummary(inspected, len(checkpointItems) > 0),
	}
	return result
}

// BuildRootHealthChecks performs the read-only live checks that are too
// expensive for list/show: persisted input bytes and retained worktree state.
func BuildRootHealthChecks(sessionDir string, meta map[string]any, root map[string]any) []any {
	if root == nil || !IsRootSession(meta) {
		return nil
	}
	checks := []any{}
	artifactValidation, _ := root["artifact_validation"].(map[string]any)
	checks = append(checks, healthCheckFromOK("root_artifact_refs", boolFromAny(artifactValidation["ok"]), artifactValidation))

	inputs, _ := root["named_inputs"].(map[string]any)
	inputStatus := strings.TrimSpace(stringFromAny(inputs["status"]))
	checks = append(checks, map[string]any{
		"name":   "root_named_input_digests",
		"status": map[bool]string{true: "ok", false: "error"}[inputStatus == "ok" || inputStatus == "not_applicable"],
		"detail": inputs,
	})

	workspaceCheck := map[string]any{"name": "root_workspace_registration", "status": "ok"}
	recovered, err := workspace.Recover(context.Background(), store.New(sessionDir))
	if err != nil {
		workspaceCheck["status"] = "error"
		workspaceCheck["error"] = err.Error()
	} else {
		workspaceCheck["execution_cwd_present"] = recovered != nil && strings.TrimSpace(recovered.ExecutionCWD) != ""
		workspaceCheck["retained_worktree"] = recovered != nil && strings.TrimSpace(recovered.WorktreePath) != ""
	}
	checks = append(checks, workspaceCheck)

	workspaceState, _ := root["workspace"].(map[string]any)
	sourceCheck := map[string]any{
		"name":           "root_source_integrity",
		"status":         "ok",
		"source_check":   valueOr(workspaceState["source_check"], "pending"),
		"source_changed": valueOr(workspaceState["source_changed"], false),
		"source_mutated": valueOr(workspaceState["source_mutated"], false),
	}
	if boolFromAny(workspaceState["source_mutated"]) {
		sourceCheck["status"] = "error"
	} else if terminalRootStatus(meta) && strings.TrimSpace(stringFromAny(workspaceState["source_check"])) == "pending" {
		sourceCheck["status"] = "error"
	}
	checks = append(checks, sourceCheck)

	checkpoints, _ := root["checkpoints"].(map[string]any)
	checks = append(checks, healthCheckFromOK("root_checkpoints", boolFromAny(checkpoints["ok"]), checkpoints))

	cleanup, _ := root["cleanup"].(map[string]any)
	cleanupStatus := "ok"
	if boolFromAny(cleanup["recovery_pending"]) || strings.TrimSpace(stringFromAny(cleanup["administrative_status"])) == "failed" {
		cleanupStatus = "error"
	}
	checks = append(checks, map[string]any{"name": "root_cleanup_state", "status": cleanupStatus, "detail": cleanup})
	return checks
}

func inspectGenericRootRef(st *store.Store, name string, ref any, required bool, includeRaw bool) inspectedRootRef {
	if ref == nil {
		return inspectedRootRef{status: missingRefStatus(name, required)}
	}
	status := InspectContractRef(st, name, ref, includeRaw)
	status["required"] = required
	status["status"] = map[bool]string{true: "valid", false: "invalid"}[boolFromAny(status["ok"])]
	var payload map[string]any
	if boolFromAny(status["ok"]) {
		artifactRef, _ := status["ref"].(map[string]any)
		payload, _ = st.LoadArtifactPayloadRaw(artifactRef)
	}
	return inspectedRootRef{status: status, payload: payload}
}

func inspectTypedRootRef(st *store.Store, name string, ref any, kind string, ordinal int, required bool, includeRaw bool) inspectedRootRef {
	if ref == nil {
		return inspectedRootRef{status: missingRefStatus(name, required)}
	}
	item := inspectGenericRootRef(st, name, ref, required, false)
	status := item.status
	if !boolFromAny(status["ok"]) {
		return item
	}
	if ordinal < 0 {
		status["ok"] = false
		status["status"] = "invalid"
		status["root_contract_valid"] = false
		status["error"] = fmt.Sprintf("%s has an invalid ordinal identity", name)
		return inspectedRootRef{status: status, payload: item.payload}
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, kind, ordinal, item.payload); err != nil {
		status["ok"] = false
		status["status"] = "invalid"
		status["root_contract_valid"] = false
		status["error_type"] = errorType(err)
		status["error"] = errorMessage(err)
		return inspectedRootRef{status: status, payload: item.payload}
	}
	status["root_contract_valid"] = true
	status["kind"] = kind
	status["summary"] = summarizeRootArtifact(item.payload)
	if includeRaw {
		status["payload"] = cloneObject(item.payload)
	}
	return inspectedRootRef{status: status, payload: item.payload}
}

func inspectRootRefList(st *store.Store, name string, value any, kind string, includeRaw bool) ([]any, []inspectedRootRef) {
	rawRefs := asSlice(value)
	items := make([]any, 0, len(rawRefs))
	inspected := make([]inspectedRootRef, 0, len(rawRefs))
	for index, raw := range rawRefs {
		item := inspectTypedRootRef(st, fmt.Sprintf("%s_%d", name, index+1), raw, kind, ordinalFromRootRef(raw, kind), true, includeRaw)
		items = append(items, item.status)
		inspected = append(inspected, item)
	}
	return items, inspected
}

func ordinalFromRootRef(value any, kind string) int {
	if value == nil {
		return 0
	}
	ref, ok := value.(map[string]any)
	if !ok {
		return -1
	}
	id := strings.TrimSpace(stringFromAny(ref["id"]))
	prefix := kind + ":"
	if !strings.HasPrefix(id, prefix) {
		return -1
	}
	ordinal, err := contracts.RootArtifactOrdinalFromID(kind, strings.TrimPrefix(id, prefix))
	if err != nil {
		return -1
	}
	return ordinal
}

func rootCheckpointSummary(items []any, inspected []inspectedRootRef, latest inspectedRootRef) map[string]any {
	valid := 0
	chainValid := true
	for index, item := range inspected {
		if boolFromAny(item.status["ok"]) {
			valid++
		} else {
			chainValid = false
			continue
		}
		ordinal := intFromAny(item.payload["ordinal"], 0)
		if ordinal != index+1 {
			chainValid = false
		}
		if index > 1 {
			previous, _ := item.payload["previous_checkpoint_ref"].(map[string]any)
			priorRef, _ := inspected[index-1].status["ref"].(map[string]any)
			if !sameArtifactRef(previous, priorRef) {
				chainValid = false
			}
		}
	}
	latestMatches := len(inspected) > 0 && sameArtifactRef(mapFromAny(latest.status["ref"]), mapFromAny(inspected[len(inspected)-1].status["ref"]))
	ok := len(inspected) > 0 && valid == len(inspected) && chainValid && boolFromAny(latest.status["ok"]) && latestMatches
	return map[string]any{
		"count":          len(items),
		"valid_count":    valid,
		"refs":           items,
		"latest_ref":     latest.status,
		"chain_valid":    chainValid,
		"latest_matches": latestMatches,
		"ok":             ok,
	}
}

func rootArtifactValidationSummary(items []inspectedRootRef, hasCheckpoint bool) map[string]any {
	checked, valid, invalid, missingRequired := 0, 0, 0, 0
	seen := map[string]bool{}
	for _, item := range items {
		status := item.status
		if !boolFromAny(status["present"]) {
			if boolFromAny(status["required"]) {
				missingRequired++
			}
			continue
		}
		ref, _ := status["ref"].(map[string]any)
		key := stringFromAny(ref["id"]) + "\x00" + stringFromAny(ref["digest"])
		if key != "\x00" && seen[key] {
			continue
		}
		seen[key] = true
		checked++
		if boolFromAny(status["ok"]) {
			valid++
		} else {
			invalid++
		}
	}
	if !hasCheckpoint {
		missingRequired++
	}
	return map[string]any{
		"ok":               invalid == 0 && missingRequired == 0,
		"checked":          checked,
		"valid":            valid,
		"invalid":          invalid,
		"required_missing": missingRequired,
	}
}

func namedInputIntegrity(st *store.Store, manifest inspectedRootRef, contractID string) map[string]any {
	if !boolFromAny(manifest.status["present"]) {
		return map[string]any{"status": "not_applicable", "ok": true, "input_count": 0}
	}
	if !boolFromAny(manifest.status["ok"]) {
		return map[string]any{"status": "error", "ok": false, "error": valueOr(manifest.status["error"], "invalid named input manifest")}
	}
	ref, _ := manifest.status["ref"].(map[string]any)
	values, err := namedinputs.LoadAssertionInputs(st, ref, contractID)
	if err != nil {
		return map[string]any{"status": "error", "ok": false, "error": err.Error()}
	}
	count := 0
	names := make([]any, 0, len(values))
	for name, items := range values {
		names = append(names, map[string]any{"name": name, "value_count": len(items)})
		count += len(items)
	}
	sort.Slice(names, func(left int, right int) bool {
		return stringFromAny(mapFromAny(names[left])["name"]) < stringFromAny(mapFromAny(names[right])["name"])
	})
	inputCount := intFromAny(manifest.payload["input_count"], count)
	return map[string]any{"status": "ok", "ok": true, "input_count": inputCount, "json_value_count": count, "json_inputs": names}
}

func rootWorkspaceSummary(meta map[string]any, ref inspectedRootRef) map[string]any {
	summary := map[string]any{
		"ref":                  ref.status,
		"configured_policy":    meta["workspace_isolation"],
		"effective_policy":     meta["workspace_effective_policy"],
		"achieved_policy":      meta["workspace_achieved_policy"],
		"source_check":         "pending",
		"source_changed":       valueOr(meta["source_changed"], false),
		"source_mutated":       valueOr(meta["source_mutated"], false),
		"source_before_digest": meta["source_before_digest"],
		"source_after_digest":  meta["source_after_digest"],
	}
	if ref.payload != nil {
		if policy, ok := ref.payload["policy"].(map[string]any); ok {
			summary["configured_policy"] = valueOr(summary["configured_policy"], policy["requested"])
			summary["effective_policy"] = valueOr(summary["effective_policy"], policy["effective"])
			summary["achieved_policy"] = valueOr(summary["achieved_policy"], policy["achieved"])
		}
		for _, key := range []string{"source_check", "source_changed", "source_mutated", "source_before_digest", "source_after_digest"} {
			if ref.payload[key] != nil {
				summary[key] = ref.payload[key]
			}
		}
	}
	return summary
}

func rootProviderSummaries(meta map[string]any) map[string]any {
	participants := []any{}
	for index, raw := range asSlice(meta["slots"]) {
		envelope, _ := raw.(map[string]any)
		summary := sanitizeProviderEnvelope(envelope)
		summary["role"] = "participant"
		summary["ordinal"] = index + 1
		participants = append(participants, summary)
	}
	return map[string]any{
		"participants": participants,
		"facilitator":  providerRoleSummary(meta, "facilitator"),
		"reducer":      providerRoleSummary(meta, "reducer"),
	}
}

func providerRoleSummary(meta map[string]any, role string) any {
	var envelope map[string]any
	for _, key := range []string{role + "_provider_state", role + "_provider"} {
		if candidate, ok := meta[key].(map[string]any); ok && !contracts.IsArtifactRef(candidate) {
			envelope = candidate
			break
		}
	}
	configured := mapFromAny(meta[role])
	if envelope == nil && len(configured) == 0 && meta[role+"_backend"] == nil {
		return nil
	}
	summary := sanitizeProviderEnvelope(envelope)
	for _, key := range []string{"backend", "model", "effort", "profile_id"} {
		if summary[key] == nil {
			summary[key] = firstPresent(meta[role+"_"+key], configured[key])
		}
	}
	summary["role"] = role
	return summary
}

func sanitizeProviderEnvelope(envelope map[string]any) map[string]any {
	result := map[string]any{}
	if envelope == nil {
		return result
	}
	for _, key := range []string{"backend", "profile_id", "slot_id", "logical_slot_id", "generation", "label", "model", "effort"} {
		if envelope[key] != nil {
			result[key] = envelope[key]
		}
	}
	if envelope["state"] != nil {
		result["provider_state_recorded"] = true
	}
	return result
}

func rootCleanupSummary(meta map[string]any) map[string]any {
	result := map[string]any{
		"result_cleanup_status": valueOr(meta["cleanup_status"], "pending"),
		"recovery_pending":      valueOr(meta["root_recovery_pending"], false),
		"administrative_status": "not_started",
	}
	if cleanup, ok := meta["workspace_cleanup"].(map[string]any); ok {
		result["administrative_status"] = valueOr(cleanup["status"], "unknown")
		result["stage"] = cleanup["stage"]
		result["attempt"] = cleanup["attempt"]
		result["retryable"] = strings.TrimSpace(stringFromAny(cleanup["status"])) == "failed"
		if cleanup["error"] != nil {
			result["error"] = cleanup["error"]
		}
	}
	return result
}

func summarizeRootArtifact(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	summary := map[string]any{"kind": payload["kind"], "schema_version": payload["schema_version"]}
	for _, key := range []string{
		"recipe_id", "integration_contract_id", "contract_id", "input_count",
		"ordinal", "phase", "status", "participant_turns", "participant_turns_completed",
		"result_source", "participant_turn", "content_bytes", "transport",
		"validation_status", "cleanup_status", "source_check", "source_changed", "source_mutated",
	} {
		if payload[key] != nil {
			summary[key] = payload[key]
		}
	}
	return summary
}

// LoadDisplayRootResults resolves result content only through digest-validated
// refs. Ordinary inspection reports never include these payload fields.
func LoadDisplayRootResults(sessionDir string, meta map[string]any) (map[string]string, error) {
	if !IsRootSession(meta) {
		return nil, nil
	}
	st := store.New(sessionDir)
	results := map[string]string{}
	if meta["raw_result_ref"] != nil {
		item := inspectTypedRootRef(st, "raw_result_ref", meta["raw_result_ref"], contracts.RootArtifactKindRawResult, 0, true, false)
		if !boolFromAny(item.status["ok"]) {
			return nil, fmt.Errorf("resolve root result output: %v", valueOr(item.status["error"], "invalid raw_result_ref"))
		}
		results["raw"] = stringFromAny(item.payload["content"])
	}
	if meta["canonical_result_ref"] != nil {
		item := inspectTypedRootRef(st, "canonical_result_ref", meta["canonical_result_ref"], contracts.RootArtifactKindCanonicalResult, 0, true, false)
		if !boolFromAny(item.status["ok"]) {
			return nil, fmt.Errorf("resolve canonical result: %v", valueOr(item.status["error"], "invalid canonical_result_ref"))
		}
		results["canonical"] = stringFromAny(item.payload["canonical_json"])
	}
	return results, nil
}

// FormatRootSummary renders the same compact root fields for all human
// inspection commands.
func FormatRootSummary(root map[string]any) string {
	if root == nil {
		return ""
	}
	recipe := mapFromAny(root["recipe"])
	integration := mapFromAny(root["integration"])
	turns := mapFromAny(root["turns"])
	result := mapFromAny(root["result"])
	workspaceState := mapFromAny(root["workspace"])
	providers := mapFromAny(root["providers"])
	checkpoints := mapFromAny(root["checkpoints"])
	cleanup := mapFromAny(root["cleanup"])
	artifacts := mapFromAny(root["artifact_validation"])
	contract := firstNonEmpty(integration["contract_id"], "none")
	providerCount := len(asSlice(providers["participants"]))
	if providers["facilitator"] != nil {
		providerCount++
	}
	if providers["reducer"] != nil {
		providerCount++
	}
	return strings.Join([]string{
		fmt.Sprintf("Root recipe: %v", valueOr(recipe["id"], "unknown")),
		fmt.Sprintf("Root status: %v (%v)", valueOr(root["status"], "unknown"), valueOr(root["phase"], "unknown")),
		fmt.Sprintf("Integration contract: %s", contract),
		fmt.Sprintf("Participant turns: %v/%v", valueOr(turns["completed"], 0), valueOr(turns["configured"], "?")),
		fmt.Sprintf("Result: %v (%v)", valueOr(result["source"], "pending"), valueOr(result["validation_status"], "pending")),
		fmt.Sprintf("Workspace: %v/%v, source %v", valueOr(workspaceState["effective_policy"], "unknown"), valueOr(workspaceState["achieved_policy"], "unknown"), valueOr(workspaceState["source_check"], "pending")),
		fmt.Sprintf("Providers: %d sanitized summaries", providerCount),
		fmt.Sprintf("Checkpoints: %v (%v valid, chain=%v)", valueOr(checkpoints["count"], 0), valueOr(checkpoints["valid_count"], 0), valueOr(checkpoints["chain_valid"], false)),
		fmt.Sprintf("Cleanup: result=%v, administrative=%v", valueOr(cleanup["result_cleanup_status"], "pending"), valueOr(cleanup["administrative_status"], "not_started")),
		fmt.Sprintf("Root artifact refs: %v/%v valid", valueOr(artifacts["valid"], 0), valueOr(artifacts["checked"], 0)),
	}, "\n")
}

func healthCheckFromOK(name string, ok bool, detail map[string]any) map[string]any {
	status := "error"
	if ok {
		status = "ok"
	}
	return map[string]any{"name": name, "status": status, "detail": detail}
}

func missingRefStatus(name string, required bool) map[string]any {
	status := "not_applicable"
	if required {
		status = "missing"
	}
	return map[string]any{"name": name, "present": false, "ok": false, "required": required, "status": status}
}

func sameArtifactRef(left map[string]any, right map[string]any) bool {
	return left != nil && right != nil && left["id"] == right["id"] && left["digest"] == right["digest"]
}

func cloneObject(value map[string]any) map[string]any {
	cloned, _ := contracts.Materialize(value).(map[string]any)
	if cloned == nil {
		return map[string]any{}
	}
	return cloned
}

func mapFromAny(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func boolFromAny(value any) bool {
	result, _ := value.(bool)
	return result
}

func firstPresent(values ...any) any {
	for _, value := range values {
		if value != nil && strings.TrimSpace(stringFromAny(value)) != "" {
			return value
		}
	}
	return nil
}

func emptyStringAsNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func terminalRootStatus(meta map[string]any) bool {
	switch strings.TrimSpace(stringFromAny(meta["status"])) {
	case "ready", "running", "participant_turns", "participant_turns_complete", "reducer_complete", "candidate_complete", "root_validation_complete":
		return false
	default:
		return true
	}
}
