package runner

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/charlesnpx/agentbus/engine"
)

func TestEmbeddedTurnPrefersResultMessageAndMapsFinalObservation(t *testing.T) {
	final := &engine.TurnFinalObservation{
		BackendSessionID: "thread-1",
		ReturnCodeKnown:  true,
		ReturnCode:       0,
	}
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventAgentText, Text: "delta one "},
		{Type: engine.EventAgentText, Text: "delta two"},
		{Type: engine.EventWarning, Text: "provider warning"},
		{Type: engine.EventResultMessage, Text: "authoritative result"},
		{Type: engine.EventTurnFinal, TurnFinal: final},
	}}
	result, gotFinal, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Content != "authoritative result" {
		t.Fatalf("content = %q, want authoritative result", result.Content)
	}
	if gotFinal != final {
		t.Fatalf("final = %#v, want %#v", gotFinal, final)
	}
	wantProvider := ProviderResult{
		Backend:         "codex",
		ReturnCode:      0,
		ReturnCodeKnown: true,
		Warnings:        []string{"provider warning"},
	}
	if !reflect.DeepEqual(result.ProviderResult, wantProvider) {
		t.Fatalf("provider result = %#v, want %#v", result.ProviderResult, wantProvider)
	}
}

func TestEmbeddedTurnRecordsLastReportedModel(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventModelReported, ModelReported: "initial-model"},
		{Type: engine.EventModelReported, Text: "fallback-model"},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if !reflect.DeepEqual(result.ProviderResult.Extra, map[string]any{"model_reported": "fallback-model"}) {
		t.Fatalf("provider result extra = %#v", result.ProviderResult.Extra)
	}
}

func TestEmbeddedTurnIgnoresToolUseAndProgress(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventAgentText, Text: "answer"},
		{Type: engine.EventToolUse, Name: "shell", Text: "tool activity"},
		{Type: engine.EventProgress, Text: "heartbeat"},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Content != "answer" {
		t.Fatalf("content = %q, want answer", result.Content)
	}
	if len(result.ProviderResult.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", result.ProviderResult.Warnings)
	}
}

func TestEmbeddedTurnMapsTerminalErrorAndFinalWarnings(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventAgentText, Text: "partial"},
		{Type: engine.EventWarning, Text: "provider warning"},
		{Type: engine.EventTerminalError, Text: "backend exploded"},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{
			ReturnCodeKnown: true,
			ReturnCode:      9,
			Signal:          "SIGTERM",
			ExecutionFailed: true,
			CleanupFailed:   true,
		}},
	}}
	result, _, err := runEmbeddedTurn(context.Background(), session, "claude", "Claude Code", "prompt", 0)
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("error = %T %v, want BackendRunError", err, err)
	}
	if backendErr.Label != "Claude Code" || backendErr.Detail != "backend exploded" {
		t.Fatalf("backend error = %#v", backendErr)
	}
	if result.Content != "partial" || result.ProviderResult.ReturnCode != 9 || !result.ProviderResult.ReturnCodeKnown {
		t.Fatalf("turn result = %#v", result)
	}
	wantWarnings := []string{
		"provider warning",
		"agentbus process signal: SIGTERM",
		"agentbus execution failed",
		"agentbus cleanup failed",
	}
	if !reflect.DeepEqual(result.ProviderResult.Warnings, wantWarnings) {
		t.Fatalf("warnings = %#v, want %#v", result.ProviderResult.Warnings, wantWarnings)
	}
}

func TestEmbeddedTurnClassifiesRetryableTerminalError(t *testing.T) {
	const retryableText = "API Error: rate limit exceeded"
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventTerminalError, Text: retryableText},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	var retryableErr RetryableProviderError
	if !errors.As(err, &retryableErr) {
		t.Fatalf("error = %T %v, want RetryableProviderError", err, err)
	}
	if retryableErr.Label != "Codex" || retryableErr.Detail != retryableText {
		t.Fatalf("retryable error = %#v", retryableErr)
	}
	if result.ProviderResult.RetryableError != retryableText {
		t.Fatalf("retryable error = %q, want %q", result.ProviderResult.RetryableError, retryableText)
	}
}

func TestEmbeddedTurnExecutionFailedWithoutOutputReturnsBackendError(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{
			ReturnCodeKnown: true,
			ReturnCode:      17,
			ExecutionFailed: true,
		}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("error = %T %v, want BackendRunError", err, err)
	}
	if backendErr.Label != "Codex" || backendErr.Detail != "process exited 17" {
		t.Fatalf("backend error = %#v", backendErr)
	}
	if result.Content != "" || result.ProviderResult.ReturnCode != 17 || !result.ProviderResult.ReturnCodeKnown {
		t.Fatalf("turn result = %#v", result)
	}
}

