package namedinputs

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestPreparePreservesGlobalAndPerNameOrderAndEqualsInPaths(t *testing.T) {
	root := t.TempDir()
	firstMany := writeInputFile(t, root, "many=first.txt", []byte("first"))
	one := writeInputFile(t, root, "one.txt", []byte("one"))
	secondMany := writeInputFile(t, root, "many-second.txt", []byte("second"))
	selected := selectedContract(t, map[string]any{
		"optional-one":  inputDeclaration(false, integration.CardinalityOne, "text/plain", 64, nil),
		"required-one":  inputDeclaration(true, integration.CardinalityOne, "text/plain", 64, nil),
		"optional-many": inputDeclaration(false, integration.CardinalityMany, "text/plain", 64, nil),
		"required-many": inputDeclaration(true, integration.CardinalityMany, "text/plain", 64, nil),
	})

	prepared, err := Prepare(Options{
		Contract:     selected,
		SourceAnchor: root,
		Bindings: []string{
			"required-many=" + filepath.Base(firstMany),
			"required-one=" + filepath.Base(one),
			"required-many=" + filepath.Base(secondMany),
		},
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	items := prepared.Items()
	if len(items) != 3 {
		t.Fatalf("item count = %d", len(items))
	}
	wantNames := []string{"required-many", "required-one", "required-many"}
	wantNameOrdinals := []int{1, 1, 2}
	for index, item := range items {
		if item.Ordinal != index+1 || item.Name != wantNames[index] || item.NameOrdinal != wantNameOrdinals[index] {
			t.Fatalf("item %d = %#v", index, item)
		}
		if !filepath.IsAbs(item.SourcePath) || item.SchemaStatus != SchemaStatusNotApplicable {
			t.Fatalf("item metadata = %#v", item)
		}
	}
	if items[0].DisplayName != "many=first.txt" {
		t.Fatalf("first equals-bearing path parsed incorrectly: %#v", items[0])
	}
	if prepared.ContractID() != testContractID {
		t.Fatalf("contract id = %q", prepared.ContractID())
	}
}

func TestPrepareEnforcesEveryCardinality(t *testing.T) {
	root := t.TempDir()
	value := writeInputFile(t, root, "value.txt", []byte("value"))
	tests := []struct {
		name        string
		required    bool
		cardinality string
		count       int
		wantError   bool
	}{
		{name: "optional one zero", cardinality: integration.CardinalityOne, count: 0},
		{name: "optional one one", cardinality: integration.CardinalityOne, count: 1},
		{name: "optional one duplicate", cardinality: integration.CardinalityOne, count: 2, wantError: true},
		{name: "required one missing", required: true, cardinality: integration.CardinalityOne, count: 0, wantError: true},
		{name: "required one exact", required: true, cardinality: integration.CardinalityOne, count: 1},
		{name: "required one duplicate", required: true, cardinality: integration.CardinalityOne, count: 2, wantError: true},
		{name: "optional many zero", cardinality: integration.CardinalityMany, count: 0},
		{name: "optional many several", cardinality: integration.CardinalityMany, count: 3},
		{name: "required many missing", required: true, cardinality: integration.CardinalityMany, count: 0, wantError: true},
		{name: "required many one", required: true, cardinality: integration.CardinalityMany, count: 1},
		{name: "required many several", required: true, cardinality: integration.CardinalityMany, count: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected := selectedContract(t, map[string]any{
				"value": inputDeclaration(test.required, test.cardinality, "application/octet-stream", 64, nil),
			})
			bindings := make([]string, test.count)
			for index := range bindings {
				bindings[index] = "value=" + value
			}
			prepared, err := Prepare(Options{Contract: selected, Bindings: bindings})
			if test.wantError {
				requireDiagnosticCode(t, err, DiagnosticCodeCardinality)
				return
			}
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if len(prepared.Items()) != test.count {
				t.Fatalf("item count = %d, want %d", len(prepared.Items()), test.count)
			}
		})
	}
}

func TestPrepareRejectsMissingContractUndeclaredNamesAndPositionalContext(t *testing.T) {
	root := t.TempDir()
	value := writeInputFile(t, root, "value.txt", []byte("value"))
	if prepared, err := Prepare(Options{}); err != nil || prepared.ContractID() != "" || len(prepared.Items()) != 0 {
		t.Fatalf("contractless empty prepare = %#v, %v", prepared, err)
	}
	_, err := Prepare(Options{Bindings: []string{"value=" + value}})
	requireDiagnosticCode(t, err, DiagnosticCodeContractRequired)

	selected := selectedContract(t, map[string]any{
		"value": inputDeclaration(false, integration.CardinalityMany, "text/plain", 64, nil),
	})
	_, err = Prepare(Options{Contract: selected, Bindings: []string{"other=" + value}})
	requireDiagnosticCode(t, err, DiagnosticCodeUndeclaredInput)
	_, err = Prepare(Options{Contract: selected, PositionalContextCount: 1})
	requireDiagnosticCode(t, err, DiagnosticCodeContextConflict)

	withoutInputs := selectedContract(t, map[string]any{})
	if _, err := Prepare(Options{Contract: withoutInputs, PositionalContextCount: 2}); err != nil {
		t.Fatalf("contract without named inputs rejected positional context: %v", err)
	}
	for _, invalid := range []string{"missing-equals", "=path", "   =path", "name="} {
		_, err := ParseBinding(invalid)
		requireDiagnosticCode(t, err, DiagnosticCodeInvalidBinding)
	}
	_, err = Prepare(Options{Contract: selected, Bindings: []string{"value=" + value, "missing-equals"}})
	diagnostic := requireDiagnosticCode(t, err, DiagnosticCodeInvalidBinding)
	if diagnostic.Path != "/bindings/1" {
		t.Fatalf("malformed binding path = %q", diagnostic.Path)
	}
}

func TestPrepareUsesExactOpaqueNamesAndSafeSourceAnchoring(t *testing.T) {
	root := t.TempDir()
	path := writeInputFile(t, root, "input=with=equals.bin", []byte{0, 1, 2})
	name := " ../opaque/雪~name "
	selected := selectedContract(t, map[string]any{
		name: inputDeclaration(true, integration.CardinalityOne, "application/octet-stream", 3, nil),
	})
	binding, err := ParseBinding(name + "=" + filepath.Base(path))
	if err != nil || binding.Name != name || binding.Path != filepath.Base(path) {
		t.Fatalf("binding = %#v, %v", binding, err)
	}
	prepared, err := Prepare(Options{Contract: selected, SourceAnchor: root, Bindings: []string{name + "=" + filepath.Base(path)}})
	if err != nil {
		t.Fatalf("prepare opaque name: %v", err)
	}
	item := prepared.Items()[0]
	if item.Name != name || item.SourcePath != filepath.Clean(path) {
		t.Fatalf("opaque item = %#v", item)
	}
	_, err = Prepare(Options{Contract: selected, SourceAnchor: root, Bindings: []string{strings.TrimSpace(name) + "=" + filepath.Base(path)}})
	requireDiagnosticCode(t, err, DiagnosticCodeUndeclaredInput)
}

func TestPrepareRejectsNonregularMissingAndOversizedFilesAtBoundaries(t *testing.T) {
	root := t.TempDir()
	exact := writeInputFile(t, root, "exact.bin", []byte("1234"))
	over := writeInputFile(t, root, "over.bin", []byte("12345"))
	selected := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "application/octet-stream", 4, nil),
	})
	if prepared, err := Prepare(Options{Contract: selected, Bindings: []string{"value=" + exact}}); err != nil || prepared.Items()[0].SizeBytes != 4 {
		t.Fatalf("exact boundary = %#v, %v", prepared, err)
	}
	_, err := Prepare(Options{Contract: selected, Bindings: []string{"value=" + over}})
	requireDiagnosticCode(t, err, DiagnosticCodeFileTooLarge)
	_, err = Prepare(Options{Contract: selected, Bindings: []string{"value=" + root}})
	requireDiagnosticCode(t, err, DiagnosticCodeFileNotRegular)
	_, err = Prepare(Options{Contract: selected, Bindings: []string{"value=" + filepath.Join(root, "missing.bin")}})
	requireDiagnosticCode(t, err, DiagnosticCodeFileUnavailable)

	symlink := filepath.Join(root, "link.bin")
	if err := os.Symlink(exact, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err = Prepare(Options{Contract: selected, Bindings: []string{"value=" + symlink}})
	requireDiagnosticCode(t, err, DiagnosticCodeFileNotRegular)
}

