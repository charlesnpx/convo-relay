package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func LoadBundleFile(path string, maxBytes int64) (*Bundle, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, wrapPreflightError(
			err,
			DiagnosticCodeBundleReadFailed,
			"",
			"Integration bundle path could not be resolved.",
			map[string]any{"source_path": path},
		)
	}
	data, err := contracts.ReadFileBytesLimited(absolutePath, maxBytes)
	if err != nil {
		return nil, wrapPreflightError(
			err,
			DiagnosticCodeBundleReadFailed,
			"",
			"Integration bundle could not be read within the configured byte limit.",
			map[string]any{"source_path": absolutePath, "max_bytes": maxBytes},
		)
	}
	bundle, err := DecodeBundleBytes(data)
	if err != nil {
		return nil, err
	}
	return bundle, nil
}

func DecodeBundleBytes(data []byte) (*Bundle, error) {
	object, err := contracts.DecodeStrictJSONObjectBytes(data)
	if err != nil {
		return nil, err
	}
	return normalizeBundle(object)
}

func SelectContract(bundle *Bundle, contractID string, requirement ScheduleRequirement) (*SelectedContract, error) {
	if bundle == nil {
		return nil, preflightError(DiagnosticCodeInvalidBundle, "", "Integration bundle is required.", nil)
	}
	contract, exists := bundle.contracts[contractID]
	if !exists {
		return nil, preflightError(
			DiagnosticCodeContractNotFound,
			appendPointer("/contracts", contractID),
			"Integration bundle does not implement the requested contract.",
			map[string]any{"contract_id": contractID},
		)
	}
	if err := validateContractSchedule(contractID, contract, requirement); err != nil {
		return nil, err
	}
	selected := &SelectedContract{id: contractID, contract: contract, bundleVersion: bundle.schemaVersion}
	digest, err := integrationSemanticDigestForVersion(selected.ToMap(), selected.bundleVersion)
	if err != nil {
		return nil, wrapPreflightError(
			err,
			DiagnosticCodeInvalidBundle,
			appendPointer("/contracts", contractID),
			"Selected integration contract could not be hashed.",
			nil,
		)
	}
	selected.digest = digest
	return selected, nil
}

func normalizeBundle(object map[string]any) (*Bundle, error) {
	if err := rejectUnknownFields(object, []string{"schema_version", "id", "contracts"}, "", "integration bundle", DiagnosticCodeInvalidBundle); err != nil {
		return nil, err
	}
	version, err := contracts.RequireStringVersion(object, contracts.ContractIntegrationBundle)
	if err != nil {
		return nil, preflightError(
			contracts.DiagnosticCodeUnsupportedContractVersion,
			"/schema_version",
			"Unsupported integration bundle schema version.",
			map[string]any{
				"observed":  object["schema_version"],
				"supported": []any{BundleSchemaVersionV1, BundleSchemaVersionV2},
			},
		)
	}
	bundleID, err := requireString(object, "id", "", false, DiagnosticCodeInvalidBundle)
	if err != nil {
		return nil, err
	}
	contractObjects, err := requireObject(object, "contracts", "", DiagnosticCodeInvalidBundle)
	if err != nil {
		return nil, err
	}
	if len(contractObjects) == 0 {
		return nil, preflightError(DiagnosticCodeInvalidBundle, "/contracts", "contracts must contain at least one contract.", nil)
	}

	bundle := &Bundle{
		schemaVersion: version,
		id:            bundleID,
		contracts:     make(map[string]*Contract, len(contractObjects)),
	}
	for _, contractID := range sortedKeys(contractObjects) {
		if strings.TrimSpace(contractID) == "" {
			return nil, preflightError(DiagnosticCodeInvalidBundle, appendPointer("/contracts", contractID), "Contract ids must be non-empty opaque strings.", nil)
		}
		path := appendPointer("/contracts", contractID)
		contractObject, ok := contractObjects[contractID].(map[string]any)
		if !ok {
			return nil, preflightError(DiagnosticCodeInvalidBundle, path, "Integration contract must be an object.", nil)
		}
		contract, err := normalizeContract(contractObject, path, version)
		if err != nil {
			return nil, err
		}
		bundle.contracts[contractID] = contract
	}
	digest, err := integrationSemanticDigestForVersion(bundle.ToMap(), bundle.schemaVersion)
	if err != nil {
		return nil, wrapPreflightError(err, DiagnosticCodeInvalidBundle, "", "Integration bundle could not be hashed.", nil)
	}
	bundle.digest = digest
	return bundle, nil
}

