package runner

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

type rootInvocationSpec struct {
	phase              string
	actor              string
	participantOrdinal int
	backend            Backend
	backendName        string
	profile            map[string]any
	prompt             string
	promptAvailable    bool
}

type rootInvocationProgress struct {
	persistedAttempts      int
	providerLaunchAttempts int
}

type rootInvocationPersistenceError struct{ cause error }

func (e rootInvocationPersistenceError) Error() string          { return e.cause.Error() }
func (e rootInvocationPersistenceError) Unwrap() error          { return e.cause }
func (e rootInvocationPersistenceError) suppressProviderRetry() {}

// rootProviderInvocationAfterSave is a test-only failpoint after a provider
// invocation ref is durable in metadata. Production leaves it nil.
var rootProviderInvocationAfterSave func(rootInvocationSpec, int) error

// rootProviderLaunchMarkerAfterSave is a test-only failpoint after the durable
// launch marker is written but before the provider call begins.
var rootProviderLaunchMarkerAfterSave func(rootInvocationSpec, int) error

// rootProviderResultAfterSave is a test-only failpoint after a provider result
// is durable but before provider_attempts.json is completed. Production leaves
// it nil.
var rootProviderResultAfterSave func(rootInvocationSpec, int) error

func (s *rootExecutionState) recordsProviderInvocations() bool {
	if s == nil || s.preflight == nil {
		return false
	}
	return s.meta.String("prompt_policy_version") == PromptPolicyVersionV2 ||
		intFromAny(s.preflight.rootPlan["schema_version"], 1) == 2
}

func (s *rootExecutionState) persistRenderedPrompt(spec rootInvocationSpec) (map[string]any, string, error) {
	if !s.recordsProviderInvocations() || !spec.promptAvailable {
		return nil, "", nil
	}
	record := contracts.RenderedPromptRecord([]byte(spec.prompt))
	if _, err := contracts.ValidateRenderedPromptRecord(record); err != nil {
		return nil, "", err
	}
	payload, err := contracts.NormalizeRootArtifactVersion(
		contracts.RootArtifactKindRenderedPrompt,
		contracts.RootArtifactSchemaVersionV2,
		map[string]any{"rendered_prompt": record},
	)
	if err != nil {
		return nil, "", err
	}
	ref, err := saveRootArtifact(s.st, contracts.RootArtifactKindRenderedPrompt, s.renderedPromptOrdinal(spec), payload)
	if err != nil {
		return nil, "", err
	}
	s.meta = appendUniqueMetaRef(s.meta, "rendered_prompt_refs", ref)
	if err := s.saveProgress(); err != nil {
		return nil, "", err
	}
	return ref, stringFromAny(record["raw_digest"]), nil
}

