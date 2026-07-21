package contracts

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestDecodeStrictJSONBytesAcceptsEveryRootKind(t *testing.T) {
	tests := []struct {
		name string
		data string
		want any
	}{
		{name: "object", data: ` {"name":"value"} `, want: map[string]any{"name": "value"}},
		{name: "array", data: `[true,null,"value"]`, want: []any{true, nil, "value"}},
		{name: "string", data: `"value"`, want: "value"},
		{name: "number", data: `123.4500`, want: json.Number("123.4500")},
		{name: "boolean", data: `false`, want: false},
		{name: "null", data: `null`, want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecodeStrictJSONBytes([]byte(test.data))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("decoded = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestDecodeStrictJSONBytesPreservesJSONNumbers(t *testing.T) {
	value, err := DecodeStrictJSONBytes([]byte(`{"integer":9007199254740993123456789,"decimal":1.2300e-40}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	object := value.(map[string]any)
	if got := object["integer"]; got != json.Number("9007199254740993123456789") {
		t.Fatalf("integer = %#v (%T)", got, got)
	}
	if got := object["decimal"]; got != json.Number("1.2300e-40") {
		t.Fatalf("decimal = %#v (%T)", got, got)
	}
}

func TestDecodeStrictJSONBytesRejectsInvalidUTF8(t *testing.T) {
	_, err := DecodeStrictJSONBytes([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})
	diagnostic := requireDiagnosticError(t, err, DiagnosticCodeInvalidUTF8)
	if diagnostic.Path != "" || diagnostic.Details["offset"] != 6 {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func TestDecodeStrictJSONBytesRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	tests := []struct {
		name string
		data string
		path string
	}{
		{name: "root", data: `{"name":1,"name":2}`, path: "/name"},
		{name: "nested object", data: `{"outer":{"name":1,"name":2}}`, path: "/outer/name"},
		{name: "object in array", data: `[{"name":1,"name":2}]`, path: "/0/name"},
		{name: "decoded key equality", data: `{"name":1,"na\u006de":2}`, path: "/name"},
		{name: "escaped pointer", data: `{"outer":{"a/b~c":1,"a/b~c":2}}`, path: "/outer/a~1b~0c"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeStrictJSONBytes([]byte(test.data))
			diagnostic := requireDiagnosticError(t, err, DiagnosticCodeDuplicateJSONKey)
			if diagnostic.Path != test.path {
				t.Fatalf("path = %q, want %q", diagnostic.Path, test.path)
			}
		})
	}
}

func TestDecodeStrictJSONBytesRejectsTrailingContent(t *testing.T) {
	tests := []string{
		`{} {}`,
		`[1] true`,
		`"value" suffix`,
		`null,`,
	}
	for _, data := range tests {
		t.Run(data, func(t *testing.T) {
			_, err := DecodeStrictJSONBytes([]byte(data))
			requireDiagnosticError(t, err, DiagnosticCodeTrailingJSON)
		})
	}
	if _, err := DecodeStrictJSONBytes([]byte("{} \t\r\n")); err != nil {
		t.Fatalf("trailing JSON whitespace rejected: %v", err)
	}
}

func TestDecodeStrictJSONBytesRejectsMalformedAndEmptyInput(t *testing.T) {
	for _, data := range []string{"", "   ", `{"missing":`, `[1,]`, `{]`} {
		t.Run(data, func(t *testing.T) {
			_, err := DecodeStrictJSONBytes([]byte(data))
			requireDiagnosticError(t, err, DiagnosticCodeInvalidJSON)
		})
	}
}

func TestDecodeStrictJSONObjectBytesRequiresObjectRoot(t *testing.T) {
	if _, err := DecodeStrictJSONObjectBytes([]byte(`{"ok":true}`)); err != nil {
		t.Fatalf("object rejected: %v", err)
	}
	_, err := DecodeStrictJSONObjectBytes([]byte(`[1,2]`))
	requireDiagnosticError(t, err, DiagnosticCodeInvalidJSONRoot)
}

func TestCompatibilityDecoderBehaviorIsUnchanged(t *testing.T) {
	value, err := DecodeJSONBytes([]byte(`{"name":"first","name":"last"} {"ignored":true}`))
	if err != nil {
		t.Fatalf("compatibility decoder: %v", err)
	}
	object := value.(map[string]any)
	if object["name"] != "last" {
		t.Fatalf("compatibility object = %#v", object)
	}
}

func requireDiagnosticError(t *testing.T, err error, code string) Diagnostic {
	t.Helper()
	if err == nil {
		t.Fatalf("expected diagnostic error %q", code)
	}
	var typed *DiagnosticError
	if !errors.As(err, &typed) {
		t.Fatalf("error type = %T, want *DiagnosticError: %v", err, err)
	}
	if len(typed.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v", typed.Diagnostics)
	}
	diagnostic := typed.Diagnostics[0]
	if diagnostic.Code != code {
		t.Fatalf("code = %q, want %q (error: %v)", diagnostic.Code, code, err)
	}
	if diagnostic.Phase != DiagnosticPhaseDecode {
		t.Fatalf("phase = %q", diagnostic.Phase)
	}
	return diagnostic
}
