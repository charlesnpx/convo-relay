package integration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

const validBundleJSON = `{
  "schema_version": "relay-integration-bundle-v1",
  "id": "neutral/integration-v1",
  "contracts": {
    "neutral/contract-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Present the input."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Challenge the presentation."}
      ],
      "reducer": {"instructions": "Return one JSON object."},
      "inputs": {
        "payload": {
          "cardinality": "one",
          "max_bytes": 64,
          "schema": {"type": "object"}
        }
      },
      "result": {
        "transport": "json",
        "schema": {
          "type": "object",
          "required": ["value"],
          "properties": {"value": {"type": "string"}},
          "additionalProperties": false
        }
      }
    }
  }
}`

func TestDecodeBundleNormalizesDefaultsAndComputesSeparateDigests(t *testing.T) {
	bundle, err := DecodeBundleBytes([]byte(validBundleJSON))
	if err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if bundle.SchemaVersion() != BundleSchemaVersion || bundle.ID() != "neutral/integration-v1" {
		t.Fatalf("bundle identity = %#v", bundle)
	}
	contract, exists := bundle.Contract("neutral/contract-v1")
	if !exists {
		t.Fatal("normalized contract missing")
	}
	input := contract.Inputs["payload"]
	if input.Required || input.Cardinality != CardinalityOne || input.MediaType != DefaultMediaType || input.MaxBytes != 64 {
		t.Fatalf("normalized input = %#v", input)
	}
	if got := contract.Result.Assertions; got == nil || len(got) != 0 {
		t.Fatalf("default assertions = %#v, want nonnil empty", got)
	}
	wantBundleDigest, err := integrationSemanticDigest(bundle.ToMap())
	if err != nil {
		t.Fatalf("bundle digest: %v", err)
	}
	if bundle.Digest() != wantBundleDigest || !strings.HasPrefix(bundle.Digest(), contracts.DigestPrefix) {
		t.Fatalf("bundle digest = %q, want %q", bundle.Digest(), wantBundleDigest)
	}

	turns, err := AlternatingSchedule(2)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	selected, err := SelectContract(bundle, "neutral/contract-v1", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
	if err != nil {
		t.Fatalf("select contract: %v", err)
	}
	wantContractDigest, err := integrationSemanticDigest(selected.ToMap())
	if err != nil {
		t.Fatalf("selected digest: %v", err)
	}
	if selected.Digest() != wantContractDigest || selected.Digest() == bundle.Digest() {
		t.Fatalf("selected digest = %q, bundle digest = %q", selected.Digest(), bundle.Digest())
	}

	normalized := bundle.ToMap()
	normalizedContract := normalized["contracts"].(map[string]any)["neutral/contract-v1"].(map[string]any)
	normalizedInput := normalizedContract["inputs"].(map[string]any)["payload"].(map[string]any)
	if normalizedInput["required"] != false || normalizedInput["media_type"] != DefaultMediaType {
		t.Fatalf("normalized map input = %#v", normalizedInput)
	}
	assertions := normalizedContract["result"].(map[string]any)["assertions"].([]any)
	if assertions == nil || len(assertions) != 0 {
		t.Fatalf("normalized assertions = %#v", assertions)
	}
	if _, exists := normalized["source_path"]; exists {
		t.Fatalf("informational source path leaked into normalized bundle: %#v", normalized)
	}
}

func TestLoadBundleFileEnforcesRawByteLimitBeforeDecode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bundle.json")
	if err := os.WriteFile(path, []byte(validBundleJSON), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	bundle, err := LoadBundleFile(path, int64(len(validBundleJSON)))
	if err != nil {
		t.Fatalf("load exact boundary: %v", err)
	}
	absolutePath, _ := filepath.Abs(path)
	if bundle.SourcePath() != absolutePath {
		t.Fatalf("source path = %q, want %q", bundle.SourcePath(), absolutePath)
	}

	assertDiagnostic(t, loadBundleError(path, int64(len(validBundleJSON)-1)), DiagnosticCodeBundleReadFailed, contracts.DiagnosticPhasePreflight)
	assertDiagnostic(t, loadBundleError(filepath.Join(root, "missing.json"), 100), DiagnosticCodeBundleReadFailed, contracts.DiagnosticPhasePreflight)
	assertDiagnostic(t, loadBundleError(root, 100), DiagnosticCodeBundleReadFailed, contracts.DiagnosticPhasePreflight)
	assertDiagnostic(t, loadBundleError(path, 0), DiagnosticCodeBundleReadFailed, contracts.DiagnosticPhasePreflight)
}

func TestDecodeBundleUsesStrictJSONObjectBoundary(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		code string
	}{
		{name: "invalid UTF-8", data: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}, code: contracts.DiagnosticCodeInvalidUTF8},
		{name: "duplicate key", data: []byte(`{"schema_version":"relay-integration-bundle-v1","id":"first","id":"second","contracts":{}}`), code: contracts.DiagnosticCodeDuplicateJSONKey},
		{name: "trailing value", data: []byte(validBundleJSON + ` {}`), code: contracts.DiagnosticCodeTrailingJSON},
		{name: "array root", data: []byte(`[]`), code: contracts.DiagnosticCodeInvalidJSONRoot},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeBundleBytes(test.data)
			assertDiagnostic(t, err, test.code, contracts.DiagnosticPhaseDecode)
		})
	}
}

