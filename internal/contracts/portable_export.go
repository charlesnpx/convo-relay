package contracts

import (
	"path"
	"strings"
)

func PortableExportManifest(fields map[string]any) (map[string]any, error) {
	manifest, _ := Materialize(fields).(map[string]any)
	if manifest == nil {
		manifest = map[string]any{}
	}
	manifest["schema_version"] = PortableExportV2
	manifest["digest_profile"] = DigestProfileV1
	inventoryDigest, err := SemanticJSONDigest(manifest["payload_inventory"])
	if err != nil {
		return nil, err
	}
	manifest["inventory_digest"] = inventoryDigest
	delete(manifest, "manifest_digest")
	manifestDigest, err := SemanticJSONDigest(manifest)
	if err != nil {
		return nil, err
	}
	manifest["manifest_digest"] = manifestDigest
	return ValidatePortableExportManifest(manifest)
}

func ValidatePortableExportManifest(value any) (map[string]any, error) {
	manifest, err := requireObjectValue(value, "portable export manifest")
	if err != nil {
		return nil, err
	}
	if err := validateAllowedKeys("portable export manifest", manifest, []string{
		"schema_version", "convo_relay_version", "digest_profile", "terminal_status", "stop_reason",
		"session_payload", "transcript_payload", "diagnostics_payload", "payload_inventory",
		"inventory_digest", "manifest_digest",
	}); err != nil {
		return nil, err
	}
	if _, err := RequireStringVersion(manifest, "portable_export"); err != nil {
		return nil, err
	}
	if _, err := requireString(manifest, "convo_relay_version", false); err != nil {
		return nil, err
	}
	if manifest["digest_profile"] != DigestProfileV1 {
		return nil, NewValidationError("portable export requires digest_profile %s", DigestProfileV1)
	}
	if _, err := requireString(manifest, "terminal_status", false); err != nil {
		return nil, err
	}
	if manifest["stop_reason"] != nil {
		if _, ok := manifest["stop_reason"].(string); !ok {
			return nil, NewValidationError("portable export stop_reason must be null or a string")
		}
	}

	rawInventory, ok := manifest["payload_inventory"].([]any)
	if !ok || len(rawInventory) == 0 {
		return nil, NewValidationError("portable export payload_inventory must be a non-empty array")
	}
	inventory := make([]any, 0, len(rawInventory))
	paths := map[string]bool{}
	portableIDs := map[string]bool{}
	previousPath := ""
	for index, raw := range rawInventory {
		entry, err := validatePortableInventoryEntry(raw, index)
		if err != nil {
			return nil, err
		}
		entryPath := entry["path"].(string)
		portableID := entry["portable_id"].(string)
		if paths[entryPath] || portableIDs[portableID] || previousPath >= entryPath && previousPath != "" {
			return nil, NewValidationError("portable export payload_inventory must use unique ids and ascending paths")
		}
		paths[entryPath] = true
		portableIDs[portableID] = true
		previousPath = entryPath
		inventory = append(inventory, entry)
	}
	for _, field := range []string{"session_payload", "transcript_payload", "diagnostics_payload"} {
		payloadPath, err := requireString(manifest, field, false)
		if err != nil || !paths[payloadPath] {
			return nil, NewValidationError("portable export %s must name an inventoried payload", field)
		}
	}
	manifest["payload_inventory"] = inventory
	if err := requireMatchingSemanticDigest(manifest["inventory_digest"], inventory, "portable export inventory"); err != nil {
		return nil, err
	}
	material := Materialize(manifest).(map[string]any)
	delete(material, "manifest_digest")
	if err := requireMatchingSemanticDigest(manifest["manifest_digest"], material, "portable export manifest"); err != nil {
		return nil, err
	}
	return manifest, nil
}

func validatePortableInventoryEntry(value any, index int) (map[string]any, error) {
	entry, err := requireObjectValue(value, "portable export inventory entry")
	if err != nil {
		return nil, err
	}
	if err := validateAllowedKeys("portable export inventory entry", entry, []string{
		"kind", "portable_id", "path", "media_type", "size_bytes", "digest_class", "digest", "source_artifact_id", "source_artifact_digest",
	}); err != nil {
		return nil, err
	}
	kind, err := requireString(entry, "kind", false)
	if err != nil {
		return nil, err
	}
	portableID, err := requireString(entry, "portable_id", false)
	if err != nil {
		return nil, err
	}
	payloadPath, err := requireString(entry, "path", false)
	if err != nil {
		return nil, err
	}
	if !portablePathComponent(kind) || !portablePathComponent(portableID) ||
		payloadPath != path.Join("payloads", kind, portableID+".json") {
		return nil, NewValidationError("portable export payload_inventory[%d] has an invalid path identity", index)
	}
	if entry["media_type"] != "application/json" || entry["digest_class"] != string(DigestClassRawBytes) {
		return nil, NewValidationError("portable export payload_inventory[%d] must describe raw JSON bytes", index)
	}
	size, ok := exactJSONInteger(entry["size_bytes"])
	if !ok || size < 0 {
		return nil, NewValidationError("portable export payload_inventory[%d] size_bytes must be nonnegative", index)
	}
	digest, err := requireString(entry, "digest", false)
	if err != nil {
		return nil, err
	}
	if _, err := ValidateArtifactRef(map[string]any{"kind": "artifact_ref", "schema_version": 1, "id": "portable", "digest": digest}); err != nil {
		return nil, err
	}
	entry["size_bytes"] = size
	if entry["source_artifact_id"] != nil {
		value, err := requireString(entry, "source_artifact_id", false)
		if err != nil {
			return nil, err
		}
		if !refIDRE.MatchString(value) {
			return nil, NewValidationError("portable export source_artifact_id contains unsupported characters")
		}
		digest, err := requireString(entry, "source_artifact_digest", false)
		if err != nil {
			return nil, err
		}
		if _, err := ValidateArtifactRef(map[string]any{"kind": "artifact_ref", "schema_version": 1, "id": value, "digest": digest}); err != nil {
			return nil, NewValidationError("portable export source_artifact_digest is invalid")
		}
	} else if entry["source_artifact_digest"] != nil {
		return nil, NewValidationError("portable export source_artifact_digest requires source_artifact_id")
	}
	return entry, nil
}

func portablePathComponent(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return false
	}
	first := value[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func requireMatchingSemanticDigest(raw any, value any, label string) error {
	digest, ok := raw.(string)
	if !ok {
		return NewValidationError("%s digest must be a string", label)
	}
	want, err := SemanticJSONDigest(value)
	if err != nil {
		return err
	}
	if digest != want {
		return NewValidationError("%s digest mismatch", label)
	}
	return nil
}
