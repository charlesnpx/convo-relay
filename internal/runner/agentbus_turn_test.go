package runner

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

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
	result, gotFinal, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
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

func TestEmbeddedTurnForwardsProvidedWriteAndTimeout(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{{
		Type:      engine.EventTurnFinal,
		TurnFinal: &engine.TurnFinalObservation{},
	}}}

	if _, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", false, 11, 0); err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if len(session.turnInputs) != 1 {
		t.Fatalf("turn calls = %d, want one", len(session.turnInputs))
	}
	input := session.turnInputs[0]
	if input.Prompt != "prompt" || input.Write || input.Timeout != 11*time.Second {
		t.Fatalf("turn input = %#v", input)
	}
}

func TestEmbeddedTurnRecordsLastReportedModel(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventModelReported, ModelReported: "initial-model"},
		{Type: engine.EventModelReported, Text: "fallback-model"},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if !reflect.DeepEqual(result.ProviderResult.Extra, map[string]any{"model_reported": "fallback-model"}) {
		t.Fatalf("provider result extra = %#v", result.ProviderResult.Extra)
	}
}

func TestEmbeddedTurnRetainsReportedModelAfterEmptyReport(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventModelReported, ModelReported: "initial-model"},
		{Type: engine.EventModelReported},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if !reflect.DeepEqual(result.ProviderResult.Extra, map[string]any{"model_reported": "initial-model"}) {
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

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
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

func TestEmbeddedTurnProgressKeepsWatchdogAlive(t *testing.T) {
	session := &fakeEmbeddedSession{
		onTurn: func(context.Context, engine.TurnInput) (<-chan engine.Event, error) {
			events := make(chan engine.Event, 1)
			events <- engine.Event{Type: engine.EventProgress}
			go func() {
				defer close(events)
				for range 3 {
					time.Sleep(100 * time.Millisecond)
					events <- engine.Event{Type: engine.EventProgress}
				}
				events <- engine.Event{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}}
			}()
			return events, nil
		},
	}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 1)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Stalled || result.ProviderResult.Stalled {
		t.Fatalf("turn stalled despite progress events: %#v", result)
	}
	if session.interruptCalls != 0 {
		t.Fatalf("interrupt calls = %d, want none", session.interruptCalls)
	}
}

func TestEmbeddedTurnStallWithAgentTextDuringInterruptDrainRecovers(t *testing.T) {
	events := make(chan engine.Event, 2)
	session := &fakeEmbeddedSession{}
	session.onTurn = func(context.Context, engine.TurnInput) (<-chan engine.Event, error) {
		return events, nil
	}
	session.onInterrupt = func(context.Context) error {
		events <- engine.Event{Type: engine.EventAgentText, Text: "partial response"}
		events <- engine.Event{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}}
		close(events)
		return nil
	}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 1)
	if err != nil {
		t.Fatalf("stalled turn: %v", err)
	}
	if result.Content != "partial response" || !result.Stalled || !result.ProviderResult.Stalled || !result.Recovered ||
		!result.ProviderResult.Recovered || result.ProviderResult.RecoverySource != "event_stream" {
		t.Fatalf("stalled recovery result = %#v", result)
	}
	if !reflect.DeepEqual(result.ProviderResult.Warnings, []string{"codex stalled - no stream activity for 1s, interrupting turn"}) {
		t.Fatalf("stalled warnings = %#v", result.ProviderResult.Warnings)
	}
	if session.interruptCalls != 1 {
		t.Fatalf("interrupt calls = %d, want one", session.interruptCalls)
	}
}

func TestEmbeddedTurnStallWithAuthTextReturnsBackendError(t *testing.T) {
	const authText = "Authentication error: token expired"
	events := make(chan engine.Event, 1)
	events <- engine.Event{Type: engine.EventAgentText, Text: authText}
	session := &fakeEmbeddedSession{
		onTurn: func(context.Context, engine.TurnInput) (<-chan engine.Event, error) {
			return events, nil
		},
		onInterrupt: func(context.Context) error {
			close(events)
			return nil
		},
	}

	result, _, err := runEmbeddedTurnWithWatchdogTimeout(context.Background(), session, "codex", "Codex", "prompt", true, 0, 1, 10*time.Millisecond)
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("error = %T %v, want BackendRunError", err, err)
	}
	if backendErr.Label != "Codex" || backendErr.Detail != authText {
		t.Fatalf("backend error = %#v", backendErr)
	}
	if !result.Stalled || !result.ProviderResult.Stalled || result.Recovered || result.ProviderResult.Recovered {
		t.Fatalf("stalled auth result = %#v", result)
	}
}

