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
