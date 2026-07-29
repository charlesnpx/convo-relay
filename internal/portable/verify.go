package portable

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

type verifiedPortablePayload struct {
	entry map[string]any
	value any
}

func VerifyDirectory(directory string) (map[string]any, error) {
	root, err := canonicalDirectory(directory)
	if err != nil {
		return nil, err
	}
	manifestBody, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil, err
	}
	manifestValue, err := contracts.DecodeStrictJSONObjectBytes(manifestBody)
	if err != nil {
		return nil, err
	}
	manifest, err := contracts.ValidatePortableExportManifest(manifestValue)
	if err != nil {
		return nil, err
	}
	expectedFiles := map[string]bool{"manifest.json": true}
	inventoryByID := map[string]map[string]any{}
	sourceRefs := map[string]string{}
	payloads := make([]verifiedPortablePayload, 0, len(manifest["payload_inventory"].([]any)))
	for _, raw := range manifest["payload_inventory"].([]any) {
		entry := raw.(map[string]any)
		if err := validatePortableInventorySource(entry, sourceRefs); err != nil {
			return nil, err
		}
		inventoryByID[entry["portable_id"].(string)] = entry
		relative := entry["path"].(string)
		expectedFiles[relative] = true
		filename := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(filename)
		if err != nil || !info.Mode().IsRegular() {
			return nil, contracts.NewValidationError("portable export payload %s is missing or not a regular file", relative)
		}
		declaredSize := int64(entry["size_bytes"].(int))
		if info.Size() != declaredSize {
			return nil, contracts.NewValidationError("portable export payload %s size or digest mismatch", relative)
		}
		body, err := os.ReadFile(filename)
		if err != nil {
			return nil, err
		}
		if int64(len(body)) != declaredSize || contracts.RawBytesDigest(body) != entry["digest"] {
			return nil, contracts.NewValidationError("portable export payload %s size or digest mismatch", relative)
		}
		value, err := contracts.DecodeStrictJSONBytes(body)
		if err != nil {
			return nil, fmt.Errorf("decode portable export payload %s: %w", relative, err)
		}
		payloads = append(payloads, verifiedPortablePayload{entry: entry, value: value})
	}
	for _, payload := range payloads {
		if err := validatePortablePayloadMatchesInventory(payload); err != nil {
			return nil, err
		}
		if refs := contracts.FindArtifactRefs(payload.value); len(refs) > 0 {
			return nil, contracts.NewValidationError("portable export retains a source-session artifact ref")
		}
		if err := validatePortablePayloadRefs(payload.value, inventoryByID); err != nil {
			return nil, err
		}
	}
	if err := validatePortableProviderLineage(payloads, inventoryByID); err != nil {
		return nil, err
	}
	if err := verifyClosedFileSet(root, expectedFiles); err != nil {
		return nil, err
	}
	return map[string]any{
		"schema_version":  manifest["schema_version"],
		"status":          "valid",
		"terminal_status": manifest["terminal_status"],
		"payload_count":   len(payloads),
		"manifest_digest": manifest["manifest_digest"],
	}, nil
}

