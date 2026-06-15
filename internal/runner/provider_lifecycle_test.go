package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestClassifyRetryableProviderErrorPolicy(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		retryable bool
	}{
		{name: "api error", text: "API Error: rate limit exceeded", retryable: true},
		{name: "http status", text: "request failed with status code 503", retryable: true},
		{name: "auth", text: "Authentication error: token expired", retryable: false},
		{name: "network", text: "network error: connection reset", retryable: true},
		{name: "missing binary", text: "command not found: claude", retryable: false},
		{name: "session collision", text: "Session ID abc is already in use", retryable: false},
		{name: "invalid option", text: "unknown option --bad", retryable: false},
		{name: "empty", text: "   ", retryable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyRetryableProviderError(tt.text) != ""
			if got != tt.retryable {
				t.Fatalf("retryable = %v, want %v for %q", got, tt.retryable, tt.text)
			}
		})
	}
}

func TestCodexBuildCommandPlacesOptionsAfterExecSubcommand(t *testing.T) {
	backend := newCodexBackend("/tmp/session", "slot_0", "Codex", "/tmp/project", SlotConfig{
		Model:  "gpt-test",
		Effort: "high",
	})
	first := backend.buildCommand()
	wantFirst := []string{"codex", "exec", "-m", "gpt-test", "-c", `model_reasoning_effort="high"`, "--json", "-"}
	if !slices.Equal(first, wantFirst) {
		t.Fatalf("first command = %#v, want %#v", first, wantFirst)
	}

	if err := backend.RestoreState(map[string]any{"started": true, "thread_id": "thread-123"}, SlotConfig{
		Model:  "gpt-test",
		Effort: "high",
	}); err != nil {
		t.Fatalf("restore codex state: %v", err)
	}
	resume := backend.buildCommand()
	wantResume := []string{"codex", "exec", "resume", "-m", "gpt-test", "-c", `model_reasoning_effort="high"`, "--json", "thread-123", "-"}
	if !slices.Equal(resume, wantResume) {
		t.Fatalf("resume command = %#v, want %#v", resume, wantResume)
	}
}

func TestRunPersistsProviderResultOnTranscriptAndTurnEvent(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "phase8-provider-result")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "PHASE8_SUCCESS",
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	transcript := mustLoadTranscript(t, sessionDir)
	if len(transcript) != 1 {
		t.Fatalf("transcript entries = %d, want 1", len(transcript))
	}
	providerResult := transcript[0]["provider_result"].(map[string]any)
	if providerResult["backend"] != "codex" ||
		providerResult["timed_out"] != false ||
		providerResult["stalled"] != false ||
		providerResult["recovered"] != false ||
		intFromAny(providerResult["return_code"], -1) != 0 {
		t.Fatalf("provider_result = %#v", providerResult)
	}

	events, err := store.New(sessionDir).ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	turnEvent := lastEventOfType(events, "turn_completed")
	if turnEvent == nil {
		t.Fatalf("turn_completed event missing: %#v", events)
	}
	eventProviderResult := turnEvent["payload"].(map[string]any)["provider_result"].(map[string]any)
	if eventProviderResult["backend"] != "codex" || intFromAny(eventProviderResult["return_code"], -1) != 0 {
		t.Fatalf("event provider_result = %#v", eventProviderResult)
	}

	report, err := inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	validation := report["validation"].(map[string]any)
	if validation["ok"] != true {
		t.Fatalf("strict validation = %#v", validation)
	}
}

