package runner

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const rootProviderAttemptsFilename = "provider_attempts.json"

type rootProviderAttemptMarker struct {
	ArtifactOrdinal       int
	InvocationID          string
	Phase                 string
	Actor                 string
	RunnerAttempt         int
	ProviderRetry         string
	Backend               string
	StartedAt             string
	CompletedAt           string
	Outcome               string
	FailureStage          string
	Classification        string
	ProviderResultRef     map[string]any
	ProviderInvocationRef map[string]any
}

func loadRootProviderAttemptMarkers(st *store.Store) ([]rootProviderAttemptMarker, error) {
	if st == nil {
		return nil, nil
	}
	body, err := os.ReadFile(filepath.Join(st.Root, rootProviderAttemptsFilename))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	value, err := contracts.DecodeStrictJSONObjectBytes(body)
	if err != nil {
		return nil, err
	}
	if value["kind"] != "provider_attempts" || intFromAny(value["schema_version"], 0) != 1 {
		return nil, contracts.NewValidationError("provider_attempts marker has invalid envelope")
	}
	rawAttempts, ok := value["attempts"].([]any)
	if !ok {
		return nil, contracts.NewValidationError("provider_attempts marker requires attempts array")
	}
	markers := make([]rootProviderAttemptMarker, 0, len(rawAttempts))
	seenOrdinals := map[int]bool{}
	seenAttempts := map[string]bool{}
	for index, raw := range rawAttempts {
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, contracts.NewValidationError("provider_attempts[%d] must be an object", index)
		}
		marker, err := rootProviderAttemptMarkerFromMap(object)
		if err != nil {
			return nil, err
		}
		if seenOrdinals[marker.ArtifactOrdinal] {
			return nil, contracts.NewValidationError("provider_attempts marker has duplicate artifact ordinal")
		}
		seenOrdinals[marker.ArtifactOrdinal] = true
		key := providerAttemptKey(marker.InvocationID, marker.RunnerAttempt)
		if seenAttempts[key] {
			return nil, contracts.NewValidationError("provider_attempts marker has duplicate invocation attempt")
		}
		seenAttempts[key] = true
		markers = append(markers, marker)
	}
	sort.Slice(markers, func(i, j int) bool {
		return markers[i].ArtifactOrdinal < markers[j].ArtifactOrdinal
	})
	return markers, nil
}

func rootProviderAttemptMarkerFromMap(object map[string]any) (rootProviderAttemptMarker, error) {
	ordinal := intFromAny(object["artifact_ordinal"], 0)
	runnerAttempt := intFromAny(object["runner_attempt"], 0)
	marker := rootProviderAttemptMarker{
		ArtifactOrdinal: ordinal,
		InvocationID:    strings.TrimSpace(stringFromAny(object["invocation_id"])),
		Phase:           strings.TrimSpace(stringFromAny(object["phase"])),
		Actor:           strings.TrimSpace(stringFromAny(object["actor"])),
		RunnerAttempt:   runnerAttempt,
		ProviderRetry:   strings.TrimSpace(stringFromAny(object["provider_retry"])),
		Backend:         strings.TrimSpace(stringFromAny(object["backend"])),
		StartedAt:       strings.TrimSpace(stringFromAny(object["started_at"])),
		CompletedAt:     strings.TrimSpace(stringFromAny(object["completed_at"])),
		Outcome:         strings.TrimSpace(stringFromAny(object["outcome"])),
		FailureStage:    strings.TrimSpace(stringFromAny(object["failure_stage"])),
		Classification:  strings.TrimSpace(stringFromAny(object["classification"])),
	}
	if ordinal < 1 || runnerAttempt < 1 || marker.InvocationID == "" || marker.Phase == "" || marker.Actor == "" ||
		marker.ProviderRetry == "" || marker.Backend == "" || marker.StartedAt == "" {
		return rootProviderAttemptMarker{}, contracts.NewValidationError("provider_attempts marker attempt identity is incomplete")
	}
	if object["provider_launch_attempted"] != true {
		return rootProviderAttemptMarker{}, contracts.NewValidationError("provider_attempts marker only records launched attempts")
	}
	if ref, ok := object["provider_result_ref"].(map[string]any); ok && ref != nil {
		validated, err := contracts.ValidateArtifactRef(ref)
		if err != nil {
			return rootProviderAttemptMarker{}, err
		}
		marker.ProviderResultRef = validated
	} else if object["provider_result_ref"] != nil {
		return rootProviderAttemptMarker{}, contracts.NewValidationError("provider_attempts provider_result_ref must be null or artifact_ref")
	}
	if ref, ok := object["provider_invocation_ref"].(map[string]any); ok && ref != nil {
		validated, err := contracts.ValidateArtifactRef(ref)
		if err != nil {
			return rootProviderAttemptMarker{}, err
		}
		marker.ProviderInvocationRef = validated
	} else if object["provider_invocation_ref"] != nil {
		return rootProviderAttemptMarker{}, contracts.NewValidationError("provider_attempts provider_invocation_ref must be null or artifact_ref")
	}
	return marker, nil
}

func saveRootProviderAttemptMarkers(st *store.Store, markers []rootProviderAttemptMarker) error {
	sort.Slice(markers, func(i, j int) bool {
		return markers[i].ArtifactOrdinal < markers[j].ArtifactOrdinal
	})
	attempts := make([]any, 0, len(markers))
	for _, marker := range markers {
		attempts = append(attempts, marker.ToMap())
	}
	body, err := contracts.CanonicalJSONBytes(map[string]any{
		"kind":           "provider_attempts",
		"schema_version": 1,
		"attempts":       attempts,
	})
	if err != nil {
		return err
	}
	return st.WriteFileAtomically(filepath.Join(st.Root, rootProviderAttemptsFilename), body, 0o600)
}

func (m rootProviderAttemptMarker) ToMap() map[string]any {
	return map[string]any{
		"artifact_ordinal":          m.ArtifactOrdinal,
		"invocation_id":             m.InvocationID,
		"phase":                     m.Phase,
		"actor":                     m.Actor,
		"runner_attempt":            m.RunnerAttempt,
		"provider_launch_attempted": true,
		"provider_retry":            m.ProviderRetry,
		"backend":                   m.Backend,
		"started_at":                m.StartedAt,
		"completed_at":              emptyStringAsNil(m.CompletedAt),
		"outcome":                   emptyStringAsNil(m.Outcome),
		"failure_stage":             emptyStringAsNil(m.FailureStage),
		"classification":            emptyStringAsNil(m.Classification),
		"provider_result_ref":       emptyMapAsNil(m.ProviderResultRef),
		"provider_invocation_ref":   emptyMapAsNil(m.ProviderInvocationRef),
	}
}

func providerAttemptKey(invocationID string, runnerAttempt int) string {
	return strings.TrimSpace(invocationID) + "\x00" + strconv.Itoa(runnerAttempt)
}
