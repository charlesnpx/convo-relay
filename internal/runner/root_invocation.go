package runner

import (
	"context"
	"errors"
	"fmt"
	"path"
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
	promptRef map[string]any,
	promptDigest string,
) error {
	if !s.recordsProviderInvocations() {
		return nil
	}
	progress, err := s.ensureInvocationProgress()
	if err != nil {
		return err
	}
	invocationID := logicalInvocationID(spec)
	current := progress[invocationID]
	if runnerAttempt != current.persistedAttempts+1 {
		return contracts.NewValidationError("provider invocation runner_attempt must continue persisted sequence for %s", invocationID)
	}
	policy := rootProviderRetryPolicy(s.meta)
	if policy == recipes.ProviderRetryForbid && providerLaunchAttempted && current.providerLaunchAttempts > 0 {
		return providerRetryForbiddenTerminalDiagnostic()
	}
	providerResult := providerResultForTurn(spec.backendName, result)
	outcome, classification := invocationOutcome(providerResult, runErr)
	if failureStage == "" && runErr != nil {
		failureStage = "provider"
	}
	manifestRefs := []any{}
	if s.persisted.inputManifestRef != nil {
		manifestRefs = append(manifestRefs, cloneMap(s.persisted.inputManifestRef))
	}
	record, err := contracts.ProviderInvocationRecord(map[string]any{
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
		"completed_at":              utcNow(),
		"outcome":                   outcome,
		"failure_stage":             emptyStringAsNil(failureStage),
		"classification":            emptyStringAsNil(classification),
		"provider_result_ref":       nil,
	})
	if err != nil {
		return err
	}
	payload, err := contracts.NormalizeRootArtifactVersion(
		contracts.RootArtifactKindProviderInvocation,
		contracts.RootArtifactSchemaVersionV2,
		map[string]any{"invocation": record},
	)
	if err != nil {
		return err
	}
	ordinal := len(s.meta.Slice("invocation_refs")) + 1
	ref, err := saveRootArtifact(s.st, contracts.RootArtifactKindProviderInvocation, ordinal, payload)
	if err != nil {
		return err
	}
	s.meta = s.meta.AppendToSlice("invocation_refs", ref)
	if err := s.saveProgress(); err != nil {
		return err
	}
	current.persistedAttempts++
	if providerLaunchAttempted {
		current.providerLaunchAttempts++
	}
	progress[invocationID] = current
	if rootProviderInvocationAfterSave != nil {
		if err := rootProviderInvocationAfterSave(spec, runnerAttempt); err != nil {
			return err
		}
	}
	return nil
}

func (s *rootExecutionState) recordUnlaunchedInvocation(spec rootInvocationSpec, failureStage string, cause error) error {
	if !s.recordsProviderInvocations() {
		return nil
	}
	attempt, err := s.nextProviderInvocationAttempt(spec)
	if err != nil {
		return err
	}
	return s.persistProviderInvocation(spec, attempt, utcNow(), TurnResult{}, cause, false, failureStage, nil, "")
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

func appendUniqueMetaRef(meta model.SessionMeta, field string, ref map[string]any) model.SessionMeta {
	for _, raw := range meta.Slice(field) {
		existing, _ := raw.(map[string]any)
		if existing["id"] == ref["id"] && existing["digest"] == ref["digest"] {
			return meta
		}
	}
	return meta.AppendToSlice(field, ref)
}

func validatePersistedRootInvocationRecords(st *store.Store, meta model.SessionMeta) (map[string]rootInvocationProgress, error) {
	progress := map[string]rootInvocationProgress{}
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
	for index, raw := range meta.Slice("invocation_refs") {
		ref, _ := raw.(map[string]any)
		artifact, err := loadRootRecoveryArtifactRef(st, ref, contracts.RootArtifactKindProviderInvocation, index+1)
		if err != nil {
			return nil, err
		}
		record, err := contracts.ValidateProviderInvocationRecord(artifact.payload["invocation"])
		if err != nil {
			return nil, persistenceIntegrityError("Persisted provider invocation is invalid.", map[string]any{"ordinal": index + 1, "cause": err.Error()})
		}
		invocationID := stringFromAny(record["invocation_id"])
		current := progress[invocationID]
		current.persistedAttempts++
		if intFromAny(record["runner_attempt"], 0) != current.persistedAttempts {
			return nil, persistenceIntegrityError("Persisted provider invocation attempt sequence is invalid.", map[string]any{"invocation_id": invocationID})
		}
		if record["provider_launch_attempted"] == true {
			current.providerLaunchAttempts++
			if rootProviderRetryPolicy(meta) == recipes.ProviderRetryForbid && current.providerLaunchAttempts > 1 {
				return nil, persistenceIntegrityError("Persisted provider invocation attempt sequence is invalid.", map[string]any{"invocation_id": invocationID})
			}
		}
		progress[invocationID] = current
		promptRef, _ := record["rendered_prompt_ref"].(map[string]any)
		if promptRef == nil {
			if record["provider_launch_attempted"] == true {
				return nil, persistenceIntegrityError("Launched provider invocation is missing its rendered prompt ref.", map[string]any{"invocation_id": invocationID})
			}
			continue
		}
		if _, err := contracts.ValidateArtifactRef(promptRef); err != nil {
			return nil, persistenceIntegrityError("Provider invocation rendered prompt ref is invalid.", map[string]any{"invocation_id": invocationID, "cause": err.Error()})
		}
		if digest, ok := promptDigests[artifactRefKey(promptRef)]; !ok || digest != record["rendered_prompt_digest"] {
			return nil, persistenceIntegrityError("Provider invocation rendered prompt binding is invalid.", map[string]any{"invocation_id": invocationID})
		}
	}
	return progress, nil
}

func artifactRefKey(ref map[string]any) string {
	return stringFromAny(ref["id"]) + "\x00" + stringFromAny(ref["digest"])
}
