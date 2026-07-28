package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/namedinputs"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const defaultRootRetainedInputVerificationTimeout = 30 * time.Second

type rootRetainedInputVerifier func(
	context.Context,
	*store.Store,
	map[string]any,
	string,
	string,
	*int,
) error

type rootNamedInputIntegrityError struct {
	cause           error
	role            string
	boundary        string
	providerAttempt *int
	providerCause   error
	providerFailure map[string]any
}

func (e *rootNamedInputIntegrityError) Error() string {
	if e == nil || e.cause == nil {
		return "retained named input integrity verification failed"
	}
	return e.cause.Error()
}

func (e *rootNamedInputIntegrityError) Unwrap() []error {
	if e == nil {
		return nil
	}
	causes := []error{}
	if e.cause != nil {
		causes = append(causes, e.cause)
	}
	if e.providerCause != nil {
		causes = append(causes, e.providerCause)
	}
	return causes
}

// suppressProviderRetry keeps a provider's secondary retryable failure from
// overriding the primary retained-input integrity failure.
func (e *rootNamedInputIntegrityError) suppressProviderRetry() {}

func runRootProviderTurnWithRetainedIntegrity(
	ctx context.Context,
	state *rootExecutionState,
	spec rootInvocationSpec,
	operation func() (TurnResult, error),
) (TurnResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	promptRef, promptDigest, err := state.persistRenderedPrompt(spec)
	if err != nil {
		return TurnResult{}, rootInvocationPersistenceError{cause: err}
	}
	persistedProgress, err := state.providerInvocationProgress(spec)
	if err != nil {
		return TurnResult{}, rootInvocationPersistenceError{cause: err}
	}
	providerAttempt := persistedProgress.persistedAttempts
	return runWithProviderRetryPolicy(ctx, spec.actor, state.meta.String("provider_retry"), func() (TurnResult, error) {
		providerAttempt++
		currentAttempt := providerAttempt
		attemptRef := &currentAttempt
		startedAt := utcNow()
		if err := state.verifyRetainedInputs(
			ctx,
			spec.phase,
			namedinputs.IntegrityBoundaryBeforeAttempt,
			attemptRef,
		); err != nil {
			if _, recordErr := state.persistProviderInvocation(spec, currentAttempt, startedAt, TurnResult{}, err, false, "pre_launch_integrity", 0, promptRef, promptDigest); recordErr != nil {
				return TurnResult{}, rootInvocationPersistenceError{cause: errors.Join(err, recordErr)}
			}
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return TurnResult{}, ctx.Err()
			}
			return TurnResult{}, &rootNamedInputIntegrityError{
				cause:           err,
				role:            spec.phase,
				boundary:        namedinputs.IntegrityBoundaryBeforeAttempt,
				providerAttempt: attemptRef,
			}
		}

		if err := state.ensureProviderLaunchAllowed(spec); err != nil {
			return TurnResult{}, err
		}
		artifactOrdinal := 0
		if state.recordsProviderInvocations() {
			var ordinalErr error
			artifactOrdinal, ordinalErr = state.nextProviderInvocationArtifactOrdinal()
			if ordinalErr != nil {
				return TurnResult{}, rootInvocationPersistenceError{cause: ordinalErr}
			}
			if err := state.recordProviderAttemptLaunchMarker(spec, currentAttempt, artifactOrdinal, startedAt); err != nil {
				return TurnResult{}, rootInvocationPersistenceError{cause: err}
			}
		}
		result, providerErr := operation()
		postBase := context.WithoutCancel(ctx)
		postCtx, cancel := context.WithTimeout(postBase, state.retainedInputVerificationTimeout())
		verifyErr := state.verifyRetainedInputs(
			postCtx,
			spec.phase,
			namedinputs.IntegrityBoundaryAfterAttempt,
			attemptRef,
		)
		cancel()
		if verifyErr == nil {
			result, recordErr := state.persistProviderInvocation(spec, currentAttempt, startedAt, result, providerErr, true, "", artifactOrdinal, promptRef, promptDigest)
			if recordErr != nil {
				return TurnResult{}, rootInvocationPersistenceError{cause: recordErr}
			}
			return result, providerErr
		}
		verifyErr = normalizePostAttemptVerificationError(
			verifyErr,
			spec.phase,
			namedinputs.IntegrityBoundaryAfterAttempt,
			attemptRef,
		)

		providerCause := rootProviderSecondaryCause(spec.actor, result, providerErr)
		if providerCause == nil {
			providerCause = ctx.Err()
		}
		var providerFailure map[string]any
		if providerCause != nil {
			providerFailure = providerFailurePayload(
				spec.phase,
				spec.actor,
				spec.backendName,
				providerCause,
				providerResultForTurn(spec.backendName, result),
			)
			if errors.Is(providerCause, context.Canceled) {
				providerFailure["category"] = "canceled"
				providerFailure["retryable"] = false
				providerFailure["remediation_code"] = ""
				providerFailure["remediation"] = ""
			}
		}
		// The provider result is deliberately replaced with its zero value. No
		// caller can accidentally persist content rejected by the post-attempt
		// integrity boundary.
		integrityErr := &rootNamedInputIntegrityError{
			cause:           verifyErr,
			role:            spec.phase,
			boundary:        namedinputs.IntegrityBoundaryAfterAttempt,
			providerAttempt: attemptRef,
			providerCause:   providerCause,
			providerFailure: providerFailure,
		}
		result, recordErr := state.persistProviderInvocation(spec, currentAttempt, startedAt, result, integrityErr, true, "post_launch_integrity", artifactOrdinal, promptRef, promptDigest)
		if recordErr != nil {
			return TurnResult{}, rootInvocationPersistenceError{cause: errors.Join(integrityErr, recordErr)}
		}
		return TurnResult{}, integrityErr
	})
}