func TestBundleEnvelopeAndNestedUnknownFieldsAreRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		code   string
	}{
		{name: "unknown envelope field", mutate: func(bundle map[string]any) { bundle["command"] = "run" }, code: DiagnosticCodeInvalidBundle},
		{name: "unknown version", mutate: func(bundle map[string]any) { bundle["schema_version"] = "relay-integration-bundle-v3" }, code: contracts.DiagnosticCodeUnsupportedContractVersion},
		{name: "empty id", mutate: func(bundle map[string]any) { bundle["id"] = " " }, code: DiagnosticCodeInvalidBundle},
		{name: "empty contracts", mutate: func(bundle map[string]any) { bundle["contracts"] = map[string]any{} }, code: DiagnosticCodeInvalidBundle},
		{name: "contracts array", mutate: func(bundle map[string]any) { bundle["contracts"] = []any{} }, code: DiagnosticCodeInvalidBundle},
		{name: "unknown contract field", mutate: func(bundle map[string]any) { firstContract(bundle)["executable"] = "tool" }, code: DiagnosticCodeInvalidBundle},
		{name: "unknown turn field", mutate: func(bundle map[string]any) { firstTurn(bundle)["mode"] = "dynamic" }, code: DiagnosticCodeInvalidBundle},
		{name: "unknown input field", mutate: func(bundle map[string]any) { firstInput(bundle)["path"] = "/tmp/value" }, code: DiagnosticCodeInvalidBundle},
		{name: "unknown result field", mutate: func(bundle map[string]any) { firstResult(bundle)["callback"] = "https://example.invalid" }, code: DiagnosticCodeInvalidBundle},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := validBundleObject(t)
			test.mutate(object)
			_, err := DecodeBundleBytes(encodeJSON(t, object))
			assertDiagnostic(t, err, test.code, contracts.DiagnosticPhasePreflight)
		})
	}
}

