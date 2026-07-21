package integration

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"math"
	"math/big"
	"mime"
	"sort"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

// AssertionEvaluator is an immutable, preflight-validated assertion program.
// Input values are bound before provider execution; Validate supplies only the
// eventual result document and reports data-dependent failures.
type AssertionEvaluator struct {
	assertions []compiledAssertion
	inputs     map[string]any
}

type compiledAssertion struct {
	kind  string
	path  string
	left  compiledAssertionOperand
	right compiledAssertionOperand
}

type compiledAssertionOperand struct {
	source       string
	pointer      pointerPattern
	itemsPointer pointerPattern
	keyPointer   pointerPattern
	valuePointer pointerPattern
}

type pointerPattern struct {
	raw      string
	segments []pointerSegment
}

type pointerSegment struct {
	value    string
	wildcard bool
}

type assertionSourceBinder struct {
	contract   *Contract
	values     map[string][]any
	bound      map[string]any
	bindValues bool
}

type keyedAssertionValue struct {
	key         any
	value       any
	index       int
	fingerprint semanticFingerprint
}

type normalizedAssertionNumber struct {
	negative    bool
	coefficient string
	exponent    *big.Int
}

type semanticFingerprint [sha256.Size]byte

type semanticIndexedValue struct {
	value       any
	index       int
	fingerprint semanticFingerprint
}

type semanticValueIndex struct {
	entries []semanticIndexedValue
	buckets map[semanticFingerprint][]int
}

// PrepareAssertionEvaluator validates assertion pointers and sources during
// preflight. Each input slice contains its independently decoded JSON values
// in command-line order. A many-valued input is bound as one virtual array.
func PrepareAssertionEvaluator(selected *SelectedContract, inputs map[string][]any) (*AssertionEvaluator, error) {
	if selected == nil || selected.contract == nil {
		return nil, preflightError(DiagnosticCodeInvalidAssertion, "", "A selected integration contract is required for assertion preflight.", nil)
	}
	binder := assertionSourceBinder{
		contract:   selected.contract,
		values:     inputs,
		bound:      make(map[string]any),
		bindValues: true,
	}
	compiled, err := compileAssertionDeclarations(selected, &binder)
	if err != nil {
		return nil, err
	}
	return &AssertionEvaluator{assertions: compiled, inputs: binder.bound}, nil
}

func validateAssertionDeclarations(selected *SelectedContract) error {
	if selected == nil || selected.contract == nil {
		return preflightError(DiagnosticCodeInvalidAssertion, "", "A selected integration contract is required for assertion preflight.", nil)
	}
	binder := assertionSourceBinder{contract: selected.contract}
	_, err := compileAssertionDeclarations(selected, &binder)
	return err
}

func compileAssertionDeclarations(selected *SelectedContract, binder *assertionSourceBinder) ([]compiledAssertion, error) {
	assertionsPath := appendPointer(appendPointer(appendPointer("/contracts", selected.id), "result"), "assertions")
	compiled := make([]compiledAssertion, 0, len(selected.contract.Result.Assertions))
	for index, declaration := range selected.contract.Result.Assertions {
		path := appendPointer(assertionsPath, strconv.Itoa(index))
		assertion := compiledAssertion{kind: declaration.Type, path: path}
		var err error
		switch declaration.Type {
		case "unique":
			assertion.left, err = binder.compileSimpleOperand(
				declaration.Source,
				declaration.Pointer,
				appendPointer(path, "source"),
				appendPointer(path, "pointer"),
			)
		case "set_equal", "value_equal":
			assertion.left, err = binder.compileOperand(declaration.Left, appendPointer(path, "left"), false)
			if err == nil {
				assertion.right, err = binder.compileOperand(declaration.Right, appendPointer(path, "right"), false)
			}
		case "field_equal_by_key":
			assertion.left, err = binder.compileOperand(declaration.Left, appendPointer(path, "left"), true)
			if err == nil {
				assertion.right, err = binder.compileOperand(declaration.Right, appendPointer(path, "right"), true)
			}
		default:
			err = preflightError(
				DiagnosticCodeInvalidAssertion,
				appendPointer(path, "type"),
				"Unsupported assertion type.",
				map[string]any{"type": declaration.Type},
			)
		}
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, assertion)
	}
	return compiled, nil
}