func (s *rootExecutionState) persistProviderInvocation(
	spec rootInvocationSpec,
	runnerAttempt int,
	startedAt string,
	result TurnResult,
	runErr error,
	providerLaunchAttempted bool,
	failureStage string,
	artifactOrdinal int,
	promptRef map[string]any,
	promptDigest string,
) (TurnResult, error) {
	if !s.recordsProviderInvocations() {
		return result, nil
	}
	progress, err := s.ensureInvocationProgress()
	if err != nil {
		return result, err
	}
	invocationID := logicalInvocationID(spec)
	current := progress[invocationID]
	if runnerAttempt != current.persistedAttempts+1 {
		return result, contracts.NewValidationError("provider invocation runner_attempt must continue persisted sequence for %s", invocationID)
	}
	policy := rootProviderRetryPolicy(s.meta)
	if policy == recipes.ProviderRetryForbid && providerLaunchAttempted && current.providerLaunchAttempts > 0 {
		return result, providerRetryForbiddenTerminalDiagnostic()
	}
	providerResult := providerResultForTurn(spec.backendName, result)
	outcome, classification := invocationOutcome(providerResult, runErr)
	if failureStage == "" && runErr != nil {
		failureStage = "provider"
	}
	completedAt := utcNow()
	manifestRefs := []any{}
	if s.persisted.inputManifestRef != nil {
		manifestRefs = append(manifestRefs, cloneMap(s.persisted.inputManifestRef))
	}
	if artifactOrdinal < 1 {
		artifactOrdinal, err = s.nextProviderInvocationArtifactOrdinal()
		if err != nil {
			return result, err
		}
	}
	recordFields := map[string]any{
		"invocation_id":             invocationID,
		"phase":                     spec.phase,
		"actor":                     spec.actor,
		"slot":                      invocationSlot(spec),
		"participant_ordinal":       emptyPositiveInt(spec.participantOrdinal),
		"reducer_fresh":             spec.phase == "reducer",
		"rendered_prompt_ref":       emptyMapAsNil(promptRef),
		"rendered_prompt_digest":    emptyStringAsNil(promptDigest),
		"recipe_ref":                cloneMap(s.persisted.recipeRef),
		"root_recipe_plan_ref":      cloneMap(s.persisted.rootPlanRef),
		"selected_contract_ref":     emptyMapAsNil(s.persisted.contractRef),
		"named_input_manifest_refs": manifestRefs,
		"backend":                   spec.backendName,
		"requested_model":           emptyStringAsNil(stringFromAny(spec.profile["model"])),
		"requested_effort":          emptyStringAsNil(stringFromAny(spec.profile["effort"])),
		"provider_session_id":       providerSessionID(spec.backend),
		"workspace_ref":             cloneMap(s.persisted.workspaceRef),
		"mapped_working_directory":  s.mappedWorkingDirectory(),
		"runner_attempt":            runnerAttempt,
		"provider_launch_attempted": providerLaunchAttempted,
		"provider_retry":            policy,
		"started_at":                startedAt,
		"completed_at":              completedAt,
		"outcome":                   outcome,
		"failure_stage":             emptyStringAsNil(failureStage),
		"classification":            emptyStringAsNil(classification),
		"provider_result_ref":       nil,
	}
	if providerLaunchAttempted {
		resultRef, err := s.persistProviderResult(artifactOrdinal, recordFields, providerResult)
		if err != nil {
			return result, err
		}
		result.ProviderResultRef = cloneMap(resultRef)
		if rootProviderResultAfterSave != nil {
			if err := rootProviderResultAfterSave(spec, runnerAttempt); err != nil {
				return result, err
			}
		}
		if err := s.completeProviderAttemptMarker(spec, runnerAttempt, artifactOrdinal, completedAt, outcome, failureStage, classification, resultRef); err != nil {
			return result, err
		}
		recordFields["provider_result_ref"] = cloneMap(resultRef)
	}
	record, err := contracts.ProviderInvocationRecord(recordFields)
	if err != nil {
		return result, err
	}
	payload, err := contracts.NormalizeRootArtifactVersion(
		contracts.RootArtifactKindProviderInvocation,
		contracts.RootArtifactSchemaVersionV2,
		map[string]any{"invocation": record},
	)
	if err != nil {
		return result, err
	}
	ref, err := saveRootArtifact(s.st, contracts.RootArtifactKindProviderInvocation, artifactOrdinal, payload)
	if err != nil {
		return result, err
	}
	s.meta = withRootInvocationRef(s.meta, ref)
	if err := s.saveProgress(); err != nil {
		return result, err
	}
	if providerLaunchAttempted {
		if err := s.bindProviderAttemptInvocationRef(spec, runnerAttempt, artifactOrdinal, ref); err != nil {
			return result, err
		}
	}
	current.persistedAttempts++
	if providerLaunchAttempted {
		current.providerLaunchAttempts++
	}
	progress[invocationID] = current
	if rootProviderInvocationAfterSave != nil {
		if err := rootProviderInvocationAfterSave(spec, runnerAttempt); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *rootExecutionState) recordUnlaunchedInvocation(spec rootInvocationSpec, failureStage string, cause error) error {
	if !s.recordsProviderInvocations() {
		return nil
	}
	attempt, err := s.nextProviderInvocationAttempt(spec)
	if err != nil {
		return err
	}
	_, err = s.persistProviderInvocation(spec, attempt, utcNow(), TurnResult{}, cause, false, failureStage, 0, nil, "")
	return err
}

func (s *rootExecutionState) nextProviderInvocationAttempt(spec rootInvocationSpec) (int, error) {
	progress, err := s.ensureInvocationProgress()
	if err != nil {
		return 0, err
	}
	return progress[logicalInvocationID(spec)].persistedAttempts + 1, nil
}

func (s *rootExecutionState) providerInvocationProgress(spec rootInvocationSpec) (rootInvocationProgress, error) {
	progress, err := s.ensureInvocationProgress()
	if err != nil {
		return rootInvocationProgress{}, err
	}
	return progress[logicalInvocationID(spec)], nil
}

func (s *rootExecutionState) ensureProviderLaunchAllowed(spec rootInvocationSpec) error {
	if rootProviderRetryPolicy(s.meta) != recipes.ProviderRetryForbid {
		return nil
	}
	progress, err := s.providerInvocationProgress(spec)
	if err != nil {
		return err
	}
	if progress.providerLaunchAttempts > 0 {
		return providerRetryForbiddenTerminalDiagnostic()
	}
	return nil
}

func (s *rootExecutionState) ensureInvocationProgress() (map[string]rootInvocationProgress, error) {
	if s.invocationProgress != nil {
		return s.invocationProgress, nil
	}
	progress, err := validatePersistedRootInvocationRecords(s.st, s.meta)
	if err != nil {
		return nil, err
	}
	s.invocationProgress = progress
	return progress, nil
}

func cloneRootInvocationProgress(source map[string]rootInvocationProgress) map[string]rootInvocationProgress {
	result := make(map[string]rootInvocationProgress, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func rootProviderRetryPolicy(meta model.SessionMeta) string {
	if strings.TrimSpace(meta.String("provider_retry")) == recipes.ProviderRetryForbid {
		return recipes.ProviderRetryForbid
	}
	return recipes.ProviderRetryAllow
}

func (s *rootExecutionState) renderedPromptOrdinal(spec rootInvocationSpec) int {
	ordinal := spec.participantOrdinal
	if ordinal < 1 {
		ordinal = s.meta.Int("participant_turns", 0) + 1
	}
	switch spec.phase {
	case "participant":
		return ordinal*2 - 1
	case "facilitator":
		return ordinal * 2
	default:
		return s.meta.Int("participant_turns", 0)*2 + 1
	}
}

func logicalInvocationID(spec rootInvocationSpec) string {
	ordinal := spec.participantOrdinal
	if ordinal < 1 {
		ordinal = 1
	}
	return fmt.Sprintf("%s:%06d", spec.phase, ordinal)
}

func invocationSlot(spec rootInvocationSpec) string {
	if spec.backend != nil && strings.TrimSpace(spec.backend.SlotID()) != "" {
		return spec.backend.SlotID()
	}
	return spec.phase
}

func providerSessionID(backend Backend) any {
	if backend == nil {
		return nil
	}
	state := backend.SessionState()
	for _, key := range []string{"thread_id", "session_id", "session_ref"} {
		if value := strings.TrimSpace(stringFromAny(state[key])); value != "" {
			return value
		}
	}
	return nil
}

func invocationOutcome(result ProviderResult, runErr error) (string, string) {
	switch {
	case errors.Is(runErr, context.Canceled):
		return "canceled", "canceled"
	case result.TimedOut:
		return "timed_out", "timeout"
	case result.Stalled:
		return "stalled", "stall"
	case runErr != nil:
		return "failed", providerFailureCategory(providerFailureDetail(runErr, result))
	default:
		return "completed", ""
	}
}

func (s *rootExecutionState) mappedWorkingDirectory() string {
	identity, _ := s.persisted.workspaceArtifact["identity"].(map[string]any)
	value := strings.TrimSpace(stringFromAny(identity["launch_subpath"]))
	if value == "" {
		source, _ := s.persisted.workspaceArtifact["source"].(map[string]any)
		value = strings.TrimSpace(stringFromAny(source["launch_subpath"]))
	}
	value = path.Clean(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || value == "/" || value == ".." || strings.HasPrefix(value, "../") || strings.Contains(value, ":") {
		return "."
	}
	return value
}

func (s *rootExecutionState) participantProfile(ordinal int) map[string]any {
	profiles, _ := s.preflight.rootPlan["participants"].([]any)
	if len(profiles) == 0 {
		return nil
	}
	index := (ordinal - 1) % len(profiles)
	profile, _ := profiles[index].(map[string]any)
	return profile
}

func emptyPositiveInt(value int) any {
	if value < 1 {
		return nil
	}
	return value
}

func emptyMapAsNil(value map[string]any) any {
	if len(value) == 0 {
		return nil
	}
	return cloneMap(value)
}

func (s *rootExecutionState) persistProviderResult(
	artifactOrdinal int,
	invocationDraft map[string]any,
	providerResult ProviderResult,
) (map[string]any, error) {
	draft := cloneMap(invocationDraft)
	draft["schema_version"] = contracts.ProviderInvocationV2
	draft["provider_result_ref"] = nil
	record, err := contracts.ProviderResultRecord(map[string]any{
		"invocation_id":   draft["invocation_id"],
		"phase":           draft["phase"],
		"actor":           draft["actor"],
		"runner_attempt":  draft["runner_attempt"],
		"provider_retry":  draft["provider_retry"],
		"backend":         draft["backend"],
		"started_at":      draft["started_at"],
		"completed_at":    draft["completed_at"],
		"outcome":         draft["outcome"],
		"failure_stage":   draft["failure_stage"],
		"classification":  draft["classification"],
		"provider_result": sanitizedProviderResultMap(providerResult),
		"invocation":      draft,
	})
	if err != nil {
		return nil, err
	}
	payload, err := contracts.NormalizeRootArtifactVersion(
		contracts.RootArtifactKindProviderResult,
		contracts.RootArtifactSchemaVersionV2,
		record,
	)
	if err != nil {
		return nil, err
	}
	ref, err := saveRootArtifact(s.st, contracts.RootArtifactKindProviderResult, artifactOrdinal, payload)
	if err != nil {
		return nil, err
	}
	persisted, err := s.st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, err
	}
	if _, err := contracts.ValidateProviderResultRecord(persisted); err != nil {
		return nil, err
	}
	return ref, nil
}

func (s *rootExecutionState) nextProviderInvocationArtifactOrdinal() (int, error) {
	maximum := 0
	for _, raw := range s.meta.Slice("invocation_refs") {
		ref, _ := raw.(map[string]any)
		ordinal, err := rootArtifactOrdinalFromRef(ref, contracts.RootArtifactKindProviderInvocation)
		if err == nil && ordinal > maximum {
			maximum = ordinal
		}
	}
	markers, err := loadRootProviderAttemptMarkers(s.st)
	if err != nil {
		return 0, err
	}
	for _, marker := range markers {
		if marker.ArtifactOrdinal > maximum {
			maximum = marker.ArtifactOrdinal
		}
	}
	return maximum + 1, nil
}

func (s *rootExecutionState) recordProviderAttemptLaunchMarker(
	spec rootInvocationSpec,
	runnerAttempt int,
	artifactOrdinal int,
	startedAt string,
) error {
	markers, err := loadRootProviderAttemptMarkers(s.st)
	if err != nil {
		return err
	}
	marker := rootProviderAttemptMarker{
		ArtifactOrdinal: artifactOrdinal,
		InvocationID:    logicalInvocationID(spec),
		Phase:           spec.phase,
		Actor:           spec.actor,
		RunnerAttempt:   runnerAttempt,
		ProviderRetry:   rootProviderRetryPolicy(s.meta),
		Backend:         spec.backendName,
		StartedAt:       startedAt,
	}
	for _, existing := range markers {
		if existing.ArtifactOrdinal == artifactOrdinal || providerAttemptKey(existing.InvocationID, existing.RunnerAttempt) == providerAttemptKey(marker.InvocationID, marker.RunnerAttempt) {
			return contracts.NewValidationError("provider attempt marker already exists for %s attempt %d", marker.InvocationID, marker.RunnerAttempt)
		}
	}
	markers = append(markers, marker)
	return saveRootProviderAttemptMarkers(s.st, markers)
}

func (s *rootExecutionState) completeProviderAttemptMarker(
	spec rootInvocationSpec,
	runnerAttempt int,
	artifactOrdinal int,
	completedAt string,
	outcome string,
	failureStage string,
	classification string,
	resultRef map[string]any,
) error {
	return s.updateProviderAttemptMarker(spec, runnerAttempt, artifactOrdinal, func(marker *rootProviderAttemptMarker) {
		marker.CompletedAt = completedAt
		marker.Outcome = outcome
		marker.FailureStage = failureStage
		marker.Classification = classification
		marker.ProviderResultRef = cloneMap(resultRef)
	})
}

func (s *rootExecutionState) bindProviderAttemptInvocationRef(
	spec rootInvocationSpec,
	runnerAttempt int,
	artifactOrdinal int,
	invocationRef map[string]any,
) error {
	return s.updateProviderAttemptMarker(spec, runnerAttempt, artifactOrdinal, func(marker *rootProviderAttemptMarker) {
		marker.ProviderInvocationRef = cloneMap(invocationRef)
	})
}

func (s *rootExecutionState) updateProviderAttemptMarker(
	spec rootInvocationSpec,
	runnerAttempt int,
	artifactOrdinal int,
	update func(*rootProviderAttemptMarker),
) error {
	return s.updateProviderAttemptMarkerByIdentity(logicalInvocationID(spec), runnerAttempt, artifactOrdinal, update)
}

func (s *rootExecutionState) updateProviderAttemptMarkerByIdentity(
	invocationID string,
	runnerAttempt int,
	artifactOrdinal int,
	update func(*rootProviderAttemptMarker),
) error {
	markers, err := loadRootProviderAttemptMarkers(s.st)
	if err != nil {
		return err
	}
	key := providerAttemptKey(invocationID, runnerAttempt)
	for index := range markers {
		if markers[index].ArtifactOrdinal == artifactOrdinal && providerAttemptKey(markers[index].InvocationID, markers[index].RunnerAttempt) == key {
			update(&markers[index])
			return saveRootProviderAttemptMarkers(s.st, markers)
		}
	}
	return contracts.NewValidationError("provider attempt marker is missing for %s attempt %d", invocationID, runnerAttempt)
}

func appendUniqueMetaRef(meta model.SessionMeta, field string, ref map[string]any) model.SessionMeta {
	for _, raw := range meta.Slice(field) {
		existing, _ := raw.(map[string]any)
		if existing["id"] == ref["id"] && existing["digest"] == ref["digest"] {
			return meta
		}
	}
	return meta.AppendToSlice(field, ref)
}

func withRootInvocationRef(meta model.SessionMeta, ref map[string]any) model.SessionMeta {
	if ref == nil {
		return meta
	}
	wantID := strings.TrimSpace(stringFromAny(ref["id"]))
	refs := append([]any{}, meta.Slice("invocation_refs")...)
	replaced := false
	for index, raw := range refs {
		candidate, _ := raw.(map[string]any)
		if strings.TrimSpace(stringFromAny(candidate["id"])) == wantID {
			refs[index] = cloneMap(ref)
			replaced = true
		}
	}
	if !replaced {
		refs = append(refs, cloneMap(ref))
	}
	sort.SliceStable(refs, func(left int, right int) bool {
		leftRef, _ := refs[left].(map[string]any)
		rightRef, _ := refs[right].(map[string]any)
		leftOrdinal, leftErr := rootArtifactOrdinalFromRef(leftRef, contracts.RootArtifactKindProviderInvocation)
		rightOrdinal, rightErr := rootArtifactOrdinalFromRef(rightRef, contracts.RootArtifactKindProviderInvocation)
		if leftErr != nil || rightErr != nil {
			return stringFromAny(leftRef["id"]) < stringFromAny(rightRef["id"])
		}
		return leftOrdinal < rightOrdinal
	})
	return meta.With("invocation_refs", refs)
}

type rootInvocationValidationState struct {
	progress                   map[string]rootInvocationProgress
	pendingProviderInvocations []rootProviderAttemptMarker
}

type rootInvocationFact struct {
	invocationID  string
	runnerAttempt int
	launched      bool
	ordinal       int
}

func validatePersistedRootInvocationRecords(st *store.Store, meta model.SessionMeta) (map[string]rootInvocationProgress, error) {
	state, err := validatePersistedRootInvocationState(st, meta)
	if err != nil {
		return nil, err
	}
	return state.progress, nil
}

func validatePersistedRootInvocationState(st *store.Store, meta model.SessionMeta) (*rootInvocationValidationState, error) {
	promptDigests := map[string]string{}
	for index, raw := range meta.Slice("rendered_prompt_refs") {
		ref, _ := raw.(map[string]any)
		artifact, err := loadRootRecoveryArtifactRef(st, ref, contracts.RootArtifactKindRenderedPrompt, index+1)
		if err != nil {
			return nil, err
		}
		record, err := contracts.ValidateRenderedPromptRecord(artifact.payload["rendered_prompt"])
		if err != nil {
			return nil, persistenceIntegrityError("Persisted rendered prompt is invalid.", map[string]any{"ordinal": index + 1, "cause": err.Error()})
		}
		promptDigests[artifactRefKey(ref)] = stringFromAny(record["raw_digest"])
	}

	markers, err := loadRootProviderAttemptMarkers(st)
	if err != nil {
		return nil, err
	}
	markerByOrdinal := map[int]rootProviderAttemptMarker{}
	for _, marker := range markers {
		markerByOrdinal[marker.ArtifactOrdinal] = marker
	}

	facts := []rootInvocationFact{}
	invocationOrdinals := map[int]bool{}
	previousOrdinal := 0
	for _, raw := range meta.Slice("invocation_refs") {
		ref, _ := raw.(map[string]any)
		ordinal, err := rootArtifactOrdinalFromRef(ref, contracts.RootArtifactKindProviderInvocation)
		if err != nil {
			return nil, err
		}
		if ordinal <= previousOrdinal {
			return nil, persistenceIntegrityError("Persisted provider invocation refs must be ordered by root artifact ordinal.", nil)
		}
		previousOrdinal = ordinal
		artifact, err := loadRootRecoveryArtifactRef(st, ref, contracts.RootArtifactKindProviderInvocation, ordinal)
		if err != nil {
			return nil, err
		}
		record, err := contracts.ValidateProviderInvocationRecord(artifact.payload["invocation"])
		if err != nil {
			return nil, persistenceIntegrityError("Persisted provider invocation is invalid.", map[string]any{"ordinal": ordinal, "cause": err.Error()})
		}
		invocationID := stringFromAny(record["invocation_id"])
		promptRef, _ := record["rendered_prompt_ref"].(map[string]any)
		if promptRef == nil {
			if record["provider_launch_attempted"] == true {
				return nil, persistenceIntegrityError("Launched provider invocation is missing its rendered prompt ref.", map[string]any{"invocation_id": invocationID})
			}
		} else if _, err := contracts.ValidateArtifactRef(promptRef); err != nil {
			return nil, persistenceIntegrityError("Provider invocation rendered prompt ref is invalid.", map[string]any{"invocation_id": invocationID, "cause": err.Error()})
		} else if digest, ok := promptDigests[artifactRefKey(promptRef)]; !ok || digest != record["rendered_prompt_digest"] {
			return nil, persistenceIntegrityError("Provider invocation rendered prompt binding is invalid.", map[string]any{"invocation_id": invocationID})
		}
		if record["provider_launch_attempted"] == true {
			resultRef, _ := record["provider_result_ref"].(map[string]any)
			resultArtifact, err := validateRootProviderResultRef(st, resultRef, ordinal)
			if err != nil {
				return nil, persistenceIntegrityError("Provider invocation result binding is invalid.", map[string]any{"invocation_id": invocationID, "cause": err.Error()})
			}
			if _, _, err := contracts.ValidateProviderInvocationResultBinding(record, resultArtifact.payload); err != nil {
				return nil, persistenceIntegrityError("Provider invocation result binding is invalid.", map[string]any{"invocation_id": invocationID, "cause": err.Error()})
			}
			if marker, ok := markerByOrdinal[ordinal]; ok {
				if err := validateProviderAttemptMarkerAgainstInvocation(marker, record, ref); err != nil {
					return nil, err
				}
			}
		}
		invocationOrdinals[ordinal] = true
		facts = append(facts, rootInvocationFact{
			invocationID:  invocationID,
			runnerAttempt: intFromAny(record["runner_attempt"], 0),
			launched:      record["provider_launch_attempted"] == true,
			ordinal:       ordinal,
		})
	}

	pending := []rootProviderAttemptMarker{}
	for _, marker := range markers {
		if invocationOrdinals[marker.ArtifactOrdinal] {
			continue
		}
		if marker.ProviderResultRef == nil {
			resultArtifact, found, err := loadLatestRootRecoveryArtifact(st, contracts.RootArtifactKindProviderResult, marker.ArtifactOrdinal)
			if err != nil {
				return nil, persistenceIntegrityError("Durable provider result discovery failed.", map[string]any{"invocation_id": marker.InvocationID, "cause": err.Error()})
			}
			if found {
				record, err := contracts.ValidateProviderResultRecord(resultArtifact.payload)
				if err != nil {
					return nil, persistenceIntegrityError("Discovered durable provider result is invalid.", map[string]any{"invocation_id": marker.InvocationID, "cause": err.Error()})
				}
				if err := validateProviderAttemptMarkerAgainstResult(marker, resultArtifact.payload); err != nil {
					return nil, err
				}
				marker.ProviderResultRef = cloneMap(resultArtifact.ref)
				marker.CompletedAt = stringFromAny(record["completed_at"])
				marker.Outcome = stringFromAny(record["outcome"])
				marker.FailureStage = stringFromAny(record["failure_stage"])
				marker.Classification = stringFromAny(record["classification"])
			}
		}
		if marker.ProviderInvocationRef != nil {
			ordinal, err := rootArtifactOrdinalFromRef(marker.ProviderInvocationRef, contracts.RootArtifactKindProviderInvocation)
			if err != nil || ordinal != marker.ArtifactOrdinal {
				return nil, persistenceIntegrityError("Provider attempt marker invocation ref identity is invalid.", map[string]any{"invocation_id": marker.InvocationID})
			}
			artifact, err := loadRootRecoveryArtifactRef(st, marker.ProviderInvocationRef, contracts.RootArtifactKindProviderInvocation, marker.ArtifactOrdinal)
			if err != nil {
				return nil, err
			}
			record, err := contracts.ValidateProviderInvocationRecord(artifact.payload["invocation"])
			if err != nil {
				return nil, persistenceIntegrityError("Provider attempt marker invocation record is invalid.", map[string]any{"invocation_id": marker.InvocationID, "cause": err.Error()})
			}
			if err := validateProviderAttemptMarkerAgainstInvocation(marker, record, marker.ProviderInvocationRef); err != nil {
				return nil, err
			}
			pending = append(pending, marker)
		} else if marker.ProviderResultRef != nil {
			resultArtifact, err := validateRootProviderResultRef(st, marker.ProviderResultRef, marker.ArtifactOrdinal)
			if err != nil {
				return nil, persistenceIntegrityError("Provider attempt marker result ref is invalid.", map[string]any{"invocation_id": marker.InvocationID, "cause": err.Error()})
			}
			if err := validateProviderAttemptMarkerAgainstResult(marker, resultArtifact.payload); err != nil {
				return nil, err
			}
			pending = append(pending, marker)
		}
		facts = append(facts, rootInvocationFact{
			invocationID:  marker.InvocationID,
			runnerAttempt: marker.RunnerAttempt,
			launched:      true,
			ordinal:       marker.ArtifactOrdinal,
		})
	}
	progress, err := rootInvocationProgressFromFacts(facts, meta)
	if err != nil {
		return nil, err
	}
	return &rootInvocationValidationState{progress: progress, pendingProviderInvocations: pending}, nil
}

func artifactRefKey(ref map[string]any) string {
	return stringFromAny(ref["id"]) + "\x00" + stringFromAny(ref["digest"])
}

func validateRootProviderResultRef(st *store.Store, ref map[string]any, ordinal int) (*persistedRootRecoveryArtifact, error) {
	resultOrdinal, err := rootArtifactOrdinalFromRef(ref, contracts.RootArtifactKindProviderResult)
	if err != nil {
		return nil, err
	}
	if resultOrdinal != ordinal {
		return nil, contracts.NewValidationError("provider result ordinal must match provider invocation ordinal")
	}
	artifact, err := loadRootRecoveryArtifactRef(st, ref, contracts.RootArtifactKindProviderResult, ordinal)
	if err != nil {
		return nil, err
	}
	if _, err := contracts.ValidateProviderResultRecord(artifact.payload); err != nil {
		return nil, err
	}
	return artifact, nil
}

func validateProviderAttemptMarkerAgainstInvocation(marker rootProviderAttemptMarker, invocation map[string]any, invocationRef map[string]any) error {
	if marker.InvocationID != stringFromAny(invocation["invocation_id"]) ||
		marker.Phase != stringFromAny(invocation["phase"]) ||
		marker.Actor != stringFromAny(invocation["actor"]) ||
		marker.RunnerAttempt != intFromAny(invocation["runner_attempt"], 0) ||
		marker.ProviderRetry != stringFromAny(invocation["provider_retry"]) ||
		marker.Backend != stringFromAny(invocation["backend"]) ||
		marker.StartedAt != stringFromAny(invocation["started_at"]) {
		return persistenceIntegrityError("Provider attempt marker does not match provider invocation.", map[string]any{"invocation_id": marker.InvocationID})
	}
	if invocation["provider_launch_attempted"] != true {
		return persistenceIntegrityError("Provider attempt marker cannot bind an unlaunched invocation.", map[string]any{"invocation_id": marker.InvocationID})
	}
	if marker.ProviderResultRef != nil {
		resultRef, _ := invocation["provider_result_ref"].(map[string]any)
		if requireMatchingArtifactRef(marker.ProviderResultRef, resultRef, "provider attempt marker result ref") != nil {
			return persistenceIntegrityError("Provider attempt marker result ref does not match provider invocation.", map[string]any{"invocation_id": marker.InvocationID})
		}
	}
	if marker.ProviderInvocationRef != nil && requireMatchingArtifactRef(marker.ProviderInvocationRef, invocationRef, "provider attempt marker invocation ref") != nil {
		return persistenceIntegrityError("Provider attempt marker invocation ref does not match metadata.", map[string]any{"invocation_id": marker.InvocationID})
	}
	return nil
}

func validateProviderAttemptMarkerAgainstResult(marker rootProviderAttemptMarker, result map[string]any) error {
	record, err := contracts.ValidateProviderResultRecord(result)
	if err != nil {
		return err
	}
	if marker.InvocationID != stringFromAny(record["invocation_id"]) ||
		marker.Phase != stringFromAny(record["phase"]) ||
		marker.Actor != stringFromAny(record["actor"]) ||
		marker.RunnerAttempt != intFromAny(record["runner_attempt"], 0) ||
		marker.ProviderRetry != stringFromAny(record["provider_retry"]) ||
		marker.Backend != stringFromAny(record["backend"]) ||
		marker.StartedAt != stringFromAny(record["started_at"]) {
		return persistenceIntegrityError("Provider attempt marker does not match provider result.", map[string]any{"invocation_id": marker.InvocationID})
	}
	return nil
}

func rootInvocationProgressFromFacts(facts []rootInvocationFact, meta model.SessionMeta) (map[string]rootInvocationProgress, error) {
	byInvocation := map[string][]rootInvocationFact{}
	for _, fact := range facts {
		byInvocation[fact.invocationID] = append(byInvocation[fact.invocationID], fact)
	}
	progress := map[string]rootInvocationProgress{}
	for invocationID, items := range byInvocation {
		sort.Slice(items, func(i, j int) bool {
			if items[i].runnerAttempt != items[j].runnerAttempt {
				return items[i].runnerAttempt < items[j].runnerAttempt
			}
			return items[i].ordinal < items[j].ordinal
		})
		current := rootInvocationProgress{}
		for index, item := range items {
			if item.runnerAttempt != index+1 {
				return nil, persistenceIntegrityError("Persisted provider invocation attempt sequence is invalid.", map[string]any{"invocation_id": invocationID})
			}
			current.persistedAttempts++
			if item.launched {
				current.providerLaunchAttempts++
			}
		}
		if rootProviderRetryPolicy(meta) == recipes.ProviderRetryForbid && current.providerLaunchAttempts > 1 {
			return nil, persistenceIntegrityError("Persisted provider invocation attempt sequence is invalid.", map[string]any{"invocation_id": invocationID})
		}
		progress[invocationID] = current
	}
	return progress, nil
}

func rootArtifactOrdinalFromRef(ref map[string]any, kind string) (int, error) {
	artifactRef, err := contracts.ValidateArtifactRef(ref)
	if err != nil {
		return 0, err
	}
	refID := strings.TrimSpace(stringFromAny(artifactRef["id"]))
	prefix := kind + ":"
	if !strings.HasPrefix(refID, prefix) {
		return 0, contracts.NewValidationError("root artifact ref id %q does not use %s prefix", refID, prefix)
	}
	return contracts.RootArtifactOrdinalFromID(kind, strings.TrimPrefix(refID, prefix))
}
