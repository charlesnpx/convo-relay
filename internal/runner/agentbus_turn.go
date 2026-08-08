package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/engine"
)

var embeddedDrainGrace = 5 * time.Second

func runEmbeddedTurn(ctx context.Context, session engine.Session, backend string, label string, prompt string, write bool, timeoutSeconds int, stallTimeoutSeconds int) (TurnResult, *engine.TurnFinalObservation, error) {
	return runEmbeddedTurnWithWatchdogTimeout(ctx, session, backend, label, prompt, write, timeoutSeconds, stallTimeoutSeconds, embeddedTurnTimeout(stallTimeoutSeconds))
}

func runEmbeddedTurnWithWatchdogTimeout(ctx context.Context, session engine.Session, backend string, label string, prompt string, write bool, timeoutSeconds int, stallTimeoutSeconds int, watchdogTimeout time.Duration) (TurnResult, *engine.TurnFinalObservation, error) {
	events, err := session.Turn(ctx, engine.TurnInput{
		Prompt:  prompt,
		Write:   write,
		Timeout: embeddedTurnTimeout(timeoutSeconds),
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return TurnResult{}, nil, ctxErr
		}
		return TurnResult{}, nil, err
	}

	var agentText strings.Builder
	var resultText string
	var hasResultText bool
	var terminalErrors []string
	var reportedModel string
	warnings := []string{}
	var final *engine.TurnFinalObservation
	var stallTimer *time.Timer
	if watchdogTimeout > 0 {
		stallTimer = time.NewTimer(watchdogTimeout)
		defer stallTimer.Stop()
	}
	stalled := false
	canceled := false
	streamDidNotClose := false
	var stallInterruptResult <-chan error
	var drainTimer *time.Timer
	defer func() {
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	startDrainTimer := func() {
		if drainTimer == nil {
			drainTimer = time.NewTimer(embeddedDrainGrace)
		}
	}

eventLoop:
	for {
		var (
			event engine.Event
			ok    bool
		)
		if stalled || canceled {
			select {
			case event, ok = <-events:
			case interruptErr := <-stallInterruptResult:
				if interruptErr != nil {
					warnings = append(warnings, fmt.Sprintf("stall interrupt failed: %v", interruptErr))
				}
				stallInterruptResult = nil
				continue
			case <-drainTimer.C:
				streamDidNotClose = true
				break eventLoop
			}
		} else if stallTimer == nil {
			select {
			case event, ok = <-events:
			case <-ctx.Done():
				canceled = true
				startDrainTimer()
				continue
			}
		} else {
			select {
			case event, ok = <-events:
			case <-ctx.Done():
				canceled = true
				startDrainTimer()
				continue
			case <-stallTimer.C:
				if ctx.Err() != nil {
					canceled = true
					startDrainTimer()
					continue
				}
				stalled = true
				startDrainTimer()
				interruptResult := make(chan error, 1)
				stallInterruptResult = interruptResult
				go func() {
					interruptResult <- session.Interrupt(context.Background())
				}()
				continue
			}
		}
		if !ok {
			break
		}
		if stallTimer != nil && !stalled && !canceled {
			if !stallTimer.Stop() {
				select {
				case <-stallTimer.C:
				default:
				}
			}
			stallTimer.Reset(watchdogTimeout)
		}
		switch event.Type {
		case engine.EventAgentText:
			agentText.WriteString(event.Text)
		case engine.EventModelReported:
			candidate := event.ModelReported
			if candidate == "" {
				candidate = event.Text
			}
			if candidate != "" {
				reportedModel = candidate
			}
		case engine.EventToolUse:
			// Tool activity is not persisted by the relay.
		case engine.EventProgress:
			// Heartbeats are not persisted by the relay.
		case engine.EventResultMessage:
			if event.Text != "" {
				hasResultText = true
				resultText = event.Text
			}
		case engine.EventTerminalError:
			text := strings.TrimSpace(event.Text)
			if text == "" {
				text = "agentbus terminal error"
			}
			terminalErrors = append(terminalErrors, text)
		case engine.EventWarning:
			if text := strings.TrimSpace(event.Text); text != "" {
				warnings = append(warnings, text)
			}
		case engine.EventTurnFinal:
			if event.TurnFinal != nil {
				final = event.TurnFinal
			}
		}
	}
	if stallInterruptResult != nil {
		select {
		case interruptErr := <-stallInterruptResult:
			if interruptErr != nil {
				warnings = append(warnings, fmt.Sprintf("stall interrupt failed: %v", interruptErr))
			}
		default:
		}
	}
	providerResult := ProviderResult{
		Backend:  backend,
		Warnings: warnings,
	}
	if reportedModel != "" {
		providerResult.Extra = map[string]any{"model_reported": reportedModel}
	}
	if final == nil {
		providerResult.Warnings = append(providerResult.Warnings, "agentbus turn ended without a final observation")
	} else {
		providerResult.TimedOut = final.TimedOut
		providerResult.ReturnCode = final.ReturnCode
		providerResult.ReturnCodeKnown = final.ReturnCodeKnown
		providerResult.Warnings = append(providerResult.Warnings, embeddedTurnWarnings(final)...)
	}

	content := agentText.String()
	if hasResultText {
		content = resultText
	}
	hasUsableOutput := hasResultText || agentText.Len() > 0
	result := TurnResult{
		Content:        content,
		TimedOut:       providerResult.TimedOut,
		ProviderResult: providerResult,
	}
	if stalled {
		stallDetail := fmt.Sprintf("%s stalled - no stream activity for %ds, interrupting turn", backend, stallTimeoutSeconds)
		providerResult.Stalled = true
		providerResult.Warnings = append(providerResult.Warnings, stallDetail)
		result.Stalled = true
	}
	if streamDidNotClose {
		providerResult.Warnings = append(providerResult.Warnings, "agentbus event stream did not close before drain grace elapsed")
	}
	result.ProviderResult = providerResult

	callerCanceled := ctx.Err() != nil
	// A provider-side canceled final can contain a retryable or authentication
	// failure. Only suppress that classification when the caller canceled the
	// turn or the final follows the watchdog's own interruption.
	cancellationOutcome := callerCanceled || (stalled && final != nil && final.Canceled)
	abnormalOutcome := stalled || len(terminalErrors) > 0 || final == nil || (final != nil && (final.ExecutionFailed || final.TimedOut || final.Canceled))
	failureText := ""
	if abnormalOutcome && !cancellationOutcome {
		failureParts := make([]string, 0, len(terminalErrors)+1)
		seenFailureParts := make(map[string]struct{}, len(terminalErrors)+1)
		for _, terminalError := range terminalErrors {
			failureParts = append(failureParts, terminalError)
			seenFailureParts[terminalError] = struct{}{}
		}
		if visibleContent := strings.TrimSpace(content); visibleContent != "" {
			if _, alreadyIncluded := seenFailureParts[visibleContent]; !alreadyIncluded {
				failureParts = append(failureParts, visibleContent)
			}
		}
		failureText = strings.Join(failureParts, "; ")
	}
	if retryableError := classifyRetryableProviderError(failureText); retryableError != "" {
		providerResult.RetryableError = retryableError
		result.ProviderResult = providerResult
		return result, final, RetryableProviderError{Label: label, Detail: retryableError}
	}
	if failureText != "" && providerFailureCategory(failureText) == "auth" {
		result.ProviderResult = providerResult
		return result, final, BackendRunError{Label: label, Detail: failureText}
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !stalled {
		if streamDidNotClose {
			return result, final, ctxErr
		}
		return TurnResult{}, final, ctxErr
	}
	if stalled {
		if hasUsableOutput {
			providerResult.Recovered = true
			providerResult.RecoverySource = "event_stream"
			result.Recovered = true
		} else {
			result.Content = fmt.Sprintf("[%s stalled after %ds of no stream activity]", label, stallTimeoutSeconds)
		}
		result.ProviderResult = providerResult
		return result, final, nil
	}
	if len(terminalErrors) > 0 {
		if hasUsableOutput {
			providerResult.Warnings = append(providerResult.Warnings, terminalErrors...)
			providerResult.Recovered = true
			providerResult.RecoverySource = "event_stream"
			result.Recovered = true
			result.ProviderResult = providerResult
			return result, final, nil
		}
		return result, final, BackendRunError{
			Label:  label,
			Detail: failureText,
		}
	}
	if final == nil {
		return result, nil, BackendRunError{
			Label:  label,
			Detail: "turn ended without final observation",
		}
	}

	if (final.ExecutionFailed || final.TimedOut || final.Canceled) && hasUsableOutput {
		providerResult.Recovered = true
		providerResult.RecoverySource = "event_stream"
		result.Recovered = true
		result.ProviderResult = providerResult
		return result, final, nil
	}
	if final.Canceled {
		return result, final, BackendRunError{
			Label:  label,
			Detail: "turn canceled",
		}
	}
	if final.ExecutionFailed {
		return result, final, BackendRunError{
			Label:  label,
			Detail: embeddedTurnFailureDetail(final),
		}
	}
	if final.TimedOut {
		timeoutDetail := fmt.Sprintf("%s timed out after %ds with no recoverable response", backend, timeoutSeconds)
		providerResult.Warnings = append(providerResult.Warnings, timeoutDetail)
		result.Content = fmt.Sprintf("[%s timed out after %ds]", label, timeoutSeconds)
		result.ProviderResult = providerResult
		return result, final, nil
	}
	if result.Content == "" {
		result.Content = fmt.Sprintf("[No response from %s]", label)
	}
	return result, final, nil
}

func embeddedTurnFailureDetail(final *engine.TurnFinalObservation) string {
	if final.ReturnCodeKnown {
		return fmt.Sprintf("process exited %d", final.ReturnCode)
	}
	if signal := strings.TrimSpace(final.Signal); signal != "" {
		return fmt.Sprintf("process terminated by signal %s", signal)
	}
	return "execution failed"
}

func embeddedTurnTimeout(timeoutSeconds int) time.Duration {
	if timeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(timeoutSeconds) * time.Second
}

func embeddedTurnWarnings(final *engine.TurnFinalObservation) []string {
	warnings := []string{}
	if final.TimedOut {
		warnings = append(warnings, "agentbus turn timed out")
	}
	if final.Canceled {
		warnings = append(warnings, "agentbus turn canceled")
	}
	if final.Signal != "" {
		warnings = append(warnings, fmt.Sprintf("agentbus process signal: %s", final.Signal))
	}
	if final.ExecutionFailed {
		warnings = append(warnings, "agentbus execution failed")
	}
	if final.CleanupFailed {
		warnings = append(warnings, "agentbus cleanup failed")
	}
	return warnings
}
