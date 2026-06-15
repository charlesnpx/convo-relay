package inspect

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

var ContractRefKeys = []string{
	"recipe_ref",
	"compiled_plan_ref",
	"child_invocation_ref",
	"child_result_ref",
}

func StrictValidationResult(st *store.Store) map[string]any {
	validation, err := st.ValidateStrictV1("native_v1")
	if err == nil {
		result := map[string]any{"ok": true}
		for key, value := range validation {
			result[key] = value
		}
		return result
	}
	return map[string]any{
		"ok":         false,
		"error_type": errorType(err),
		"error":      errorMessage(err),
	}
}

func InspectContractRef(st *store.Store, name string, ref any, includeRaw bool) map[string]any {
	status := map[string]any{
		"name":    name,
		"present": false,
		"ok":      false,
	}
	if !contracts.IsArtifactRef(ref) {
		if ref == nil {
			status["error"] = "missing artifact_ref"
		} else {
			status["error"] = "value is not an artifact_ref"
		}
		return status
	}

	artifactRef, err := contracts.ValidateArtifactRef(ref)
	if err != nil {
		status["present"] = true
		status["error_type"] = errorType(err)
		status["error"] = errorMessage(err)
		return status
	}
	status["present"] = true
	status["ref"] = artifactRef

	payload, err := st.LoadArtifactPayloadRaw(artifactRef)
	if err != nil {
		status["ok"] = false
		status["digest_valid"] = false
		status["error_type"] = errorType(err)
		status["error"] = errorMessage(err)
		return status
	}

	path, err := st.ArtifactPathForRef(artifactRef)
	if err != nil {
		status["ok"] = false
		status["digest_valid"] = false
		status["error_type"] = errorType(err)
		status["error"] = errorMessage(err)
		return status
	}

	status["ok"] = true
	status["digest_valid"] = true
	status["path"] = path
	status["kind"] = payload["kind"]
	status["schema_version"] = payload["schema_version"]
	if includeRaw {
		status["payload"] = payload
	}
	return status
}

func ChildContractBundleReport(st *store.Store, event map[string]any, includeRaw bool) map[string]any {
	payload, _ := event["payload"].(map[string]any)
	contractRefs, _ := payload["contract_refs"].(map[string]any)
	if contractRefs == nil {
		contractRefs = map[string]any{}
	}

	refs := map[string]any{}
	for _, key := range ContractRefKeys {
		refs[key] = InspectContractRef(st, key, contractRefs[key], includeRaw)
	}

	return map[string]any{
		"seq":              event["seq"],
		"event_id":         event["event_id"],
		"event_type":       event["event_type"],
		"node_id":          event["node_id"],
		"composition_path": payload["composition_path"],
		"child_session_id": payload["child_session_id"],
		"proposal_id":      payload["proposal_id"],
		"recipe_id":        payload["recipe_id"],
		"refs":             refs,
	}
}

func BuildContractsReport(sessionDir string, includeRaw bool, refID string, digest string) (map[string]any, error) {
	st := store.New(sessionDir)
	validation := StrictValidationResult(st)
	index := st.ArtifactIndex()

	events, err := st.ReadEvents()
	var eventsError string
	if err != nil {
		events = []map[string]any{}
		eventsError = errorMessage(err)
	}

	relayBackendBundles := []any{}
	dynamicBundles := []any{}
	for _, event := range events {
		switch event["event_type"] {
		case "relay_backend_child_completed":
			relayBackendBundles = append(relayBackendBundles, ChildContractBundleReport(st, event, includeRaw))
		case "child_session_completed":
			dynamicBundles = append(dynamicBundles, ChildContractBundleReport(st, event, includeRaw))
		}
	}

	eventCount := len(events)
	if count, ok := validation["event_count"]; ok {
		eventCount = intFromAny(count, eventCount)
	}
	report := map[string]any{
		"session_id":                           filepath.Base(filepath.Clean(sessionDir)),
		"validation":                           validation,
		"event_count":                          eventCount,
		"artifact_index_entry_count":           indexEntryCount(index),
		"artifact_index":                       index,
		"relay_backend_child_contract_bundles": relayBackendBundles,
		"dynamic_child_contract_bundles":       dynamicBundles,
	}
	if meta, err := LoadMeta(sessionDir); err == nil {
		report["runtime_config_ref"] = InspectContractRef(st, "runtime_config_ref", meta["runtime_config_ref"], includeRaw)
		report["transient_recipe_refs"] = inspectRefList(st, "transient_recipe_ref", asSlice(meta["transient_recipe_refs"]), includeRaw)
		report["slot_replacement_history"] = asSlice(meta["slot_replacement_history"])
	}
	if eventsError != "" {
		report["events_error"] = eventsError
	}
	if refID != "" {
		resolved, err := st.ResolveArtifactRef(refID, digest)
		if err != nil {
			return nil, err
		}
		report["resolved_ref"] = InspectContractRef(st, "resolved_ref", resolved, includeRaw)
	}
	return report, nil
}

