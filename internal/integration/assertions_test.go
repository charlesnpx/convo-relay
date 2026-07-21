package integration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestPointerPatternsSelectDeterministically(t *testing.T) {
	document := decodeJSONValue(t, `{
      "a/b":{"~name":"escaped"},
      "array":[{"id":"first"},{"id":"second"}],
      "object":{"z":{"id":3},"a":{"id":1},"m":{"id":2}},
      "groups":[{"values":{"b":2,"a":1}},{"values":{"d":4,"c":3}}],
      "literal*":{"value":true}
    }`)
	tests := []struct {
		name    string
		pointer string
		want    []any
	}{
		{name: "root", pointer: "", want: []any{document}},
		{name: "escaped segments", pointer: "/a~1b/~0name", want: []any{"escaped"}},
		{name: "array index", pointer: "/array/1/id", want: []any{"second"}},
		{name: "array wildcard", pointer: "/array/*/id", want: []any{"first", "second"}},
		{name: "object wildcard lexical", pointer: "/object/*/id", want: []any{json.Number("1"), json.Number("2"), json.Number("3")}},
		{name: "nested wildcard order", pointer: "/groups/*/values/*", want: []any{json.Number("1"), json.Number("2"), json.Number("3"), json.Number("4")}},
		{name: "partial star is literal", pointer: "/literal*/value", want: []any{true}},
		{name: "missing property", pointer: "/missing/value", want: []any{}},
		{name: "wildcard on scalar", pointer: "/array/0/id/*", want: []any{}},
		{name: "leading-zero array index", pointer: "/array/01", want: []any{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pattern, err := compilePointerPattern(test.pointer, "/pointer")
			if err != nil {
				t.Fatalf("compile pointer: %v", err)
			}
			if got := pattern.selectValues(document); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("selection = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestPointerPatternsRejectInvalidRFC6901Syntax(t *testing.T) {
	for _, pointer := range []string{"relative", "/incomplete~", "/invalid~2escape"} {
		t.Run(pointer, func(t *testing.T) {
			_, err := compilePointerPattern(pointer, "/pointer")
			diagnostic := assertDiagnostic(t, err, DiagnosticCodeInvalidAssertion, contracts.DiagnosticPhasePreflight)
			if diagnostic.Path != "/pointer" {
				t.Fatalf("path = %q", diagnostic.Path)
			}
		})
	}
}

func TestSemanticJSONEquality(t *testing.T) {
	tests := []struct {
		name  string
		left  any
		right any
		want  bool
	}{
		{name: "mathematical numbers", left: json.Number("1e2"), right: json.Number("100.0"), want: true},
		{name: "very large positive exponent", left: json.Number("1e1000000000"), right: json.Number("10e999999999"), want: true},
		{name: "very large negative exponent", left: json.Number("1e-1000000000"), right: json.Number("0.1e-999999999"), want: true},
		{name: "integer and float", left: 1, right: 1.0, want: true},
		{name: "object order", left: map[string]any{"a": json.Number("1"), "b": true}, right: map[string]any{"b": true, "a": json.Number("1.0")}, want: true},
		{name: "array order equal", left: []any{json.Number("1"), "x"}, right: []any{json.Number("1.0"), "x"}, want: true},
		{name: "array order differs", left: []any{1, 2}, right: []any{2, 1}, want: false},
		{name: "different object keys", left: map[string]any{"a": 1}, right: map[string]any{"b": 1}, want: false},
		{name: "number and string", left: json.Number("1"), right: "1", want: false},
		{name: "null", left: nil, right: nil, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := semanticJSONEqual(test.left, test.right); got != test.want {
				t.Fatalf("equal = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSemanticValueIndexUsesNormalizedFingerprintsAndEqualityFallback(t *testing.T) {
	index := newSemanticValueIndex(3)
	first := map[string]any{"a": json.Number("1"), "b": []any{true, "x"}}
	equivalent := map[string]any{"b": []any{true, "x"}, "a": json.Number("1.0")}
	different := map[string]any{"a": json.Number("2"), "b": []any{true, "x"}}

	inserted, duplicate := index.add(first, 7)
	if duplicate || inserted.index != 7 {
		t.Fatalf("first insertion = %#v, duplicate=%v", inserted, duplicate)
	}
	existing, duplicate := index.add(equivalent, 9)
	if !duplicate || existing.index != 7 {
		t.Fatalf("semantic duplicate = %#v, duplicate=%v", existing, duplicate)
	}
	if _, duplicate := index.add(different, 11); duplicate {
		t.Fatal("different semantic value was treated as a duplicate")
	}
}

func TestAssertionPreflightValidatesPointersAndSources(t *testing.T) {
	tests := []struct {
		name      string
		assertion map[string]any
		mutate    func(map[string]any)
		inputs    map[string][]any
		wantPath  string
		selection bool
	}{
		{
			name:      "invalid pointer",
			assertion: map[string]any{"type": "unique", "source": "result", "pointer": "items/*"},
			wantPath:  "/contracts/neutral~1contract-v1/result/assertions/0/pointer",
			selection: true,
		},
		{
			name:      "invalid source syntax",
			assertion: map[string]any{"type": "unique", "source": "output", "pointer": ""},
			wantPath:  "/contracts/neutral~1contract-v1/result/assertions/0/source",
			selection: true,
		},
		{
			name:      "undeclared input",
			assertion: map[string]any{"type": "unique", "source": "input:missing", "pointer": ""},
			wantPath:  "/contracts/neutral~1contract-v1/result/assertions/0/source",
			selection: true,
		},
		{
			name:      "non JSON input",
			assertion: map[string]any{"type": "unique", "source": "input:payload", "pointer": ""},
			inputs:    map[string][]any{"payload": {map[string]any{}}},
			wantPath:  "/contracts/neutral~1contract-v1/result/assertions/0/source",
			selection: true,
		},
		{
			name:      "missing one value",
			assertion: map[string]any{"type": "unique", "source": "input:payload", "pointer": ""},
			mutate: func(bundle map[string]any) {
				firstInput(bundle)["media_type"] = "application/json"
			},
			wantPath: "/contracts/neutral~1contract-v1/result/assertions/0/source",
		},
		{
			name:      "multiple one values",
			assertion: map[string]any{"type": "unique", "source": "input:payload", "pointer": ""},
			mutate: func(bundle map[string]any) {
				firstInput(bundle)["media_type"] = "application/json"
			},
			inputs:   map[string][]any{"payload": {json.Number("1"), json.Number("2")}},
			wantPath: "/contracts/neutral~1contract-v1/result/assertions/0/source",
		},
		{
			name:      "invalid JSON input value",
			assertion: map[string]any{"type": "unique", "source": "input:payload", "pointer": ""},
			mutate: func(bundle map[string]any) {
				firstInput(bundle)["media_type"] = "application/json"
			},
			inputs:   map[string][]any{"payload": {json.Number("1/2")}},
			wantPath: "/contracts/neutral~1contract-v1/result/assertions/0/source",
		},
		{
			name:      "required many empty",
			assertion: map[string]any{"type": "unique", "source": "input:payload", "pointer": "/*"},
			mutate: func(bundle map[string]any) {
				input := firstInput(bundle)
				input["media_type"] = "application/json"
				input["cardinality"] = "many"
				input["required"] = true
			},
			wantPath: "/contracts/neutral~1contract-v1/result/assertions/0/source",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, err := selectWithAssertions(t, []any{test.assertion}, test.mutate)
			if !test.selection && err == nil {
				_, err = PrepareAssertionEvaluator(selected, test.inputs)
			}
			diagnostic := assertDiagnostic(t, err, DiagnosticCodeInvalidAssertion, contracts.DiagnosticPhasePreflight)
			if diagnostic.Path != test.wantPath {
				t.Fatalf("path = %q, want %q", diagnostic.Path, test.wantPath)
			}
		})
	}
}

func TestManyValuedJSONInputIsAnImmutableOrderedVirtualArray(t *testing.T) {
	assertions := []any{
		map[string]any{
			"type":  "value_equal",
			"left":  map[string]any{"source": "input:payload", "pointer": "/0/id"},
			"right": map[string]any{"source": "result", "pointer": "/first"},
		},
		map[string]any{
			"type":  "value_equal",
			"left":  map[string]any{"source": "input:payload", "pointer": "/1/id"},
			"right": map[string]any{"source": "result", "pointer": "/second"},
		},
	}
	selected := selectedWithAssertions(t, assertions, func(bundle map[string]any) {
		input := firstInput(bundle)
		input["cardinality"] = "many"
		input["media_type"] = "application/vnd.neutral+json; charset=utf-8"
	})
	inputs := map[string][]any{"payload": {
		map[string]any{"id": "first"},
		map[string]any{"id": "second"},
	}}
	evaluator, err := PrepareAssertionEvaluator(selected, inputs)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	inputs["payload"][0].(map[string]any)["id"] = "mutated"
	if err := evaluator.Validate(map[string]any{"first": "first", "second": "second"}); err != nil {
		t.Fatalf("ordered virtual array rejected: %v", err)
	}
	assertDiagnostic(
		t,
		evaluator.Validate(map[string]any{"first": "second", "second": "first"}),
		DiagnosticCodeAssertionFailed,
		contracts.DiagnosticPhaseAssertion,
	)
}

func TestOptionalManyInputMayBindAsAnEmptyArray(t *testing.T) {
	selected := selectedWithAssertions(t, []any{
		map[string]any{"type": "unique", "source": "input:payload", "pointer": "/*"},
	}, func(bundle map[string]any) {
		input := firstInput(bundle)
		input["cardinality"] = "many"
		input["media_type"] = "application/json"
	})
	evaluator, err := PrepareAssertionEvaluator(selected, nil)
	if err != nil {
		t.Fatalf("prepare empty many: %v", err)
	}
	if err := evaluator.Validate(map[string]any{}); err != nil {
		t.Fatalf("empty many assertion: %v", err)
	}
}

func TestUniqueUsesSemanticJSONEquality(t *testing.T) {
	selected := selectedWithAssertions(t, []any{
		map[string]any{"type": "unique", "source": "result", "pointer": "/items/*"},
	}, nil)
	evaluator, err := PrepareAssertionEvaluator(selected, nil)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, result := range []any{
		map[string]any{},
		map[string]any{"items": []any{json.Number("1")}},
		map[string]any{"items": []any{[]any{1, 2}, []any{2, 1}}},
	} {
		if err := evaluator.Validate(result); err != nil {
			t.Fatalf("unique value rejected: %#v: %v", result, err)
		}
	}
	failures := []any{
		map[string]any{"items": []any{json.Number("1"), json.Number("1.0")}},
		map[string]any{"items": []any{map[string]any{"a": 1, "b": 2}, map[string]any{"b": 2.0, "a": 1.0}}},
	}
	for _, result := range failures {
		assertDiagnostic(t, evaluator.Validate(result), DiagnosticCodeAssertionFailed, contracts.DiagnosticPhaseAssertion)
	}
}

func TestSetEqualIgnoresOrderingAndDuplicates(t *testing.T) {
	selected := selectedWithAssertions(t, []any{
		map[string]any{
			"type":  "set_equal",
			"left":  map[string]any{"source": "result", "pointer": "/left/*"},
			"right": map[string]any{"source": "result", "pointer": "/right/*"},
		},
	}, nil)
	evaluator, err := PrepareAssertionEvaluator(selected, nil)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := evaluator.Validate(map[string]any{
		"left":  []any{json.Number("1"), "x", json.Number("1.0")},
		"right": []any{"x", json.Number("1e0")},
	}); err != nil {
		t.Fatalf("equal sets rejected: %v", err)
	}
	assertDiagnostic(t, evaluator.Validate(map[string]any{
		"left":  []any{1, 2},
		"right": []any{1, 3},
	}), DiagnosticCodeAssertionFailed, contracts.DiagnosticPhaseAssertion)
}

func TestValueEqualRequiresOneSemanticMatchPerSide(t *testing.T) {
	selected := selectedWithAssertions(t, []any{
		map[string]any{
			"type":  "value_equal",
			"left":  map[string]any{"source": "result", "pointer": "/left/*"},
			"right": map[string]any{"source": "result", "pointer": "/right"},
		},
	}, nil)
	evaluator, err := PrepareAssertionEvaluator(selected, nil)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := evaluator.Validate(map[string]any{"left": []any{json.Number("2.0")}, "right": json.Number("2e0")}); err != nil {
		t.Fatalf("equal values rejected: %v", err)
	}
	for _, result := range []any{
		map[string]any{"left": []any{}, "right": 1},
		map[string]any{"left": []any{1, 1}, "right": 1},
		map[string]any{"left": []any{1}, "right": 2},
	} {
		assertDiagnostic(t, evaluator.Validate(result), DiagnosticCodeAssertionFailed, contracts.DiagnosticPhaseAssertion)
	}
}

func TestFieldEqualByKeyComparesOnlyMatchingUniqueScalarKeys(t *testing.T) {
	selected := selectedWithAssertions(t, []any{fieldEqualityAssertion("/left", "/id", "/value", "/right", "/id", "/value")}, nil)
	evaluator, err := PrepareAssertionEvaluator(selected, nil)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result := map[string]any{
		"left": []any{
			map[string]any{"id": json.Number("1"), "value": map[string]any{"a": 1, "b": []any{"x", "y"}}},
			map[string]any{"id": "left-only", "value": false},
		},
		"right": []any{
			map[string]any{"id": "right-only", "value": true},
			map[string]any{"id": json.Number("1.0"), "value": map[string]any{"b": []any{"x", "y"}, "a": 1.0}},
		},
	}
	if err := evaluator.Validate(result); err != nil {
		t.Fatalf("matching fields rejected: %v", err)
	}
}

func TestFieldEqualByKeyRejectsInvalidProjectionsAndMismatches(t *testing.T) {
	tests := []struct {
		name      string
		assertion map[string]any
		result    any
	}{
		{
			name:      "items not array",
			assertion: fieldEqualityAssertion("/left", "/id", "/value", "/right", "/id", "/value"),
			result:    map[string]any{"left": map[string]any{}, "right": []any{}},
		},
		{
			name:      "items selection ambiguous",
			assertion: fieldEqualityAssertion("/left/*", "/id", "/value", "/right", "/id", "/value"),
			result: map[string]any{
				"left":  []any{[]any{}, []any{}},
				"right": []any{},
			},
		},
		{
			name:      "duplicate semantic key",
			assertion: fieldEqualityAssertion("/left", "/id", "/value", "/right", "/id", "/value"),
			result: map[string]any{
				"left": []any{
					map[string]any{"id": json.Number("1"), "value": "a"},
					map[string]any{"id": json.Number("1.0"), "value": "b"},
				},
				"right": []any{},
			},
		},
		{
			name:      "key not scalar",
			assertion: fieldEqualityAssertion("/left", "/id", "/value", "/right", "/id", "/value"),
			result: map[string]any{
				"left":  []any{map[string]any{"id": map[string]any{"nested": true}, "value": "a"}},
				"right": []any{},
			},
		},
		{
			name:      "missing key",
			assertion: fieldEqualityAssertion("/left", "/id", "/value", "/right", "/id", "/value"),
			result: map[string]any{
				"left":  []any{map[string]any{"value": "a"}},
				"right": []any{},
			},
		},
		{
			name:      "ambiguous key",
			assertion: fieldEqualityAssertion("/left", "/ids/*", "/value", "/right", "/id", "/value"),
			result: map[string]any{
				"left":  []any{map[string]any{"ids": []any{1, 2}, "value": "a"}},
				"right": []any{},
			},
		},
		{
			name:      "missing value",
			assertion: fieldEqualityAssertion("/left", "/id", "/value", "/right", "/id", "/value"),
			result: map[string]any{
				"left":  []any{map[string]any{"id": "a"}},
				"right": []any{},
			},
		},
		{
			name:      "matching value differs",
			assertion: fieldEqualityAssertion("/left", "/id", "/value", "/right", "/id", "/value"),
			result: map[string]any{
				"left":  []any{map[string]any{"id": "a", "value": 1}},
				"right": []any{map[string]any{"id": "a", "value": 2}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected := selectedWithAssertions(t, []any{test.assertion}, nil)
			evaluator, err := PrepareAssertionEvaluator(selected, nil)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			assertDiagnostic(t, evaluator.Validate(test.result), DiagnosticCodeAssertionFailed, contracts.DiagnosticPhaseAssertion)
		})
	}
}

func TestAssertionDiagnosticsIncludeCompleteOperandContext(t *testing.T) {
	tests := []struct {
		name       string
		assertion  map[string]any
		result     any
		wantDetail map[string]any
	}{
		{
			name: "set equal",
			assertion: map[string]any{
				"type":  "set_equal",
				"left":  map[string]any{"source": "result", "pointer": "/left/*"},
				"right": map[string]any{"source": "result", "pointer": "/right/*"},
			},
			result: map[string]any{"left": []any{1}, "right": []any{2}},
			wantDetail: map[string]any{
				"left_source": "result", "left_pointer": "/left/*",
				"right_source": "result", "right_pointer": "/right/*",
			},
		},
		{
			name: "value equal",
			assertion: map[string]any{
				"type":  "value_equal",
				"left":  map[string]any{"source": "result", "pointer": "/left"},
				"right": map[string]any{"source": "result", "pointer": "/right"},
			},
			result: map[string]any{"left": 1, "right": 2},
			wantDetail: map[string]any{
				"left_source": "result", "left_pointer": "/left",
				"right_source": "result", "right_pointer": "/right",
			},
		},
		{
			name:      "field equal by key",
			assertion: fieldEqualityAssertion("/left", "/id", "/value", "/right", "/key", "/field"),
			result: map[string]any{
				"left":  []any{map[string]any{"id": "a", "value": 1}},
				"right": []any{map[string]any{"key": "a", "field": 2}},
			},
			wantDetail: map[string]any{
				"left_source": "result", "left_items_pointer": "/left", "left_key_pointer": "/id", "left_value_pointer": "/value",
				"right_source": "result", "right_items_pointer": "/right", "right_key_pointer": "/key", "right_value_pointer": "/field",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected := selectedWithAssertions(t, []any{test.assertion}, nil)
			evaluator, err := PrepareAssertionEvaluator(selected, nil)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			diagnostic := assertDiagnostic(t, evaluator.Validate(test.result), DiagnosticCodeAssertionFailed, contracts.DiagnosticPhaseAssertion)
			for key, want := range test.wantDetail {
				if got := diagnostic.Details[key]; !reflect.DeepEqual(got, want) {
					t.Errorf("details[%q] = %#v, want %#v; details=%#v", key, got, want, diagnostic.Details)
				}
			}
		})
	}
}

func TestAssertionEvaluatorRejectsNonJSONRuntimeValues(t *testing.T) {
	selected := selectedWithAssertions(t, []any{
		map[string]any{"type": "unique", "source": "result", "pointer": ""},
	}, nil)
	evaluator, err := PrepareAssertionEvaluator(selected, nil)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	assertDiagnostic(t, evaluator.Validate(make(chan int)), DiagnosticCodeAssertionFailed, contracts.DiagnosticPhaseAssertion)
}

func fieldEqualityAssertion(leftItems string, leftKey string, leftValue string, rightItems string, rightKey string, rightValue string) map[string]any {
	return map[string]any{
		"type": "field_equal_by_key",
		"left": map[string]any{
			"source":        "result",
			"items_pointer": leftItems,
			"key_pointer":   leftKey,
			"value_pointer": leftValue,
		},
		"right": map[string]any{
			"source":        "result",
			"items_pointer": rightItems,
			"key_pointer":   rightKey,
			"value_pointer": rightValue,
		},
	}
}

func selectedWithAssertions(t *testing.T, assertions []any, mutate func(map[string]any)) *SelectedContract {
	t.Helper()
	selected, err := selectWithAssertions(t, assertions, mutate)
	if err != nil {
		t.Fatalf("select assertion contract: %v", err)
	}
	return selected
}

func selectWithAssertions(t *testing.T, assertions []any, mutate func(map[string]any)) (*SelectedContract, error) {
	t.Helper()
	bundleObject := validBundleObject(t)
	firstResult(bundleObject)["assertions"] = assertions
	if mutate != nil {
		mutate(bundleObject)
	}
	bundle, err := DecodeBundleBytes(encodeJSON(t, bundleObject))
	if err != nil {
		t.Fatalf("decode assertion bundle: %v", err)
	}
	turns, _ := AlternatingSchedule(2)
	selected, err := SelectContract(bundle, "neutral/contract-v1", ScheduleRequirement{Turns: turns, ResultSource: ResultSourceReducer})
	return selected, err
}