func normalizeContract(object map[string]any, path string, bundleVersion string) (*Contract, error) {
	allowed := []string{"turns", "reducer", "inputs", "result"}
	if bundleVersion == BundleSchemaVersionV2 {
		allowed = append(allowed, "prompt_context")
	}
	if err := rejectUnknownFields(object, allowed, path, "integration contract", DiagnosticCodeInvalidBundle); err != nil {
		return nil, err
	}
	turnValues, err := requireArray(object, "turns", path, DiagnosticCodeInvalidBundle)
	if err != nil {
		return nil, err
	}
	if len(turnValues) == 0 {
		return nil, preflightError(DiagnosticCodeInvalidBundle, appendPointer(path, "turns"), "turns must contain at least one declaration.", nil)
	}
	turns := make([]TurnDeclaration, 0, len(turnValues))
	seenTurns := map[int]bool{}
	for index, value := range turnValues {
		turnPath := appendPointer(appendPointer(path, "turns"), strconv.Itoa(index))
		turnObject, ok := value.(map[string]any)
		if !ok {
			return nil, preflightError(DiagnosticCodeInvalidBundle, turnPath, "Turn declaration must be an object.", nil)
		}
		turn, err := normalizeTurn(turnObject, turnPath)
		if err != nil {
			return nil, err
		}
		if seenTurns[turn.ParticipantTurn] {
			return nil, preflightError(
				DiagnosticCodeScheduleMismatch,
				appendPointer(turnPath, "participant_turn"),
				"Participant turn declarations must be unique.",
				map[string]any{"participant_turn": turn.ParticipantTurn},
			)
		}
		seenTurns[turn.ParticipantTurn] = true
		turns = append(turns, turn)
	}
	sort.Slice(turns, func(left, right int) bool {
		return turns[left].ParticipantTurn < turns[right].ParticipantTurn
	})

	var reducer *ReducerDeclaration
	if rawReducer, exists := object["reducer"]; exists {
		reducerObject, ok := rawReducer.(map[string]any)
		if !ok {
			return nil, preflightError(DiagnosticCodeInvalidBundle, appendPointer(path, "reducer"), "reducer must be an object.", nil)
		}
		reducer, err = normalizeReducer(reducerObject, appendPointer(path, "reducer"))
		if err != nil {
			return nil, err
		}
	}

	inputs := map[string]*InputDeclaration{}
	if rawInputs, exists := object["inputs"]; exists {
		inputObjects, ok := rawInputs.(map[string]any)
		if !ok {
			return nil, preflightError(DiagnosticCodeInvalidBundle, appendPointer(path, "inputs"), "inputs must be an object.", nil)
		}
		for _, name := range sortedKeys(inputObjects) {
			inputPath := appendPointer(appendPointer(path, "inputs"), name)
			if strings.TrimSpace(name) == "" {
				return nil, preflightError(DiagnosticCodeInvalidBundle, inputPath, "Input names must be non-empty opaque strings.", nil)
			}
			inputObject, ok := inputObjects[name].(map[string]any)
			if !ok {
				return nil, preflightError(DiagnosticCodeInvalidBundle, inputPath, "Input declaration must be an object.", nil)
			}
			input, err := normalizeInput(inputObject, inputPath)
			if err != nil {
				return nil, err
			}
			inputs[name] = input
		}
	}

	resultObject, err := requireObject(object, "result", path, DiagnosticCodeInvalidBundle)
	if err != nil {
		return nil, err
	}
	result, err := normalizeResult(resultObject, appendPointer(path, "result"))
	if err != nil {
		return nil, err
	}
	promptContext := PromptContextProjection{
		SchemaVersion:         PromptContextPolicyVersion,
		ParticipantTranscript: ParticipantTranscriptComplete,
		FacilitatorLedger:     FacilitatorLedgerInclude,
	}
	if rawProjection, exists := object["prompt_context"]; exists {
		projectionObject, ok := rawProjection.(map[string]any)
		if !ok {
			return nil, preflightError(DiagnosticCodeInvalidBundle, appendPointer(path, "prompt_context"), "prompt_context must be an object.", nil)
		}
		promptContext, err = normalizePromptContext(projectionObject, appendPointer(path, "prompt_context"))
		if err != nil {
			return nil, err
		}
	}
	return &Contract{Turns: turns, Reducer: reducer, Inputs: inputs, Result: result, PromptContext: promptContext}, nil
}

