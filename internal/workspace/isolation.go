package workspace

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func isolationReportForWorkspace(artifact map[string]any) (map[string]any, error) {
	base, ok := artifact["base"].(map[string]any)
	if !ok {
		return nil, contracts.NewValidationError("execution_workspace base must be an object")
	}
	registration, ok := artifact["registration"].(map[string]any)
	if !ok {
		return nil, contracts.NewValidationError("execution_workspace registration must be an object")
	}
	mechanism := "inherited"
	separation := "no"
	if registration["mode"] == "detached_worktree" {
		mechanism = "detached_writable_git_worktree"
		separation = "yes"
	}
	writeControl := "none"
	if strings.TrimSpace(stringValue(artifact["source_before_digest"])) != "" {
		writeControl = "post_run_detection"
	}
	return contracts.WorkspaceIsolationReportRecord(map[string]any{
		"mechanism": mechanism,
		"base_identity": map[string]any{
			"object_format": emptyStringOrNil(stringValue(base["object_format"])),
			"head_commit":   emptyStringOrNil(stringValue(base["head_commit"])),
			"head_tree":     emptyStringOrNil(stringValue(base["head_tree"])),
		},
		"source_copy_separation":      separation,
		"source_write_control":        writeControl,
		"filesystem_containment":      "none",
		"network_isolation":           "none",
		"process_containment":         "none",
		"same_user_security_boundary": "none",
		"unknown_dimensions":          []any{},
	})
}

func persistIsolationReport(st *store.Store, report map[string]any) (map[string]any, error) {
	payload, err := contracts.NormalizeRootArtifactVersion(
		contracts.RootArtifactKindIsolationReport,
		contracts.RootArtifactSchemaVersionV2,
		map[string]any{"isolation_report": report},
	)
	if err != nil {
		return nil, err
	}
	identity, err := contracts.RootArtifactIdentityFor(contracts.RootArtifactKindIsolationReport, 0)
	if err != nil {
		return nil, err
	}
	ref, err := st.SaveContractArtifact(contracts.RootArtifactKindIsolationReport, identity.ArtifactID, payload, identity.RefID)
	if err != nil {
		return nil, err
	}
	persisted, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, err
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, contracts.RootArtifactKindIsolationReport, 0, persisted); err != nil {
		return nil, err
	}
	if _, err := contracts.ValidateWorkspaceIsolationReportRecord(persisted["isolation_report"]); err != nil {
		return nil, err
	}
	return ref, nil
}

func validatePersistedIsolationReport(st *store.Store, workspaceArtifact map[string]any) error {
	version, err := contracts.RequireNumericVersion(workspaceArtifact, contracts.ContractRootArtifact)
	if err != nil || version == contracts.RootArtifactSchemaVersion {
		return err
	}
	ref, ok := workspaceArtifact["isolation_report_ref"].(map[string]any)
	if !ok {
		return contracts.NewValidationError("execution_workspace v2 requires isolation_report_ref")
	}
	payload, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return fmt.Errorf("load workspace isolation report: %w", err)
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, contracts.RootArtifactKindIsolationReport, 0, payload); err != nil {
		return err
	}
	report, err := contracts.ValidateWorkspaceIsolationReportRecord(payload["isolation_report"])
	if err != nil {
		return err
	}
	expected, err := isolationReportForWorkspace(workspaceArtifact)
	if err != nil {
		return err
	}
	observedDigest, err := contracts.SemanticJSONDigest(report)
	if err != nil {
		return err
	}
	expectedDigest, err := contracts.SemanticJSONDigest(expected)
	if err != nil {
		return err
	}
	if observedDigest != expectedDigest {
		return contracts.NewValidationError("workspace isolation report disagrees with observed workspace facts")
	}
	return nil
}

func emptyStringOrNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