func rootProviderSecondaryCause(actor string, result TurnResult, runErr error) error {
	if runErr != nil {
		return runErr
	}
	switch {
	case result.Stalled:
		return BackendRunError{Label: actor, Detail: "provider stalled during the rejected attempt"}
	case result.TimedOut:
		return BackendRunError{Label: actor, Detail: "provider timed out during the rejected attempt"}
	default:
		return nil
	}
}

func normalizePostAttemptVerificationError(
	err error,
	role string,
	boundary string,
	providerAttempt *int,
) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		var diagnosticErr *contracts.DiagnosticError
		if errors.As(err, &diagnosticErr) {
			for _, diagnostic := range diagnosticErr.Diagnostics {
				category, _ := diagnostic.Details["mismatch_category"].(string)
				if strings.TrimSpace(category) != "" {
					return err
				}
			}
		}
	}
	return namedinputs.NewVerificationIncompleteError(
		err,
		role,
		boundary,
		providerAttempt,
	)
}

func (s *rootExecutionState) verifyRetainedInputs(
	ctx context.Context,
	role string,
	boundary string,
	providerAttempt *int,
) error {
	if s == nil || s.preflight == nil || s.preflight.selectedContract == nil {
		return nil
	}
	if s.persisted == nil || s.persisted.st == nil || s.persisted.retainedInputRef == nil {
		return contracts.NewDiagnosticError(
			"Retained named input descriptor is missing.",
			contracts.NewDiagnostic(
				namedinputs.DiagnosticCodeIntegrity,
				contracts.DiagnosticPhasePolicy,
				"/retained_input_materialization_ref",
				"Retained named input descriptor is missing.",
				integrityBoundaryDetails(
					role,
					boundary,
					providerAttempt,
				),
			),
		)
	}
	verifier := s.preflight.options.retainedInputVerifier
	if verifier == nil {
		verifier = namedinputs.VerifyRetained
	}
	return verifier(
		ctx,
		s.persisted.st,
		s.persisted.retainedInputRef,
		strings.TrimSpace(role),
		strings.TrimSpace(boundary),
		providerAttempt,
	)
}

func integrityBoundaryDetails(role string, boundary string, providerAttempt *int) map[string]any {
	details := map[string]any{
		"role":             strings.TrimSpace(role),
		"attempt_boundary": strings.TrimSpace(boundary),
	}
	if providerAttempt != nil && *providerAttempt > 0 {
		details["provider_attempt"] = *providerAttempt
	}
	return details
}

func (s *rootExecutionState) retainedInputVerificationTimeout() time.Duration {
	if s != nil && s.preflight != nil && s.preflight.options.retainedInputVerificationTimeout > 0 {
		return s.preflight.options.retainedInputVerificationTimeout
	}
	return defaultRootRetainedInputVerificationTimeout
}

func (s *rootExecutionState) verifyRetainedInputsIndependently(role string, boundary string) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.retainedInputVerificationTimeout())
	defer cancel()
	return s.verifyRetainedInputs(ctx, role, boundary, nil)
}

func asRootNamedInputIntegrityError(err error) (*rootNamedInputIntegrityError, bool) {
	var integrityErr *rootNamedInputIntegrityError
	ok := errors.As(err, &integrityErr)
	return integrityErr, ok
}