func TestBundleVersionDispatchPreservesV1AndNormalizesV2PromptContext(t *testing.T) {
	v1 := validBundleObject(t)
	firstContract(v1)["prompt_context"] = map[string]any{
		"participant_transcript": ParticipantTranscriptComplete,
		"facilitator_ledger":     FacilitatorLedgerTraceOnly,
	}
	_, err := DecodeBundleBytes(encodeJSON(t, v1))
	assertDiagnostic(t, err, DiagnosticCodeInvalidBundle, contracts.DiagnosticPhasePreflight)

	v2Default := validBundleObject(t)
	v2Default["schema_version"] = BundleSchemaVersionV2
	bundle, err := DecodeBundleBytes(encodeJSON(t, v2Default))
	if err != nil {
		t.Fatalf("decode v2 defaults: %v", err)
	}
	if bundle.SchemaVersion() != BundleSchemaVersionV2 {
		t.Fatalf("bundle version = %q", bundle.SchemaVersion())
	}
	projection := firstContract(bundle.ToMap())["prompt_context"].(map[string]any)
	wantDefault := map[string]any{
		"participant_transcript": ParticipantTranscriptComplete,
		"facilitator_ledger":     FacilitatorLedgerInclude,
	}
	if !reflect.DeepEqual(projection, wantDefault) {
		t.Fatalf("default prompt context = %#v, want %#v", projection, wantDefault)
	}

	v2Explicit := validBundleObject(t)
	v2Explicit["schema_version"] = BundleSchemaVersionV2
	firstContract(v2Explicit)["prompt_context"] = map[string]any{
		"participant_transcript": ParticipantTranscriptComplete,
		"facilitator_ledger":     FacilitatorLedgerTraceOnly,
	}
	bundle, err = DecodeBundleBytes(encodeJSON(t, v2Explicit))
	if err != nil {
		t.Fatalf("decode explicit v2: %v", err)
	}
	turns, _ := AlternatingSchedule(2)
	selected, err := SelectContract(bundle, "neutral/contract-v1", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
	if err != nil {
		t.Fatalf("select explicit v2: %v", err)
	}
	if selected.BundleVersion() != BundleSchemaVersionV2 || selected.PromptContext().FacilitatorLedger != FacilitatorLedgerTraceOnly {
		t.Fatalf("selected v2 projection = %#v", selected.PromptContext())
	}
	bundleArtifact, err := bundle.ArtifactPayload()
	if err != nil || bundleArtifact["schema_version"] != 2 {
		t.Fatalf("v2 bundle artifact = %#v, %v", bundleArtifact, err)
	}
	contractArtifact, err := selected.ArtifactPayload()
	if err != nil || contractArtifact["schema_version"] != 2 {
		t.Fatalf("v2 selected-contract artifact = %#v, %v", contractArtifact, err)
	}

	for field, value := range map[string]any{
		"participant_transcript": "summary",
		"facilitator_ledger":     "omit",
	} {
		t.Run("unknown_"+field, func(t *testing.T) {
			invalid := validBundleObject(t)
			invalid["schema_version"] = BundleSchemaVersionV2
			firstContract(invalid)["prompt_context"] = map[string]any{
				"participant_transcript": ParticipantTranscriptComplete,
				"facilitator_ledger":     FacilitatorLedgerInclude,
			}
			firstContract(invalid)["prompt_context"].(map[string]any)[field] = value
			_, err := DecodeBundleBytes(encodeJSON(t, invalid))
			assertDiagnostic(t, err, DiagnosticCodeInvalidBundle, contracts.DiagnosticPhasePreflight)
		})
	}
}

func TestInputAndResultDeclarationsEnforceRequiredContracts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		code   string
	}{
		{name: "missing cardinality", mutate: func(bundle map[string]any) { delete(firstInput(bundle), "cardinality") }, code: DiagnosticCodeInvalidBundle},
		{name: "invalid cardinality", mutate: func(bundle map[string]any) { firstInput(bundle)["cardinality"] = "several" }, code: DiagnosticCodeInvalidBundle},
		{name: "missing max bytes", mutate: func(bundle map[string]any) { delete(firstInput(bundle), "max_bytes") }, code: DiagnosticCodeInvalidBundle},
		{name: "zero max bytes", mutate: func(bundle map[string]any) { firstInput(bundle)["max_bytes"] = json.Number("0") }, code: DiagnosticCodeInvalidBundle},
		{name: "fractional max bytes", mutate: func(bundle map[string]any) { firstInput(bundle)["max_bytes"] = json.Number("1.5") }, code: DiagnosticCodeInvalidBundle},
		{name: "exponent max bytes", mutate: func(bundle map[string]any) { firstInput(bundle)["max_bytes"] = json.Number("1e2") }, code: DiagnosticCodeInvalidBundle},
		{name: "required not boolean", mutate: func(bundle map[string]any) { firstInput(bundle)["required"] = "true" }, code: DiagnosticCodeInvalidBundle},
		{name: "empty media type", mutate: func(bundle map[string]any) { firstInput(bundle)["media_type"] = "" }, code: DiagnosticCodeInvalidBundle},
		{name: "missing result", mutate: func(bundle map[string]any) { delete(firstContract(bundle), "result") }, code: DiagnosticCodeInvalidBundle},
		{name: "unsupported transport", mutate: func(bundle map[string]any) { firstResult(bundle)["transport"] = "text" }, code: DiagnosticCodeInvalidBundle},
		{name: "missing result schema", mutate: func(bundle map[string]any) { delete(firstResult(bundle), "schema") }, code: DiagnosticCodeInvalidSchema},
		{name: "boolean result schema", mutate: func(bundle map[string]any) { firstResult(bundle)["schema"] = true }, code: DiagnosticCodeInvalidSchema},
		{name: "assertions not array", mutate: func(bundle map[string]any) { firstResult(bundle)["assertions"] = map[string]any{} }, code: DiagnosticCodeInvalidAssertion},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := validBundleObject(t)
			test.mutate(object)
			_, err := DecodeBundleBytes(encodeJSON(t, object))
			assertDiagnostic(t, err, test.code, contracts.DiagnosticPhasePreflight)
		})
	}
}

