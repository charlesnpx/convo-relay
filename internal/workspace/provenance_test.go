package workspace

import (
	"reflect"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestWorkspaceProvenanceRoundTripsAllLaunchScenarios(t *testing.T) {
	tests := []struct {
		name            string
		achieved        string
		contentSource   string
		changesIncluded bool
	}{
		{
			name:            "inherited",
			achieved:        PolicyInherited,
			contentSource:   WorkspaceContentSourceWorkingTree,
			changesIncluded: true,
		},
		{
			name:            "clean isolated",
			achieved:        PolicyEphemeral,
			contentSource:   WorkspaceContentSourceCommittedHead,
			changesIncluded: false,
		},
		{
			name:            "dirty override isolated",
			achieved:        PolicyEphemeral,
			contentSource:   WorkspaceContentSourceCommittedHead,
			changesIncluded: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := map[string]any{
				"policy": map[string]any{
					"effective": test.achieved,
					"achieved":  test.achieved,
				},
				WorkspaceContentSourceKey:     test.contentSource,
				WorkingTreeChangesIncludedKey: test.changesIncluded,
			}
			projection, err := ProvenanceProjection(artifact)
			if err != nil {
				t.Fatalf("projection: %v", err)
			}
			if projection[WorkspaceContentSourceKey] != test.contentSource ||
				projection[WorkingTreeChangesIncludedKey] != test.changesIncluded ||
				projection[WorkspaceProvenanceInferredKey] != false {
				t.Fatalf("projection = %#v", projection)
			}
			if _, err := ValidateProvenanceProjection(projection, artifact); err != nil {
				t.Fatalf("validate projection: %v", err)
			}
		})
	}
}

func TestLegacyWorkspaceProvenanceIsInferredWithoutChangingArtifact(t *testing.T) {
	artifact, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindExecutionWorkspace, map[string]any{
		"policy": map[string]any{
			"effective": PolicyReadOnly,
			"achieved":  PolicyEphemeral,
		},
	})
	if err != nil {
		t.Fatalf("normalize legacy artifact: %v", err)
	}
	before := contracts.Materialize(artifact).(map[string]any)
	beforeRef, err := contracts.RootArtifactRefForPayload(contracts.RootArtifactKindExecutionWorkspace, 0, artifact)
	if err != nil {
		t.Fatalf("legacy ref: %v", err)
	}

	provenance, err := ValidateProvenanceProjection(map[string]any{}, artifact)
	if err != nil {
		t.Fatalf("legacy provenance: %v", err)
	}
	if provenance.WorkspaceContentSource != WorkspaceContentSourceCommittedHead ||
		provenance.WorkingTreeChangesIncluded ||
		!provenance.Inferred {
		t.Fatalf("legacy provenance = %#v", provenance)
	}
	afterRef, err := contracts.RootArtifactRefForPayload(contracts.RootArtifactKindExecutionWorkspace, 0, artifact)
	if err != nil {
		t.Fatalf("legacy ref after projection: %v", err)
	}
	if !reflect.DeepEqual(artifact, before) ||
		beforeRef["digest"] != afterRef["digest"] {
		t.Fatalf("legacy artifact changed:\nbefore=%#v\nafter=%#v", before, artifact)
	}
}

func TestWorkspaceProvenanceRejectsIncompleteOrDisagreeingValues(t *testing.T) {
	artifact := map[string]any{
		"policy": map[string]any{
			"effective": PolicyEphemeral,
			"achieved":  PolicyEphemeral,
		},
		WorkspaceContentSourceKey:     WorkspaceContentSourceCommittedHead,
		WorkingTreeChangesIncludedKey: false,
	}
	if _, err := ValidateProvenanceProjection(map[string]any{
		WorkspaceContentSourceKey:      WorkspaceContentSourceWorkingTree,
		WorkingTreeChangesIncludedKey:  true,
		WorkspaceProvenanceInferredKey: false,
	}, artifact); err == nil {
		t.Fatal("disagreeing projection unexpectedly accepted")
	}
	delete(artifact, WorkingTreeChangesIncludedKey)
	if _, err := ProvenanceFromArtifact(artifact); err == nil {
		t.Fatal("incomplete artifact provenance unexpectedly accepted")
	}
}