func TestPrepareAppliesStrictMediaEncodingAndPerItemJSONSchemas(t *testing.T) {
	root := t.TempDir()
	invalidUTF8 := writeInputFile(t, root, "invalid.bin", []byte{0xff, 0xfe})
	textContract := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "text/plain; charset=utf-8", 16, nil),
	})
	_, err := Prepare(Options{Contract: textContract, Bindings: []string{"value=" + invalidUTF8}})
	requireDiagnosticCode(t, err, contracts.DiagnosticCodeInvalidUTF8)

	binaryContract := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "application/octet-stream", 16, nil),
	})
	if _, err := Prepare(Options{Contract: binaryContract, Bindings: []string{"value=" + invalidUTF8}}); err != nil {
		t.Fatalf("binary rejected arbitrary bytes: %v", err)
	}

	invalidMediaContract := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "not a media type", 16, nil),
	})
	_, err = Prepare(Options{Contract: invalidMediaContract, Bindings: []string{"value=" + invalidUTF8}})
	requireDiagnosticCode(t, err, DiagnosticCodeInvalidMediaType)
	incompatibleCharsetContract := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "text/plain; charset=iso-8859-1", 16, nil),
	})
	_, err = Prepare(Options{Contract: incompatibleCharsetContract, Bindings: []string{"value=" + writeInputFile(t, root, "ascii.txt", []byte("ascii"))}})
	requireDiagnosticCode(t, err, DiagnosticCodeInvalidMediaType)

	strictBad := writeInputFile(t, root, "strict-bad.json", []byte(`{"id":1,"id":2}`))
	jsonContract := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityMany, "application/vnd.example+json; charset=utf-8", 128, map[string]any{
			"type":                 "object",
			"required":             []any{"id"},
			"properties":           map[string]any{"id": map[string]any{"type": "integer"}},
			"additionalProperties": false,
		}),
	})
	_, err = Prepare(Options{Contract: jsonContract, Bindings: []string{"value=" + strictBad}})
	requireDiagnosticCode(t, err, contracts.DiagnosticCodeDuplicateJSONKey)

	good := writeInputFile(t, root, "good.json", []byte(`{"id":1}`))
	badSchema := writeInputFile(t, root, "bad-schema.json", []byte(`{"id":"wrong"}`))
	_, err = Prepare(Options{Contract: jsonContract, Bindings: []string{"value=" + good, "value=" + badSchema}})
	diagnostic := requireDiagnosticCode(t, err, integration.DiagnosticCodeSchemaMismatch)
	if diagnostic.Details["input_index"] != 1 {
		t.Fatalf("schema diagnostic = %#v", diagnostic)
	}
	prepared, err := Prepare(Options{Contract: jsonContract, Bindings: []string{"value=" + good}})
	if err != nil || prepared.Items()[0].SchemaStatus != SchemaStatusValidated {
		t.Fatalf("valid JSON prepare = %#v, %v", prepared, err)
	}
	values := prepared.AssertionInputs()
	values["value"][0].(map[string]any)["id"] = "mutated"
	if reflect.DeepEqual(values, prepared.AssertionInputs()) {
		t.Fatal("assertion input projection was not independent")
	}
}

