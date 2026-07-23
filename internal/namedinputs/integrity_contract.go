package namedinputs

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

const (
	IntegrityBoundaryInitialization = "initialization"
	IntegrityBoundaryBeforeAttempt  = "before_attempt"
	IntegrityBoundaryAfterAttempt   = "after_attempt"
	IntegrityBoundaryRecovery       = "recovery"

	IntegrityMismatchMissing    = "missing"
	IntegrityMismatchUnexpected = "unexpected"
	IntegrityMismatchReordered  = "reordered"
	IntegrityMismatchType       = "type"
	IntegrityMismatchMode       = "mode"
	IntegrityMismatchPath       = "path"
	IntegrityMismatchSize       = "size"
	IntegrityMismatchDigest     = "digest"
)

// IntegrityObservation contains only filesystem metadata. Input contents are
// intentionally unrepresentable in this contract.
type IntegrityObservation struct {
	SizeBytes *int64 `json:"size_bytes,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Type      string `json:"type,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Path      string `json:"path,omitempty"`
}

// IntegrityMismatch identifies one retained-input mismatch at an orchestration
// boundary without retaining the input's bytes.
type IntegrityMismatch struct {
	Role            string               `json:"role"`
	AttemptBoundary string               `json:"attempt_boundary"`
	InputName       string               `json:"input_name"`
	InputOrdinal    int                  `json:"input_ordinal"`
	Category        string               `json:"mismatch_category"`
	Expected        IntegrityObservation `json:"expected"`
	Observed        IntegrityObservation `json:"observed"`
}

func (m IntegrityMismatch) Diagnostic() contracts.Diagnostic {
	details := map[string]any{
		"role":              strings.TrimSpace(m.Role),
		"attempt_boundary":  strings.TrimSpace(m.AttemptBoundary),
		"input_name":        m.InputName,
		"input_ordinal":     m.InputOrdinal,
		"mismatch_category": strings.TrimSpace(m.Category),
		"expected":          integrityObservationMap(m.Expected),
		"observed":          integrityObservationMap(m.Observed),
	}
	return contracts.NewDiagnostic(
		DiagnosticCodeIntegrity,
		contracts.DiagnosticPhasePolicy,
		inputValuePointer(m.InputName, m.InputOrdinal),
		fmt.Sprintf("Retained named input %q failed integrity verification.", m.InputName),
		details,
	)
}

func integrityObservationMap(observation IntegrityObservation) map[string]any {
	result := map[string]any{}
	if observation.SizeBytes != nil {
		result["size_bytes"] = *observation.SizeBytes
	}
	if value := strings.TrimSpace(observation.Digest); value != "" {
		result["digest"] = value
	}
	if value := strings.TrimSpace(observation.Type); value != "" {
		result["type"] = value
	}
	if value := strings.TrimSpace(observation.Mode); value != "" {
		result["mode"] = value
	}
	if value := strings.TrimSpace(observation.Path); value != "" {
		result["path"] = value
	}
	return result
}