func TestEmbeddedTurnExecutionFailedWithAuthTextReturnsBackendError(t *testing.T) {
	const authText = "Authentication error: token expired"
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventAgentText, Text: authText},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{
			ReturnCodeKnown: true,
			ReturnCode:      17,
			ExecutionFailed: true,
		}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("error = %T %v, want BackendRunError", err, err)
	}
	var retryableErr RetryableProviderError
	if errors.As(err, &retryableErr) {
		t.Fatalf("auth failure was classified retryable: %v", err)
	}
	if backendErr.Label != "Codex" || backendErr.Detail != authText {
		t.Fatalf("backend error = %#v", backendErr)
	}
	if result.Recovered || result.ProviderResult.Recovered || result.ProviderResult.RetryableError != "" {
		t.Fatalf("turn result = %#v", result)
	}
}

func TestEmbeddedTurnExecutionFailedWithAgentTextRecovers(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventAgentText, Text: "partial response"},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{
			ReturnCodeKnown: true,
			ReturnCode:      17,
			ExecutionFailed: true,
		}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Content != "partial response" || !result.Recovered || !result.ProviderResult.Recovered || result.ProviderResult.RecoverySource != "event_stream" {
		t.Fatalf("turn result = %#v", result)
	}
	if !reflect.DeepEqual(result.ProviderResult.Warnings, []string{"agentbus execution failed"}) {
		t.Fatalf("warnings = %#v", result.ProviderResult.Warnings)
	}
}

func TestEmbeddedTurnTimedOutWithoutOutputReturnsPlaceholder(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{TimedOut: true}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 3)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	const timeoutDetail = "codex timed out after 3s with no recoverable response"
	if result.Content != "[Codex timed out after 3s]" || !result.TimedOut || !result.ProviderResult.TimedOut ||
		!reflect.DeepEqual(result.ProviderResult.Warnings, []string{"agentbus turn timed out", timeoutDetail}) {
		t.Fatalf("turn result = %#v", result)
	}
}

func TestEmbeddedTurnCanceledWithoutOutputReturnsBackendError(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{Canceled: true}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("error = %T %v, want BackendRunError", err, err)
	}
	if backendErr.Label != "Codex" || backendErr.Detail != "turn canceled" {
		t.Fatalf("backend error = %#v", backendErr)
	}
	if result.Content != "" || !reflect.DeepEqual(result.ProviderResult.Warnings, []string{"agentbus turn canceled"}) {
		t.Fatalf("turn result = %#v", result)
	}
}

func TestEmbeddedTurnCanceledWithAgentTextRecovers(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventAgentText, Text: "partial response"},
		{Type: engine.EventResultMessage, Text: ""},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{Canceled: true}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Content != "partial response" || !result.Recovered || !result.ProviderResult.Recovered || result.ProviderResult.RecoverySource != "event_stream" {
		t.Fatalf("turn result = %#v", result)
	}
	if !reflect.DeepEqual(result.ProviderResult.Warnings, []string{"agentbus turn canceled"}) {
		t.Fatalf("warnings = %#v", result.ProviderResult.Warnings)
	}
}

func TestEmbeddedTurnEmptyResultMessageDoesNotRecoverExecutionFailure(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventResultMessage, Text: ""},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{
			ReturnCodeKnown: true,
			ReturnCode:      17,
			ExecutionFailed: true,
		}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("error = %T %v, want BackendRunError", err, err)
	}
	if backendErr.Label != "Codex" || backendErr.Detail != "process exited 17" {
		t.Fatalf("backend error = %#v", backendErr)
	}
	if result.Content != "" {
		t.Fatalf("content = %q, want empty", result.Content)
	}
}

func TestEmbeddedTurnWithoutFinalReturnsBackendError(t *testing.T) {
	session := &fakeEmbeddedSession{}

	result, final, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", 0)
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("error = %T %v, want BackendRunError", err, err)
	}
	if final != nil || backendErr.Label != "Codex" || backendErr.Detail != "turn ended without final observation" {
		t.Fatalf("final = %#v, backend error = %#v", final, backendErr)
	}
	if !reflect.DeepEqual(result.ProviderResult.Warnings, []string{"agentbus turn ended without a final observation"}) {
		t.Fatalf("warnings = %#v", result.ProviderResult.Warnings)
	}
}