func TestContractSelectionMatchesExactScheduleAndReducerRequirement(t *testing.T) {
	bundle, err := DecodeBundleBytes([]byte(validBundleJSON))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	turns, _ := AlternatingSchedule(2)
	if _, err := SelectContract(bundle, "neutral/contract-v1", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer}); err != nil {
		t.Fatalf("exact schedule rejected: %v", err)
	}
	assertDiagnostic(t, selectContractError(bundle, "missing", turns, ResultSourceReducer), DiagnosticCodeContractNotFound, contracts.DiagnosticPhasePreflight)
	assertDiagnostic(t, selectContractError(bundle, "neutral/contract-v1", turns[:1], ResultSourceReducer), DiagnosticCodeScheduleMismatch, contracts.DiagnosticPhasePreflight)
	wrongSlot := append([]ScheduledTurn(nil), turns...)
	wrongSlot[1].Slot = "slot_0"
	assertDiagnostic(t, selectContractError(bundle, "neutral/contract-v1", wrongSlot, ResultSourceReducer), DiagnosticCodeScheduleMismatch, contracts.DiagnosticPhasePreflight)
	wrongOrdinal := append([]ScheduledTurn(nil), turns...)
	wrongOrdinal[1].ParticipantTurn = 3
	assertDiagnostic(t, selectContractError(bundle, "neutral/contract-v1", wrongOrdinal, ResultSourceReducer), DiagnosticCodeScheduleMismatch, contracts.DiagnosticPhasePreflight)
	assertDiagnostic(t, selectContractError(bundle, "neutral/contract-v1", turns, "automatic"), DiagnosticCodeScheduleMismatch, contracts.DiagnosticPhasePreflight)
	assertDiagnostic(t, selectContractError(bundle, "neutral/contract-v1", nil, ResultSourceReducer), DiagnosticCodeScheduleMismatch, contracts.DiagnosticPhasePreflight)

	withoutReducer := validBundleObject(t)
	delete(firstContract(withoutReducer), "reducer")
	noReducerBundle, err := DecodeBundleBytes(encodeJSON(t, withoutReducer))
	if err != nil {
		t.Fatalf("decode without reducer: %v", err)
	}
	if _, err := SelectContract(noReducerBundle, "neutral/contract-v1", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceLastTurn}); err != nil {
		t.Fatalf("last_turn unexpectedly required reducer: %v", err)
	}
	assertDiagnostic(t, selectContractError(noReducerBundle, "neutral/contract-v1", turns, ResultSourceReducer), DiagnosticCodeScheduleMismatch, contracts.DiagnosticPhasePreflight)
}

