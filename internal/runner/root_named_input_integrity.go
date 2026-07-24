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
) error

type rootNamedInputIntegrityError struct {
	cause           error
	role            string
	boundary        string
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
	role string,
	actor string,
	backend string,
	operation func() (TurnResult, error),
) (TurnResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return runWithRetryableProviderErrors(ctx, actor, func() (TurnResult, error) {
		if err := state.verifyRetainedInputs(ctx, role, namedinputs.IntegrityBoundaryBeforeAttempt); err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return TurnResult{}, ctx.Err()
			}
			return TurnResult{}, &rootNamedInputIntegrityError{
				cause:    err,
				role:     role,
				boundary: namedinputs.IntegrityBoundaryBeforeAttempt,
			}
		}

		result, providerErr := operation()
		postBase := context.WithoutCancel(ctx)
		postCtx, cancel := context.WithTimeout(postBase, state.retainedInputVerificationTimeout())
		verifyErr := state.verifyRetainedInputs(postCtx, role, namedinputs.IntegrityBoundaryAfterAttempt)
		cancel()
		if verifyErr == nil {
			return result, providerErr
		}

		providerCause := rootProviderSecondaryCause(actor, result, providerErr)
		if providerCause == nil {
			providerCause = ctx.Err()
		}
		var providerFailure map[string]any
		if providerCause != nil {
			providerFailure = providerFailurePayload(
				role,
				actor,
				backend,
				providerCause,
				providerResultForTurn(backend, result),
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
		return TurnResult{}, &rootNamedInputIntegrityError{
			cause:           verifyErr,
			role:            role,
			boundary:        namedinputs.IntegrityBoundaryAfterAttempt,
			providerCause:   providerCause,
			providerFailure: providerFailure,
		}
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

func (s *rootExecutionState) verifyRetainedInputs(ctx context.Context, role string, boundary string) error {
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
				map[string]any{
					"role":             strings.TrimSpace(role),
					"attempt_boundary": strings.TrimSpace(boundary),
				},
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
	)
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
	return s.verifyRetainedInputs(ctx, role, boundary)
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
	var diagnosticErr *contracts.DiagnosticError
	if failure != nil && errors.As(failure.cause, &diagnosticErr) {
		diagnostics := make([]any, 0, len(diagnosticErr.Diagnostics))
		for _, diagnostic := range diagnosticErr.Diagnostics {
			diagnostics = append(diagnostics, diagnostic.ToMap())
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
