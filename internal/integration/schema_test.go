package integration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestCompileSchemaSupportsLockedDraft2020Subset(t *testing.T) {
	schema := decodeJSONValue(t, `{
      "$defs": {
        "token": {"type": "string", "minLength": 2, "maxLength": 5}
      },
      "type": "object",
      "required": ["kind", "items", "score", "choice", "branch", "all", "conditional"],
      "properties": {
        "kind": {"const": "ok"},
        "items": {
          "type": "array",
          "minItems": 1,
          "maxItems": 2,
          "items": {"$ref": "#/$defs/token"}
        },
        "score": {
          "type": "number",
          "minimum": 0,
          "maximum": 10,
          "exclusiveMinimum": -1,
          "exclusiveMaximum": 11
        },
        "choice": {"enum": ["a", "b"]},
        "branch": {"oneOf": [{"type": "string"}, {"type": "integer"}]},
        "all": {"allOf": [{"type": "number", "minimum": 1}, {"maximum": 2}]},
        "conditional": {
          "if": {"type": "string"},
          "then": {"minLength": 1},
          "else": {"type": "number"}
        }
      },
      "additionalProperties": false
    }`)
	compiled, err := CompileSchema(schema, "/schema")
	if err != nil {
		t.Fatalf("compile supported schema: %v", err)
	}
	valid := decodeJSONValue(t, `{
      "kind":"ok",
      "items":["ab","token"],
      "score":5.5,
      "choice":"a",
      "branch":3,
      "all":1.5,
      "conditional":"present"
    }`)
	if err := compiled.Validate(valid); err != nil {
		t.Fatalf("valid instance rejected: %v", err)
	}

	invalid := []struct {
		name  string
		value string
		path  string
	}{
		{name: "required", value: `{"kind":"ok"}`, path: ""},
		{name: "const", value: `{"kind":"wrong","items":["ab"],"score":5,"choice":"a","branch":3,"all":1.5,"conditional":"x"}`, path: "/kind"},
		{name: "item length", value: `{"kind":"ok","items":["x"],"score":5,"choice":"a","branch":3,"all":1.5,"conditional":"x"}`, path: "/items/0"},
		{name: "numeric bound", value: `{"kind":"ok","items":["ab"],"score":11,"choice":"a","branch":3,"all":1.5,"conditional":"x"}`, path: "/score"},
		{name: "enum", value: `{"kind":"ok","items":["ab"],"score":5,"choice":"c","branch":3,"all":1.5,"conditional":"x"}`, path: "/choice"},
		{name: "one of", value: `{"kind":"ok","items":["ab"],"score":5,"choice":"a","branch":true,"all":1.5,"conditional":"x"}`, path: "/branch"},
		{name: "all of", value: `{"kind":"ok","items":["ab"],"score":5,"choice":"a","branch":3,"all":3,"conditional":"x"}`, path: "/all"},
		{name: "conditional", value: `{"kind":"ok","items":["ab"],"score":5,"choice":"a","branch":3,"all":1.5,"conditional":""}`, path: "/conditional"},
		{name: "additional property", value: `{"kind":"ok","items":["ab"],"score":5,"choice":"a","branch":3,"all":1.5,"conditional":"x","extra":true}`, path: ""},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			err := compiled.Validate(decodeJSONValue(t, test.value))
			diagnostic := assertDiagnostic(t, err, DiagnosticCodeSchemaMismatch, contracts.DiagnosticPhaseSchema)
			if test.path != "" && diagnostic.Path != test.path {
				t.Fatalf("path = %q, want %q", diagnostic.Path, test.path)
			}
		})
	}
}

func TestCompileSchemaSupportsAdditionalPropertiesSubschema(t *testing.T) {
	compiled, err := CompileSchema(decodeJSONValue(t, `{
      "type":"object",
      "properties":{"fixed":{"type":"string"}},
      "additionalProperties":{"type":"integer","minimum":1}
    }`), "/schema")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := compiled.Validate(decodeJSONValue(t, `{"fixed":"ok","count":2}`)); err != nil {
		t.Fatalf("valid additional property rejected: %v", err)
	}
	assertDiagnostic(t, compiled.Validate(decodeJSONValue(t, `{"fixed":"ok","count":0}`)), DiagnosticCodeSchemaMismatch, contracts.DiagnosticPhaseSchema)
}

