package namedinputs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

type Persisted struct {
	Manifest    map[string]any
	ManifestRef map[string]any
}

type persistedEntry struct {
	ordinal      int
	name         string
	nameOrdinal  int
	sourcePath   string
	displayName  string
	sizeBytes    int64
	rawDigest    string
	mediaType    string
	schemaStatus string
	contentRef   map[string]any
	data         []byte
}

// Persist stores independently verified base64 content envelopes followed by
// one ordered internal manifest. Prepared bytes, rather than live source files,
// are authoritative at this boundary.
func Persist(st *store.Store, prepared *Prepared) (*Persisted, error) {
	if st == nil || strings.TrimSpace(st.Root) == "" {
		return nil, diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, "", "Named input persistence requires a session store.", nil)
	}
	if prepared == nil || strings.TrimSpace(prepared.contractID) == "" {
		return nil, diagnosticError(nil, DiagnosticCodeContractRequired, contracts.DiagnosticPhasePreflight, "", "Named input persistence requires a selected integration contract.", nil)
	}
	manifestEntries := make([]any, 0, len(prepared.items))
	for index, item := range prepared.items {
		ordinal := index + 1
		if item.Ordinal != ordinal || int64(len(item.data)) != item.SizeBytes || rawDigest(item.data) != item.RawDigest {
			return nil, diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, inputValuePointer(item.Name, item.NameOrdinal), "Prepared named input failed its integrity check.", map[string]any{"ordinal": ordinal})
		}
		payload, err := contentPayload(item)
		if err != nil {
			return nil, err
		}
		identity, err := contracts.RootArtifactIdentityFor(contracts.RootArtifactKindNamedInputContent, ordinal)
		if err != nil {
			return nil, err
		}
		ref, err := st.SaveContractArtifact(contracts.RootArtifactKindNamedInputContent, identity.ArtifactID, payload, identity.RefID)
		if err != nil {
			return nil, err
		}
		persistedPayload, err := st.LoadArtifactPayloadRaw(ref)
		if err != nil {
			return nil, err
		}
		decoded, err := decodeContentPayload(persistedPayload, ref, ordinal)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(decoded, item.data) {
			return nil, diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, inputValuePointer(item.Name, item.NameOrdinal), "Persisted named input bytes do not match preflight bytes.", map[string]any{"ordinal": ordinal})
		}
		manifestEntries = append(manifestEntries, map[string]any{
			"ordinal":       ordinal,
			"name":          item.Name,
			"name_ordinal":  item.NameOrdinal,
			"source_path":   item.SourcePath,
			"display_name":  item.DisplayName,
			"size_bytes":    item.SizeBytes,
			"raw_digest":    item.RawDigest,
			"media_type":    item.MediaType,
			"schema_status": item.SchemaStatus,
			"content_ref":   ref,
		})
	}
	manifest, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindNamedInputManifest, map[string]any{
		"contract_id": prepared.contractID,
		"input_count": len(manifestEntries),
		"inputs":      manifestEntries,
	})
	if err != nil {
		return nil, err
	}
	identity, err := contracts.RootArtifactIdentityFor(contracts.RootArtifactKindNamedInputManifest, 0)
	if err != nil {
		return nil, err
	}
	manifestRef, err := st.SaveContractArtifact(contracts.RootArtifactKindNamedInputManifest, identity.ArtifactID, manifest, identity.RefID)
	if err != nil {
		return nil, err
	}
	persistedManifest, entries, err := loadManifest(st, manifestRef)
	if err != nil {
		return nil, err
	}
	if len(entries) != len(prepared.items) {
		return nil, diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, "", "Persisted named input manifest changed its input count.", nil)
	}
	return &Persisted{Manifest: cloneMap(persistedManifest), ManifestRef: cloneMap(manifestRef)}, nil
}

func PersistAndMaterialize(st *store.Store, prepared *Prepared, executionInputDir string) (*Persisted, map[string]any, error) {
	persisted, err := Persist(st, prepared)
	if err != nil {
		return nil, nil, err
	}
	projection, err := Materialize(st, persisted.ManifestRef, executionInputDir)
	if err != nil {
		return nil, nil, err
	}
	return persisted, projection, nil
}

