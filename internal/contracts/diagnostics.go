package contracts

import "strings"

const (
	DiagnosticPhaseDecode           = "decode"
	DiagnosticPhasePreflight        = "preflight"
	DiagnosticPhasePolicy           = "policy"
	DiagnosticPhaseSchema           = "schema"
	DiagnosticPhaseAssertion        = "assertion"
	DiagnosticPhaseResultValidation = "result_validation"
)

// Diagnostic is the stable, domain-neutral description of a contract failure.
// Path is always serialized and uses an RFC 6901 JSON Pointer when the failure
// belongs to a JSON document. The empty path identifies the document root.
type Diagnostic struct {
	Code    string         `json:"code"`
	Phase   string         `json:"phase"`
	Path    string         `json:"path"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func NewDiagnostic(code string, phase string, path string, message string, details map[string]any) Diagnostic {
	return Diagnostic{
		Code:    strings.TrimSpace(code),
		Phase:   strings.TrimSpace(phase),
		Path:    path,
		Message: strings.TrimSpace(message),
		Details: cloneDiagnosticDetails(details),
	}
}

func (d Diagnostic) ToMap() map[string]any {
	result := map[string]any{
		"code":    d.Code,
		"phase":   d.Phase,
		"path":    d.Path,
		"message": d.Message,
	}
	if len(d.Details) > 0 {
		result["details"] = cloneDiagnosticDetails(d.Details)
	}
	return result
}

// DiagnosticError carries one or more structured diagnostics through ordinary
// Go error paths. Cause is intentionally excluded from JSON and persisted maps.
type DiagnosticError struct {
	Message     string       `json:"message"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Cause       error        `json:"-"`
}

func NewDiagnosticError(message string, diagnostics ...Diagnostic) *DiagnosticError {
	return WrapDiagnosticError(nil, message, diagnostics...)
}

func WrapDiagnosticError(cause error, message string, diagnostics ...Diagnostic) *DiagnosticError {
	return &DiagnosticError{
		Message:     strings.TrimSpace(message),
		Diagnostics: append([]Diagnostic(nil), diagnostics...),
		Cause:       cause,
	}
}

func (e *DiagnosticError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if len(e.Diagnostics) > 0 && e.Diagnostics[0].Message != "" {
		return e.Diagnostics[0].Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "contract validation failed"
}

func (e *DiagnosticError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *DiagnosticError) ToMap() map[string]any {
	diagnostics := make([]any, 0, len(e.Diagnostics))
	for _, diagnostic := range e.Diagnostics {
		diagnostics = append(diagnostics, diagnostic.ToMap())
	}
	return map[string]any{
		"message":     e.Error(),
		"diagnostics": diagnostics,
	}
}

func cloneDiagnosticDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	cloned, _ := Materialize(details).(map[string]any)
	return cloned
}
