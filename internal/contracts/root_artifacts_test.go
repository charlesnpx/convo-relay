package contracts

import (
	"strings"
	"testing"
)

func TestEveryRootArtifactKindRoundTripsWithSafeRefAndRejectsTampering(t *testing.T) {
	ordinalKinds := map[string]bool{
		RootArtifactKindNamedInputContent:  true,
		RootArtifactKindRootCheckpoint:     true,
		RootArtifactKindReducerAttempt:     true,
		RootArtifactKindRenderedPrompt:     true,
		RootArtifactKindProviderInvocation: true,
		RootArtifactKindProviderResult:     true,
	}
	for _, kind := range RootArtifactKinds() {
		t.Run(kind, func(t *testing.T) {
			ordinal := 0
			if ordinalKinds[kind] {
				ordinal = 1
			}
			payload, err := NormalizeRootArtifact(kind, map[string]any{
				"value": map[string]any{"nested": []any{"stable"}},
			})
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			encoded, err := CanonicalJSONBytes(payload)
			if err != nil {
				t.Fatalf("canonical JSON: %v", err)
			}
			decoded, err := DecodeStrictJSONBytes(encoded)
			if err != nil {
				t.Fatalf("strict decode: %v", err)
			}
			validated, err := ValidateRootArtifact(decoded, kind)
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			ref, err := RootArtifactRefForPayload(kind, ordinal, validated)
			if err != nil {
				t.Fatalf("create ref: %v", err)
			}
			if _, err := ValidateRootArtifactRef(ref, kind, ordinal, validated); err != nil {
				t.Fatalf("validate ref: %v", err)
			}

			tampered := cloneObject(validated)
			tampered["value"] = "tampered"
			if _, err := ValidateRootArtifactRef(ref, kind, ordinal, tampered); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
				t.Fatalf("tampered ref error = %v", err)
			}
		})
	}
}

func TestRootArtifactIdentitiesAreConstantOrOrdinal(t *testing.T) {
	contract, err := RootArtifactIdentityFor(RootArtifactKindIntegrationContract, 0)
	if err != nil {
		t.Fatalf("contract identity: %v", err)
	}
	if contract.ArtifactID != "selected" || contract.RefID != "integration_contract:selected" {
		t.Fatalf("contract identity = %#v", contract)
	}
	content, err := RootArtifactIdentityFor(RootArtifactKindNamedInputContent, 1)
	if err != nil {
		t.Fatalf("input identity: %v", err)
	}
	if content.ArtifactID != "000001" || content.RefID != "named_input_content:000001" {
		t.Fatalf("input identity = %#v", content)
	}
	if _, err := RootArtifactIdentityFor(RootArtifactKindNamedInputContent, 0); err == nil {
		t.Fatal("zero ordinal unexpectedly accepted")
	}
	if _, err := RootArtifactIdentityFor(RootArtifactKindIntegrationContract, 1); err == nil {
		t.Fatal("ordinal unexpectedly accepted for constant identity")
	}
	if ordinal, err := RootArtifactOrdinalFromID(RootArtifactKindNamedInputContent, "000001"); err != nil || ordinal != 1 {
		t.Fatalf("parse ordinal = %d, %v", ordinal, err)
	}
	for _, invalid := range []string{"1", "000000", "name", "-00001"} {
		if _, err := RootArtifactOrdinalFromID(RootArtifactKindNamedInputContent, invalid); err == nil {
			t.Fatalf("invalid ordinal %q accepted", invalid)
		}
	}
}

func TestRootArtifactEnvelopeRejectsKindAndVersionTampering(t *testing.T) {
	if _, err := NormalizeRootArtifact(RootArtifactKindRawResult, map[string]any{
		"kind":           RootArtifactKindCanonicalResult,
		"schema_version": 1,
	}); err == nil {
		t.Fatal("mismatched declared kind accepted")
	}
	v2, err := NormalizeRootArtifactVersion(RootArtifactKindRawResult, 2, map[string]any{"digest_profile": DigestProfileV1})
	if err != nil {
		t.Fatalf("normalize v2: %v", err)
	}
	if _, err := ValidateRootArtifact(v2, RootArtifactKindRawResult); err != nil {
		t.Fatalf("validate v2: %v", err)
	}
	if _, err := NormalizeRootArtifact(RootArtifactKindRawResult, map[string]any{"digest_profile": DigestProfileV1}); err == nil {
		t.Fatal("v1 writer accepted v2-only field")
	}
	if _, err := ValidateRootArtifact(map[string]any{
		"kind":           RootArtifactKindRawResult,
		"schema_version": 1,
		"digest_profile": DigestProfileV1,
	}, RootArtifactKindRawResult); err == nil {
		t.Fatal("v1 reader accepted v2-only field")
	}
	if _, err := ValidateRootArtifact(map[string]any{
		"kind":           RootArtifactKindRawResult,
		"schema_version": 3,
	}, RootArtifactKindRawResult); err == nil {
		t.Fatal("unknown persisted version accepted")
	}
	if _, err := ValidateRootArtifact(map[string]any{
		"kind":           "consumer_specific_result",
		"schema_version": 1,
	}, ""); err == nil {
		t.Fatal("unknown root artifact kind accepted")
	}
}