// Materialize verifies the manifest, every artifact ref, base64 envelope, raw
// digest, and cross-record metadata before writing read-only ordinal files.
func Materialize(st *store.Store, manifestRef map[string]any, executionInputDir string) (map[string]any, error) {
	if st == nil || strings.TrimSpace(st.Root) == "" {
		return nil, diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, "", "Named input materialization requires a session store.", nil)
	}
	if strings.TrimSpace(executionInputDir) == "" {
		return nil, diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, "", "Named input materialization directory is required.", nil)
	}
	manifest, entries, err := loadManifest(st, manifestRef)
	if err != nil {
		return nil, err
	}
	absoluteDir, err := safeMaterializationDirectory(st.Root, executionInputDir)
	if err != nil {
		return nil, err
	}
	if err := validateMaterializationDirectory(absoluteDir, len(entries)); err != nil {
		return nil, err
	}
	providerItems := make([]any, 0, len(entries))
	for _, entry := range entries {
		filename := fmt.Sprintf("%06d", entry.ordinal)
		target := filepath.Join(absoluteDir, filename)
		if info, err := os.Lstat(target); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
			return nil, diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePolicy, "", "Named input materialization target is not a regular file.", map[string]any{"ordinal": entry.ordinal})
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := store.AtomicWriteFile(target, entry.data); err != nil {
			return nil, err
		}
		if err := os.Chmod(target, 0o444); err != nil {
			return nil, err
		}
		providerItems = append(providerItems, map[string]any{
			"name":              entry.name,
			"ordinal":           entry.ordinal,
			"name_ordinal":      entry.nameOrdinal,
			"materialized_path": target,
			"size_bytes":        entry.sizeBytes,
			"raw_digest":        entry.rawDigest,
			"media_type":        entry.mediaType,
			"schema_status":     entry.schemaStatus,
			"content_ref":       cloneMap(entry.contentRef),
		})
	}
	if err := validateMaterializationDirectory(absoluteDir, len(entries)); err != nil {
		return nil, err
	}
	return map[string]any{
		"contract_id":  manifest["contract_id"],
		"manifest_ref": cloneMap(manifestRef),
		"inputs":       providerItems,
	}, nil
}

func contentPayload(item preparedItem) (map[string]any, error) {
	payload, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindNamedInputContent, map[string]any{
		"ordinal":       item.Ordinal,
		"name":          item.Name,
		"name_ordinal":  item.NameOrdinal,
		"encoding":      "base64",
		"bytes_base64":  base64.StdEncoding.EncodeToString(item.data),
		"size_bytes":    item.SizeBytes,
		"raw_digest":    item.RawDigest,
		"media_type":    item.MediaType,
		"schema_status": item.SchemaStatus,
	})
	if err != nil {
		return nil, err
	}
	ref, err := contracts.RootArtifactRefForPayload(contracts.RootArtifactKindNamedInputContent, item.Ordinal, payload)
	if err != nil {
		return nil, err
	}
	if _, err := decodeContentPayload(payload, ref, item.Ordinal); err != nil {
		return nil, err
	}
	return payload, nil
}

func decodeContentPayload(payload map[string]any, ref map[string]any, expectedOrdinal int) ([]byte, error) {
	normalized, err := contracts.ValidateRootArtifact(payload, contracts.RootArtifactKindNamedInputContent)
	if err != nil {
		return nil, integrityError(err, "Named input content envelope is invalid.", map[string]any{"ordinal": expectedOrdinal})
	}
	if err := requireOnlyFields(normalized, "content envelope", []string{
		"kind", "schema_version", "ordinal", "name", "name_ordinal", "encoding", "bytes_base64", "size_bytes", "raw_digest", "media_type", "schema_status",
	}); err != nil {
		return nil, err
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, contracts.RootArtifactKindNamedInputContent, expectedOrdinal, normalized); err != nil {
		return nil, integrityError(err, "Named input content artifact ref is invalid.", map[string]any{"ordinal": expectedOrdinal})
	}
	ordinal, ok := integerField(normalized["ordinal"])
	if !ok || ordinal != int64(expectedOrdinal) {
		return nil, integrityError(nil, "Named input content ordinal is invalid.", map[string]any{"ordinal": expectedOrdinal})
	}
	name, nameOK := normalized["name"].(string)
	nameOrdinal, ordinalOK := integerField(normalized["name_ordinal"])
	if !nameOK || strings.TrimSpace(name) == "" || !ordinalOK || nameOrdinal < 1 {
		return nil, integrityError(nil, "Named input content identity metadata is invalid.", map[string]any{"ordinal": expectedOrdinal})
	}
	if normalized["encoding"] != "base64" {
		return nil, integrityError(nil, "Named input content encoding must be base64.", map[string]any{"ordinal": expectedOrdinal})
	}
	encoded, ok := normalized["bytes_base64"].(string)
	if !ok {
		return nil, integrityError(nil, "Named input content base64 payload is missing.", map[string]any{"ordinal": expectedOrdinal})
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, integrityError(err, "Named input content base64 payload is invalid.", map[string]any{"ordinal": expectedOrdinal})
	}
	sizeBytes, ok := integerField(normalized["size_bytes"])
	if !ok || sizeBytes < 0 || sizeBytes != int64(len(data)) {
		return nil, integrityError(nil, "Named input content size does not match decoded bytes.", map[string]any{"ordinal": expectedOrdinal})
	}
	digest, ok := normalized["raw_digest"].(string)
	if !ok || digest != rawDigest(data) {
		return nil, integrityError(nil, "Named input content raw digest does not match decoded bytes.", map[string]any{"ordinal": expectedOrdinal})
	}
	mediaType, ok := normalized["media_type"].(string)
	if !ok {
		return nil, integrityError(nil, "Named input content media type is missing.", map[string]any{"ordinal": expectedOrdinal})
	}
	if _, err := parseNamedInputMediaType(mediaType); err != nil {
		return nil, integrityError(err, "Named input content media type is invalid.", map[string]any{"ordinal": expectedOrdinal})
	}
	if !validSchemaStatus(normalized["schema_status"]) {
		return nil, integrityError(nil, "Named input content schema status is invalid.", map[string]any{"ordinal": expectedOrdinal})
	}
	return data, nil
}