func TestPersistAndMaterializeRoundTripsBinaryEmptyDuplicateAndHostileNames(t *testing.T) {
	root := t.TempDir()
	binary := []byte{0, 1, 0xff, 0x7f, '\n'}
	first := writeInputFile(t, root, "first.bin", binary)
	empty := writeInputFile(t, root, "empty.bin", []byte{})
	duplicate := writeInputFile(t, root, "duplicate.bin", binary)
	hostileName := "../../输入/~opaque"
	selected := selectedContract(t, map[string]any{
		hostileName: inputDeclaration(true, integration.CardinalityMany, "application/octet-stream", 64, nil),
		"empty":     inputDeclaration(false, integration.CardinalityOne, "application/octet-stream", 64, nil),
	})
	prepared, err := Prepare(Options{Contract: selected, Bindings: []string{
		hostileName + "=" + first,
		"empty=" + empty,
		hostileName + "=" + duplicate,
	}})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(first, []byte("source changed after preflight"), 0o644); err != nil {
		t.Fatalf("mutate source after preflight: %v", err)
	}
	sessionDir := filepath.Join(root, "session")
	executionDir := filepath.Join(sessionDir, "execution", "inputs")
	persisted, projection, err := PersistAndMaterialize(store.New(sessionDir), prepared, executionDir)
	if err != nil {
		t.Fatalf("persist and materialize: %v", err)
	}
	if persisted.Manifest["kind"] != contracts.RootArtifactKindNamedInputManifest || persisted.ManifestRef["id"] != "named_input_manifest:selected" {
		t.Fatalf("manifest = %#v ref=%#v", persisted.Manifest, persisted.ManifestRef)
	}
	manifestItems := persisted.Manifest["inputs"].([]any)
	if len(manifestItems) != 3 {
		t.Fatalf("manifest items = %#v", manifestItems)
	}
	for index, raw := range manifestItems {
		entry := raw.(map[string]any)
		if entry["source_path"] == nil || entry["display_name"] == nil || entry["content_ref"] == nil {
			t.Fatalf("manifest entry %d = %#v", index, entry)
		}
	}
	if containsKeyRecursive(projection, "source_path") || containsKeyRecursive(projection, "display_name") {
		t.Fatalf("provider projection leaked source metadata: %#v", projection)
	}
	providerItems := projection["inputs"].([]any)
	wantBytes := [][]byte{binary, {}, binary}
	for index, raw := range providerItems {
		entry := raw.(map[string]any)
		path := entry["materialized_path"].(string)
		if filepath.Base(path) != []string{"000001", "000002", "000003"}[index] {
			t.Fatalf("unsafe materialized path = %q", path)
		}
		data, err := os.ReadFile(path)
		if err != nil || !reflect.DeepEqual(data, wantBytes[index]) {
			t.Fatalf("materialized %d = %v, %v", index, data, err)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat materialized input: %v", err)
			}
			if info.Mode().Perm()&0o222 != 0 {
				t.Fatalf("materialized mode = %v", info.Mode())
			}
		}
		if entry["name"] != []string{hostileName, "empty", hostileName}[index] {
			t.Fatalf("provider item %d = %#v", index, entry)
		}
	}
	firstRef := manifestItems[0].(map[string]any)["content_ref"].(map[string]any)
	thirdRef := manifestItems[2].(map[string]any)["content_ref"].(map[string]any)
	if firstRef["id"] == thirdRef["id"] || firstRef["digest"] == thirdRef["digest"] {
		t.Fatalf("duplicate content lost distinct ordinal identity: %#v %#v", firstRef, thirdRef)
	}
}