func TestEmbeddedTurnClassifiesRetryableContentWithTerminalError(t *testing.T) {
	const terminalText = "backend exploded"
	const retryableText = "API Error: rate limit exceeded"
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventAgentText, Text: retryableText},
		{Type: engine.EventTerminalError, Text: terminalText},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
	var retryableErr RetryableProviderError
	if !errors.As(err, &retryableErr) {
		t.Fatalf("error = %T %v, want RetryableProviderError", err, err)
	}
	wantDetail := terminalText + "; " + retryableText
	if retryableErr.Label != "Codex" || retryableErr.Detail != wantDetail {
		t.Fatalf("retryable error = %#v, want detail %q", retryableErr, wantDetail)
	}
	if result.ProviderResult.RetryableError != wantDetail || result.Recovered || result.ProviderResult.Recovered {
		t.Fatalf("terminal retryable result = %#v", result)
	}
}

func TestEmbeddedTurnStallStopsDrainingWhenStreamDoesNotClose(t *testing.T) {
	const watchdogTimeout = 10 * time.Millisecond
	const drainGrace = 25 * time.Millisecond
	const interruptDelay = 5 * time.Millisecond
	const interruptFailure = "interrupt failed"
	originalDrainGrace := embeddedDrainGrace
	embeddedDrainGrace = drainGrace
	t.Cleanup(func() {
		embeddedDrainGrace = originalDrainGrace
	})

	events := make(chan engine.Event)
	interruptedAt := make(chan time.Time, 1)
	session := &fakeEmbeddedSession{
		onTurn: func(context.Context, engine.TurnInput) (<-chan engine.Event, error) {
			return events, nil
		},
		onInterrupt: func(context.Context) error {
			interruptedAt <- time.Now()
			time.Sleep(interruptDelay)
			return errors.New(interruptFailure)
		},
	}

	result, _, err := runEmbeddedTurnWithWatchdogTimeout(context.Background(), session, "codex", "Codex", "prompt", true, 0, 1, watchdogTimeout)
	if err != nil {
		t.Fatalf("stalled turn: %v", err)
	}
	if elapsed := time.Since(<-interruptedAt); elapsed > drainGrace+100*time.Millisecond {
		t.Fatalf("turn returned %s after interrupt, want it bounded by the %s drain grace", elapsed, drainGrace)
	}
	if result.Content != "[Codex stalled after 1s of no stream activity]" || !result.Stalled || !result.ProviderResult.Stalled || result.Recovered || result.ProviderResult.Recovered {
		t.Fatalf("stalled result = %#v", result)
	}
	foundDrainWarning := false
	foundInterruptWarning := false
	for _, warning := range result.ProviderResult.Warnings {
		if warning == "agentbus event stream did not close before drain grace elapsed" {
			foundDrainWarning = true
		}
		if warning == "stall interrupt failed: "+interruptFailure {
			foundInterruptWarning = true
		}
	}
	if !foundDrainWarning {
		t.Fatalf("warnings = %#v, want stream-not-closed warning", result.ProviderResult.Warnings)
	}
	if !foundInterruptWarning {
		t.Fatalf("warnings = %#v, want interrupt failure warning", result.ProviderResult.Warnings)
	}
	if session.interruptCalls != 1 {
		t.Fatalf("interrupt calls = %d, want one", session.interruptCalls)
	}
}

func TestEmbeddedTurnCancellationStopsDrainingWhenStreamDoesNotClose(t *testing.T) {
	const drainGrace = 20 * time.Millisecond
	originalDrainGrace := embeddedDrainGrace
	embeddedDrainGrace = drainGrace
	t.Cleanup(func() {
		embeddedDrainGrace = originalDrainGrace
	})

	events := make(chan engine.Event)
	turnStarted := make(chan struct{})
	session := &fakeEmbeddedSession{
		onTurn: func(context.Context, engine.TurnInput) (<-chan engine.Event, error) {
			close(turnStarted)
			return events, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result TurnResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, _, err := runEmbeddedTurn(ctx, session, "codex", "Codex", "prompt", true, 0, 0)
		done <- outcome{result: result, err: err}
	}()

	select {
	case <-turnStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	canceledAt := time.Now()
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context.Canceled", got.err)
		}
		if elapsed := time.Since(canceledAt); elapsed > drainGrace+100*time.Millisecond {
			t.Fatalf("turn returned %s after cancellation, want it bounded by the %s drain grace", elapsed, drainGrace)
		}
		foundDrainWarning := false
		for _, warning := range got.result.ProviderResult.Warnings {
			if warning == "agentbus event stream did not close before drain grace elapsed" {
				foundDrainWarning = true
				break
			}
		}
		if !foundDrainWarning {
			t.Fatalf("warnings = %#v, want stream-not-closed warning", got.result.ProviderResult.Warnings)
		}
	case <-time.After(drainGrace + 250*time.Millisecond):
		t.Fatalf("turn did not return within the %s drain grace", drainGrace)
	}
}