func TestSchemaWhitelistRejectsUnknownKeywordsAtEverySchemaPosition(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		path   string
	}{
		{name: "root", schema: `{"format":"email"}`, path: "/schema/format"},
		{name: "property schema", schema: `{"properties":{"value":{"pattern":"x"}}}`, path: "/schema/properties/value/pattern"},
		{name: "items schema", schema: `{"items":{"contains":{"type":"string"}}}`, path: "/schema/items/contains"},
		{name: "additional properties schema", schema: `{"additionalProperties":{"description":"unsupported"}}`, path: "/schema/additionalProperties/description"},
		{name: "one of schema", schema: `{"oneOf":[{"title":"unsupported"}]}`, path: "/schema/oneOf/0/title"},
		{name: "conditional schema", schema: `{"if":{"default":true}}`, path: "/schema/if/default"},
		{name: "id", schema: `{"$id":"https://example.invalid/schema"}`, path: "/schema/$id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompileSchema(decodeJSONValue(t, test.schema), "/schema")
			diagnostic := assertDiagnostic(t, err, DiagnosticCodeUnsupportedSchema, contracts.DiagnosticPhasePreflight)
			if diagnostic.Path != test.path {
				t.Fatalf("path = %q, want %q", diagnostic.Path, test.path)
			}
		})
	}
}

func TestPropertyAndDefinitionNamesAreDataNotSchemaKeywords(t *testing.T) {
	schema := decodeJSONValue(t, `{
      "$defs": {
        "format": {"type":"string"},
        "a/b~c": {"type":"integer"}
      },
      "type":"object",
      "properties": {
        "$id": {"$ref":"#/$defs/format"},
        "unknownKeyword": {"$ref":"#/$defs/a~1b~0c"}
      },
      "additionalProperties": false
    }`)
	compiled, err := CompileSchema(schema, "/schema")
	if err != nil {
		t.Fatalf("data names rejected: %v", err)
	}
	if err := compiled.Validate(decodeJSONValue(t, `{"$id":"data","unknownKeyword":2}`)); err != nil {
		t.Fatalf("valid data names rejected: %v", err)
	}
}

func TestStandaloneBooleanSchemasAreRejectedExceptAdditionalProperties(t *testing.T) {
	if _, err := CompileSchema(true, "/schema"); err == nil {
		t.Fatal("standalone root boolean schema accepted")
	} else {
		assertDiagnostic(t, err, DiagnosticCodeInvalidSchema, contracts.DiagnosticPhasePreflight)
	}
	tests := []struct {
		name   string
		schema string
		valid  bool
	}{
		{name: "property", schema: `{"properties":{"value":true}}`},
		{name: "items", schema: `{"items":false}`},
		{name: "definition", schema: `{"$defs":{"value":true}}`},
		{name: "one of", schema: `{"oneOf":[true,{"type":"string"}]}`},
		{name: "if", schema: `{"if":true}`},
		{name: "additional properties true", schema: `{"type":"object","additionalProperties":true}`, valid: true},
		{name: "additional properties false", schema: `{"type":"object","additionalProperties":false}`, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompileSchema(decodeJSONValue(t, test.schema), "/schema")
			if test.valid {
				if err != nil {
					t.Fatalf("allowed additionalProperties boolean rejected: %v", err)
				}
				return
			}
			assertDiagnostic(t, err, DiagnosticCodeInvalidSchema, contracts.DiagnosticPhasePreflight)
		})
	}
}

func TestSchemaReferencesMustBeResolvedLocalDefinitions(t *testing.T) {
	valid := []string{
		`{"$defs":{"value":{"type":"string"}},"$ref":"#/$defs/value"}`,
		`{"$defs":{"a/b~c":{"type":"integer"}},"$ref":"#/$defs/a~1b~0c"}`,
		`{"$defs":{"a b":{"type":"integer"}},"$ref":"#/$defs/a%20b"}`,
		`{"$defs":{"outer":{"$defs":{"inner":{"type":"boolean"}},"$ref":"#/$defs/outer/$defs/inner"}},"$ref":"#/$defs/outer"}`,
	}
	for index, raw := range valid {
		if _, err := CompileSchema(decodeJSONValue(t, raw), "/schema"); err != nil {
			t.Fatalf("valid ref %d rejected: %v", index, err)
		}
	}

	invalid := []struct {
		name string
		ref  string
	}{
		{name: "remote", ref: "https://example.invalid/schema.json"},
		{name: "relative", ref: "other.json#/$defs/value"},
		{name: "root fragment", ref: "#/properties/value"},
		{name: "unresolved", ref: "#/$defs/missing"},
		{name: "bad escape", ref: "#/$defs/a~2b"},
		{name: "bad percent escape", ref: "#/$defs/a%2"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			schema := map[string]any{
				"$defs": map[string]any{"value": map[string]any{"type": "string"}},
				"$ref":  test.ref,
			}
			_, err := CompileSchema(schema, "/schema")
			assertDiagnostic(t, err, DiagnosticCodeInvalidSchemaReference, contracts.DiagnosticPhasePreflight)
		})
	}
}

