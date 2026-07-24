package namedinputs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestIntegrityDiagnosticCarriesBoundaryMetadataWithoutInputContents(t *testing.T) {
	expectedSize := int64(0)
	observedSize := int64(9)
	providerAttempt := 2
	mismatch := IntegrityMismatch{
		Role:            "participant",
		ProviderAttempt: &providerAttempt,
		AttemptBoundary: IntegrityBoundaryAfterAttempt,
		InputName:       "payload",
		InputOrdinal:    2,
		Category:        IntegrityMismatchDigest,
		Expected: IntegrityObservation{
			SizeBytes: &expectedSize,
			Digest:    "sha256:expected",
			Type:      "regular",
			Mode:      "0644",
			Path:      "000002-payload",
		},
		Observed: IntegrityObservation{
			SizeBytes: &observedSize,
			Digest:    "sha256:observed",
			Type:      "regular",
			Mode:      "0600",
			Path:      "changed",
		},
	}
	diagnostic := mismatch.Diagnostic()
	if diagnostic.Code != DiagnosticCodeIntegrity ||
		diagnostic.Details["role"] != "participant" ||
		diagnostic.Details["provider_attempt"] != 2 ||
		diagnostic.Details["attempt_boundary"] != IntegrityBoundaryAfterAttempt ||
		diagnostic.Details["input_name"] != "payload" ||
		diagnostic.Details["input_ordinal"] != 2 ||
		diagnostic.Details["mismatch_category"] != IntegrityMismatchDigest {
		t.Fatalf("integrity diagnostic = %#v", diagnostic)
	}
	expected := diagnostic.Details["expected"].(map[string]any)
	observed := diagnostic.Details["observed"].(map[string]any)
	if expected["size_bytes"] != int64(0) || expected["digest"] != "sha256:expected" ||
		expected["type"] != "regular" || expected["mode"] != "0644" || expected["path"] != "000002-payload" ||
		observed["size_bytes"] != int64(9) || observed["digest"] != "sha256:observed" {
		t.Fatalf("integrity observations = expected %#v observed %#v", expected, observed)
	}

	body, err := contracts.CanonicalJSONBytes(diagnostic.ToMap())
	if err != nil {
		t.Fatalf("canonical diagnostic JSON: %v", err)
	}
	if _, err := contracts.DecodeStrictJSONBytes(body); err != nil {
		t.Fatalf("strict diagnostic JSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode diagnostic JSON: %v", err)
	}
	text := string(body)
	for _, forbidden := range []string{"input_content", "contents", "raw_bytes", "secret input body"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("diagnostic stored forbidden content marker %q: %s", forbidden, text)
		}
	}
}

func TestVerificationIncompleteDiagnosticIsContentFreeAndOptionalAttemptIsOmitted(t *testing.T) {
	cause := context.DeadlineExceeded
	err := NewVerificationIncompleteError(
		cause,
		"result_validation",
		IntegrityBoundaryResultValidation,
		nil,
	)
	if !errors.Is(err, cause) {
		t.Fatalf("verification-incomplete error lost cause: %v", err)
	}
	var diagnosticErr *contracts.DiagnosticError
	if !errors.As(err, &diagnosticErr) || len(diagnosticErr.Diagnostics) != 1 {
		t.Fatalf("verification-incomplete error = %T %v", err, err)
	}
	diagnostic := diagnosticErr.Diagnostics[0]
	if diagnostic.Details["mismatch_category"] != IntegrityMismatchIncomplete ||
		diagnostic.Details["role"] != "result_validation" ||
		diagnostic.Details["attempt_boundary"] != IntegrityBoundaryResultValidation {
		t.Fatalf("verification-incomplete diagnostic = %#v", diagnostic)
	}
	if _, exists := diagnostic.Details["provider_attempt"]; exists {
		t.Fatalf("non-provider diagnostic included provider_attempt: %#v", diagnostic)
	}
	body, encodeErr := contracts.CanonicalJSONBytes(diagnostic.ToMap())
	if encodeErr != nil {
		t.Fatalf("encode verification-incomplete diagnostic: %v", encodeErr)
	}
	if strings.Contains(string(body), cause.Error()) {
		t.Fatalf("verification-incomplete diagnostic exposed cause text: %s", body)
	}
}