func TestEmbeddedTurnRecoversTerminalErrorAfterAgentText(t *testing.T) {
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
	result, _, err := runEmbeddedTurn(context.Background(), session, "claude", "Claude Code", "prompt", true, 0, 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Content != "partial" || !result.Recovered || !result.ProviderResult.Recovered || result.ProviderResult.RecoverySource != "event_stream" ||
		result.ProviderResult.ReturnCode != 9 || !result.ProviderResult.ReturnCodeKnown {
		t.Fatalf("turn result = %#v", result)
	}
	wantWarnings := []string{
		"provider warning",
		"agentbus process signal: SIGTERM",
		"agentbus execution failed",
		"agentbus cleanup failed",
		"backend exploded",
	}
	if !reflect.DeepEqual(result.ProviderResult.Warnings, wantWarnings) {
		t.Fatalf("warnings = %#v, want %#v", result.ProviderResult.Warnings, wantWarnings)
	}
}

func TestEmbeddedTurnTerminalErrorWithoutOutputReturnsBackendError(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventTerminalError, Text: "backend exploded"},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "claude", "Claude Code", "prompt", true, 0, 0)
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("error = %T %v, want BackendRunError", err, err)
	}
	if backendErr.Label != "Claude Code" || backendErr.Detail != "backend exploded" {
		t.Fatalf("backend error = %#v", backendErr)
	}
	if result.Content != "" || result.Recovered || result.ProviderResult.Recovered {
		t.Fatalf("terminal error result = %#v", result)
	}
}

func TestEmbeddedTurnClassifiesRetryableTerminalError(t *testing.T) {
	const retryableText = "API Error: rate limit exceeded"
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventAgentText, Text: "partial response"},
		{Type: engine.EventTerminalError, Text: retryableText},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
	var retryableErr RetryableProviderError
	if !errors.As(err, &retryableErr) {
		t.Fatalf("error = %T %v, want RetryableProviderError", err, err)
	}
	wantDetail := retryableText + "; partial response"
	if retryableErr.Label != "Codex" || retryableErr.Detail != wantDetail {
		t.Fatalf("retryable error = %#v", retryableErr)
	}
	if result.ProviderResult.RetryableError != wantDetail {
		t.Fatalf("retryable error = %q, want %q", result.ProviderResult.RetryableError, wantDetail)
	}
	if result.Recovered || result.ProviderResult.Recovered {
		t.Fatalf("retryable terminal error recovered: %#v", result)
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

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
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

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
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

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
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

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 3, 0)
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

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
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

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
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

func TestEmbeddedTurnCanceledWithRetryableAgentTextRecovers(t *testing.T) {
	const retryableText = "API Error: rate limit exceeded"
	session := &fakeEmbeddedSession{events: []engine.Event{
		{Type: engine.EventAgentText, Text: retryableText},
		{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{Canceled: true}},
	}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Content != retryableText || !result.Recovered || !result.ProviderResult.Recovered || result.ProviderResult.RecoverySource != "event_stream" {
		t.Fatalf("turn result = %#v", result)
	}
	if result.ProviderResult.RetryableError != "" {
		t.Fatalf("retryable error = %q, want empty", result.ProviderResult.RetryableError)
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

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
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

func TestEmbeddedTurnEmptySuccessfulFinalUsesPlaceholder(t *testing.T) {
	session := &fakeEmbeddedSession{events: []engine.Event{{
		Type:      engine.EventTurnFinal,
		TurnFinal: &engine.TurnFinalObservation{},
	}}}

	result, _, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Content != "[No response from Codex]" {
		t.Fatalf("content = %q, want no-response placeholder", result.Content)
	}
}

func TestEmbeddedTurnWithoutFinalReturnsBackendError(t *testing.T) {
	session := &fakeEmbeddedSession{}

	result, final, err := runEmbeddedTurn(context.Background(), session, "codex", "Codex", "prompt", true, 0, 0)
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
