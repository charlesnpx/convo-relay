package format

import (
	"path"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
)

// BundleManifest creates the portable closure manifest. Manifest and inventory
// digests use semantic-json; each referenced payload has one shared BlobRef.
func BundleManifest(fields map[string]any) (map[string]any, error) {
	manifest := cloneObject(fields)
	if manifest == nil {
		manifest = map[string]any{}
	}
	manifest["kind"] = BundleV1
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
	if manifest["kind"] != BundleV1 {
		return nil, NewValidationError("portable bundle manifest kind must be %s", BundleV1)
	}
	if _, ok := nonEmptyString(manifest["convo_relay_version"]); !ok {
		return nil, NewValidationError("portable bundle manifest convo_relay_version must be a non-empty string")
	}
	if _, ok := nonEmptyString(manifest["terminal_status"]); !ok {
		return nil, NewValidationError("portable bundle manifest terminal_status must be a non-empty string")
	}
	if manifest["stop_reason"] != nil {
		if _, ok := manifest["stop_reason"].(string); !ok {
			return nil, NewValidationError("portable bundle manifest stop_reason must be null or a string")
		}
	}
	rawInventory, ok := manifest["payload_inventory"].([]any)
	if !ok || len(rawInventory) == 0 {
		return nil, NewValidationError("portable bundle payload_inventory must be a non-empty array")
	}
	inventory := make([]any, 0, len(rawInventory))
	paths := map[string]bool{}
	identities := map[string]bool{}
	previousPath := ""
	for index, raw := range rawInventory {
		entry, err := validateBundleInventoryEntry(raw, index)
		if err != nil {
			return nil, err
		}
		entryPath := entry["path"].(string)
		identity := entry["kind"].(string) + ":" + entry["portable_id"].(string)
		if paths[entryPath] || identities[identity] || previousPath != "" && previousPath >= entryPath {
			return nil, NewValidationError("portable bundle payload_inventory must use unique identities and ascending paths")
		}
		paths[entryPath] = true
		identities[identity] = true
		previousPath = entryPath
		inventory = append(inventory, entry)
	}
	for _, field := range []string{"session_payload", "transcript_payload", "diagnostics_payload"} {
		payloadPath, ok := nonEmptyString(manifest[field])
		if !ok || !paths[payloadPath] {
			return nil, NewValidationError("portable bundle %s must name an inventoried payload", field)
		}
	}
	manifest["payload_inventory"] = inventory
	if err := matchingSemanticDigest(manifest["inventory_digest"], inventory, "portable bundle inventory"); err != nil {
		return nil, err
	}
	material := cloneObject(manifest)
	delete(material, "manifest_digest")
	if err := matchingSemanticDigest(manifest["manifest_digest"], material, "portable bundle manifest"); err != nil {
		return nil, err
	}
	return manifest, nil
}

func validateBundleInventoryEntry(value any, index int) (map[string]any, error) {
	entry, ok := Materialize(value).(map[string]any)
	if !ok {
		return nil, NewValidationError("portable bundle inventory entry %d must be an object", index)
	}
	if err := rejectUnknown(entry, []string{"kind", "portable_id", "path", "blob"}, "portable bundle inventory entry"); err != nil {
		return nil, err
	}
	kind, kindOK := nonEmptyString(entry["kind"])
	portableID, idOK := nonEmptyString(entry["portable_id"])
	payloadPath, pathOK := nonEmptyString(entry["path"])
	if !kindOK || !idOK || !pathOK {
		return nil, NewValidationError("portable bundle inventory entry %d requires kind, portable_id, and path", index)
	}
	if !portablePathComponent(kind) || !portablePathComponent(portableID) || payloadPath != path.Join("payloads", kind, portableID+".json") {
		return nil, NewValidationError("portable bundle payload_inventory[%d] has an invalid path identity", index)
	}
	ref, err := blobRef(entry["blob"])
	if err != nil {
		return nil, NewValidationError("portable bundle payload_inventory[%d] has an invalid blob: %v", index, err)
	}
	return map[string]any{
		"kind":        kind,
		"portable_id": portableID,
		"path":        payloadPath,
		"blob":        normalizedBlobRef(ref),
	}, nil
}

// normalizedBlobRef is the JSON-shaped form retained in a manifest. Keeping
// this conversion at the boundary makes programmatic and decoded input follow
// the same validation and digest path.
func normalizedBlobRef(ref blobstore.BlobRef) map[string]any {
	return map[string]any{
		"sha256":     ref.SHA256,
		"size":       ref.Size,
		"media_type": ref.MediaType,
	}
}

func blobRef(value any) (blobstore.BlobRef, error) {
	if ref, ok := value.(blobstore.BlobRef); ok {
		if err := blobstore.ValidateRef(ref); err != nil {
			return blobstore.BlobRef{}, err
		}
		return ref, nil
	}
	if ref, ok := value.(*blobstore.BlobRef); ok && ref != nil {
		if err := blobstore.ValidateRef(*ref); err != nil {
			return blobstore.BlobRef{}, err
		}
		return *ref, nil
	}
	object, ok := Materialize(value).(map[string]any)
	if !ok {
		return blobstore.BlobRef{}, NewValidationError("blob must be an object")
	}
	if err := rejectUnknown(object, []string{"sha256", "size", "media_type"}, "blob"); err != nil {
		return blobstore.BlobRef{}, err
	}
	sha256, shaOK := nonEmptyString(object["sha256"])
	mediaType, mediaOK := nonEmptyString(object["media_type"])
	size, sizeOK := exactInt(object["size"])
	if !shaOK || !mediaOK || !sizeOK {
		return blobstore.BlobRef{}, NewValidationError("blob requires sha256, size, and media_type")
	}
	ref := blobstore.BlobRef{SHA256: sha256, Size: size, MediaType: mediaType}
	if err := blobstore.ValidateRef(ref); err != nil {
		return blobstore.BlobRef{}, err
	}
	return ref, nil
}

func matchingSemanticDigest(raw any, value any, label string) error {
	digest, ok := nonEmptyString(raw)
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

func portablePathComponent(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return false
	}
	first := value[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') {
		return false
	}
	for _, runeValue := range value {
		if runeValue >= 'a' && runeValue <= 'z' || runeValue >= 'A' && runeValue <= 'Z' || runeValue >= '0' && runeValue <= '9' || runeValue == '_' || runeValue == '-' || runeValue == '.' {
			continue
		}
		return false
	}
	return true
}
