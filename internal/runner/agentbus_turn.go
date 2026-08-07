package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/engine"
)

func runEmbeddedTurn(ctx context.Context, session engine.Session, backend string, label string, prompt string, timeoutSeconds int) (TurnResult, *engine.TurnFinalObservation, error) {
	events, err := session.Turn(ctx, engine.TurnInput{
		Prompt:  prompt,
		Write:   true,
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
	var hasResult bool
	var terminalErrors []string
	warnings := []string{}
	var final *engine.TurnFinalObservation
	for event := range events {
		switch event.Type {
		case engine.EventAgentText:
			agentText.WriteString(event.Text)
		case engine.EventResultMessage:
			hasResult = true
			resultText = event.Text
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return TurnResult{}, final, ctxErr
	}

	providerResult := ProviderResult{
		Backend:  backend,
		Warnings: warnings,
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
	if hasResult {
		content = resultText
	}
	result := TurnResult{
		Content:        content,
		TimedOut:       providerResult.TimedOut,
		ProviderResult: providerResult,
	}
	if len(terminalErrors) > 0 {
		return result, final, BackendRunError{
			Label:  label,
			Detail: strings.Join(terminalErrors, "; "),
		}
	}
	if final == nil {
		return result, nil, BackendRunError{
			Label:  label,
			Detail: "turn ended without final observation",
		}
	}

	hasUsableOutput := hasResult || agentText.Len() > 0
	if (final.ExecutionFailed || final.TimedOut) && hasUsableOutput {
		providerResult.Recovered = true
		providerResult.RecoverySource = "event_stream"
		result.Recovered = true
		result.ProviderResult = providerResult
		return result, final, nil
	}
	if final.ExecutionFailed {
		return result, final, BackendRunError{
			Label:  label,
			Detail: embeddedTurnFailureDetail(final),
		}
	}
	if final.TimedOut {
		providerResult.Warnings = append(providerResult.Warnings, fmt.Sprintf("%s timed out after %ds with no recoverable response", backend, timeoutSeconds))
		result.Content = fmt.Sprintf("[%s timed out after %ds]", label, timeoutSeconds)
		result.ProviderResult = providerResult
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