func loadManifest(st *store.Store, ref map[string]any) (map[string]any, []persistedEntry, error) {
	payload, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, nil, integrityError(err, "Named input manifest artifact could not be loaded.", nil)
	}
	manifest, err := contracts.ValidateRootArtifact(payload, contracts.RootArtifactKindNamedInputManifest)
	if err != nil {
		return nil, nil, integrityError(err, "Named input manifest envelope is invalid.", nil)
	}
	if err := requireOnlyFields(manifest, "manifest envelope", []string{"kind", "schema_version", "contract_id", "input_count", "inputs"}); err != nil {
		return nil, nil, err
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, contracts.RootArtifactKindNamedInputManifest, 0, manifest); err != nil {
		return nil, nil, integrityError(err, "Named input manifest ref is invalid.", nil)
	}
	contractID, ok := manifest["contract_id"].(string)
	if !ok || strings.TrimSpace(contractID) == "" {
		return nil, nil, integrityError(nil, "Named input manifest contract id is invalid.", nil)
	}
	rawInputs, ok := manifest["inputs"].([]any)
	if !ok {
		return nil, nil, integrityError(nil, "Named input manifest inputs must be an array.", nil)
	}
	inputCount, ok := integerField(manifest["input_count"])
	if !ok || inputCount != int64(len(rawInputs)) {
		return nil, nil, integrityError(nil, "Named input manifest count does not match its entries.", nil)
	}
	entries := make([]persistedEntry, 0, len(rawInputs))
	nameCounts := map[string]int{}
	for index, raw := range rawInputs {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, integrityError(nil, "Named input manifest entry must be an object.", map[string]any{"ordinal": index + 1})
		}
		decoded, err := loadManifestEntry(st, entry, index+1, nameCounts)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, decoded)
	}
	return manifest, entries, nil
}

func loadManifestEntry(st *store.Store, entry map[string]any, expectedOrdinal int, nameCounts map[string]int) (persistedEntry, error) {
	if err := requireOnlyFields(entry, "manifest entry", []string{
		"ordinal", "name", "name_ordinal", "source_path", "display_name", "size_bytes", "raw_digest", "media_type", "schema_status", "content_ref",
	}); err != nil {
		return persistedEntry{}, err
	}
	ordinal, ok := integerField(entry["ordinal"])
	if !ok || ordinal != int64(expectedOrdinal) {
		return persistedEntry{}, integrityError(nil, "Named input manifest ordinals must be contiguous.", map[string]any{"ordinal": expectedOrdinal})
	}
	name, ok := entry["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return persistedEntry{}, integrityError(nil, "Named input manifest name is invalid.", map[string]any{"ordinal": expectedOrdinal})
	}
	nameCounts[name]++
	nameOrdinal, ok := integerField(entry["name_ordinal"])
	if !ok || nameOrdinal != int64(nameCounts[name]) {
		return persistedEntry{}, integrityError(nil, "Named input per-name ordinals must be contiguous.", map[string]any{"ordinal": expectedOrdinal, "name": name})
	}
	sourcePath, sourceOK := entry["source_path"].(string)
	displayName, displayOK := entry["display_name"].(string)
	if !sourceOK || !filepath.IsAbs(sourcePath) || !displayOK || displayName == "" {
		return persistedEntry{}, integrityError(nil, "Named input manifest source metadata is invalid.", map[string]any{"ordinal": expectedOrdinal})
	}
	contentRef, ok := entry["content_ref"].(map[string]any)
	if !ok {
		return persistedEntry{}, integrityError(nil, "Named input manifest content ref is missing.", map[string]any{"ordinal": expectedOrdinal})
	}
	payload, err := st.LoadArtifactPayloadRaw(contentRef)
	if err != nil {
		return persistedEntry{}, integrityError(err, "Named input content artifact could not be loaded.", map[string]any{"ordinal": expectedOrdinal})
	}
	data, err := decodeContentPayload(payload, contentRef, expectedOrdinal)
	if err != nil {
		return persistedEntry{}, err
	}
	sizeBytes, sizeOK := integerField(entry["size_bytes"])
	rawDigestValue, digestOK := entry["raw_digest"].(string)
	mediaType, mediaOK := entry["media_type"].(string)
	schemaStatus, schemaOK := entry["schema_status"].(string)
	if !sizeOK || sizeBytes != int64(len(data)) || !digestOK || rawDigestValue != rawDigest(data) || !mediaOK || !schemaOK || !validSchemaStatus(schemaStatus) {
		return persistedEntry{}, integrityError(nil, "Named input manifest metadata does not match content bytes.", map[string]any{"ordinal": expectedOrdinal})
	}
	if payload["name"] != name || !integerEquals(payload["name_ordinal"], int64(nameOrdinal)) || payload["media_type"] != mediaType || payload["schema_status"] != schemaStatus {
		return persistedEntry{}, integrityError(nil, "Named input manifest metadata does not match its content envelope.", map[string]any{"ordinal": expectedOrdinal})
	}
	return persistedEntry{
		ordinal:      expectedOrdinal,
		name:         name,
		nameOrdinal:  int(nameOrdinal),
		sourcePath:   sourcePath,
		displayName:  displayName,
		sizeBytes:    sizeBytes,
		rawDigest:    rawDigestValue,
		mediaType:    mediaType,
		schemaStatus: schemaStatus,
		contentRef:   cloneMap(contentRef),
		data:         data,
	}, nil
}