func TestContractTurnGapsDuplicatesAndSlotMismatchesAreRejected(t *testing.T) {
	duplicate := validBundleObject(t)
	turns := firstContract(duplicate)["turns"].([]any)
	turns[1].(map[string]any)["participant_turn"] = json.Number("1")
	_, err := DecodeBundleBytes(encodeJSON(t, duplicate))
	assertDiagnostic(t, err, DiagnosticCodeScheduleMismatch, contracts.DiagnosticPhasePreflight)

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "gap", mutate: func(bundle map[string]any) {
			firstContract(bundle)["turns"].([]any)[1].(map[string]any)["participant_turn"] = json.Number("3")
		}},
		{name: "slot mismatch", mutate: func(bundle map[string]any) {
			firstContract(bundle)["turns"].([]any)[1].(map[string]any)["slot"] = "slot_0"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := validBundleObject(t)
			test.mutate(object)
			bundle, err := DecodeBundleBytes(encodeJSON(t, object))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			expected, _ := AlternatingSchedule(2)
			assertDiagnostic(t, selectContractError(bundle, "neutral/contract-v1", expected, ResultSourceReducer), DiagnosticCodeScheduleMismatch, contracts.DiagnosticPhasePreflight)
		})
	}
}

func TestContractTurnDeclarationsNormalizeByParticipantOrdinal(t *testing.T) {
	object := validBundleObject(t)
	turns := firstContract(object)["turns"].([]any)
	turns[0], turns[1] = turns[1], turns[0]
	bundle, err := DecodeBundleBytes(encodeJSON(t, object))
	if err != nil {
		t.Fatalf("decode unordered declarations: %v", err)
	}
	contract, exists := bundle.Contract("neutral/contract-v1")
	if !exists {
		t.Fatal("normalized contract missing")
	}
	for index, turn := range contract.Turns {
		if turn.ParticipantTurn != index+1 {
			t.Fatalf("normalized turn %d = %#v", index, turn)
		}
	}
	expected, _ := AlternatingSchedule(2)
	if _, err := SelectContract(bundle, "neutral/contract-v1", ScheduleRequirement{Turns: expected, ResultSource: ResultSourceReducer}); err != nil {
		t.Fatalf("select unordered declarations: %v", err)
	}
}