func validatePortablePayloadRefs(value any, inventoryByID map[string]map[string]any) error {
	if object, ok := value.(map[string]any); ok {
		if portablePayloadRefShaped(object) {
			_, err := validatePortablePayloadRefObject(object, inventoryByID)
			return err
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, item := range typed {
			if err := validatePortablePayloadRefs(item, inventoryByID); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := validatePortablePayloadRefs(item, inventoryByID); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePortablePayloadMatchesInventory(payload verifiedPortablePayload) error {
	entryKind := payload.entry["kind"].(string)
	if portableSyntheticEntry(payload.entry) {
		return nil
	}
	if err := requirePortableEntrySourceIdentity(payload.entry, "portable "+entryKind+" payload"); err != nil {
		return err
	}
	object, ok := payload.value.(map[string]any)
	if !ok {
		return contracts.NewValidationError("portable export payload %s must be an object", payload.entry["portable_id"])
	}
	payloadKind, ok := object["kind"].(string)
	if !ok || payloadKind != entryKind {
		return contracts.NewValidationError("portable export payload %s kind does not match inventory kind %s", payload.entry["portable_id"], entryKind)
	}
	return nil
}

func validatePortableInventorySource(entry map[string]any, seen map[string]string) error {
	if !portableSyntheticEntry(entry) {
		if err := requirePortableEntrySourceIdentity(entry, "portable "+stringValue(entry["kind"])+" payload"); err != nil {
			return err
		}
	}
	if entry["source_artifact_id"] == nil {
		return nil
	}
	if err := requirePortableEntrySourceIdentity(entry, "portable "+stringValue(entry["kind"])+" payload"); err != nil {
		return err
	}
	sourceID := stringValue(entry["source_artifact_id"])
	sourceDigest := stringValue(entry["source_artifact_digest"])
	sourceKey := sourceID + "\x00" + sourceDigest
	if prior := seen[sourceKey]; prior != "" {
		return contracts.NewValidationError("portable export source artifact ref is duplicated by %s and %s", prior, entry["portable_id"])
	}
	seen[sourceKey] = stringValue(entry["portable_id"])

	entryKind := stringValue(entry["kind"])
	sourceKind, _, _ := strings.Cut(sourceID, ":")
	if (portableRootArtifactKind(entryKind) || portableRootArtifactKind(sourceKind)) && entryKind != sourceKind {
		return contracts.NewValidationError("portable export payload %s kind does not match source artifact kind %s", entry["portable_id"], sourceKind)
	}
	return nil
}

func portableSyntheticEntry(entry map[string]any) bool {
	kind := stringValue(entry["kind"])
	id := stringValue(entry["portable_id"])
	return kind == "root_session" && id == "session" ||
		kind == "participant_transcript" && id == "transcript" ||
		kind == "diagnostics" && id == "diagnostics"
}

func portableRootArtifactKind(value string) bool {
	for _, kind := range contracts.RootArtifactKinds() {
		if value == kind {
			return true
		}
	}
	return false
}

func portablePayloadRefShaped(object map[string]any) bool {
	if object["kind"] == "portable_payload_ref" {
		return true
	}
	for _, key := range []string{"portable_id", "source_artifact_id", "source_artifact_digest"} {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

func validatePortablePayloadRefObject(object map[string]any, inventoryByID map[string]map[string]any) (map[string]any, error) {
	if object["kind"] != "portable_payload_ref" {
		return nil, contracts.NewValidationError("portable payload ref requires kind portable_payload_ref")
	}
	portableID, ok := object["portable_id"].(string)
	if !ok || portableID == "" {
		return nil, contracts.NewValidationError("portable payload ref requires portable_id")
	}
	entry := inventoryByID[portableID]
	if entry == nil {
		return nil, contracts.NewValidationError("portable export payload closure is missing %s", portableID)
	}
	for key := range object {
		switch key {
		case "kind", "portable_id", "source_artifact_id", "source_artifact_digest":
		default:
			return nil, contracts.NewValidationError("portable payload ref contains unsupported field %q", key)
		}
	}
	refSourceID, refIDOK := object["source_artifact_id"].(string)
	refSourceDigest, refDigestOK := object["source_artifact_digest"].(string)
	if !refIDOK || strings.TrimSpace(refSourceID) == "" || !refDigestOK || strings.TrimSpace(refSourceDigest) == "" {
		return nil, contracts.NewValidationError("portable payload ref requires source artifact identity")
	}
	if _, err := contracts.ValidateArtifactRef(map[string]any{
		"kind":           "artifact_ref",
		"schema_version": 1,
		"id":             refSourceID,
		"digest":         refSourceDigest,
	}); err != nil {
		return nil, contracts.NewValidationError("portable payload ref source artifact identity is invalid: %v", err)
	}
	entrySourceID, entryIDOK := entry["source_artifact_id"].(string)
	entrySourceDigest, entryDigestOK := entry["source_artifact_digest"].(string)
	if !entryIDOK || strings.TrimSpace(entrySourceID) == "" || !entryDigestOK || strings.TrimSpace(entrySourceDigest) == "" {
		return nil, contracts.NewValidationError("portable payload ref target %s requires source artifact identity", portableID)
	}
	if refSourceID != entrySourceID || refSourceDigest != entrySourceDigest {
		return nil, contracts.NewValidationError("portable payload ref source identity mismatch for %s", portableID)
	}
	return entry, nil
}

func validatePortableProviderLineage(payloads []verifiedPortablePayload, inventoryByID map[string]map[string]any) error {
	resultRecords := map[string]map[string]any{}
	resultKeys := map[string]string{}
	for _, payload := range payloads {
		if payload.entry["kind"] != contracts.RootArtifactKindProviderResult {
			continue
		}
		portableID := payload.entry["portable_id"].(string)
		object, _ := payload.value.(map[string]any)
		if object == nil {
			return contracts.NewValidationError("portable provider result payload must be an object")
		}
		rootArtifact, err := contracts.ValidateRootArtifact(object, contracts.RootArtifactKindProviderResult)
		if err != nil {
			return contracts.NewValidationError("portable provider result root artifact is invalid: %v", err)
		}
		record, err := contracts.ValidateProviderResultRecord(rootArtifact)
		if err != nil {
			return err
		}
		key := portableProviderAttemptKey(record)
		if prior := resultKeys[key]; prior != "" {
			return contracts.NewValidationError("portable provider results %s and %s duplicate invocation_id and runner_attempt", prior, portableID)
		}
		resultKeys[key] = portableID
		resultRecords[portableID] = record
	}

	invocationKeys := map[string]string{}
	resultIncomingEdges := map[string]int{}
	for _, payload := range payloads {
		if payload.entry["kind"] != contracts.RootArtifactKindProviderInvocation {
			continue
		}
		portableID := payload.entry["portable_id"].(string)
		object, _ := payload.value.(map[string]any)
		if object == nil {
			return contracts.NewValidationError("portable provider invocation payload must be an object")
		}
		rootArtifact, err := contracts.ValidateRootArtifact(object, contracts.RootArtifactKindProviderInvocation)
		if err != nil {
			return contracts.NewValidationError("portable provider invocation root artifact is invalid: %v", err)
		}
		rawInvocation, _ := rootArtifact["invocation"].(map[string]any)
		if rawInvocation == nil {
			return contracts.NewValidationError("portable provider invocation payload is missing invocation")
		}
		invocationDraft := contracts.Materialize(rawInvocation).(map[string]any)
		resultRefValue := invocationDraft["provider_result_ref"]
		invocationDraft["provider_result_ref"] = nil
		invocation, err := contracts.ValidateProviderInvocationDraftRecord(invocationDraft)
		if err != nil {
			return contracts.NewValidationError("portable provider invocation is invalid: %v", err)
		}
		key := portableProviderAttemptKey(invocation)
		priorInvocation := invocationKeys[key]
		if invocation["provider_launch_attempted"] == false {
			if resultRefValue != nil {
				return contracts.NewValidationError("portable unlaunched provider invocation has provider_result_ref")
			}
			if priorInvocation != "" {
				return contracts.NewValidationError("portable provider invocations %s and %s duplicate invocation_id and runner_attempt", priorInvocation, portableID)
			}
			invocationKeys[key] = portableID
			continue
		}
		resultRef, ok := resultRefValue.(map[string]any)
		if !ok || resultRef == nil {
			return contracts.NewValidationError("portable launched provider invocation requires provider_result_ref")
		}
		entry, err := validatePortablePayloadRefObject(resultRef, inventoryByID)
		if err != nil {
			return err
		}
		if entry["kind"] != contracts.RootArtifactKindProviderResult {
			return contracts.NewValidationError("portable provider invocation result ref does not target provider_result")
		}
		resultPortableID := entry["portable_id"].(string)
		resultRecord := resultRecords[resultPortableID]
		if resultRecord == nil {
			return contracts.NewValidationError("portable provider invocation result ref does not target provider_result")
		}
		resultIncomingEdges[resultPortableID]++
		if resultIncomingEdges[resultPortableID] > 1 {
			return contracts.NewValidationError("portable provider result %s has multiple incoming invocation edges", resultPortableID)
		}
		if err := validatePortableProviderResultBinding(invocation, resultRecord); err != nil {
			return err
		}
		if priorInvocation != "" {
			return contracts.NewValidationError("portable provider invocations %s and %s duplicate invocation_id and runner_attempt", priorInvocation, portableID)
		}
		invocationKeys[key] = portableID
	}
	for portableID := range resultRecords {
		switch resultIncomingEdges[portableID] {
		case 1:
			continue
		case 0:
			return contracts.NewValidationError("portable provider result %s is orphaned", portableID)
		default:
			return contracts.NewValidationError("portable provider result %s has multiple incoming invocation edges", portableID)
		}
	}
	return nil
}

func requirePortableEntrySourceIdentity(entry map[string]any, label string) error {
	sourceID, idOK := entry["source_artifact_id"].(string)
	sourceDigest, digestOK := entry["source_artifact_digest"].(string)
	if !idOK || strings.TrimSpace(sourceID) == "" || !digestOK || strings.TrimSpace(sourceDigest) == "" {
		return contracts.NewValidationError("%s requires source artifact identity", label)
	}
	if _, err := contracts.ValidateArtifactRef(map[string]any{
		"kind":           "artifact_ref",
		"schema_version": 1,
		"id":             sourceID,
		"digest":         sourceDigest,
	}); err != nil {
		return contracts.NewValidationError("%s source artifact identity is invalid: %v", label, err)
	}
	return nil
}

func validatePortableProviderResultBinding(invocation map[string]any, resultRecord map[string]any) error {
	resultDraft, _ := resultRecord["invocation"].(map[string]any)
	if !portableSemanticEqual(invocation, resultDraft) {
		return contracts.NewValidationError("portable provider invocation/result identity mismatch")
	}
	return nil
}

func portableProviderAttemptKey(record map[string]any) string {
	return stringValue(record["invocation_id"]) + "\x00" + fmt.Sprint(record["runner_attempt"])
}

func portableSemanticEqual(left any, right any) bool {
	leftBytes, leftErr := contracts.SemanticJSONBytes(left)
	rightBytes, rightErr := contracts.SemanticJSONBytes(right)
	return leftErr == nil && rightErr == nil && string(leftBytes) == string(rightBytes)
}

func verifyClosedFileSet(root string, expected map[string]bool) error {
	expectedDirectories := map[string]bool{}
	for filename := range expected {
		for directory := path.Dir(filename); directory != "."; directory = path.Dir(directory) {
			expectedDirectories[directory] = true
		}
	}
	return filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return contracts.NewValidationError("portable export contains a symlink at %s", relative)
		}
		if entry.IsDir() {
			if expectedDirectories[relative] {
				return nil
			}
			return contracts.NewValidationError("portable export contains an unexpected directory %s", relative)
		}
		if !expected[relative] {
			return contracts.NewValidationError("portable export contains an unexpected file %s", relative)
		}
		return nil
	})
}
