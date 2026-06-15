package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/recipes"
)

type phase9Env struct {
	relayHome  string
	projectDir string
	homeDir    string
}

func TestGeminiParseOutputAcceptsKnownShapesAndPlainText(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		wantText string
		wantRef  string
	}{
		{name: "response", stdout: `{"response":"Gemini reply","session_id":"gemini-session"}`, wantText: "Gemini reply", wantRef: "gemini-session"},
		{name: "text", stdout: `{"text":"Text reply","session_id":"s1"}`, wantText: "Text reply", wantRef: "s1"},
		{name: "content", stdout: `{"content":"Content reply"}`, wantText: "Content reply"},
		{name: "message", stdout: `{"message":"Message reply"}`, wantText: "Message reply"},
		{name: "structured error", stdout: `{"error":{"message":"Structured error"}}`, wantText: "Structured error"},
		{name: "string error", stdout: `{"error":"String error"}`, wantText: "String error"},
		{name: "plain text", stdout: `plain fallback`, wantText: "plain fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ref := parseGeminiOutput(tt.stdout)
			if text != tt.wantText || ref != tt.wantRef {
				t.Fatalf("parseGeminiOutput = (%q, %q), want (%q, %q)", text, ref, tt.wantText, tt.wantRef)
			}
		})
	}
}