func FormatContractsReport(report map[string]any) string {
	validation, _ := report["validation"].(map[string]any)
	validationLine := fmt.Sprintf("Validation: failed (%v)", valueOr(validation["error"], "unknown error"))
	if ok, _ := validation["ok"].(bool); ok {
		validationLine = fmt.Sprintf(
			"Validation: ok (%v, %v events)",
			valueOr(validation["mode"], "native_v1"),
			valueOr(validation["event_count"], 0),
		)
	}

	lines := []string{
		fmt.Sprintf("Contracts for %.8s", fmt.Sprint(report["session_id"])),
		validationLine,
		fmt.Sprintf("Events: %v", valueOr(report["event_count"], 0)),
		fmt.Sprintf("Artifact index entries: %v", valueOr(report["artifact_index_entry_count"], 0)),
		fmt.Sprintf("Relay-backend child contract bundles: %d", len(asSlice(report["relay_backend_child_contract_bundles"]))),
	}
	if runtimeRef, ok := report["runtime_config_ref"].(map[string]any); ok {
		lines = append(lines, fmt.Sprintf("Runtime config snapshot: %s", refStatusLabel(runtimeRef)))
	}
	if transientRefs := asSlice(report["transient_recipe_refs"]); len(transientRefs) > 0 {
		okCount := 0
		for _, rawRef := range transientRefs {
			if refStatus, ok := rawRef.(map[string]any); ok {
				if okValue, _ := refStatus["ok"].(bool); okValue {
					okCount++
				}
			}
		}
		lines = append(lines, fmt.Sprintf("Transient recipe refs: %d (%d ok)", len(transientRefs), okCount))
	}
	if replacements := asSlice(report["slot_replacement_history"]); len(replacements) > 0 {
		lines = append(lines, fmt.Sprintf("Slot replacements: %d", len(replacements)))
	}
	for _, bundle := range asSlice(report["relay_backend_child_contract_bundles"]) {
		if object, ok := bundle.(map[string]any); ok {
			lines = append(lines, formatBundleLine(object))
		}
	}
	lines = append(lines, fmt.Sprintf("Dynamic child contract bundles: %d", len(asSlice(report["dynamic_child_contract_bundles"]))))
	for _, bundle := range asSlice(report["dynamic_child_contract_bundles"]) {
		if object, ok := bundle.(map[string]any); ok {
			lines = append(lines, formatBundleLine(object))
		}
	}
	return strings.Join(lines, "\n")
}

func inspectRefList(st *store.Store, name string, refs []any, includeRaw bool) []any {
	results := make([]any, 0, len(refs))
	for _, rawRef := range refs {
		item, _ := rawRef.(map[string]any)
		artifactRef := any(nil)
		if item != nil {
			artifactRef = item["artifact_ref"]
		}
		status := InspectContractRef(st, name, artifactRef, includeRaw)
		for key, value := range item {
			if key != "artifact_ref" {
				status[key] = value
			}
		}
		results = append(results, status)
	}
	return results
}

func formatBundleLine(bundle map[string]any) string {
	label := firstNonEmpty(bundle["child_session_id"], bundle["proposal_id"], bundle["event_id"], "unknown")
	path, _ := bundle["composition_path"].(string)
	refs, _ := bundle["refs"].(map[string]any)
	statuses := make([]string, 0, len(ContractRefKeys))
	for _, key := range ContractRefKeys {
		status := map[string]any{}
		if refs != nil {
			if refStatus, ok := refs[key].(map[string]any); ok {
				status = refStatus
			}
		}
		statuses = append(statuses, strings.TrimSuffix(key, "_ref")+"="+refStatusLabel(status))
	}
	suffix := ""
	if path != "" {
		suffix = " [" + path + "]"
	}
	return fmt.Sprintf("  %s%s: %s", label, suffix, strings.Join(statuses, ", "))
}

func refStatusLabel(status map[string]any) string {
	if ok, _ := status["ok"].(bool); ok {
		return "ok"
	}
	if present, _ := status["present"].(bool); !present {
		return "missing"
	}
	return "invalid"
}

func errorType(err error) string {
	var validation contracts.ValidationError
	if errors.As(err, &validation) {
		return "ContractValidationError"
	}
	var notFound store.NotFoundError
	if errors.As(err, &notFound) {
		return "FileNotFoundError"
	}
	return fmt.Sprintf("%T", err)
}

func errorMessage(err error) string {
	if err == nil || err.Error() == "" {
		return errorType(err)
	}
	return err.Error()
}

func indexEntryCount(index map[string]any) int {
	entries, _ := index["entries"].([]any)
	return len(entries)
}

func intFromAny(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case jsonNumber:
		if value, err := typed.Int64(); err == nil {
			return int(value)
		}
	}
	return fallback
}

type jsonNumber interface {
	Int64() (int64, error)
}

func asSlice(value any) []any {
	if slice, ok := value.([]any); ok {
		return slice
	}
	return []any{}
}

func valueOr(value any, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...any) string {
	for _, value := range values {
		text := fmt.Sprint(value)
		if strings.TrimSpace(text) != "" && text != "<nil>" {
			return text
		}
	}
	return ""
}
