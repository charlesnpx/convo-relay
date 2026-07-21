package contracts

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestDiagnosticShapeAndErrorWrapping(t *testing.T) {
	cause := errors.New("decoder detail")
	diagnostic := NewDiagnostic(
		"schema_mismatch",
		DiagnosticPhaseSchema,
		"/items/0",
		"Value does not match the schema.",
		map[string]any{"keyword": "type"},
	)
	err := WrapDiagnosticError(cause, "schema validation failed", diagnostic)

	if err.Error() != "schema validation failed" {
		t.Fatalf("error = %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("diagnostic error does not unwrap its cause")
	}
	want := map[string]any{
		"message": "schema validation failed",
		"diagnostics": []any{map[string]any{
			"code":    "schema_mismatch",
			"phase":   "schema",
			"path":    "/items/0",
			"message": "Value does not match the schema.",
			"details": map[string]any{"keyword": "type"},
		}},
	}
	if got := err.ToMap(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ToMap() = %#v, want %#v", got, want)
	}
}

func TestDiagnosticJSONAlwaysIncludesPathAndOmitsEmptyDetails(t *testing.T) {
	diagnostic := NewDiagnostic("blocked", DiagnosticPhasePolicy, "", "Operation is blocked.", nil)
	encoded, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatalf("marshal diagnostic: %v", err)
	}
	if string(encoded) != `{"code":"blocked","phase":"policy","path":"","message":"Operation is blocked."}` {
		t.Fatalf("diagnostic JSON = %s", encoded)
	}
}

func TestNewDiagnosticClonesDetails(t *testing.T) {
	details := map[string]any{"nested": map[string]any{"value": "original"}}
	diagnostic := NewDiagnostic("code", DiagnosticPhasePreflight, "", "message", details)
	details["nested"].(map[string]any)["value"] = "changed"
	if diagnostic.Details["nested"].(map[string]any)["value"] != "original" {
		t.Fatalf("diagnostic details changed with caller map: %#v", diagnostic.Details)
	}
}
