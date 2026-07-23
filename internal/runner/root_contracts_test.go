package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

func TestRecipeWarningCallbackIsOptionalAndReceivesAnImmutableValue(t *testing.T) {
	retained := RecipeWarning{
		Code:                       RecipeWarningCodeDirtySourceCommittedHead,
		Message:                    "Committed HEAD will be used.",
		StagedChanges:              1,
		UnstagedChanges:            2,
		UntrackedChanges:           3,
		WorkspaceContentSource:     workspace.WorkspaceContentSourceCommittedHead,
		WorkingTreeChangesIncluded: false,
	}
	deliverRecipeWarning(nil, retained)

	var received RecipeWarning
	deliverRecipeWarning(func(warning RecipeWarning) {
		received = warning
		warning.Message = "mutated by callback"
		warning.StagedChanges = 99
		warning.WorkspaceContentSource = workspace.WorkspaceContentSourceWorkingTree
	}, retained)
	if received != retained {
		t.Fatalf("received warning = %#v, want %#v", received, retained)
	}
	if retained.Message != "Committed HEAD will be used." ||
		retained.StagedChanges != 1 ||
		retained.WorkspaceContentSource != workspace.WorkspaceContentSourceCommittedHead {
		t.Fatalf("retained warning was mutable through callback: %#v", retained)
	}
	if _, err := contracts.CanonicalJSONBytes(retained); err != nil {
		t.Fatalf("warning is not strict-JSON serializable: %v", err)
	}
}

func TestNamedInputIntegrityTerminalMetadataIsDeterministic(t *testing.T) {
	cause := errors.New("retained input digest changed")
	meta := model.NewSessionMeta(map[string]any{
		"status":                "running",
		"root_recovery_pending": true,
		"failure_causes":        []any{"provider_failed", RootFailureCauseSourceMutated},
		"source_mutated":        true,
	})
	failed := namedInputIntegrityFailureMeta(meta, cause, "provider_failed")
	if failed.String("status") != "failed" ||
		failed.String("stop_reason") != RootNamedInputIntegrityStopReason ||
		failed.String("execution_phase") != RootNamedInputIntegrityExecutionPhase ||
		failed.Get("root_recovery_pending") != false ||
		failed.String("error") != cause.Error() {
		t.Fatalf("integrity terminal metadata = %#v", failed.ToMap())
	}
	wantCauses := []any{
		RootFailureCauseNamedInputIntegrity,
		RootFailureCauseSourceMutated,
		"provider_failed",
	}
	if !reflect.DeepEqual(failed.Slice("failure_causes"), wantCauses) {
		t.Fatalf("failure causes = %#v, want %#v", failed.Slice("failure_causes"), wantCauses)
	}
	body, err := contracts.CanonicalJSONBytes(failed.ToMap())
	if err != nil {
		t.Fatalf("terminal metadata canonical JSON: %v", err)
	}
	if _, err := contracts.DecodeStrictJSONBytes(body); err != nil {
		t.Fatalf("terminal metadata strict JSON: %v", err)
	}
}

func TestWorkspaceFinalizationDoesNotOverwriteNamedInputIntegrityPrimaryFailure(t *testing.T) {
	fixture := newIsolatedSessionFixture(t, "running")
	st := store.New(fixture.sessionDir)
	meta, err := st.LoadMeta()
	if err != nil {
		t.Fatalf("load fixture metadata: %v", err)
	}
	integrityErr := errors.New("retained named input changed")
	meta = namedInputIntegrityFailureMeta(meta, integrityErr)
	if err := os.WriteFile(filepath.Join(fixture.sourceRoot, "source.txt"), []byte("source changed too\n"), 0o644); err != nil {
		t.Fatalf("mutate source: %v", err)
	}

	finalizedMeta, finalized, err := finalizeTerminalWorkspace(context.Background(), st, meta)
	var mutation *workspace.SourceMutatedError
	if !errors.As(err, &mutation) || finalized == nil || !finalized.SourceMutated {
		t.Fatalf("workspace finalization = %#v, %v", finalized, err)
	}
	if finalizedMeta.String("status") != "failed" ||
		finalizedMeta.String("stop_reason") != RootNamedInputIntegrityStopReason ||
		finalizedMeta.String("execution_phase") != RootNamedInputIntegrityExecutionPhase ||
		finalizedMeta.String("error") != integrityErr.Error() ||
		finalizedMeta.Get("root_recovery_pending") != false ||
		!finalizedMeta.Bool("source_mutated") {
		t.Fatalf("combined terminal metadata = %#v", finalizedMeta.ToMap())
	}
	if !reflect.DeepEqual(finalizedMeta.Slice("failure_causes"), []any{
		RootFailureCauseNamedInputIntegrity,
		RootFailureCauseSourceMutated,
	}) {
		t.Fatalf("combined failure causes = %#v", finalizedMeta.Slice("failure_causes"))
	}

	// The worktree remains available for administrative cleanup.
	if _, statErr := os.Stat(filepath.Clean(fixture.worktreePath)); statErr != nil {
		t.Fatalf("combined terminal failure removed worktree: %v", statErr)
	}
}