func TestBundleMayImplementMultipleOpaqueContracts(t *testing.T) {
	object := validBundleObject(t)
	contractsObject := object["contracts"].(map[string]any)
	contractsObject["opaque id / with spaces"] = contracts.Materialize(contractsObject["neutral/contract-v1"])
	bundle, err := DecodeBundleBytes(encodeJSON(t, object))
	if err != nil {
		t.Fatalf("decode multi-contract bundle: %v", err)
	}
	if len(bundle.Contracts()) != 2 {
		t.Fatalf("contracts = %d, want 2", len(bundle.Contracts()))
	}
	turns, _ := AlternatingSchedule(2)
	first, err := SelectContract(bundle, "neutral/contract-v1", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
	if err != nil {
		t.Fatalf("select first: %v", err)
	}
	second, err := SelectContract(bundle, "opaque id / with spaces", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
	if err != nil {
		t.Fatalf("select opaque: %v", err)
	}
	if first.Digest() == second.Digest() {
		t.Fatal("selected contract digest did not bind the opaque contract id")
	}
}

func TestAssertionDeclarationsNormalizeAllVersionOneShapes(t *testing.T) {
	object := validBundleObject(t)
	firstResult(object)["assertions"] = []any{
		map[string]any{"type": "unique", "source": "result", "pointer": "/items/*/id"},
		map[string]any{
			"type":  "set_equal",
			"left":  map[string]any{"source": "input:payload", "pointer": "/items/*/id"},
			"right": map[string]any{"source": "result", "pointer": "/items/*/id"},
		},
		map[string]any{
			"type":  "value_equal",
			"left":  map[string]any{"source": "input:payload", "pointer": ""},
			"right": map[string]any{"source": "result", "pointer": "/value"},
		},
		map[string]any{
			"type": "field_equal_by_key",
			"left": map[string]any{
				"source": "input:payload", "items_pointer": "/items", "key_pointer": "/id", "value_pointer": "/value",
			},
			"right": map[string]any{
				"source": "result", "items_pointer": "/items", "key_pointer": "/id", "value_pointer": "/value",
			},
		},
	}
	bundle, err := DecodeBundleBytes(encodeJSON(t, object))
	if err != nil {
		t.Fatalf("decode assertions: %v", err)
	}
	contract, exists := bundle.Contract("neutral/contract-v1")
	if !exists {
		t.Fatal("normalized contract missing")
	}
	got := contract.ToMap()["result"].(map[string]any)["assertions"]
	want := firstResult(object)["assertions"]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized assertions = %#v, want %#v", got, want)
	}

	invalid := []struct {
		name  string
		value any
	}{
		{name: "unknown type", value: map[string]any{"type": "execute", "command": "tool"}},
		{name: "missing operand", value: map[string]any{"type": "set_equal", "right": map[string]any{"source": "result", "pointer": ""}}},
		{name: "unknown unique field", value: map[string]any{"type": "unique", "source": "result", "pointer": "", "expression": "x"}},
		{name: "incomplete field operand", value: map[string]any{
			"type":  "field_equal_by_key",
			"left":  map[string]any{"source": "result", "items_pointer": "", "key_pointer": ""},
			"right": map[string]any{"source": "result", "items_pointer": "", "key_pointer": "", "value_pointer": ""},
		}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			candidate := validBundleObject(t)
			firstResult(candidate)["assertions"] = []any{test.value}
			_, err := DecodeBundleBytes(encodeJSON(t, candidate))
			assertDiagnostic(t, err, DiagnosticCodeInvalidAssertion, contracts.DiagnosticPhasePreflight)
		})
	}
}