func TestCodexProviderResultRecordsRecoveredNonzeroExit(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	backend := newCodexBackend(env.relayHome, "slot_0", "Codex", env.projectDir, SlotConfig{})

	result, err := backend.RunTurn(context.Background(), "PHASE8_NONZERO_RECOVERED", TurnOptions{TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if !strings.Contains(result.Content, "recovered after nonzero") {
		t.Fatalf("content = %q", result.Content)
	}
	providerResult := result.ProviderResult
	if providerResult.Backend != "codex" ||
		providerResult.ReturnCode != 7 ||
		!providerResult.Recovered ||
		providerResult.RecoverySource != "event_buffer" ||
		len(providerResult.Warnings) == 0 {
		t.Fatalf("provider result = %#v", providerResult)
	}
}

func TestCodexProviderResultRecordsTimeoutRecovery(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	backend := newCodexBackend(env.relayHome, "slot_0", "Codex", env.projectDir, SlotConfig{})

	result, err := backend.RunTurn(context.Background(), "PHASE8_TIMEOUT_RECOVERED", TurnOptions{TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if !strings.Contains(result.Content, "recovered before timeout") {
		t.Fatalf("content = %q", result.Content)
	}
	if !result.ProviderResult.TimedOut ||
		!result.ProviderResult.Recovered ||
		result.ProviderResult.RecoverySource != "event_buffer" {
		t.Fatalf("provider result = %#v", result.ProviderResult)
	}
}

func TestCodexProviderResultAuthTimeoutRecoveryIsNotRecovered(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	backend := newCodexBackend(env.relayHome, "slot_0", "Codex", env.projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "PHASE8_TIMEOUT_AUTH", TurnOptions{TimeoutSeconds: 1})
	var retryable RetryableProviderError
	if err == nil {
		t.Fatalf("auth timeout unexpectedly succeeded")
	}
	if errors.As(err, &retryable) {
		t.Fatalf("auth timeout was classified retryable: %v", err)
	}
	if !strings.Contains(err.Error(), "Authentication error") {
		t.Fatalf("auth timeout detail = %v", err)
	}
}

func TestCodexProviderResultRecordsTimeoutWithoutOutput(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	backend := newCodexBackend(env.relayHome, "slot_0", "Codex", env.projectDir, SlotConfig{})

	result, err := backend.RunTurn(context.Background(), "PHASE8_TIMEOUT_EMPTY", TurnOptions{TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Content != "[Codex timed out after 1s]" {
		t.Fatalf("content = %q", result.Content)
	}
	if !result.ProviderResult.TimedOut ||
		result.ProviderResult.Recovered ||
		result.ProviderResult.RecoverySource != "" ||
		len(result.ProviderResult.Warnings) == 0 {
		t.Fatalf("provider result = %#v", result.ProviderResult)
	}
}

func TestCodexProviderResultHandlesMalformedOutput(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	backend := newCodexBackend(env.relayHome, "slot_0", "Codex", env.projectDir, SlotConfig{})

	result, err := backend.RunTurn(context.Background(), "PHASE8_MALFORMED", TurnOptions{TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.Content != "[No response from Codex]" {
		t.Fatalf("content = %q", result.Content)
	}
	if result.ProviderResult.ReturnCode != 0 || result.ProviderResult.Recovered {
		t.Fatalf("provider result = %#v", result.ProviderResult)
	}
	if backend.SessionState()["started"] != false {
		t.Fatalf("malformed output should not create restorable started state: %#v", backend.SessionState())
	}
}

func TestCodexProviderResultClassifiesRetryableError(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	backend := newCodexBackend(env.relayHome, "slot_0", "Codex", env.projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "PHASE8_RETRYABLE", TurnOptions{TimeoutSeconds: 5})
	var retryable RetryableProviderError
	if !errors.As(err, &retryable) {
		t.Fatalf("error = %T %[1]v, want RetryableProviderError", err)
	}
	if !strings.Contains(retryable.Detail, "API Error") {
		t.Fatalf("retryable detail = %q", retryable.Detail)
	}
}

func TestCodexProviderResultAuthFailureIsNotRetryable(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	backend := newCodexBackend(env.relayHome, "slot_0", "Codex", env.projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "PHASE8_AUTH_FAILURE", TurnOptions{TimeoutSeconds: 5})
	var retryable RetryableProviderError
	if err == nil {
		t.Fatalf("auth failure unexpectedly succeeded")
	}
	if errors.As(err, &retryable) {
		t.Fatalf("auth failure was classified retryable: %v", err)
	}
	if !strings.Contains(err.Error(), "Authentication error") {
		t.Fatalf("auth detail = %v", err)
	}
}

func TestCodexProviderResultStderrOnlyFailureIsNotRetryable(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	backend := newCodexBackend(env.relayHome, "slot_0", "Codex", env.projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "PHASE8_STDERR_ONLY", TurnOptions{TimeoutSeconds: 5})
	var retryable RetryableProviderError
	if err == nil {
		t.Fatalf("stderr-only failure unexpectedly succeeded")
	}
	if errors.As(err, &retryable) {
		t.Fatalf("stderr-only failure was classified retryable: %v", err)
	}
	if !strings.Contains(err.Error(), "Codex failed: boom") {
		t.Fatalf("error = %v", err)
	}
}

func TestCodexProviderResultNonzeroExitWithoutOutput(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	backend := newCodexBackend(env.relayHome, "slot_0", "Codex", env.projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "PHASE8_NONZERO_EMPTY", TurnOptions{TimeoutSeconds: 5})
	if err == nil || !strings.Contains(err.Error(), "process exited 3") {
		t.Fatalf("error = %v, want process exited detail", err)
	}
}

func TestRunRetriesRetryableProviderErrorThenPersistsSuccessfulTurn(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "phase8-retry-success")
	var waits []time.Duration
	withFakeRetryBackoff(t, func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	})

	result, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "PHASE8_RETRY_THEN_SUCCESS",
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result["actual_rounds"] != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(waits) != 1 || waits[0] != 5*time.Second {
		t.Fatalf("retry waits = %#v", waits)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if !strings.Contains(stringFromAny(transcript[0]["content"]), "retry succeeded") {
		t.Fatalf("transcript = %#v", transcript)
	}
}

func TestRunFailsAfterRetryableProviderBackoffExhausted(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "phase8-retry-exhausted")
	var waits []time.Duration
	withFakeRetryBackoff(t, func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	})

	_, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "PHASE8_RETRY_ALWAYS",
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err == nil || !strings.Contains(err.Error(), "failed after retryable provider errors") {
		t.Fatalf("error = %v, want retry exhaustion", err)
	}
	expected := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second}
	if len(waits) != len(expected) {
		t.Fatalf("retry waits = %#v, want %#v", waits, expected)
	}
	for index := range expected {
		if waits[index] != expected[index] {
			t.Fatalf("retry waits = %#v, want %#v", waits, expected)
		}
	}
	meta := mustLoadMeta(t, sessionDir)
	if meta["status"] != "failed" || intFromAny(meta["actual_rounds"], -1) != 0 {
		t.Fatalf("failed meta = %#v", meta)
	}
	events, err := store.New(sessionDir).ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	failureEvent := lastEventOfType(events, "provider_failure")
	if failureEvent == nil {
		t.Fatalf("provider_failure event missing: %#v", events)
	}
	payload := failureEvent["payload"].(map[string]any)
	if payload["category"] != "transient" || payload["retryable"] != true || intFromAny(payload["attempts"], 0) != 7 {
		t.Fatalf("retry failure payload = %#v", payload)
	}
}

func TestRunAuthProviderFailureShortCircuitsAndRecordsEvent(t *testing.T) {
	env := setupPhase8FakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "phase8-auth-failure")
	var waits []time.Duration
	withFakeRetryBackoff(t, func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	})

	_, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "PHASE8_AUTH_FAILURE",
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err == nil || !strings.Contains(err.Error(), "Authentication error") {
		t.Fatalf("error = %v, want auth failure", err)
	}
	if len(waits) != 0 {
		t.Fatalf("auth failure should not back off, waits = %#v", waits)
	}
	events, readErr := store.New(sessionDir).ReadEvents()
	if readErr != nil {
		t.Fatalf("read events: %v", readErr)
	}
	failureEvent := lastEventOfType(events, "provider_failure")
	if failureEvent == nil {
		t.Fatalf("provider_failure event missing: %#v", events)
	}
	payload := failureEvent["payload"].(map[string]any)
	if payload["category"] != "auth" || payload["retryable"] != false || payload["remediation_code"] != "codex_login" {
		t.Fatalf("auth failure payload = %#v", payload)
	}
	if strings.Contains(strings.ToLower(stringFromAny(payload["sanitized_detail"])), "sk-") {
		t.Fatalf("provider detail was not sanitized: %#v", payload)
	}
}

func TestCodexProviderResultMissingBinaryIsNotRetryable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("HOME", filepath.Join(root, "home"))
	projectDir := filepath.Join(root, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	backend := newCodexBackend(root, "slot_0", "Codex", projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "prompt", TurnOptions{TimeoutSeconds: 1})
	var retryable RetryableProviderError
	if err == nil {
		t.Fatalf("missing binary unexpectedly succeeded")
	}
	if errors.As(err, &retryable) {
		t.Fatalf("missing binary was classified retryable: %v", err)
	}
}

func TestHistoricalSessionWithoutProviderResultStillInspects(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "sessions", "legacy-no-provider-result")
	st := store.New(sessionDir)
	if err := st.EnsureSession(); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := st.SaveMetaMap(map[string]any{
		"session_id": "legacy-no-provider-result",
		"status":     "completed",
		"slots": []any{
			map[string]any{"backend": "codex", "slot_id": "slot_0", "label": "Codex", "state": map[string]any{}},
			map[string]any{"backend": "codex", "slot_id": "slot_1", "label": "Codex", "state": map[string]any{}},
		},
	}); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	if err := st.SaveTranscriptItems([]any{
		map[string]any{"round": 1, "slot_id": "slot_0", "from": "Codex", "content": "legacy response"},
	}); err != nil {
		t.Fatalf("save transcript: %v", err)
	}
	if _, err := st.AppendSessionEventV1("turn_completed", "root", "legacy turn", map[string]any{
		"round":         1,
		"slot_id":       "slot_0",
		"speaker":       "Codex",
		"ledger_counts": map[string]any{"settled": 0, "contested": 0, "withdrawn": 0},
	}, store.EventOptions{}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	if _, err := inspect.BuildContractsReport(sessionDir, false, "", ""); err != nil {
		t.Fatalf("contracts report should accept legacy session without provider_result: %v", err)
	}
}

func setupPhase8FakeCodex(t *testing.T) fakeEnv {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	homeDir := filepath.Join(root, "home")
	relayHome := filepath.Join(root, "relayhome")
	projectDir := filepath.Join(root, "project")
	for _, dir := range []string{binDir, homeDir, relayHome, projectDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := exec.Command("git", "init", projectDir).Run(); err != nil {
		t.Fatalf("git init project: %v", err)
	}
	fakeCodex := `#!/usr/bin/env python3
import json
import os
import sys
import time
from pathlib import Path

def emit(text):
    suffix = Path(os.environ.get("CODEX_HOME", "slot")).name
    print(json.dumps({"type": "thread.started", "thread_id": f"thread-{suffix}"}), flush=True)
    print(json.dumps({"type": "item.completed", "item": {"text": text}}), flush=True)

def main():
    prompt = sys.stdin.read()
    if "Return the updated ledger as JSON" in prompt:
        emit('{"settled":["done"],"contested":[],"withdrawn":[]}')
        return 0
    if "PHASE8_TIMEOUT_RECOVERED" in prompt:
        emit("recovered before timeout")
        time.sleep(5)
        return 0
    if "PHASE8_TIMEOUT_AUTH" in prompt:
        emit("Authentication error: token expired")
        time.sleep(5)
        return 0
    if "PHASE8_TIMEOUT_EMPTY" in prompt:
        time.sleep(5)
        return 0
    if "PHASE8_NONZERO_RECOVERED" in prompt:
        emit("recovered after nonzero")
        print("backend exited after partial output", file=sys.stderr, flush=True)
        return 7
    if "PHASE8_RETRYABLE" in prompt:
        print("API Error: rate limit exceeded", file=sys.stderr, flush=True)
        return 1
    if "PHASE8_AUTH_FAILURE" in prompt:
        print("Authentication error: token expired", file=sys.stderr, flush=True)
        return 1
    if "PHASE8_STDERR_ONLY" in prompt:
        print("boom", file=sys.stderr, flush=True)
        return 1
    if "PHASE8_NONZERO_EMPTY" in prompt:
        return 3
    if "PHASE8_RETRY_ALWAYS" in prompt:
        print("API Error: rate limit exceeded", file=sys.stderr, flush=True)
        return 1
    if "PHASE8_RETRY_THEN_SUCCESS" in prompt:
        marker = Path(os.environ.get("CODEX_HOME", ".")) / "phase8_retry_count"
        count = int(marker.read_text(encoding="utf-8")) if marker.exists() else 0
        marker.write_text(str(count + 1), encoding="utf-8")
        if count == 0:
            print("API Error: rate limit exceeded", file=sys.stderr, flush=True)
            return 1
        emit("retry succeeded")
        return 0
    if "PHASE8_MALFORMED" in prompt:
        print("this is not codex json", flush=True)
        return 0
    emit("phase8 success")
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`
	codexPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codexPath, []byte(fakeCodex), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_CLAUDE_HOME", relayHome)
	return fakeEnv{relayHome: relayHome, projectDir: projectDir}
}

func lastEventOfType(events []map[string]any, eventType string) map[string]any {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index]["event_type"] == eventType {
			return events[index]
		}
	}
	return nil
}

func withFakeRetryBackoff(t *testing.T, fn func(context.Context, time.Duration) error) {
	t.Helper()
	previous := retryBackoff
	retryBackoff = fn
	t.Cleanup(func() {
		retryBackoff = previous
	})
}