func TestMaterializeVerifiesTamperingAndRepairsMaterializedBytes(t *testing.T) {
	root := t.TempDir()
	source := writeInputFile(t, root, "value.bin", []byte{0, 0xff, 1})
	selected := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "application/octet-stream", 16, nil),
	})
	prepared, err := Prepare(Options{Contract: selected, Bindings: []string{"value=" + source}})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	st := store.New(filepath.Join(root, "session"))
	persisted, err := Persist(st, prepared)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	executionDir := filepath.Join(st.Root, "execution", "inputs")
	projection, err := Materialize(st, persisted.ManifestRef, executionDir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	target := projection["inputs"].([]any)[0].(map[string]any)["materialized_path"].(string)
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("make materialized file writable: %v", err)
	}
	if err := os.WriteFile(target, []byte("tampered materialization"), 0o644); err != nil {
		t.Fatalf("tamper materialized file: %v", err)
	}
	if _, err := Materialize(st, persisted.ManifestRef, executionDir); err != nil {
		t.Fatalf("rematerialize: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || !reflect.DeepEqual(data, []byte{0, 0xff, 1}) {
		t.Fatalf("rematerialized bytes = %v, %v", data, err)
	}

	manifestEntry := persisted.Manifest["inputs"].([]any)[0].(map[string]any)
	contentRef := manifestEntry["content_ref"].(map[string]any)
	payload, err := st.LoadArtifactPayloadRaw(contentRef)
	if err != nil {
		t.Fatalf("load content: %v", err)
	}
	invalidBase64 := cloneMap(payload)
	invalidBase64["bytes_base64"] = "%%%"
	invalidRef, err := contracts.RootArtifactRefForPayload(contracts.RootArtifactKindNamedInputContent, 1, invalidBase64)
	if err != nil {
		t.Fatalf("ref invalid-base64 payload: %v", err)
	}
	if _, err := decodeContentPayload(invalidBase64, invalidRef, 1); err == nil {
		t.Fatal("invalid base64 envelope was accepted")
	}
	unknownField := cloneMap(payload)
	unknownField["path"] = "/tmp/digest-excluded-field"
	if _, err := decodeContentPayload(unknownField, contentRef, 1); err == nil {
		t.Fatal("unknown digest-excluded content field was accepted")
	}
	digestMismatch := cloneMap(payload)
	digestMismatch["bytes_base64"] = base64.StdEncoding.EncodeToString([]byte("different"))
	digestMismatch["size_bytes"] = len("different")
	digestRef, err := contracts.RootArtifactRefForPayload(contracts.RootArtifactKindNamedInputContent, 1, digestMismatch)
	if err != nil {
		t.Fatalf("ref digest-mismatch payload: %v", err)
	}
	if _, err := decodeContentPayload(digestMismatch, digestRef, 1); err == nil {
		t.Fatal("raw digest mismatch was accepted")
	}

	artifactPath, err := st.ArtifactPathForRef(contentRef)
	if err != nil {
		t.Fatalf("content path: %v", err)
	}
	if !filepath.IsAbs(artifactPath) {
		artifactPath = filepath.Join(st.Root, artifactPath)
	}
	tampered := cloneMap(payload)
	tampered["bytes_base64"] = base64.StdEncoding.EncodeToString([]byte("persisted tamper"))
	body, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("marshal tamper: %v", err)
	}
	if err := os.WriteFile(artifactPath, body, 0o644); err != nil {
		t.Fatalf("tamper persisted artifact: %v", err)
	}
	_, err = Materialize(st, persisted.ManifestRef, filepath.Join(st.Root, "execution", "tampered-output"))
	requireDiagnosticCode(t, err, DiagnosticCodeIntegrity)
}

