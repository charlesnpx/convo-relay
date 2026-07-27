package portable

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

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
	portableIDs := map[string]bool{}
	values := make([]any, 0, len(manifest["payload_inventory"].([]any)))
	for _, raw := range manifest["payload_inventory"].([]any) {
		entry := raw.(map[string]any)
		portableIDs[entry["portable_id"].(string)] = true
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
		values = append(values, value)
	}
	for _, value := range values {
		if refs := contracts.FindArtifactRefs(value); len(refs) > 0 {
			return nil, contracts.NewValidationError("portable export retains a source-session artifact ref")
		}
		for _, portableID := range findPortablePayloadRefs(value) {
			if !portableIDs[portableID] {
				return nil, contracts.NewValidationError("portable export payload closure is missing %s", portableID)
			}
		}
	}
	if err := verifyClosedFileSet(root, expectedFiles); err != nil {
		return nil, err
	}
	return map[string]any{
		"schema_version":  manifest["schema_version"],
		"status":          "valid",
		"terminal_status": manifest["terminal_status"],
		"payload_count":   len(values),
		"manifest_digest": manifest["manifest_digest"],
	}, nil
}

func findPortablePayloadRefs(value any) []string {
	if object, ok := value.(map[string]any); ok && object["kind"] == "portable_payload_ref" {
		portableID, _ := object["portable_id"].(string)
		return []string{portableID}
	}
	result := []string{}
	switch typed := value.(type) {
	case map[string]any:
		for _, item := range typed {
			result = append(result, findPortablePayloadRefs(item)...)
		}
	case []any:
		for _, item := range typed {
			result = append(result, findPortablePayloadRefs(item)...)
		}
	}
	return result
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