func TestUnrelatedBundleContractsDoNotChangeSelectedContractDigest(t *testing.T) {
	firstBundle, err := DecodeBundleBytes([]byte(validBundleJSON))
	if err != nil {
		t.Fatalf("decode first: %v", err)
	}
	object := validBundleObject(t)
	other := contracts.Materialize(firstContract(object)).(map[string]any)
	other["turns"].([]any)[0].(map[string]any)["instructions"] = "Unrelated instructions."
	object["contracts"].(map[string]any)["other/contract"] = other
	secondBundle, err := DecodeBundleBytes(encodeJSON(t, object))
	if err != nil {
		t.Fatalf("decode second: %v", err)
	}
	turns, _ := AlternatingSchedule(2)
	firstSelected, _ := SelectContract(firstBundle, "neutral/contract-v1", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
	secondSelected, _ := SelectContract(secondBundle, "neutral/contract-v1", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
	if firstBundle.Digest() == secondBundle.Digest() {
		t.Fatal("bundle digest ignored an added contract")
	}
	if firstSelected.Digest() != secondSelected.Digest() {
		t.Fatalf("unrelated contract changed selected digest: %q != %q", firstSelected.Digest(), secondSelected.Digest())
	}
}

func TestIntegrationDigestsBindSemanticKeysExcludedByLegacyDigest(t *testing.T) {
	tests := []struct {
		name       string
		contractID string
		prepare    func(map[string]any)
		mutate     func(map[string]any)
	}{
		{
			name:       "contract id path",
			contractID: "path",
			prepare: func(bundle map[string]any) {
				contractsMap := bundle["contracts"].(map[string]any)
				contractsMap["path"] = contractsMap["neutral/contract-v1"]
				delete(contractsMap, "neutral/contract-v1")
			},
			mutate: func(bundle map[string]any) {
				bundle["contracts"].(map[string]any)["path"].(map[string]any)["turns"].([]any)[0].(map[string]any)["instructions"] = "Changed semantics."
			},
		},
		{
			name:       "input name path",
			contractID: "neutral/contract-v1",
			prepare: func(bundle map[string]any) {
				inputs := firstContract(bundle)["inputs"].(map[string]any)
				inputs["path"] = inputs["payload"]
				delete(inputs, "payload")
			},
			mutate: func(bundle map[string]any) {
				firstContract(bundle)["inputs"].(map[string]any)["path"].(map[string]any)["max_bytes"] = json.Number("65")
			},
		},
		{
			name:       "property name created_at",
			contractID: "neutral/contract-v1",
			prepare: func(bundle map[string]any) {
				firstResult(bundle)["schema"] = map[string]any{
					"type":       "object",
					"properties": map[string]any{"created_at": map[string]any{"type": "string"}},
				}
			},
			mutate: func(bundle map[string]any) {
				firstResult(bundle)["schema"].(map[string]any)["properties"].(map[string]any)["created_at"].(map[string]any)["type"] = "integer"
			},
		},
		{
			name:       "definition name path",
			contractID: "neutral/contract-v1",
			prepare: func(bundle map[string]any) {
				firstResult(bundle)["schema"] = map[string]any{
					"$defs": map[string]any{"path": map[string]any{"type": "string"}},
					"$ref":  "#/$defs/path",
				}
			},
			mutate: func(bundle map[string]any) {
				firstResult(bundle)["schema"].(map[string]any)["$defs"].(map[string]any)["path"].(map[string]any)["type"] = "integer"
			},
		},
		{
			name:       "const member path",
			contractID: "neutral/contract-v1",
			prepare: func(bundle map[string]any) {
				firstResult(bundle)["schema"] = map[string]any{"const": map[string]any{"path": "first"}}
			},
			mutate: func(bundle map[string]any) {
				firstResult(bundle)["schema"].(map[string]any)["const"].(map[string]any)["path"] = "second"
			},
		},
	}

	turns, _ := AlternatingSchedule(2)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			beforeObject := validBundleObject(t)
			test.prepare(beforeObject)
			afterObject := contracts.Materialize(beforeObject).(map[string]any)
			test.mutate(afterObject)

			before, err := DecodeBundleBytes(encodeJSON(t, beforeObject))
			if err != nil {
				t.Fatalf("decode before: %v", err)
			}
			after, err := DecodeBundleBytes(encodeJSON(t, afterObject))
			if err != nil {
				t.Fatalf("decode after: %v", err)
			}
			if before.Digest() == after.Digest() {
				t.Fatal("semantic bundle change did not change bundle digest")
			}

			beforeSelected, err := SelectContract(before, test.contractID, ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
			if err != nil {
				t.Fatalf("select before: %v", err)
			}
			afterSelected, err := SelectContract(after, test.contractID, ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
			if err != nil {
				t.Fatalf("select after: %v", err)
			}
			if beforeSelected.Digest() == afterSelected.Digest() {
				t.Fatal("semantic contract change did not change selected-contract digest")
			}
		})
	}
}

func TestBundleAndSelectionAccessorsCannotInvalidateCachedDigests(t *testing.T) {
	bundle, err := DecodeBundleBytes([]byte(validBundleJSON))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	bundleMap := bundle.ToMap()
	bundleDigest := bundle.Digest()

	contract, exists := bundle.Contract("neutral/contract-v1")
	if !exists {
		t.Fatal("contract missing")
	}
	contract.Turns[0].Instructions = "mutated copy"
	contract.Inputs["payload"].MaxBytes = 1
	contractsCopy := bundle.Contracts()
	delete(contractsCopy, "neutral/contract-v1")
	returnedMap := bundle.ToMap()
	returnedMap["id"] = "mutated copy"
	returnedMap["contracts"].(map[string]any)["neutral/contract-v1"].(map[string]any)["turns"].([]any)[0].(map[string]any)["instructions"] = "mutated map"

	if !reflect.DeepEqual(bundle.ToMap(), bundleMap) {
		t.Fatalf("bundle changed through accessor copy: %#v", bundle.ToMap())
	}
	if bundle.Digest() != bundleDigest {
		t.Fatalf("bundle digest changed through accessor copy: %q != %q", bundle.Digest(), bundleDigest)
	}
	wantBundleDigest, err := integrationSemanticDigest(bundle.ToMap())
	if err != nil || wantBundleDigest != bundle.Digest() {
		t.Fatalf("bundle digest no longer matches material: got %q want %q err=%v", bundle.Digest(), wantBundleDigest, err)
	}

	turns, _ := AlternatingSchedule(2)
	selected, err := SelectContract(bundle, "neutral/contract-v1", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	selectedMap := selected.ToMap()
	selectedDigest := selected.Digest()
	selectedCopy := selected.Contract()
	selectedCopy.Turns[0].Instructions = "mutated selected copy"
	selectedSchemaCopy := selectedCopy.Result.Schema.Document()
	selectedSchemaCopy["type"] = "array"
	if !reflect.DeepEqual(selected.ToMap(), selectedMap) {
		t.Fatalf("selected contract changed through accessor copy: %#v", selected.ToMap())
	}
	if selected.ID() != "neutral/contract-v1" || selected.Digest() != selectedDigest {
		t.Fatalf("selected identity changed: id=%q digest=%q", selected.ID(), selected.Digest())
	}
	wantSelectedDigest, err := integrationSemanticDigest(selected.ToMap())
	if err != nil || wantSelectedDigest != selected.Digest() {
		t.Fatalf("selected digest no longer matches material: got %q want %q err=%v", selected.Digest(), wantSelectedDigest, err)
	}
}

func loadBundleError(path string, limit int64) error {
	_, err := LoadBundleFile(path, limit)
	return err
}

func selectContractError(bundle *Bundle, id string, turns []ScheduledTurn, resultSource string) error {
	_, err := SelectContract(bundle, id, ScheduleRequirement{Turns: turns, ResultSource: resultSource})
	return err
}

func validBundleObject(t *testing.T) map[string]any {
	t.Helper()
	object, err := contracts.DecodeStrictJSONObjectBytes([]byte(validBundleJSON))
	if err != nil {
		t.Fatalf("decode test bundle: %v", err)
	}
	return object
}

func firstContract(bundle map[string]any) map[string]any {
	return bundle["contracts"].(map[string]any)["neutral/contract-v1"].(map[string]any)
}

func firstTurn(bundle map[string]any) map[string]any {
	return firstContract(bundle)["turns"].([]any)[0].(map[string]any)
}

func firstInput(bundle map[string]any) map[string]any {
	return firstContract(bundle)["inputs"].(map[string]any)["payload"].(map[string]any)
}

func firstResult(bundle map[string]any) map[string]any {
	return firstContract(bundle)["result"].(map[string]any)
}

func encodeJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := contracts.CanonicalJSONBytes(value)
	if err != nil {
		t.Fatalf("encode JSON: %v", err)
	}
	return data
}

func assertDiagnostic(t *testing.T, err error, code string, phase string) contracts.Diagnostic {
	t.Helper()
	if err == nil {
		t.Fatalf("expected diagnostic %s", code)
	}
	var typed *contracts.DiagnosticError
	if !errors.As(err, &typed) {
		t.Fatalf("error type = %T, want *contracts.DiagnosticError: %v", err, err)
	}
	if len(typed.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v", typed.Diagnostics)
	}
	diagnostic := typed.Diagnostics[0]
	if diagnostic.Code != code || diagnostic.Phase != phase {
		t.Fatalf("diagnostic = %#v, want code=%q phase=%q", diagnostic, code, phase)
	}
	return diagnostic
}