func TestMaterializeRejectsPathsOutsideSessionAndUnexpectedEntries(t *testing.T) {
	root := t.TempDir()
	source := writeInputFile(t, root, "value.bin", []byte("value"))
	selected := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "application/octet-stream", 16, nil),
	})
	prepared, err := Prepare(Options{Contract: selected, Bindings: []string{"value=" + source}})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	st := store.New(filepath.Join(root, "session"))
	persisted, err := Persist(st, prepared)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	_, err = Materialize(st, persisted.ManifestRef, filepath.Join(root, "outside-session"))
	requireDiagnosticCode(t, err, DiagnosticCodeIntegrity)
	_, err = Materialize(st, persisted.ManifestRef, filepath.Join(st.Root, "artifacts", "inputs"))
	requireDiagnosticCode(t, err, DiagnosticCodeIntegrity)

	executionDir := filepath.Join(st.Root, "execution", "inputs")
	if err := os.MkdirAll(executionDir, 0o700); err != nil {
		t.Fatalf("mkdir execution inputs: %v", err)
	}
	unexpected := filepath.Join(executionDir, "not-an-input")
	if err := os.WriteFile(unexpected, []byte("do not expose"), 0o600); err != nil {
		t.Fatalf("write unexpected entry: %v", err)
	}
	_, err = Materialize(st, persisted.ManifestRef, executionDir)
	requireDiagnosticCode(t, err, DiagnosticCodeIntegrity)
	if _, err := os.Stat(filepath.Join(executionDir, "000001")); !os.IsNotExist(err) {
		t.Fatalf("materialization wrote before rejecting contaminated directory: %v", err)
	}
}