func (b *assertionSourceBinder) compileSimpleOperand(source string, pointer string, sourcePath string, pointerPath string) (compiledAssertionOperand, error) {
	if err := b.bindSource(source, sourcePath); err != nil {
		return compiledAssertionOperand{}, err
	}
	compiledPointer, err := compilePointerPattern(pointer, pointerPath)
	if err != nil {
		return compiledAssertionOperand{}, err
	}
	return compiledAssertionOperand{source: source, pointer: compiledPointer}, nil
}

func (b *assertionSourceBinder) compileOperand(declaration *AssertionOperand, path string, fields bool) (compiledAssertionOperand, error) {
	if declaration == nil {
		return compiledAssertionOperand{}, preflightError(DiagnosticCodeInvalidAssertion, path, "Assertion operand is required.", nil)
	}
	if !fields {
		return b.compileSimpleOperand(
			declaration.Source,
			declaration.Pointer,
			appendPointer(path, "source"),
			appendPointer(path, "pointer"),
		)
	}
	if err := b.bindSource(declaration.Source, appendPointer(path, "source")); err != nil {
		return compiledAssertionOperand{}, err
	}
	itemsPointer, err := compilePointerPattern(declaration.ItemsPointer, appendPointer(path, "items_pointer"))
	if err != nil {
		return compiledAssertionOperand{}, err
	}
	keyPointer, err := compilePointerPattern(declaration.KeyPointer, appendPointer(path, "key_pointer"))
	if err != nil {
		return compiledAssertionOperand{}, err
	}
	valuePointer, err := compilePointerPattern(declaration.ValuePointer, appendPointer(path, "value_pointer"))
	if err != nil {
		return compiledAssertionOperand{}, err
	}
	return compiledAssertionOperand{
		source:       declaration.Source,
		itemsPointer: itemsPointer,
		keyPointer:   keyPointer,
		valuePointer: valuePointer,
	}, nil
}

func (b *assertionSourceBinder) bindSource(source string, path string) error {
	if source == "result" {
		return nil
	}
	if !strings.HasPrefix(source, "input:") || len(source) == len("input:") {
		return preflightError(
			DiagnosticCodeInvalidAssertion,
			path,
			"Assertion source must be result or input:<name>.",
			map[string]any{"source": source},
		)
	}
	if _, exists := b.bound[source]; exists {
		return nil
	}

	name := strings.TrimPrefix(source, "input:")
	declaration, exists := b.contract.Inputs[name]
	if !exists || declaration == nil {
		return preflightError(
			DiagnosticCodeInvalidAssertion,
			path,
			"Assertion source names an undeclared input.",
			map[string]any{"source": source, "input_name": name},
		)
	}
	if !isJSONMediaType(declaration.MediaType) {
		return preflightError(
			DiagnosticCodeInvalidAssertion,
			path,
			"Assertion input sources must declare a JSON media type.",
			map[string]any{"source": source, "media_type": declaration.MediaType},
		)
	}
	if !b.bindValues {
		return nil
	}

	values := b.values[name]
	switch declaration.Cardinality {
	case CardinalityOne:
		if len(values) != 1 {
			return preflightError(
				DiagnosticCodeInvalidAssertion,
				path,
				"A referenced one-valued assertion input must resolve to exactly one JSON document.",
				map[string]any{"source": source, "value_count": len(values)},
			)
		}
		if err := validateAssertionJSONValue(values[0]); err != nil {
			return wrapPreflightError(err, DiagnosticCodeInvalidAssertion, path, "Assertion input source is not a JSON value.", map[string]any{"source": source})
		}
		b.bound[source] = contracts.Materialize(values[0])
	case CardinalityMany:
		if declaration.Required && len(values) == 0 {
			return preflightError(
				DiagnosticCodeInvalidAssertion,
				path,
				"A referenced required many-valued assertion input must contain at least one JSON document.",
				map[string]any{"source": source, "value_count": 0},
			)
		}
		virtual := make([]any, len(values))
		for index, value := range values {
			if err := validateAssertionJSONValue(value); err != nil {
				return wrapPreflightError(
					err,
					DiagnosticCodeInvalidAssertion,
					path,
					"Assertion input source contains a non-JSON value.",
					map[string]any{"source": source, "input_index": index},
				)
			}
			virtual[index] = contracts.Materialize(value)
		}
		b.bound[source] = virtual
	default:
		return preflightError(
			DiagnosticCodeInvalidAssertion,
			path,
			"Assertion input source has an unsupported cardinality.",
			map[string]any{"source": source, "cardinality": declaration.Cardinality},
		)
	}
	return nil
}

func isJSONMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func compilePointerPattern(raw string, path string) (pointerPattern, error) {
	if raw == "" {
		return pointerPattern{raw: raw}, nil
	}
	if !strings.HasPrefix(raw, "/") {
		return pointerPattern{}, invalidPointerPattern(path, raw, "JSON Pointer patterns must be empty or begin with '/'.")
	}
	rawSegments := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	segments := make([]pointerSegment, len(rawSegments))
	for index, rawSegment := range rawSegments {
		segment, err := decodePointerSegment(rawSegment)
		if err != nil {
			return pointerPattern{}, invalidPointerPattern(path, raw, err.Error())
		}
		segments[index] = pointerSegment{value: segment, wildcard: segment == "*"}
	}
	return pointerPattern{raw: raw, segments: segments}, nil
}

func decodePointerSegment(raw string) (string, error) {
	var builder strings.Builder
	for offset := 0; offset < len(raw); offset++ {
		if raw[offset] != '~' {
			builder.WriteByte(raw[offset])
			continue
		}
		if offset+1 >= len(raw) {
			return "", fmt.Errorf("JSON Pointer escape is incomplete")
		}
		offset++
		switch raw[offset] {
		case '0':
			builder.WriteByte('~')
		case '1':
			builder.WriteByte('/')
		default:
			return "", fmt.Errorf("JSON Pointer escape ~%c is invalid", raw[offset])
		}
	}
	return builder.String(), nil
}

func invalidPointerPattern(path string, pointer string, message string) error {
	return preflightError(
		DiagnosticCodeInvalidAssertion,
		path,
		message,
		map[string]any{"pointer": pointer},
	)
}

