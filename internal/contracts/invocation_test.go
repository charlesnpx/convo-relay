package contracts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderInvocationV2RequiresResultRefOnlyForLaunchedAttempts(t *testing.T) {
	base := providerInvocationTestRecord()
	if _, err := ProviderInvocationRecord(base); err == nil || !strings.Contains(err.Error(), "provider_result_ref") {
		t.Fatalf("launched invocation without result ref error = %v", err)
	}

	base["provider_launch_attempted"] = false
	base["provider_result_ref"] = nil
	if record, err := ProviderInvocationRecord(base); err != nil || record["provider_result_ref"] != nil {
		t.Fatalf("unlaunched invocation = %#v, %v", record, err)
	}

	base["provider_result_ref"] = providerResultTestRef(t)
	if _, err := ProviderInvocationRecord(base); err == nil || !strings.Contains(err.Error(), "unlaunched") {
		t.Fatalf("unlaunched invocation with result ref error = %v", err)
	}
}

func TestProviderInvocationResultBindingRejectsIdentityMismatch(t *testing.T) {
	resultRef := providerResultTestRef(t)
	draft := providerInvocationTestRecord()
	draft["schema_version"] = ProviderInvocationV2
	draft["provider_result_ref"] = nil
	result, err := ProviderResultRecord(map[string]any{
		"invocation_id":   draft["invocation_id"],
		"phase":           draft["phase"],
		"actor":           draft["actor"],
		"runner_attempt":  draft["runner_attempt"],
		"provider_retry":  draft["provider_retry"],
		"backend":         draft["backend"],
		"started_at":      draft["started_at"],
		"completed_at":    draft["completed_at"],
		"outcome":         draft["outcome"],
		"failure_stage":   draft["failure_stage"],
		"classification":  draft["classification"],
		"provider_result": map[string]any{"backend": "codex", "return_code": 0},
		"invocation":      draft,
	})
	if err != nil {
		t.Fatalf("provider result record: %v", err)
	}
	resultArtifact, err := NormalizeRootArtifactVersion(RootArtifactKindProviderResult, RootArtifactSchemaVersionV2, result)
	if err != nil {
		t.Fatalf("provider result artifact: %v", err)
	}
	invocation := cloneObject(draft)
	invocation["provider_result_ref"] = resultRef
	bound, err := ProviderInvocationRecord(invocation)
	if err != nil {
		t.Fatalf("provider invocation record: %v", err)
	}
	if _, _, err := ValidateProviderInvocationResultBinding(bound, resultArtifact); err != nil {
		t.Fatalf("valid binding: %v", err)
	}

	tampered := cloneObject(bound)
	tampered["phase"] = "facilitator"
	if _, _, err := ValidateProviderInvocationResultBinding(tampered, resultArtifact); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tampered binding error = %v", err)
	}
}

func TestProviderResultNeutralGoldenFixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contracts", "provider-result-neutral-v2.json"))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	value, err := DecodeStrictJSONObjectBytes(body)
	if err != nil {
		t.Fatalf("decode golden fixture: %v", err)
	}
	artifact, err := ValidateRootArtifact(value, RootArtifactKindProviderResult)
	if err != nil {
		t.Fatalf("validate golden root artifact: %v", err)
	}
	if _, err := ValidateProviderResultRecord(artifact); err != nil {
		t.Fatalf("validate golden provider result: %v", err)
	}
}

func providerInvocationTestRecord() map[string]any {
	return map[string]any{
		"invocation_id":             "participant:000001",
		"phase":                     "participant",
		"actor":                     "Agent A",
		"participant_ordinal":       1,
		"backend":                   "codex",
		"mapped_working_directory":  ".",
		"runner_attempt":            1,
		"provider_launch_attempted": true,
		"provider_retry":            "allow",
		"started_at":                "2026-01-01T00:00:00Z",
		"completed_at":              "2026-01-01T00:00:01Z",
		"outcome":                   "completed",
		"failure_stage":             nil,
		"classification":            nil,
		"provider_result_ref":       nil,
	}
}

func providerResultTestRef(t *testing.T) map[string]any {
	t.Helper()
	ref, err := ArtifactRefForPayload("provider_result:000001", map[string]any{
		"kind":           RootArtifactKindProviderResult,
		"schema_version": RootArtifactSchemaVersionV2,
		"digest_profile": DigestProfileV1,
		"value":          "test",
	})
	if err != nil {
		t.Fatalf("provider result ref: %v", err)
	}
	return ref
}
