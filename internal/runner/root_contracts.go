package runner

import (
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/namedinputs"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const (
	RootInitializationStateInitializing       = "initializing"
	RootInitializationStateFailed             = "initialization_failed"
	RootInitializationStateReady              = "ready"
	RootNamedInputIntegrityStopReason         = namedinputs.DiagnosticCodeIntegrity
	RootNamedInputIntegrityExecutionPhase     = "named_input_integrity_check"
	RootFailureCauseNamedInputIntegrity       = namedinputs.DiagnosticCodeIntegrity
	RootFailureCauseSourceMutated             = workspace.StopReasonSourceMutated
	RecipeWarningCodeDirtySourceCommittedHead = "dirty_source_committed_head"
)

// RecipeWarning is delivered by value and deliberately contains no maps,
// slices, or pointers. A callback may mutate its local copy without changing
// the warning retained by orchestration.
type RecipeWarning struct {
	Code                       string `json:"code"`
	Message                    string `json:"message"`
	StagedChanges              int64  `json:"staged_changes"`
	UnstagedChanges            int64  `json:"unstaged_changes"`
	UntrackedChanges           int64  `json:"untracked_changes"`
	WorkspaceContentSource     string `json:"workspace_content_source"`
	WorkingTreeChangesIncluded bool   `json:"working_tree_changes_included"`
}

type RecipeWarningCallback func(RecipeWarning)

func deliverRecipeWarning(callback RecipeWarningCallback, warning RecipeWarning) {
	if callback != nil {
		callback(warning)
	}
}

func namedInputIntegrityFailureMeta(meta model.SessionMeta, cause error, secondaryCauses ...string) model.SessionMeta {
	causes := append([]string{RootFailureCauseNamedInputIntegrity}, secondaryCauses...)
	if meta.Bool("source_mutated") {
		causes = append(causes, RootFailureCauseSourceMutated)
	}
	meta = withRootFailureCauses(meta, causes...).
		WithStatus("failed").
		With("stop_reason", RootNamedInputIntegrityStopReason).
		With("execution_phase", RootNamedInputIntegrityExecutionPhase).
		With("root_recovery_pending", false).
		With("failed_at", utcNow())
	if cause != nil {
		meta = meta.With("error", cause.Error())
	}
	return meta
}

func withRootFailureCauses(meta model.SessionMeta, additions ...string) model.SessionMeta {
	causes := map[string]bool{}
	for _, raw := range meta.Slice("failure_causes") {
		var cause string
		switch typed := raw.(type) {
		case string:
			cause = typed
		case map[string]any:
			cause = stringFromAny(typed["code"])
		}
		if cause = strings.TrimSpace(cause); cause != "" {
			causes[cause] = true
		}
	}
	for _, cause := range additions {
		if cause = strings.TrimSpace(cause); cause != "" {
			causes[cause] = true
		}
	}
	ordered := make([]string, 0, len(causes))
	for cause := range causes {
		ordered = append(ordered, cause)
	}
	sort.Slice(ordered, func(left int, right int) bool {
		leftRank := rootFailureCauseRank(ordered[left])
		rightRank := rootFailureCauseRank(ordered[right])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return ordered[left] < ordered[right]
	})
	projected := make([]any, 0, len(ordered))
	for _, cause := range ordered {
		projected = append(projected, cause)
	}
	return meta.With("failure_causes", projected)
}

func rootFailureCauseRank(cause string) int {
	switch cause {
	case RootFailureCauseNamedInputIntegrity:
		return 0
	case RootFailureCauseSourceMutated:
		return 1
	default:
		return 2
	}
}