func (p pointerPattern) selectValues(root any) []any {
	values := []any{root}
	for _, segment := range p.segments {
		next := make([]any, 0)
		for _, value := range values {
			if segment.wildcard {
				switch typed := value.(type) {
				case []any:
					next = append(next, typed...)
				case map[string]any:
					keys := make([]string, 0, len(typed))
					for key := range typed {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					for _, key := range keys {
						next = append(next, typed[key])
					}
				}
				continue
			}
			switch typed := value.(type) {
			case map[string]any:
				if selected, exists := typed[segment.value]; exists {
					next = append(next, selected)
				}
			case []any:
				if index, ok := pointerArrayIndex(segment.value); ok && index < len(typed) {
					next = append(next, typed[index])
				}
			}
		}
		values = next
		if len(values) == 0 {
			break
		}
	}
	return values
}

func pointerArrayIndex(segment string) (int, bool) {
	if segment == "0" {
		return 0, true
	}
	if segment == "" || segment[0] < '1' || segment[0] > '9' {
		return 0, false
	}
	for index := 1; index < len(segment); index++ {
		if segment[index] < '0' || segment[index] > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.Atoi(segment)
	return parsed, err == nil
}

// Validate evaluates all assertions in declaration order. The first failed
// assertion is returned as a typed assertion-phase diagnostic for the result
// validator to classify as invalid_result.
func (e *AssertionEvaluator) Validate(result any) error {
	if e == nil {
		return preflightError(DiagnosticCodeInvalidAssertion, "", "Assertion evaluator is required.", nil)
	}
	if len(e.assertions) == 0 {
		return nil
	}
	if err := validateAssertionJSONValue(result); err != nil {
		return assertionFailure(
			e.assertions[0],
			"Result assertion source is not a JSON value.",
			map[string]any{"source": "result"},
		)
	}
	result = contracts.Materialize(result)
	for _, assertion := range e.assertions {
		if err := e.evaluate(assertion, result); err != nil {
			return err
		}
	}
	return nil
}

func (e *AssertionEvaluator) evaluate(assertion compiledAssertion, result any) error {
	switch assertion.kind {
	case "unique":
		values := assertion.left.pointer.selectValues(e.source(assertion.left.source, result))
		seen := newSemanticValueIndex(len(values))
		for index, value := range values {
			if existing, duplicate := seen.add(value, index); duplicate {
				return assertionFailure(assertion, "Assertion selected duplicate values.", map[string]any{
					"first_index":     existing.index,
					"duplicate_index": index,
				})
			}
		}
		return nil
	case "set_equal":
		left := indexSemanticValues(assertion.left.pointer.selectValues(e.source(assertion.left.source, result)))
		right := indexSemanticValues(assertion.right.pointer.selectValues(e.source(assertion.right.source, result)))
		if missing, ok := firstSemanticSetDifference(left, right); ok {
			return assertionFailure(assertion, "Assertion value sets are not equal.", map[string]any{
				"missing_from": "right",
				"value":        missing,
			})
		}
		if missing, ok := firstSemanticSetDifference(right, left); ok {
			return assertionFailure(assertion, "Assertion value sets are not equal.", map[string]any{
				"missing_from": "left",
				"value":        missing,
			})
		}
		return nil
	case "value_equal":
		left := assertion.left.pointer.selectValues(e.source(assertion.left.source, result))
		right := assertion.right.pointer.selectValues(e.source(assertion.right.source, result))
		if len(left) != 1 || len(right) != 1 {
			return assertionFailure(assertion, "value_equal requires exactly one selected value on each side.", map[string]any{
				"left_count":  len(left),
				"right_count": len(right),
			})
		}
		if !semanticJSONEqual(left[0], right[0]) {
			return assertionFailure(assertion, "Selected values are not equal.", map[string]any{
				"left":  left[0],
				"right": right[0],
			})
		}
		return nil
	case "field_equal_by_key":
		left, err := e.projectFields(assertion, assertion.left, "left", result)
		if err != nil {
			return err
		}
		right, err := e.projectFields(assertion, assertion.right, "right", result)
		if err != nil {
			return err
		}
		rightByFingerprint := make(map[semanticFingerprint][]keyedAssertionValue, len(right))
		for _, entry := range right {
			rightByFingerprint[entry.fingerprint] = append(rightByFingerprint[entry.fingerprint], entry)
		}
		for _, leftEntry := range left {
			for _, rightEntry := range rightByFingerprint[leftEntry.fingerprint] {
				if !semanticJSONEqual(leftEntry.key, rightEntry.key) {
					continue
				}
				if !semanticJSONEqual(leftEntry.value, rightEntry.value) {
					return assertionFailure(assertion, "Fields for a matching key are not equal.", map[string]any{
						"key":         leftEntry.key,
						"left_index":  leftEntry.index,
						"right_index": rightEntry.index,
						"left_value":  leftEntry.value,
						"right_value": rightEntry.value,
					})
				}
				break
			}
		}
		return nil
	default:
		return preflightError(DiagnosticCodeInvalidAssertion, appendPointer(assertion.path, "type"), "Unsupported compiled assertion type.", map[string]any{"type": assertion.kind})
	}
}

func (e *AssertionEvaluator) source(name string, result any) any {
	if name == "result" {
		return result
	}
	return e.inputs[name]
}

func (e *AssertionEvaluator) projectFields(assertion compiledAssertion, operand compiledAssertionOperand, side string, result any) ([]keyedAssertionValue, error) {
	selected := operand.itemsPointer.selectValues(e.source(operand.source, result))
	if len(selected) != 1 {
		return nil, assertionFailure(assertion, "field_equal_by_key requires exactly one selected items array on each side.", map[string]any{
			"side":            side,
			"source":          operand.source,
			"items_pointer":   operand.itemsPointer.raw,
			"selection_count": len(selected),
		})
	}
	items, ok := selected[0].([]any)
	if !ok {
		return nil, assertionFailure(assertion, "field_equal_by_key items_pointer must select an array.", map[string]any{
			"side":          side,
			"source":        operand.source,
			"items_pointer": operand.itemsPointer.raw,
		})
	}

	entries := make([]keyedAssertionValue, 0, len(items))
	keysByValue := newSemanticValueIndex(len(items))
	for index, item := range items {
		keys := operand.keyPointer.selectValues(item)
		if len(keys) != 1 {
			return nil, assertionFailure(assertion, "field_equal_by_key requires one selected key per item.", map[string]any{
				"side":            side,
				"source":          operand.source,
				"item_index":      index,
				"key_pointer":     operand.keyPointer.raw,
				"selection_count": len(keys),
			})
		}
		if !isJSONScalar(keys[0]) {
			return nil, assertionFailure(assertion, "field_equal_by_key keys must be JSON scalars.", map[string]any{
				"side":        side,
				"source":      operand.source,
				"item_index":  index,
				"key_pointer": operand.keyPointer.raw,
			})
		}
		indexedKey, duplicate := keysByValue.add(keys[0], index)
		if duplicate {
			return nil, assertionFailure(assertion, "field_equal_by_key keys must be unique on each side.", map[string]any{
				"side":            side,
				"source":          operand.source,
				"first_index":     indexedKey.index,
				"duplicate_index": index,
				"key":             keys[0],
			})
		}
		values := operand.valuePointer.selectValues(item)
		if len(values) != 1 {
			return nil, assertionFailure(assertion, "field_equal_by_key requires one selected value per item.", map[string]any{
				"side":            side,
				"source":          operand.source,
				"item_index":      index,
				"value_pointer":   operand.valuePointer.raw,
				"selection_count": len(values),
			})
		}
		entries = append(entries, keyedAssertionValue{
			key:         keys[0],
			value:       values[0],
			index:       index,
			fingerprint: indexedKey.fingerprint,
		})
	}
	return entries, nil
}

func assertionFailure(assertion compiledAssertion, message string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	addAssertionOperandContext(assertion, details)
	details["assertion_type"] = assertion.kind
	diagnostic := contracts.NewDiagnostic(
		DiagnosticCodeAssertionFailed,
		contracts.DiagnosticPhaseAssertion,
		assertion.path,
		message,
		details,
	)
	return contracts.NewDiagnosticError(message, diagnostic)
}

func addAssertionOperandContext(assertion compiledAssertion, details map[string]any) {
	switch assertion.kind {
	case "unique":
		details["source"] = assertion.left.source
		details["pointer"] = assertion.left.pointer.raw
	case "set_equal", "value_equal":
		details["left_source"] = assertion.left.source
		details["left_pointer"] = assertion.left.pointer.raw
		details["right_source"] = assertion.right.source
		details["right_pointer"] = assertion.right.pointer.raw
	case "field_equal_by_key":
		details["left_source"] = assertion.left.source
		details["left_items_pointer"] = assertion.left.itemsPointer.raw
		details["left_key_pointer"] = assertion.left.keyPointer.raw
		details["left_value_pointer"] = assertion.left.valuePointer.raw
		details["right_source"] = assertion.right.source
		details["right_items_pointer"] = assertion.right.itemsPointer.raw
		details["right_key_pointer"] = assertion.right.keyPointer.raw
		details["right_value_pointer"] = assertion.right.valuePointer.raw
	}
}

func newSemanticValueIndex(capacity int) *semanticValueIndex {
	return &semanticValueIndex{
		entries: make([]semanticIndexedValue, 0, capacity),
		buckets: make(map[semanticFingerprint][]int, capacity),
	}
}

// add returns the first semantically equal entry when value is a duplicate.
// Fingerprints make the normal path linear while equality checks preserve
// correctness in the event of a hash collision.
func (i *semanticValueIndex) add(value any, index int) (semanticIndexedValue, bool) {
	entry := semanticIndexedValue{
		value:       value,
		index:       index,
		fingerprint: semanticJSONFingerprint(value),
	}
	for _, entryIndex := range i.buckets[entry.fingerprint] {
		existing := i.entries[entryIndex]
		if semanticJSONEqual(existing.value, value) {
			return existing, true
		}
	}
	i.buckets[entry.fingerprint] = append(i.buckets[entry.fingerprint], len(i.entries))
	i.entries = append(i.entries, entry)
	return entry, false
}

func (i *semanticValueIndex) contains(candidate semanticIndexedValue) bool {
	for _, entryIndex := range i.buckets[candidate.fingerprint] {
		if semanticJSONEqual(i.entries[entryIndex].value, candidate.value) {
			return true
		}
	}
	return false
}

func indexSemanticValues(values []any) *semanticValueIndex {
	index := newSemanticValueIndex(len(values))
	for ordinal, value := range values {
		index.add(value, ordinal)
	}
	return index
}

func firstSemanticSetDifference(left *semanticValueIndex, right *semanticValueIndex) (any, bool) {
	for _, candidate := range left.entries {
		if !right.contains(candidate) {
			return candidate.value, true
		}
	}
	return nil, false
}

func semanticJSONEqual(left any, right any) bool {
	if leftNumber, ok := assertionNumber(left); ok {
		rightNumber, rightOK := assertionNumber(right)
		return rightOK &&
			leftNumber.negative == rightNumber.negative &&
			leftNumber.coefficient == rightNumber.coefficient &&
			leftNumber.exponent.Cmp(rightNumber.exponent) == 0
	}
	switch typed := left.(type) {
	case nil:
		return right == nil
	case bool:
		other, ok := right.(bool)
		return ok && typed == other
	case string:
		other, ok := right.(string)
		return ok && typed == other
	case []any:
		other, ok := right.([]any)
		if !ok || len(typed) != len(other) {
			return false
		}
		for index := range typed {
			if !semanticJSONEqual(typed[index], other[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		other, ok := right.(map[string]any)
		if !ok || len(typed) != len(other) {
			return false
		}
		for key, value := range typed {
			otherValue, exists := other[key]
			if !exists || !semanticJSONEqual(value, otherValue) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func semanticJSONFingerprint(value any) semanticFingerprint {
	hasher := sha256.New()
	writeSemanticJSONFingerprint(hasher, value)
	var fingerprint semanticFingerprint
	copy(fingerprint[:], hasher.Sum(nil))
	return fingerprint
}

func writeSemanticJSONFingerprint(hasher hash.Hash, value any) {
	if number, ok := assertionNumber(value); ok {
		writeFingerprintByte(hasher, 'n')
		if number.negative {
			writeFingerprintByte(hasher, '-')
		} else {
			writeFingerprintByte(hasher, '+')
		}
		writeFingerprintText(hasher, number.coefficient)
		writeFingerprintText(hasher, number.exponent.String())
		return
	}

	switch typed := value.(type) {
	case nil:
		writeFingerprintByte(hasher, '0')
	case bool:
		if typed {
			writeFingerprintByte(hasher, 't')
		} else {
			writeFingerprintByte(hasher, 'f')
		}
	case string:
		writeFingerprintByte(hasher, 's')
		writeFingerprintText(hasher, typed)
	case []any:
		writeFingerprintByte(hasher, 'a')
		writeFingerprintSize(hasher, len(typed))
		for _, item := range typed {
			writeSemanticJSONFingerprint(hasher, item)
		}
	case map[string]any:
		writeFingerprintByte(hasher, 'o')
		writeFingerprintSize(hasher, len(typed))
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writeFingerprintText(hasher, key)
			writeSemanticJSONFingerprint(hasher, typed[key])
		}
	default:
		// Assertion values are validated before indexing, so this branch is a
		// defensive discriminator rather than a supported JSON representation.
		writeFingerprintByte(hasher, 'x')
		writeFingerprintText(hasher, fmt.Sprintf("%T", value))
	}
}

func writeFingerprintByte(hasher hash.Hash, value byte) {
	_, _ = hasher.Write([]byte{value})
}

func writeFingerprintSize(hasher hash.Hash, value int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = hasher.Write(encoded[:])
}

func writeFingerprintText(hasher hash.Hash, value string) {
	writeFingerprintSize(hasher, len(value))
	_, _ = hasher.Write([]byte(value))
}

func assertionNumber(value any) (normalizedAssertionNumber, bool) {
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = typed.String()
	case int:
		text = strconv.FormatInt(int64(typed), 10)
	case int8:
		text = strconv.FormatInt(int64(typed), 10)
	case int16:
		text = strconv.FormatInt(int64(typed), 10)
	case int32:
		text = strconv.FormatInt(int64(typed), 10)
	case int64:
		text = strconv.FormatInt(typed, 10)
	case uint:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint8:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint16:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint32:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint64:
		text = strconv.FormatUint(typed, 10)
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return normalizedAssertionNumber{}, false
		}
		text = strconv.FormatFloat(float64(typed), 'g', -1, 32)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return normalizedAssertionNumber{}, false
		}
		text = strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		return normalizedAssertionNumber{}, false
	}
	return normalizeAssertionNumber(text)
}

// normalizeAssertionNumber compares arbitrary JSON numbers without expanding
// powers of ten. This keeps mathematical equality exact even for very large
// exponents while using memory proportional to the source text.
func normalizeAssertionNumber(text string) (normalizedAssertionNumber, bool) {
	if text == "" {
		return normalizedAssertionNumber{}, false
	}
	offset := 0
	negative := false
	if text[offset] == '-' {
		negative = true
		offset++
		if offset == len(text) {
			return normalizedAssertionNumber{}, false
		}
	}

	integerStart := offset
	if text[offset] == '0' {
		offset++
		if offset < len(text) && text[offset] >= '0' && text[offset] <= '9' {
			return normalizedAssertionNumber{}, false
		}
	} else {
		if text[offset] < '1' || text[offset] > '9' {
			return normalizedAssertionNumber{}, false
		}
		for offset < len(text) && text[offset] >= '0' && text[offset] <= '9' {
			offset++
		}
	}
	integerDigits := text[integerStart:offset]

	fractionDigits := ""
	if offset < len(text) && text[offset] == '.' {
		offset++
		fractionStart := offset
		for offset < len(text) && text[offset] >= '0' && text[offset] <= '9' {
			offset++
		}
		if fractionStart == offset {
			return normalizedAssertionNumber{}, false
		}
		fractionDigits = text[fractionStart:offset]
	}

	exponent := new(big.Int)
	if offset < len(text) && (text[offset] == 'e' || text[offset] == 'E') {
		offset++
		exponentNegative := false
		if offset < len(text) && (text[offset] == '+' || text[offset] == '-') {
			exponentNegative = text[offset] == '-'
			offset++
		}
		exponentStart := offset
		for offset < len(text) && text[offset] >= '0' && text[offset] <= '9' {
			offset++
		}
		if exponentStart == offset {
			return normalizedAssertionNumber{}, false
		}
		if _, ok := exponent.SetString(text[exponentStart:offset], 10); !ok {
			return normalizedAssertionNumber{}, false
		}
		if exponentNegative {
			exponent.Neg(exponent)
		}
	}
	if offset != len(text) {
		return normalizedAssertionNumber{}, false
	}

	coefficient := strings.TrimLeft(integerDigits+fractionDigits, "0")
	if coefficient == "" {
		return normalizedAssertionNumber{coefficient: "0", exponent: new(big.Int)}, true
	}
	trailingZeros := len(coefficient) - len(strings.TrimRight(coefficient, "0"))
	coefficient = strings.TrimRight(coefficient, "0")
	exponent.Sub(exponent, big.NewInt(int64(len(fractionDigits))))
	exponent.Add(exponent, big.NewInt(int64(trailingZeros)))
	return normalizedAssertionNumber{negative: negative, coefficient: coefficient, exponent: exponent}, true
}

func isJSONScalar(value any) bool {
	if value == nil {
		return true
	}
	switch value.(type) {
	case bool, string:
		return true
	default:
		_, ok := assertionNumber(value)
		return ok
	}
}

func validateAssertionJSONValue(value any) error {
	switch typed := value.(type) {
	case nil, bool, string:
		return nil
	case json.Number:
		if _, ok := assertionNumber(typed); !ok {
			return fmt.Errorf("invalid JSON number")
		}
		return nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return nil
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return fmt.Errorf("non-finite JSON number")
		}
		return nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return fmt.Errorf("non-finite JSON number")
		}
		return nil
	case []any:
		for _, item := range typed {
			if err := validateAssertionJSONValue(item); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for _, item := range typed {
			if err := validateAssertionJSONValue(item); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported JSON value type %T", value)
	}
}
