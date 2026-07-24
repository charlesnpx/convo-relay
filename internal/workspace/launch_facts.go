package workspace

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

const (
	SourceStagedChangesKey             = "source_staged_changes"
	SourceUnstagedChangesKey           = "source_unstaged_changes"
	SourceUnignoredUntrackedChangesKey = "source_unignored_untracked_changes"
	AllowDirtySourceRequestedKey       = "allow_dirty_source_requested"
)

// LaunchFacts is the list/show projection of dirty-source launch evidence.
type LaunchFacts struct {
	StagedChanges             int64
	UnstagedChanges           int64
	UnignoredUntrackedChanges int64
	AllowDirtySourceRequested bool
	Recorded                  bool
}

func (f LaunchFacts) Projection() map[string]any {
	if !f.Recorded {
		return map[string]any{}
	}
	return map[string]any{
		SourceStagedChangesKey:             f.StagedChanges,
		SourceUnstagedChangesKey:           f.UnstagedChanges,
		SourceUnignoredUntrackedChangesKey: f.UnignoredUntrackedChanges,
		AllowDirtySourceRequestedKey:       f.AllowDirtySourceRequested,
	}
}

func LaunchFactsFromArtifact(artifact map[string]any) (LaunchFacts, error) {
	if artifact == nil {
		return LaunchFacts{}, contracts.NewValidationError("execution_workspace artifact is required for launch facts")
	}
	keys := []string{
		SourceStagedChangesKey,
		SourceUnstagedChangesKey,
		SourceUnignoredUntrackedChangesKey,
		AllowDirtySourceRequestedKey,
	}
	present := 0
	for _, key := range keys {
		if _, ok := artifact[key]; ok {
			present++
		}
	}
	if present == 0 {
		return LaunchFacts{}, nil
	}
	if present != len(keys) {
		return LaunchFacts{}, contracts.NewValidationError("execution_workspace dirty-source launch facts are incomplete")
	}
	staged, stagedOK := nonnegativeInt64(artifact[SourceStagedChangesKey])
	unstaged, unstagedOK := nonnegativeInt64(artifact[SourceUnstagedChangesKey])
	untracked, untrackedOK := nonnegativeInt64(artifact[SourceUnignoredUntrackedChangesKey])
	allowed, allowedOK := artifact[AllowDirtySourceRequestedKey].(bool)
	if !stagedOK || !unstagedOK || !untrackedOK || !allowedOK {
		return LaunchFacts{}, contracts.NewValidationError("execution_workspace dirty-source launch facts are invalid")
	}
	policy, err := workspacePolicy(artifact)
	if err != nil {
		return LaunchFacts{}, err
	}
	if allowed && policy.achieved == PolicyInherited {
		return LaunchFacts{}, contracts.NewValidationError("inherited execution cannot record a dirty-source override")
	}
	if !allowed && policy.achieved != PolicyInherited && (staged > 0 || unstaged > 0 || untracked > 0) {
		return LaunchFacts{}, contracts.NewValidationError("dirty isolated execution requires an explicit dirty-source override")
	}
	return LaunchFacts{
		StagedChanges:             staged,
		UnstagedChanges:           unstaged,
		UnignoredUntrackedChanges: untracked,
		AllowDirtySourceRequested: allowed,
		Recorded:                  true,
	}, nil
}

func ValidateLaunchFactsProjection(meta map[string]any, artifact map[string]any) (LaunchFacts, error) {
	facts, err := LaunchFactsFromArtifact(artifact)
	if err != nil {
		return LaunchFacts{}, err
	}
	if !facts.Recorded {
		for _, key := range []string{
			SourceStagedChangesKey,
			SourceUnstagedChangesKey,
			SourceUnignoredUntrackedChangesKey,
			AllowDirtySourceRequestedKey,
		} {
			if _, exists := meta[key]; exists {
				return LaunchFacts{}, contracts.NewValidationError(
					"session dirty-source projection records %s without authoritative artifact evidence",
					key,
				)
			}
		}
		return facts, nil
	}
	projection := facts.Projection()
	for key, expected := range projection {
		observed, exists := meta[key]
		if !exists || !launchFactEqual(observed, expected) {
			return facts, contracts.NewValidationError(
				"session dirty-source projection disagrees with execution_workspace field %s",
				key,
			)
		}
	}
	return facts, nil
}

func launchFactEqual(observed any, expected any) bool {
	switch value := expected.(type) {
	case int64:
		parsed, ok := nonnegativeInt64(observed)
		return ok && parsed == value
	case bool:
		parsed, ok := observed.(bool)
		return ok && parsed == value
	default:
		return strings.TrimSpace(stringValue(observed)) == strings.TrimSpace(stringValue(expected))
	}
}

func nonnegativeInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), typed >= 0
	case int8:
		return int64(typed), typed >= 0
	case int16:
		return int64(typed), typed >= 0
	case int32:
		return int64(typed), typed >= 0
	case int64:
		return typed, typed >= 0
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case float32:
		parsed := int64(typed)
		return parsed, typed >= 0 && float32(parsed) == typed
	case float64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		parsed := int64(typed)
		return parsed, typed >= 0 && float64(parsed) == typed
	case json.Number:
		parsed, err := strconv.ParseInt(typed.String(), 10, 64)
		return parsed, err == nil && parsed >= 0
	default:
		return 0, false
	}
}