func (s *rootExecutionState) markNamedInputIntegrityFailed(runErr error) (map[string]any, error) {
	integrityErr, ok := asRootNamedInputIntegrityError(runErr)
	if !ok {
		integrityErr = &rootNamedInputIntegrityError{
			cause:    runErr,
			role:     "orchestrator",
			boundary: "unknown",
		}
		runErr = integrityErr
	}

	secondaryCauses := []string{}
	if len(integrityErr.providerFailure) > 0 {
		secondaryCause := RootFailureCauseProviderFailed
		if errors.Is(integrityErr.providerCause, context.Canceled) {
			secondaryCause = RootFailureCauseProviderCanceled
		}
		secondaryCauses = append(secondaryCauses, secondaryCause)
		s.meta = s.meta.AppendToSlice("provider_failures", cloneMap(integrityErr.providerFailure))
	}
	failure := rootNamedInputIntegrityProjection(integrityErr)
	s.meta = namedInputIntegrityFailureMeta(s.meta, integrityErr.cause, secondaryCauses...).
		With("named_input_integrity_failure", failure).
		With("actual_participant_turns", s.transcript.Len()).
		With("participant_turns_completed", s.transcript.Len())
	s.lastProviderFailure = nil

	var terminalErr error
	s.meta, _, terminalErr = finalizeTerminalWorkspace(context.Background(), s.st, s.meta)
	if terminalErr != nil && !isSourceMutationError(terminalErr) {
		s.meta = withRootFailureCauses(s.meta, stopReasonWorkspaceIntegrityFailed)
	}

	effectiveErr := errors.Join(runErr, terminalErr)
	if err := s.saveProgress(); err != nil {
		return s.result(), errors.Join(effectiveErr, err)
	}
	payload := map[string]any{
		"actual_participant_turns": s.transcript.Len(),
		"phase":                    RootNamedInputIntegrityExecutionPhase,
		"stop_reason":              RootNamedInputIntegrityStopReason,
		"integrity_failure":        failure,
		"failure_causes":           s.meta.Slice("failure_causes"),
		"source_mutated":           s.meta.Bool("source_mutated"),
	}
	if len(integrityErr.providerFailure) > 0 {
		payload["provider_failure"] = cloneMap(integrityErr.providerFailure)
	}
	if terminalErr != nil && !isSourceMutationError(terminalErr) {
		payload["workspace_error"] = terminalErr.Error()
	}
	if _, err := s.st.AppendSessionEventV1(
		"node_failed",
		graph.RootNodeID,
		"Root recipe retained named input integrity verification failed",
		payload,
		store.EventOptions{},
	); err != nil {
		return s.result(), errors.Join(effectiveErr, err)
	}
	if err := s.saveGraph("failed"); err != nil {
		return s.result(), errors.Join(effectiveErr, err)
	}
	return s.result(), effectiveErr
}

func rootNamedInputIntegrityProjection(failure *rootNamedInputIntegrityError) map[string]any {
	result := map[string]any{
		"code":             RootFailureCauseNamedInputIntegrity,
		"role":             strings.TrimSpace(failure.role),
		"attempt_boundary": strings.TrimSpace(failure.boundary),
		"diagnostics":      []any{},
	}
	if failure != nil && failure.providerAttempt != nil && *failure.providerAttempt > 0 {
		result["provider_attempt"] = *failure.providerAttempt
	}
	var diagnosticErr *contracts.DiagnosticError
	if failure != nil && errors.As(failure.cause, &diagnosticErr) {
		diagnostics := make([]any, 0, len(diagnosticErr.Diagnostics))
		for _, diagnostic := range diagnosticErr.Diagnostics {
			projected := diagnostic.ToMap()
			details, _ := projected["details"].(map[string]any)
			if failure.providerAttempt != nil && *failure.providerAttempt > 0 {
				if details == nil {
					details = map[string]any{}
				}
				details["provider_attempt"] = *failure.providerAttempt
			} else if details != nil {
				delete(details, "provider_attempt")
			}
			if len(details) > 0 {
				projected["details"] = details
			} else {
				delete(projected, "details")
			}
			diagnostics = append(diagnostics, projected)
		}
		result["diagnostics"] = diagnostics
	}
	if failure != nil && failure.cause != nil {
		result["error"] = failure.cause.Error()
		result["error_type"] = fmt.Sprintf("%T", failure.cause)
	}
	if failure != nil && len(failure.providerFailure) > 0 {
		result["provider_failure"] = cloneMap(failure.providerFailure)
	}
	return result
}