func normalizePromptContext(object map[string]any, path string) (PromptContextProjection, error) {
	if err := rejectUnknownFields(object, []string{"participant_transcript", "facilitator_ledger"}, path, "prompt context projection", DiagnosticCodeInvalidBundle); err != nil {
		return PromptContextProjection{}, err
	}
	version := PromptContextPolicyVersion
	participantTranscript := ParticipantTranscriptComplete
	if raw, exists := object["participant_transcript"]; exists {
		value, ok := raw.(string)
		if !ok || value != ParticipantTranscriptComplete {
			return PromptContextProjection{}, preflightError(
				DiagnosticCodeInvalidBundle,
				appendPointer(path, "participant_transcript"),
				"participant_transcript must be complete.",
				map[string]any{"participant_transcript": raw},
			)
		}
		participantTranscript = value
	}
	facilitatorLedger := FacilitatorLedgerInclude
	if raw, exists := object["facilitator_ledger"]; exists {
		value, ok := raw.(string)
		if !ok || (value != FacilitatorLedgerInclude && value != FacilitatorLedgerTraceOnly) {
			return PromptContextProjection{}, preflightError(
				DiagnosticCodeInvalidBundle,
				appendPointer(path, "facilitator_ledger"),
				"facilitator_ledger must be include or trace_only.",
				map[string]any{"facilitator_ledger": raw},
			)
		}
		facilitatorLedger = value
	}
	return PromptContextProjection{
		SchemaVersion:         version,
		ParticipantTranscript: participantTranscript,
		FacilitatorLedger:     facilitatorLedger,
	}, nil
}