func safeMaterializationDirectory(sessionRoot string, requestedPath string) (string, error) {
	absoluteRoot, err := filepath.Abs(sessionRoot)
	if err != nil {
		return "", err
	}
	absolutePath, err := filepath.Abs(requestedPath)
	if err != nil {
		return "", err
	}
	relativePath, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil || relativePath == "." || filepath.IsAbs(relativePath) || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return "", diagnosticError(
			err,
			DiagnosticCodeIntegrity,
			contracts.DiagnosticPhasePolicy,
			"",
			"Named inputs must be materialized in a dedicated directory inside the session.",
			map[string]any{"execution_input_dir": absolutePath},
		)
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return "", err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePolicy, "", "Named input session root must be a real directory.", nil)
	}
	currentPath := absoluteRoot
	for _, component := range strings.Split(relativePath, string(filepath.Separator)) {
		currentPath = filepath.Join(currentPath, component)
		info, statErr := os.Lstat(currentPath)
		switch {
		case statErr == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePolicy, "", "Named input materialization path must not contain symlinks or non-directory components.", map[string]any{"path": currentPath})
			}
		case os.IsNotExist(statErr):
			if err := os.Mkdir(currentPath, 0o700); err != nil {
				return "", err
			}
		default:
			return "", statErr
		}
	}
	if err := os.Chmod(absolutePath, 0o700); err != nil {
		return "", err
	}
	return absolutePath, nil
}

func validateMaterializationDirectory(path string, inputCount int) error {
	expected := make(map[string]bool, inputCount)
	for ordinal := 1; ordinal <= inputCount; ordinal++ {
		expected[fmt.Sprintf("%06d", ordinal)] = true
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !expected[entry.Name()] {
			return diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePolicy, "", "Named input materialization directory contains an unexpected entry.", map[string]any{"entry": entry.Name()})
		}
		info, err := os.Lstat(filepath.Join(path, entry.Name()))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePolicy, "", "Named input materialization target is not a regular file.", map[string]any{"entry": entry.Name()})
		}
	}
	return nil
}

func validSchemaStatus(value any) bool {
	status, ok := value.(string)
	if !ok {
		return false
	}
	return status == SchemaStatusValidated || status == SchemaStatusNotDeclared || status == SchemaStatusNotApplicable
}

func requireOnlyFields(object map[string]any, label string, allowed []string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = true
	}
	unknown := make([]string, 0)
	for field := range object {
		if !allowedSet[field] {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return integrityError(nil, "Named input "+label+" contains unknown fields.", map[string]any{"fields": unknown})
}

func integerEquals(value any, expected int64) bool {
	integer, ok := integerField(value)
	return ok && integer == expected
}

func integerField(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		return parsed, err == nil
	case float64:
		integer := int64(typed)
		return integer, float64(integer) == typed
	default:
		return 0, false
	}
}

func integrityError(cause error, message string, details map[string]any) error {
	return diagnosticError(cause, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, "", message, details)
}

func cloneMap(value map[string]any) map[string]any {
	cloned, _ := contracts.Materialize(value).(map[string]any)
	return cloned
}