func TestSchemaReferencesCannotTargetDataObjects(t *testing.T) {
	tests := []struct {
		name   string
		schema string
	}{
		{
			name: "const object",
			schema: `{
              "$defs":{"holder":{"const":{"type":"string","pattern":"^allowed$"}}},
              "$ref":"#/$defs/holder/const"
            }`,
		},
		{
			name: "enum object",
			schema: `{
              "$defs":{"holder":{"enum":[{"type":"string","pattern":"^allowed$"}]}},
				"$ref":"#/$defs/holder/enum/0"
			}`,
		},
		{
			name: "percent encoded separator",
			schema: `{
              "$defs":{
                "holder":{"const":{"type":"string","pattern":"^allowed$"}},
                "holder%2Fconst":{"type":"integer"}
              },
              "$ref":"#/$defs/holder%2Fconst"
            }`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompileSchema(decodeJSONValue(t, test.schema), "/schema")
			diagnostic := assertDiagnostic(t, err, DiagnosticCodeInvalidSchemaReference, contracts.DiagnosticPhasePreflight)
			if diagnostic.Path != "/schema/$ref" {
				t.Fatalf("path = %q, want /schema/$ref", diagnostic.Path)
			}
		})
	}
}

func TestSchemaKeywordShapesAndBoundsAreValidated(t *testing.T) {
	tests := []string{
		`{"type":[]}`,
		`{"type":["string","string"]}`,
		`{"type":"date"}`,
		`{"required":"value"}`,
		`{"required":["value","value"]}`,
		`{"properties":[]}`,
		`{"enum":[]}`,
		`{"minLength":-1}`,
		`{"maxItems":1.5}`,
		`{"minimum":"zero"}`,
		`{"additionalProperties":1}`,
		`{"allOf":[]}`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			_, err := CompileSchema(decodeJSONValue(t, raw), "/schema")
			assertDiagnostic(t, err, DiagnosticCodeInvalidSchema, contracts.DiagnosticPhasePreflight)
		})
	}
}

func TestCompiledSchemaOwnsImmutableDocumentCopy(t *testing.T) {
	original := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{"type": "string"},
		},
	}
	compiled, err := CompileSchema(original, "/schema")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	original["type"] = "array"
	document := compiled.Document()
	document["type"] = "boolean"
	if got := compiled.Document()["type"]; got != "object" {
		t.Fatalf("compiled document mutated through caller: %#v", got)
	}
	if err := compiled.Validate(map[string]any{"value": "ok"}); err != nil {
		t.Fatalf("compiled validator changed with caller maps: %v", err)
	}
	if reflect.DeepEqual(original, compiled.Document()) {
		t.Fatal("compiled schema retained mutable input map")
	}
}

func decodeJSONValue(t *testing.T, raw string) any {
	t.Helper()
	value, err := contracts.DecodeStrictJSONBytes([]byte(raw))
	if err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return value
}

func TestSchemaNumberBoundsRetainJSONNumberPrecision(t *testing.T) {
	compiled, err := CompileSchema(map[string]any{
		"type":    "number",
		"minimum": json.Number("9007199254740993123456789"),
	}, "/schema")
	if err != nil {
		t.Fatalf("compile precise bound: %v", err)
	}
	if err := compiled.Validate(json.Number("9007199254740993123456789")); err != nil {
		t.Fatalf("exact large number rejected: %v", err)
	}
	assertDiagnostic(t, compiled.Validate(json.Number("9007199254740993123456788")), DiagnosticCodeSchemaMismatch, contracts.DiagnosticPhaseSchema)
}