// integrationSemanticDigest hashes every field in a normalized integration
// value. ContractDigest deliberately omits storage metadata keys recursively,
// but those same names are valid semantic data in opaque integration maps and
// JSON Schemas.
func integrationSemanticDigest(value any) (string, error) {
	canonical, err := contracts.CanonicalJSONBytes(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return contracts.DigestPrefix + hex.EncodeToString(sum[:]), nil
}

func integrationSemanticDigestForVersion(value any, version string) (string, error) {
	if version == BundleSchemaVersionV2 {
		return contracts.SemanticJSONDigest(value)
	}
	return integrationSemanticDigest(value)
}

func normalizeTurn(object map[string]any, path string) (TurnDeclaration, error) {
	if err := rejectUnknownFields(object, []string{"participant_turn", "slot", "instructions"}, path, "turn declaration", DiagnosticCodeInvalidBundle); err != nil {
		return TurnDeclaration{}, err
	}
	turn, err := requirePositiveInteger(object, "participant_turn", path)
	if err != nil {
		return TurnDeclaration{}, err
	}
	if turn > math.MaxInt {
		return TurnDeclaration{}, preflightError(DiagnosticCodeInvalidBundle, appendPointer(path, "participant_turn"), "participant_turn is too large.", nil)
	}
	slot, err := requireString(object, "slot", path, false, DiagnosticCodeInvalidBundle)
	if err != nil {
		return TurnDeclaration{}, err
	}
	instructions, err := requireString(object, "instructions", path, false, DiagnosticCodeInvalidBundle)
	if err != nil {
		return TurnDeclaration{}, err
	}
	return TurnDeclaration{ParticipantTurn: int(turn), Slot: slot, Instructions: instructions}, nil
}

func normalizeReducer(object map[string]any, path string) (*ReducerDeclaration, error) {
	if err := rejectUnknownFields(object, []string{"instructions"}, path, "reducer declaration", DiagnosticCodeInvalidBundle); err != nil {
		return nil, err
	}
	instructions, err := requireString(object, "instructions", path, false, DiagnosticCodeInvalidBundle)
	if err != nil {
		return nil, err
	}
	return &ReducerDeclaration{Instructions: instructions}, nil
}

func normalizeInput(object map[string]any, path string) (*InputDeclaration, error) {
	if err := rejectUnknownFields(object, []string{"required", "cardinality", "media_type", "max_bytes", "schema"}, path, "input declaration", DiagnosticCodeInvalidBundle); err != nil {
		return nil, err
	}
	cardinality, err := requireString(object, "cardinality", path, false, DiagnosticCodeInvalidBundle)
	if err != nil {
		return nil, err
	}
	if cardinality != CardinalityOne && cardinality != CardinalityMany {
		return nil, preflightError(
			DiagnosticCodeInvalidBundle,
			appendPointer(path, "cardinality"),
			"cardinality must be one or many.",
			map[string]any{"cardinality": cardinality},
		)
	}
	maxBytes, err := requirePositiveInteger(object, "max_bytes", path)
	if err != nil {
		return nil, err
	}
	required := false
	if rawRequired, exists := object["required"]; exists {
		value, ok := rawRequired.(bool)
		if !ok {
			return nil, preflightError(DiagnosticCodeInvalidBundle, appendPointer(path, "required"), "required must be a boolean.", nil)
		}
		required = value
	}
	mediaType := DefaultMediaType
	if _, exists := object["media_type"]; exists {
		mediaType, err = requireString(object, "media_type", path, false, DiagnosticCodeInvalidBundle)
		if err != nil {
			return nil, err
		}
	}
	return &InputDeclaration{
		Required:    required,
		Cardinality: cardinality,
		MediaType:   mediaType,
		MaxBytes:    maxBytes,
	}, nil
}

func normalizeResult(object map[string]any, path string) (ResultDeclaration, error) {
	if err := rejectUnknownFields(object, []string{"format", "schema", "transport"}, path, "result declaration", DiagnosticCodeInvalidBundle); err != nil {
		return ResultDeclaration{}, err
	}
	format, exists := object["format"].(string)
	if !exists || strings.TrimSpace(format) == "" {
		// Existing consumer bundles used transport for the only formerly-supported
		// format. Normalize that input spelling away rather than retaining it in
		// the selected contract or plan.
		format, _ = object["transport"].(string)
	}
	if format != "text" && format != "json" {
		return ResultDeclaration{}, preflightError(
			DiagnosticCodeInvalidBundle,
			appendPointer(path, "format"),
			"result.format must be text or json.",
			map[string]any{"format": format},
		)
	}
	schema, exists := object["schema"]
	if !exists {
		return ResultDeclaration{Format: format}, nil
	}
	if err := validateStandardSchema(schema, appendPointer(path, "schema")); err != nil {
		return ResultDeclaration{}, err
	}
	return ResultDeclaration{Format: format, Schema: contracts.Materialize(schema)}, nil
}

const resultSchemaURL = "https://convo-relay.invalid/integration-result-schema.json"

type resultSchemaLoader struct{}

func (resultSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external result schema loading is disabled: %s", url)
}

func validateStandardSchema(schema any, path string) error {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(resultSchemaLoader{})
	if err := compiler.AddResource(resultSchemaURL, contracts.Materialize(schema)); err != nil {
		return wrapPreflightError(err, DiagnosticCodeInvalidSchema, path, "Result schema could not be registered.", map[string]any{"error": err.Error()})
	}
	if _, err := compiler.Compile(resultSchemaURL); err != nil {
		return wrapPreflightError(err, DiagnosticCodeInvalidSchema, path, "Result schema is invalid.", map[string]any{"error": err.Error()})
	}
	return nil
}

func validateContractSchedule(contractID string, contract *Contract, requirement ScheduleRequirement) error {
	contractPath := appendPointer("/contracts", contractID)
	if requirement.ResultSource != ResultSourceLastTurn && requirement.ResultSource != ResultSourceReducer {
		return preflightError(
			DiagnosticCodeScheduleMismatch,
			contractPath,
			"Result source must be last_turn or reducer before contract selection.",
			map[string]any{"result_source": requirement.ResultSource},
		)
	}
	if len(requirement.Turns) == 0 {
		return preflightError(DiagnosticCodeScheduleMismatch, appendPointer(contractPath, "turns"), "Compiled participant schedule must contain at least one turn.", nil)
	}
	if len(contract.Turns) != len(requirement.Turns) {
		return preflightError(
			DiagnosticCodeScheduleMismatch,
			appendPointer(contractPath, "turns"),
			"Contract turn count does not match the compiled participant schedule.",
			map[string]any{"declared": len(contract.Turns), "expected": len(requirement.Turns)},
		)
	}
	for index, expected := range requirement.Turns {
		declared := contract.Turns[index]
		turnPath := appendPointer(appendPointer(contractPath, "turns"), strconv.Itoa(index))
		if expected.ParticipantTurn != index+1 {
			return preflightError(
				DiagnosticCodeScheduleMismatch,
				appendPointer(turnPath, "participant_turn"),
				"Compiled participant schedule ordinals must be contiguous from one.",
				map[string]any{"participant_turn": expected.ParticipantTurn, "expected": index + 1},
			)
		}
		if declared.ParticipantTurn != expected.ParticipantTurn {
			return preflightError(
				DiagnosticCodeScheduleMismatch,
				appendPointer(turnPath, "participant_turn"),
				"Contract participant turn does not match the compiled schedule.",
				map[string]any{"declared": declared.ParticipantTurn, "expected": expected.ParticipantTurn},
			)
		}
		if declared.Slot != expected.Slot {
			return preflightError(
				DiagnosticCodeScheduleMismatch,
				appendPointer(turnPath, "slot"),
				"Contract slot does not match the compiled schedule.",
				map[string]any{"declared": declared.Slot, "expected": expected.Slot},
			)
		}
	}
	if requirement.ResultSource == ResultSourceReducer && contract.Reducer == nil {
		return preflightError(
			DiagnosticCodeScheduleMismatch,
			appendPointer(contractPath, "reducer"),
			"Reducer instructions are required when result_source is reducer.",
			nil,
		)
	}
	return nil
}

func requirePositiveInteger(object map[string]any, key string, path string) (int64, error) {
	value, exists := object[key]
	if !exists {
		return 0, preflightError(DiagnosticCodeInvalidBundle, appendPointer(path, key), key+" must be a positive integer.", nil)
	}
	parsed, ok := nonNegativeJSONInteger(value)
	if !ok || parsed < 1 {
		return 0, preflightError(DiagnosticCodeInvalidBundle, appendPointer(path, key), key+" must be a positive integer.", nil)
	}
	return parsed, nil
}

func nonNegativeJSONInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(typed.String(), 10, 64)
		return parsed, err == nil && parsed >= 0
	case int:
		return int64(typed), typed >= 0
	case int8:
		return int64(typed), typed >= 0
	case int16:
		return int64(typed), typed >= 0
	case int32:
		return int64(typed), typed >= 0
	case int64:
		return typed, typed >= 0
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
