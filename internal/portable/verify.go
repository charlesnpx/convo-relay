package portable

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

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
	payloads := make([]verifiedPortablePayload, 0, len(manifest["payload_inventory"].([]any)))
	for _, raw := range manifest["payload_inventory"].([]any) {
		entry := raw.(map[string]any)
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
	if object, ok := value.(map[string]any); ok && object["kind"] == "portable_payload_ref" {
		_, err := validatePortablePayloadRefObject(object, inventoryByID)
		return err
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

func validatePortablePayloadRefObject(object map[string]any, inventoryByID map[string]map[string]any) (map[string]any, error) {
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
	refSourceID, _ := object["source_artifact_id"].(string)
	refSourceDigest, _ := object["source_artifact_digest"].(string)
	entrySourceID, _ := entry["source_artifact_id"].(string)
	entrySourceDigest, _ := entry["source_artifact_digest"].(string)
	if refSourceID != entrySourceID || refSourceDigest != entrySourceDigest {
		return nil, contracts.NewValidationError("portable payload ref source identity mismatch for %s", portableID)
	}
	return entry, nil
}

func validatePortableProviderLineage(payloads []verifiedPortablePayload, inventoryByID map[string]map[string]any) error {
	payloadByID := map[string]any{}
	for _, payload := range payloads {
		payloadByID[payload.entry["portable_id"].(string)] = payload.value
	}
	for _, payload := range payloads {
		object, _ := payload.value.(map[string]any)
		if object == nil || object["kind"] != contracts.RootArtifactKindProviderInvocation {
			continue
		}
		invocation, _ := object["invocation"].(map[string]any)
		if invocation == nil {
			return contracts.NewValidationError("portable provider invocation payload is missing invocation")
		}
		if invocation["schema_version"] != contracts.ProviderInvocationV2 {
			return contracts.NewValidationError("portable provider invocation requires %s", contracts.ProviderInvocationV2)
		}
		if invocation["provider_launch_attempted"] == false {
			if invocation["provider_result_ref"] != nil {
				return contracts.NewValidationError("portable unlaunched provider invocation has provider_result_ref")
			}
			continue
		}
		resultRef, ok := invocation["provider_result_ref"].(map[string]any)
		if !ok || resultRef == nil {
			return contracts.NewValidationError("portable launched provider invocation requires provider_result_ref")
		}
		entry, err := validatePortablePayloadRefObject(resultRef, inventoryByID)
		if err != nil {
			return err
		}
		resultPayload, _ := payloadByID[entry["portable_id"].(string)].(map[string]any)
		if resultPayload == nil || resultPayload["kind"] != contracts.RootArtifactKindProviderResult {
			return contracts.NewValidationError("portable provider invocation result ref does not target provider_result")
		}
		if err := validatePortableProviderResultBinding(invocation, resultPayload); err != nil {
			return err
		}
	}
	return nil
}

func validatePortableProviderResultBinding(invocation map[string]any, resultPayload map[string]any) error {
	resultDraft, _ := resultPayload["invocation"].(map[string]any)
	if resultDraft == nil {
		return contracts.NewValidationError("portable provider result is missing invocation draft")
	}
	if resultPayload["provider_result"] == nil {
		return contracts.NewValidationError("portable provider result is missing provider_result")
	}
	for _, key := range []string{
		"invocation_id", "phase", "actor", "runner_attempt", "provider_retry", "backend",
		"started_at", "completed_at", "outcome", "failure_stage", "classification",
	} {
		if !portableSemanticEqual(resultPayload[key], resultDraft[key]) {
			return contracts.NewValidationError("portable provider result %s does not match invocation draft", key)
		}
	}
	boundInvocation := contracts.Materialize(invocation).(map[string]any)
	boundInvocation["provider_result_ref"] = nil
	if !portableSemanticEqual(boundInvocation, resultDraft) {
		return contracts.NewValidationError("portable provider invocation/result identity mismatch")
	}
	return nil
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
