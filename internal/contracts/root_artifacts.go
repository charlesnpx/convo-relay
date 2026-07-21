package contracts

import (
	"fmt"
	"strconv"
)

const RootArtifactSchemaVersion = 1

const (
	RootArtifactKindRootRecipePlan      = "root_recipe_plan"
	RootArtifactKindIntegrationBundle   = "integration_bundle"
	RootArtifactKindIntegrationContract = "integration_contract"
	RootArtifactKindNamedInputManifest  = "named_input_manifest"
	RootArtifactKindNamedInputContent   = "named_input_content"
	RootArtifactKindExecutionWorkspace  = "execution_workspace"
	RootArtifactKindRootCheckpoint      = "root_checkpoint"
	RootArtifactKindReducerAttempt      = "reducer_attempt"
	RootArtifactKindRawResult           = "raw_result"
	RootArtifactKindResultValidation    = "result_validation"
	RootArtifactKindCanonicalResult     = "canonical_result"
)

type RootArtifactIdentity struct {
	ArtifactID string
	RefID      string
}

type rootArtifactSpec struct {
	kind    string
	ordinal bool
}

var rootArtifactSpecs = []rootArtifactSpec{
	{kind: RootArtifactKindRootRecipePlan},
	{kind: RootArtifactKindIntegrationBundle},
	{kind: RootArtifactKindIntegrationContract},
	{kind: RootArtifactKindNamedInputManifest},
	{kind: RootArtifactKindNamedInputContent, ordinal: true},
	{kind: RootArtifactKindExecutionWorkspace},
	{kind: RootArtifactKindRootCheckpoint, ordinal: true},
	{kind: RootArtifactKindReducerAttempt, ordinal: true},
	{kind: RootArtifactKindRawResult},
	{kind: RootArtifactKindResultValidation},
	{kind: RootArtifactKindCanonicalResult},
}

func RootArtifactKinds() []string {
	kinds := make([]string, 0, len(rootArtifactSpecs))
	for _, spec := range rootArtifactSpecs {
		kinds = append(kinds, spec.kind)
	}
	return kinds
}

func NormalizeRootArtifact(kind string, fields map[string]any) (map[string]any, error) {
	if _, ok := rootArtifactSpecForKind(kind); !ok {
		return nil, NewValidationError("unsupported root artifact kind %q", kind)
	}
	result := map[string]any{}
	for key, value := range fields {
		result[key] = Materialize(value)
	}
	if declaredKind, exists := result["kind"]; exists && declaredKind != kind {
		return nil, NewValidationError("root artifact kind %q does not match expected kind %q", declaredKind, kind)
	}
	if version, exists := result["schema_version"]; exists && !schemaVersionIsOne(version) {
		return nil, NewValidationError("%s requires numeric schema_version 1", kind)
	}
	result["kind"] = kind
	result["schema_version"] = RootArtifactSchemaVersion
	return result, nil
}

func ValidateRootArtifact(value any, expectedKind string) (map[string]any, error) {
	object, err := requireObjectValue(value, "root artifact")
	if err != nil {
		return nil, err
	}
	kind, ok := object["kind"].(string)
	if !ok {
		return nil, NewValidationError("root artifact kind must be a string")
	}
	if _, supported := rootArtifactSpecForKind(kind); !supported {
		return nil, NewValidationError("unsupported root artifact kind %q", kind)
	}
	if expectedKind != "" && kind != expectedKind {
		return nil, NewValidationError("root artifact kind %q does not match expected kind %q", kind, expectedKind)
	}
	if !schemaVersionIsOne(object["schema_version"]) {
		return nil, NewValidationError("%s requires numeric schema_version 1", kind)
	}
	object["kind"] = kind
	object["schema_version"] = RootArtifactSchemaVersion
	return object, nil
}

func RootArtifactIdentityFor(kind string, ordinal int) (RootArtifactIdentity, error) {
	spec, ok := rootArtifactSpecForKind(kind)
	if !ok {
		return RootArtifactIdentity{}, NewValidationError("unsupported root artifact kind %q", kind)
	}
	artifactID := "selected"
	if spec.ordinal {
		if ordinal < 1 {
			return RootArtifactIdentity{}, NewValidationError("%s requires a positive ordinal", kind)
		}
		artifactID = fmt.Sprintf("%06d", ordinal)
	} else if ordinal != 0 {
		return RootArtifactIdentity{}, NewValidationError("%s uses the constant selected identity", kind)
	}
	return RootArtifactIdentity{
		ArtifactID: artifactID,
		RefID:      kind + ":" + artifactID,
	}, nil
}

func RootArtifactRefForPayload(kind string, ordinal int, payload any) (map[string]any, error) {
	normalized, err := ValidateRootArtifact(payload, kind)
	if err != nil {
		return nil, err
	}
	identity, err := RootArtifactIdentityFor(kind, ordinal)
	if err != nil {
		return nil, err
	}
	return ArtifactRefForPayload(identity.RefID, normalized)
}

func ValidateRootArtifactRef(ref any, kind string, ordinal int, payload any) (map[string]any, error) {
	normalized, err := ValidateRootArtifact(payload, kind)
	if err != nil {
		return nil, err
	}
	identity, err := RootArtifactIdentityFor(kind, ordinal)
	if err != nil {
		return nil, err
	}
	artifactRef, err := ValidateArtifactRef(ref)
	if err != nil {
		return nil, err
	}
	if artifactRef["id"] != identity.RefID {
		return nil, NewValidationError("root artifact ref id must be %s", identity.RefID)
	}
	digest, err := ContractDigest(normalized)
	if err != nil {
		return nil, err
	}
	if artifactRef["digest"] != digest {
		return nil, NewValidationError("root artifact ref %s digest mismatch", identity.RefID)
	}
	return artifactRef, nil
}

func rootArtifactSpecForKind(kind string) (rootArtifactSpec, bool) {
	for _, spec := range rootArtifactSpecs {
		if spec.kind == kind {
			return spec, true
		}
	}
	return rootArtifactSpec{}, false
}

func RootArtifactOrdinalFromID(kind string, artifactID string) (int, error) {
	spec, ok := rootArtifactSpecForKind(kind)
	if !ok || !spec.ordinal {
		return 0, NewValidationError("%s does not use ordinal identities", kind)
	}
	ordinal, err := strconv.Atoi(artifactID)
	if err != nil || ordinal < 1 || fmt.Sprintf("%06d", ordinal) != artifactID {
		return 0, NewValidationError("%s artifact id must be a zero-padded positive ordinal", kind)
	}
	return ordinal, nil
}
