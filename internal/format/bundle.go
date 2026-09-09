package format

import (
	"encoding/json"

	"github.com/charlesnpx/convo-relay/v2/bundle"
)

// BundleManifest creates the portable closure manifest through the public
// bundle document and returns its legacy map-shaped internal projection.
func BundleManifest(fields map[string]any) (map[string]any, error) {
	manifest := cloneObject(fields)
	if manifest == nil {
		manifest = map[string]any{}
	}
	manifest["kind"] = bundle.Kind
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
	return ValidateBundleManifest(manifest)
}

// ValidateBundleManifest decodes the public bundle manifest and returns its
// normalized internal map projection for existing CLI code.
func ValidateBundleManifest(value any) (map[string]any, error) {
	manifest, ok := Materialize(value).(map[string]any)
	if !ok {
		return nil, NewValidationError("portable bundle manifest must be an object")
	}
	if err := rejectUnknown(manifest, []string{
		"kind", "convo_relay_version", "terminal_status", "stop_reason",
		"session_payload", "transcript_payload", "diagnostics_payload", "payload_inventory",
		"inventory_digest", "manifest_digest",
	}, "portable bundle manifest"); err != nil {
		return nil, err
	}
	if inventory, ok := manifest["payload_inventory"].([]any); ok {
		for _, raw := range inventory {
			entry, entryOK := Materialize(raw).(map[string]any)
			if !entryOK {
				continue
			}
			if err := rejectUnknown(entry, []string{"kind", "portable_id", "path", "blob"}, "portable bundle inventory entry"); err != nil {
				return nil, err
			}
		}
	}
	decoded, err := decodeBundleManifest(manifest)
	if err != nil {
		return nil, err
	}
	if err := bundle.Validate(decoded); err != nil {
		return nil, NewValidationError("%s", err)
	}
	return encodeBundleManifest(decoded)
}

func decodeBundleManifest(value map[string]any) (bundle.Manifest, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return bundle.Manifest{}, err
	}
	var manifest bundle.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return bundle.Manifest{}, err
	}
	return manifest, nil
}

func encodeBundleManifest(value bundle.Manifest) (map[string]any, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var manifest map[string]any
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func rejectUnknown(object map[string]any, allowed []string, label string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for _, key := range sortedKeys(object) {
		if !allowedSet[key] {
			return NewValidationError("%s has unsupported field %q", label, key)
		}
	}
	return nil
}
