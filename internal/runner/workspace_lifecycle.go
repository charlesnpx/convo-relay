package runner

import (
	"context"
	"errors"

	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const stopReasonWorkspaceIntegrityFailed = "workspace_integrity_failed"

func finalizeTerminalWorkspace(ctx context.Context, st *store.Store, meta model.SessionMeta) (model.SessionMeta, *workspace.Finalization, error) {
	finalized, err := workspace.Finalize(ctx, st)
	if err != nil {
		return meta, nil, err
	}
	if finalized == nil || !finalized.Managed {
		return meta, finalized, nil
	}
	meta = meta.With("execution_workspace_ref", finalized.ArtifactRef).
		With("source_changed", finalized.SourceChanged).
		With("source_mutated", finalized.SourceMutated).
		With("workspace_effective_policy", finalized.EffectivePolicy).
		With("workspace_achieved_policy", finalized.AchievedPolicy)
	if finalized.SourceBeforeDigest != "" {
		meta = meta.With("source_before_digest", finalized.SourceBeforeDigest)
	}
	if finalized.SourceAfterDigest != "" {
		meta = meta.With("source_after_digest", finalized.SourceAfterDigest)
	}
	mutationErr := finalized.MutationError()
	if mutationErr == nil {
		return meta, finalized, nil
	}
	meta = meta.With("terminal_status_before_source_check", meta.String("status")).
		WithStatus("failed").
		With("stop_reason", workspace.StopReasonSourceMutated).
		With("error", mutationErr.Error()).
		With("failed_at", utcNow())
	return meta, finalized, mutationErr
}

func workspaceIntegrityFailureMeta(meta model.SessionMeta, err error) model.SessionMeta {
	if err == nil {
		return meta
	}
	return meta.With("terminal_status_before_source_check", meta.String("status")).
		WithStatus("failed").
		With("stop_reason", stopReasonWorkspaceIntegrityFailed).
		With("error", err.Error()).
		With("failed_at", utcNow())
}

func finalizeAdministrativeTerminal(st *store.Store, meta model.SessionMeta) (model.SessionMeta, error) {
	finalized, _, err := finalizeTerminalWorkspace(context.Background(), st, meta)
	if err == nil || isSourceMutationError(err) {
		return finalized, nil
	}
	return workspaceIntegrityFailureMeta(finalized, err), err
}

func isSourceMutationError(err error) bool {
	var mutation *workspace.SourceMutatedError
	return errors.As(err, &mutation)
}
