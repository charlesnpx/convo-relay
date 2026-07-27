package workspace

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

const (
	WorkspaceContentSourceWorkingTree   = "working_tree"
	WorkspaceContentSourceCommittedHead = "committed_head"

	WorkspaceContentSourceKey      = "workspace_content_source"
	WorkingTreeChangesIncludedKey  = "working_tree_changes_included"
	WorkspaceProvenanceInferredKey = "workspace_provenance_inferred"
)

// Provenance is the list/show projection of the authoritative
// execution_workspace artifact. Inferred is true only for legacy artifacts
// that predate the two provenance fields.
type Provenance struct {
	WorkspaceContentSource     string `json:"workspace_content_source"`
	WorkingTreeChangesIncluded bool   `json:"working_tree_changes_included"`
	Inferred                   bool   `json:"workspace_provenance_inferred"`
}

func (p Provenance) Projection() map[string]any {
	return map[string]any{
		WorkspaceContentSourceKey:      p.WorkspaceContentSource,
		WorkingTreeChangesIncludedKey:  p.WorkingTreeChangesIncluded,
		WorkspaceProvenanceInferredKey: p.Inferred,
	}
}

// ProvenanceFromArtifact validates explicit provenance or derives compatible
// values in memory for a legacy artifact. It never mutates the artifact.
func ProvenanceFromArtifact(artifact map[string]any) (Provenance, error) {
	if artifact == nil {
		return Provenance{}, contracts.NewValidationError("execution_workspace artifact is required for provenance")
	}
	sourceValue, sourceExists := artifact[WorkspaceContentSourceKey]
	includedValue, includedExists := artifact[WorkingTreeChangesIncludedKey]
	if sourceExists != includedExists {
		return Provenance{}, contracts.NewValidationError(
			"execution_workspace provenance must provide both %s and %s",
			WorkspaceContentSourceKey,
			WorkingTreeChangesIncludedKey,
		)
	}

	policy, err := workspacePolicy(artifact)
	if err != nil {
		return Provenance{}, err
	}
	if !sourceExists {
		provenance := provenanceForAchievedPolicy(policy.achieved)
		provenance.Inferred = true
		return provenance, nil
	}

	source, ok := sourceValue.(string)
	if !ok {
		return Provenance{}, contracts.NewValidationError("execution_workspace %s must be a string", WorkspaceContentSourceKey)
	}
	source = strings.TrimSpace(source)
	included, ok := includedValue.(bool)
	if !ok {
		return Provenance{}, contracts.NewValidationError("execution_workspace %s must be a boolean", WorkingTreeChangesIncludedKey)
	}
	provenance := Provenance{
		WorkspaceContentSource:     source,
		WorkingTreeChangesIncluded: included,
	}
	if err := validateProvenanceForPolicy(provenance, policy.achieved); err != nil {
		return Provenance{}, err
	}
	return provenance, nil
}

func ProvenanceProjection(artifact map[string]any) (map[string]any, error) {
	provenance, err := ProvenanceFromArtifact(artifact)
	if err != nil {
		return nil, err
	}
	return provenance.Projection(), nil
}

// ValidateProvenanceProjection rejects disagreement between session metadata
// and the authoritative execution_workspace artifact. Missing metadata remains
// compatible only when the artifact itself is legacy and the values are being
// inferred for list/show.
func ValidateProvenanceProjection(meta map[string]any, artifact map[string]any) (Provenance, error) {
	provenance, err := ProvenanceFromArtifact(artifact)
	if err != nil {
		return Provenance{}, err
	}
	_, sourceExists := meta[WorkspaceContentSourceKey]
	_, includedExists := meta[WorkingTreeChangesIncludedKey]
	_, inferredExists := meta[WorkspaceProvenanceInferredKey]
	if !sourceExists && !includedExists && !inferredExists && provenance.Inferred {
		return provenance, nil
	}
	if !sourceExists || !includedExists || !inferredExists {
		return provenance, contracts.NewValidationError("session workspace provenance projection is incomplete")
	}
	source, sourceOK := meta[WorkspaceContentSourceKey].(string)
	included, includedOK := meta[WorkingTreeChangesIncludedKey].(bool)
	inferred, inferredOK := meta[WorkspaceProvenanceInferredKey].(bool)
	if !sourceOK || !includedOK || !inferredOK ||
		strings.TrimSpace(source) != provenance.WorkspaceContentSource ||
		included != provenance.WorkingTreeChangesIncluded ||
		inferred != provenance.Inferred {
		return provenance, contracts.NewValidationError(
			"session workspace provenance projection disagrees with the execution_workspace artifact",
		)
	}
	return provenance, nil
}

func provenanceForAchievedPolicy(achieved string) Provenance {
	if achieved == PolicyInherited {
		return Provenance{
			WorkspaceContentSource:     WorkspaceContentSourceWorkingTree,
			WorkingTreeChangesIncluded: true,
		}
	}
	return Provenance{
		WorkspaceContentSource:     WorkspaceContentSourceCommittedHead,
		WorkingTreeChangesIncluded: false,
	}
}

func validateProvenanceForPolicy(provenance Provenance, achieved string) error {
	expected := provenanceForAchievedPolicy(achieved)
	if provenance.WorkspaceContentSource != expected.WorkspaceContentSource ||
		provenance.WorkingTreeChangesIncluded != expected.WorkingTreeChangesIncluded {
		return contracts.NewValidationError(
			"execution_workspace provenance %s/%t is inconsistent with achieved policy %s",
			provenance.WorkspaceContentSource,
			provenance.WorkingTreeChangesIncluded,
			achieved,
		)
	}
	if provenance.WorkspaceContentSource != WorkspaceContentSourceWorkingTree &&
		provenance.WorkspaceContentSource != WorkspaceContentSourceCommittedHead {
		return fmt.Errorf("unsupported workspace content source %q", provenance.WorkspaceContentSource)
	}
	return nil
}