func TestMaterializeRejectsDigestExcludedUnknownManifestFields(t *testing.T) {
	root := t.TempDir()
	source := writeInputFile(t, root, "value.bin", []byte("value"))
	selected := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "application/octet-stream", 16, nil),
	})
	prepared, err := Prepare(Options{Contract: selected, Bindings: []string{"value=" + source}})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	st := store.New(filepath.Join(root, "session"))
	persisted, err := Persist(st, prepared)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	tampered := cloneMap(persisted.Manifest)
	tampered["inputs"].([]any)[0].(map[string]any)["path"] = "/tmp/digest-excluded-field"
	digest, err := contracts.ContractDigest(tampered)
	if err != nil {
		t.Fatalf("digest tampered manifest: %v", err)
	}
	if digest != persisted.ManifestRef["digest"] {
		t.Fatalf("test requires path to remain digest-excluded: got %q", digest)
	}
	manifestPath, err := st.ArtifactPathForRef(persisted.ManifestRef)
	if err != nil {
		t.Fatalf("manifest path: %v", err)
	}
	if !filepath.IsAbs(manifestPath) {
		manifestPath = filepath.Join(st.Root, manifestPath)
	}
	body, err := contracts.CanonicalJSONBytes(tampered)
	if err != nil {
		t.Fatalf("marshal tampered manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
		t.Fatalf("tamper manifest: %v", err)
	}
	_, err = Materialize(st, persisted.ManifestRef, filepath.Join(st.Root, "execution", "inputs"))
	requireDiagnosticCode(t, err, DiagnosticCodeIntegrity)
}

func inputDeclaration(required bool, cardinality string, mediaType string, maxBytes int64, schema map[string]any) map[string]any {
	declaration := map[string]any{
		"required":    required,
		"cardinality": cardinality,
		"media_type":  mediaType,
		"max_bytes":   maxBytes,
	}
	if schema != nil {
		declaration["schema"] = schema
	}
	return declaration
}

const testContractID = "test/named-inputs-v1"

func selectedContract(t *testing.T, inputDeclarations map[string]any) *integration.SelectedContract {
	t.Helper()
	bundleObject := map[string]any{
		"schema_version": integration.BundleSchemaVersion,
		"id":             "test/named-input-bundle",
		"contracts": map[string]any{
			testContractID: map[string]any{
				"turns": []any{
					map[string]any{"participant_turn": 1, "slot": "slot_0", "instructions": "Use the inputs."},
				},
				"inputs": inputDeclarations,
				"result": map[string]any{
					"transport": "json",
					"schema":    map[string]any{"type": "object"},
				},
			},
		},
	}
	data, err := json.Marshal(bundleObject)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	bundle, err := integration.DecodeBundleBytes(data)
	if err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	selected, err := integration.SelectContract(bundle, testContractID, integration.ScheduleRequirement{
		Turns:        []integration.ScheduledTurn{{ParticipantTurn: 1, Slot: "slot_0"}},
		ResultSource: integration.ResultSourceLastTurn,
	})
	if err != nil {
		t.Fatalf("select contract: %v", err)
	}
	return selected
}

func writeInputFile(t *testing.T, root string, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write input %s: %v", name, err)
	}
	return path
}

func requireDiagnosticCode(t *testing.T, err error, code string) contracts.Diagnostic {
	t.Helper()
	if err == nil {
		t.Fatalf("expected diagnostic %s", code)
	}
	var structured *contracts.DiagnosticError
	if !errors.As(err, &structured) {
		t.Fatalf("error = %T %[1]v, want *contracts.DiagnosticError", err)
	}
	for _, diagnostic := range structured.Diagnostics {
		if diagnostic.Code == code {
			return diagnostic
		}
	}
	t.Fatalf("diagnostics = %#v, want code %s", structured.Diagnostics, code)
	return contracts.Diagnostic{}
}

func containsKeyRecursive(value any, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if _, exists := typed[key]; exists {
			return true
		}
		for _, item := range typed {
			if containsKeyRecursive(item, key) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if containsKeyRecursive(item, key) {
				return true
			}
		}
	}
	return false
}
