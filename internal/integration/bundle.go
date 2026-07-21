package integration

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
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
	bundle.SourcePath = absolutePath
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
	contract, exists := bundle.Contracts[contractID]
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
	selected := &SelectedContract{ID: contractID, Contract: contract}
	digest, err := contracts.ContractDigest(selected.ToMap())
	if err != nil {
		return nil, wrapPreflightError(
			err,
			DiagnosticCodeInvalidBundle,
			appendPointer("/contracts", contractID),
			"Selected integration contract could not be hashed.",
			nil,
		)
	}
	selected.Digest = digest
	return selected, nil
}

func normalizeBundle(object map[string]any) (*Bundle, error) {
	if err := rejectUnknownFields(object, []string{"schema_version", "id", "contracts"}, "", "integration bundle", DiagnosticCodeInvalidBundle); err != nil {
		return nil, err
	}
	version, err := requireString(object, "schema_version", "", false, DiagnosticCodeInvalidBundle)
	if err != nil {
		return nil, err
	}
	if version != BundleSchemaVersion {
		return nil, preflightError(
			DiagnosticCodeInvalidBundle,
			"/schema_version",
			fmt.Sprintf("schema_version must be %s.", BundleSchemaVersion),
			map[string]any{"schema_version": version},
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
		SchemaVersion: BundleSchemaVersion,
		ID:            bundleID,
		Contracts:     make(map[string]*Contract, len(contractObjects)),
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
		contract, err := normalizeContract(contractObject, path)
		if err != nil {
			return nil, err
		}
		bundle.Contracts[contractID] = contract
	}
	digest, err := contracts.ContractDigest(bundle.ToMap())
	if err != nil {
		return nil, wrapPreflightError(err, DiagnosticCodeInvalidBundle, "", "Integration bundle could not be hashed.", nil)
	}
	bundle.Digest = digest
	return bundle, nil
}

func normalizeContract(object map[string]any, path string) (*Contract, error) {
	if err := rejectUnknownFields(object, []string{"turns", "reducer", "inputs", "result"}, path, "integration contract", DiagnosticCodeInvalidBundle); err != nil {
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
	return &Contract{Turns: turns, Reducer: reducer, Inputs: inputs, Result: result}, nil
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
	var schema *CompiledSchema
	if rawSchema, exists := object["schema"]; exists {
		schema, err = CompileSchema(rawSchema, appendPointer(path, "schema"))
		if err != nil {
			return nil, err
		}
	}
	return &InputDeclaration{
		Required:    required,
		Cardinality: cardinality,
		MediaType:   mediaType,
		MaxBytes:    maxBytes,
		Schema:      schema,
	}, nil
}

func normalizeResult(object map[string]any, path string) (ResultDeclaration, error) {
	if err := rejectUnknownFields(object, []string{"transport", "schema", "assertions"}, path, "result declaration", DiagnosticCodeInvalidBundle); err != nil {
		return ResultDeclaration{}, err
	}
	transport, err := requireString(object, "transport", path, false, DiagnosticCodeInvalidBundle)
	if err != nil {
		return ResultDeclaration{}, err
	}
	if transport != ResultTransportJSON {
		return ResultDeclaration{}, preflightError(
			DiagnosticCodeInvalidBundle,
			appendPointer(path, "transport"),
			"Version 1 result transport must be json.",
			map[string]any{"transport": transport},
		)
	}
	rawSchema, exists := object["schema"]
	if !exists {
		return ResultDeclaration{}, preflightError(DiagnosticCodeInvalidSchema, appendPointer(path, "schema"), "Result schema is required.", nil)
	}
	schema, err := CompileSchema(rawSchema, appendPointer(path, "schema"))
	if err != nil {
		return ResultDeclaration{}, err
	}
	assertions := []AssertionDeclaration{}
	if rawAssertions, exists := object["assertions"]; exists {
		items, ok := rawAssertions.([]any)
		if !ok {
			return ResultDeclaration{}, preflightError(DiagnosticCodeInvalidAssertion, appendPointer(path, "assertions"), "assertions must be an array.", nil)
		}
		assertions = make([]AssertionDeclaration, 0, len(items))
		for index, item := range items {
			assertionPath := appendPointer(appendPointer(path, "assertions"), strconv.Itoa(index))
			assertionObject, ok := item.(map[string]any)
			if !ok {
				return ResultDeclaration{}, preflightError(DiagnosticCodeInvalidAssertion, assertionPath, "Assertion declaration must be an object.", nil)
			}
			assertion, err := normalizeAssertion(assertionObject, assertionPath)
			if err != nil {
				return ResultDeclaration{}, err
			}
			assertions = append(assertions, assertion)
		}
	}
	return ResultDeclaration{Transport: ResultTransportJSON, Schema: schema, Assertions: assertions}, nil
}

func normalizeAssertion(object map[string]any, path string) (AssertionDeclaration, error) {
	assertionType, err := requireString(object, "type", path, false, DiagnosticCodeInvalidAssertion)
	if err != nil {
		return AssertionDeclaration{}, err
	}
	switch assertionType {
	case "unique":
		if err := rejectUnknownFields(object, []string{"type", "source", "pointer"}, path, "unique assertion", DiagnosticCodeInvalidAssertion); err != nil {
			return AssertionDeclaration{}, err
		}
		source, err := requireString(object, "source", path, false, DiagnosticCodeInvalidAssertion)
		if err != nil {
			return AssertionDeclaration{}, err
		}
		pointer, err := requireString(object, "pointer", path, true, DiagnosticCodeInvalidAssertion)
		if err != nil {
			return AssertionDeclaration{}, err
		}
		return AssertionDeclaration{Type: assertionType, Source: source, Pointer: pointer}, nil
	case "set_equal", "value_equal":
		if err := rejectUnknownFields(object, []string{"type", "left", "right"}, path, assertionType+" assertion", DiagnosticCodeInvalidAssertion); err != nil {
			return AssertionDeclaration{}, err
		}
		left, err := normalizeAssertionOperand(object["left"], appendPointer(path, "left"), false)
		if err != nil {
			return AssertionDeclaration{}, err
		}
		right, err := normalizeAssertionOperand(object["right"], appendPointer(path, "right"), false)
		if err != nil {
			return AssertionDeclaration{}, err
		}
		return AssertionDeclaration{Type: assertionType, Left: left, Right: right}, nil
	case "field_equal_by_key":
		if err := rejectUnknownFields(object, []string{"type", "left", "right"}, path, "field_equal_by_key assertion", DiagnosticCodeInvalidAssertion); err != nil {
			return AssertionDeclaration{}, err
		}
		left, err := normalizeAssertionOperand(object["left"], appendPointer(path, "left"), true)
		if err != nil {
			return AssertionDeclaration{}, err
		}
		right, err := normalizeAssertionOperand(object["right"], appendPointer(path, "right"), true)
		if err != nil {
			return AssertionDeclaration{}, err
		}
		return AssertionDeclaration{Type: assertionType, Left: left, Right: right}, nil
	default:
		return AssertionDeclaration{}, preflightError(
			DiagnosticCodeInvalidAssertion,
			appendPointer(path, "type"),
			"Unsupported assertion type.",
			map[string]any{"type": assertionType},
		)
	}
}

func normalizeAssertionOperand(value any, path string, fields bool) (*AssertionOperand, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, preflightError(DiagnosticCodeInvalidAssertion, path, "Assertion operand must be an object.", nil)
	}
	allowed := []string{"source", "pointer"}
	if fields {
		allowed = []string{"source", "items_pointer", "key_pointer", "value_pointer"}
	}
	if err := rejectUnknownFields(object, allowed, path, "assertion operand", DiagnosticCodeInvalidAssertion); err != nil {
		return nil, err
	}
	source, err := requireString(object, "source", path, false, DiagnosticCodeInvalidAssertion)
	if err != nil {
		return nil, err
	}
	operand := &AssertionOperand{Source: source}
	if !fields {
		operand.Pointer, err = requireString(object, "pointer", path, true, DiagnosticCodeInvalidAssertion)
		if err != nil {
			return nil, err
		}
		return operand, nil
	}
	operand.ItemsPointer, err = requireString(object, "items_pointer", path, true, DiagnosticCodeInvalidAssertion)
	if err != nil {
		return nil, err
	}
	operand.KeyPointer, err = requireString(object, "key_pointer", path, true, DiagnosticCodeInvalidAssertion)
	if err != nil {
		return nil, err
	}
	operand.ValuePointer, err = requireString(object, "value_pointer", path, true, DiagnosticCodeInvalidAssertion)
	if err != nil {
		return nil, err
	}
	return operand, nil
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

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
