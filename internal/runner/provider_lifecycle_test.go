package runner

import (
	"context"
	"os"
	"path/filepath"
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
	writeFakeCodexAppServer(t, binDir)
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

func withFakeRetryBackoff(t *testing.T, fake func(context.Context, time.Duration) error) {
	t.Helper()
	original := retryBackoff
	retryBackoff = fake
	t.Cleanup(func() {
		retryBackoff = original
	})
}