func TestGeminiBackendRunTurnStateAndHomeSeeding(t *testing.T) {
	env := setupPhase9FakeProviders(t)
	realGeminiConfig := filepath.Join(env.homeDir, ".gemini")
	if err := os.MkdirAll(filepath.Join(realGeminiConfig, "tmp"), 0o755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(realGeminiConfig, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realGeminiConfig, "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	backend := newGeminiBackend(env.relayHome, "slot_1", "Gemini", env.projectDir, SlotConfig{
		Model:  "gemini-2.5-pro",
		Effort: "medium",
	})

	result, err := backend.RunTurn(context.Background(), "PHASE9_SUCCESS", TurnOptions{TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if !strings.Contains(result.Content, "model=gemini-2.5-pro") {
		t.Fatalf("content = %q", result.Content)
	}
	if result.ProviderResult.Backend != "gemini" || result.ProviderResult.ReturnCode != 0 {
		t.Fatalf("provider result = %#v", result.ProviderResult)
	}
	state := backend.SessionState()
	if state["session_ref"] != "gemini-slot_1" ||
		state["started"] != true ||
		state["cwd"] != env.projectDir ||
		state["model"] != "gemini-2.5-pro" ||
		state["effort"] != "medium" {
		t.Fatalf("state = %#v", state)
	}
	seeded := filepath.Join(env.relayHome, "gemini", "slot_1", ".gemini", "settings.json")
	if target, err := os.Readlink(seeded); err != nil || target != filepath.Join(realGeminiConfig, "settings.json") {
		t.Fatalf("seeded settings symlink target = %q, err = %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(env.relayHome, "gemini", "slot_1", ".gemini", "tmp")); !os.IsNotExist(err) {
		t.Fatalf("tmp should not be seeded")
	}
	if _, err := os.Lstat(filepath.Join(env.relayHome, "gemini", "slot_1", ".gemini", "logs")); !os.IsNotExist(err) {
		t.Fatalf("logs should not be seeded")
	}
}

func TestGeminiBackendLifecycleOutcomes(t *testing.T) {
	tests := []struct {
		name           string
		prompt         string
		timeout        int
		wantContent    string
		wantTimedOut   bool
		wantRecovered  bool
		wantReturnCode int
		wantSource     string
	}{
		{name: "timeout recovery", prompt: "PHASE9_TIMEOUT_RECOVERED", timeout: 1, wantContent: "recovered before timeout", wantTimedOut: true, wantRecovered: true, wantReturnCode: -1, wantSource: "output"},
		{name: "timeout empty", prompt: "PHASE9_TIMEOUT_EMPTY", timeout: 1, wantContent: "[Gemini timed out after 1s]", wantTimedOut: true, wantReturnCode: -1},
		{name: "nonzero recovery", prompt: "PHASE9_NONZERO_RECOVERED", timeout: 5, wantContent: "recovered after nonzero", wantRecovered: true, wantReturnCode: 7, wantSource: "output"},
		{name: "malformed plain fallback", prompt: "PHASE9_MALFORMED", timeout: 5, wantContent: "plain malformed fallback", wantReturnCode: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setupPhase9FakeProviders(t)
			backend := newGeminiBackend(env.relayHome, "slot_0", "Gemini", env.projectDir, SlotConfig{})

			result, err := backend.RunTurn(context.Background(), tt.prompt, TurnOptions{TimeoutSeconds: tt.timeout})
			if err != nil {
				t.Fatalf("run turn: %v", err)
			}
			if result.Content != tt.wantContent ||
				result.ProviderResult.TimedOut != tt.wantTimedOut ||
				result.ProviderResult.Recovered != tt.wantRecovered ||
				result.ProviderResult.ReturnCode != tt.wantReturnCode ||
				result.ProviderResult.RecoverySource != tt.wantSource {
				t.Fatalf("result = %#v, provider = %#v", result, result.ProviderResult)
			}
		})
	}
}

func TestGeminiBackendRaisesRetryableAndNonRetryableErrors(t *testing.T) {
	env := setupPhase9FakeProviders(t)
	backend := newGeminiBackend(env.relayHome, "slot_0", "Gemini", env.projectDir, SlotConfig{})
	_, err := backend.RunTurn(context.Background(), "PHASE9_RETRYABLE", TurnOptions{TimeoutSeconds: 5})
	var retryable RetryableProviderError
	if !errors.As(err, &retryable) {
		t.Fatalf("error = %T %[1]v, want RetryableProviderError", err)
	}

	backend = newGeminiBackend(env.relayHome, "slot_1", "Gemini", env.projectDir, SlotConfig{})
	_, err = backend.RunTurn(context.Background(), "PHASE9_STDERR_ONLY", TurnOptions{TimeoutSeconds: 5})
	if err == nil || errors.As(err, &retryable) || !strings.Contains(err.Error(), "Gemini failed: boom") {
		t.Fatalf("stderr-only error = %v", err)
	}
}

func TestGeminiBackendAuthTimeoutRecoveryIsNotRecovered(t *testing.T) {
	env := setupPhase9FakeProviders(t)
	backend := newGeminiBackend(env.relayHome, "slot_0", "Gemini", env.projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "PHASE9_TIMEOUT_AUTH", TurnOptions{TimeoutSeconds: 1})
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

func TestGeminiBackendMissingBinaryIsNotRetryable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("HOME", filepath.Join(root, "home"))
	projectDir := filepath.Join(root, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	backend := newGeminiBackend(root, "slot_0", "Gemini", projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "prompt", TurnOptions{TimeoutSeconds: 1})
	var retryable RetryableProviderError
	if err == nil {
		t.Fatalf("missing binary unexpectedly succeeded")
	}
	if errors.As(err, &retryable) {
		t.Fatalf("missing binary was classified retryable: %v", err)
	}
}

func TestBuildAndRestoreSlotsSupportGemini(t *testing.T) {
	env := setupPhase9FakeProviders(t)
	profiles := map[string]map[string]any{
		"phase9-gemini": {
			"backend": "gemini",
			"model":   "gemini-2.5-pro",
			"effort":  "low",
		},
	}
	slots, err := buildSlots(
		[]string{"codex", "phase9-gemini"},
		env.relayHome,
		env.projectDir,
		[]SlotConfig{{}, {}},
		recipes.RuntimeConfig{BackendProfiles: profiles},
		"",
		0,
		1,
	)
	if err != nil {
		t.Fatalf("build slots: %v", err)
	}
	if slots[1].Name() != "gemini" || slots[1].Label() != "Gemini" {
		t.Fatalf("slots = %#v", slots)
	}
	geminiState := slots[1].SessionState()
	if geminiState["model"] != "gemini-2.5-pro" || geminiState["effort"] != "low" {
		t.Fatalf("gemini state = %#v", geminiState)
	}

	meta := map[string]any{
		"slots": []any{
			slotEnvelope(slots[0]),
			map[string]any{
				"backend": "gemini",
				"slot_id": "slot_1",
				"label":   "Gemini",
				"state": map[string]any{
					"session_ref": "gemini-existing",
					"started":     true,
					"cwd":         env.projectDir,
					"model":       "old-model",
					"effort":      "old-effort",
				},
			},
		},
	}
	restored, err := restoreSlots(meta, env.relayHome, []SlotConfig{{}, {Model: "new-model"}}, recipes.RuntimeConfig{}, "", 0, 1)
	if err != nil {
		t.Fatalf("restore slots: %v", err)
	}
	restoredState := restored[1].SessionState()
	if restored[1].Name() != "gemini" ||
		restoredState["session_ref"] != "gemini-existing" ||
		restoredState["model"] != "new-model" ||
		restoredState["effort"] != "old-effort" {
		t.Fatalf("restored gemini state = %#v", restoredState)
	}
}

func TestRunResumeAndCleanGeminiSessions(t *testing.T) {
	env := setupPhase9FakeProviders(t)
	cases := []struct {
		name   string
		agents []string
		rounds int
	}{
		{name: "gemini-gemini", agents: []string{"gemini", "gemini"}, rounds: 1},
		{name: "codex-gemini", agents: []string{"codex", "gemini"}, rounds: 2},
		{name: "gemini-codex", agents: []string{"gemini", "codex"}, rounds: 1},
		{name: "profile-gemini-vision", agents: []string{"gemini-vision", "codex"}, rounds: 1},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			sessionDir := filepath.Join(env.relayHome, "sessions", tt.name)
			if _, err := Run(context.Background(), Options{
				SessionDir:     sessionDir,
				Task:           "PHASE9_SUCCESS",
				Agents:         tt.agents,
				Rounds:         tt.rounds,
				TimeoutSeconds: 5,
				LaunchCWD:      env.projectDir,
			}); err != nil {
				t.Fatalf("run: %v", err)
			}
			meta := mustLoadMeta(t, sessionDir)
			slots := meta["slots"].([]any)
			foundGemini := false
			for _, rawSlot := range slots {
				slot := rawSlot.(map[string]any)
				if slot["backend"] == "gemini" {
					foundGemini = true
					state := slot["state"].(map[string]any)
					if state["started"] == true && state["session_ref"] == "" {
						t.Fatalf("gemini slot state = %#v", state)
					}
				}
			}
			if !foundGemini {
				t.Fatalf("session has no gemini slot: %#v", slots)
			}
			transcript := mustLoadTranscript(t, sessionDir)
			foundGeminiTurn := false
			for _, entry := range transcript {
				if strings.HasPrefix(stringFromAny(entry["from"]), "Gemini") {
					foundGeminiTurn = true
					providerResult := entry["provider_result"].(map[string]any)
					if providerResult["backend"] != "gemini" {
						t.Fatalf("gemini provider_result = %#v", providerResult)
					}
				}
			}
			if !foundGeminiTurn {
				t.Fatalf("transcript has no gemini turn: %#v", transcript)
			}
			if _, err := inspect.BuildContractsReport(sessionDir, false, "", ""); err != nil {
				t.Fatalf("contracts report: %v", err)
			}
		})
	}

	resumeDir := filepath.Join(env.relayHome, "sessions", "gemini-resume-clean")
	if _, err := Run(context.Background(), Options{
		SessionDir:     resumeDir,
		Task:           "PHASE9_SUCCESS",
		Agents:         []string{"gemini", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run resume seed: %v", err)
	}
	if _, err := Resume(context.Background(), resumeDir, ResumeOptions{Rounds: 1, TimeoutSeconds: 5}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if transcript := mustLoadTranscript(t, resumeDir); len(transcript) != 2 {
		t.Fatalf("resumed transcript = %#v", transcript)
	}
	cleanReport, err := CleanSession(resumeDir)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if cleanReport["status"] != "deleted" {
		t.Fatalf("clean report = %#v", cleanReport)
	}
	if _, err := os.Stat(resumeDir); !os.IsNotExist(err) {
		t.Fatalf("cleaned session still exists")
	}
}

func setupPhase9FakeProviders(t *testing.T) phase9Env {
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
from pathlib import Path

def main():
    prompt = sys.stdin.read()
    codex_home = Path(os.environ.get("CODEX_HOME", ""))
    suffix = codex_home.name or "default"
    if "resume" in sys.argv:
        idx = sys.argv.index("resume")
        thread_id = sys.argv[idx + 2] if idx + 2 < len(sys.argv) and sys.argv[idx + 1] == "--json" else "thread-resumed"
    else:
        thread_id = f"thread-{suffix}"
        print(json.dumps({"type": "thread.started", "thread_id": thread_id}), flush=True)
    if "Return the updated ledger as JSON" in prompt:
        text = '{"settled":["done"],"contested":[],"withdrawn":[]}'
    else:
        text = f"Fake Codex {suffix}"
    print(json.dumps({"type": "item.completed", "item": {"text": text}}), flush=True)
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`
	fakeGemini := `#!/usr/bin/env python3
import json
import os
import sys
import time
from pathlib import Path

def model_arg():
    if "--model" in sys.argv:
        idx = sys.argv.index("--model")
        if idx + 1 < len(sys.argv):
            return sys.argv[idx + 1]
    return ""

def main():
    prompt = sys.stdin.read()
    gemini_home = Path(os.environ.get("GEMINI_CLI_HOME") or os.environ.get("HOME", ""))
    suffix = gemini_home.name or "default"
    model = model_arg()
    if "PHASE9_TIMEOUT_RECOVERED" in prompt:
        print(json.dumps({"response": "recovered before timeout", "session_id": f"gemini-{suffix}"}), flush=True)
        time.sleep(5)
        return 0
    if "PHASE9_TIMEOUT_AUTH" in prompt:
        print(json.dumps({"response": "Authentication error: token expired", "session_id": f"gemini-{suffix}"}), flush=True)
        time.sleep(5)
        return 0
    if "PHASE9_TIMEOUT_EMPTY" in prompt:
        time.sleep(5)
        return 0
    if "PHASE9_NONZERO_RECOVERED" in prompt:
        print(json.dumps({"response": "recovered after nonzero", "session_id": f"gemini-{suffix}"}), flush=True)
        print("backend exited after partial output", file=sys.stderr, flush=True)
        return 7
    if "PHASE9_RETRYABLE" in prompt:
        print("API Error: rate limit exceeded", file=sys.stderr, flush=True)
        return 1
    if "PHASE9_STDERR_ONLY" in prompt:
        print("boom", file=sys.stderr, flush=True)
        return 1
    if "PHASE9_MALFORMED" in prompt:
        print("plain malformed fallback", flush=True)
        return 0
    print(json.dumps({"response": f"Fake Gemini {suffix} model={model}", "session_id": f"gemini-{suffix}"}), flush=True)
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(fakeCodex), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gemini"), []byte(fakeGemini), 0o755); err != nil {
		t.Fatalf("write fake gemini: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_CLAUDE_HOME", relayHome)
	return phase9Env{relayHome: relayHome, projectDir: projectDir, homeDir: homeDir}
}
