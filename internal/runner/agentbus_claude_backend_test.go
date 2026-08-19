package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/recipes"
)

type claudeFakeEnv struct {
	relayHome  string
	projectDir string
	homeDir    string
}

func TestEmbeddedClaudeBackendRunTurnStateAndResumeProtocol(t *testing.T) {
	env := setupClaudeFakeProviders(t)
	backend, err := newBackend("claude", env.relayHome, "slot_0", "Claude Code", env.projectDir, SlotConfig{
		Model:  "claude-sonnet",
		Effort: "high",
	})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	first, err := backend.RunTurn(context.Background(), "CLAUDE_SUCCESS first", TurnOptions{TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("first run turn: %v", err)
	}
	firstSessionID := stringFromAny(backend.SessionState()["session_id"])
	if firstSessionID == "" {
		t.Fatalf("first turn session state = %#v", backend.SessionState())
	}
	second, err := backend.RunTurn(context.Background(), "CLAUDE_SUCCESS second", TurnOptions{TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("second run turn: %v", err)
	}
	if first.Content != "first reply" || second.Content != "second reply" {
		t.Fatalf("responses = %q / %q", first.Content, second.Content)
	}
	if first.ProviderResult.Backend != "claude" || first.ProviderResult.ReturnCode != 0 {
		t.Fatalf("provider result = %#v", first.ProviderResult)
	}
	state := backend.SessionState()
	if state["session_id"] == "" ||
		state["started"] != true ||
		state["cwd"] != env.projectDir ||
		state["model"] != "claude-sonnet" ||
		state["effort"] != "high" {
		t.Fatalf("state = %#v", state)
	}

	commands := readClaudeCommands(t, env.homeDir)
	if len(commands) != 2 {
		t.Fatalf("commands = %#v", commands)
	}
	if !contains(commands[0], "-p") ||
		!contains(commands[0], "--input-format") ||
		!contains(commands[0], "--output-format") ||
		!contains(commands[0], "--verbose") ||
		!contains(commands[0], "--dangerously-skip-permissions") ||
		contains(commands[0], "--resume") {
		t.Fatalf("first stream-json command = %#v", commands[0])
	}
	if inputFormat, ok := valueAfter(commands[0], "--input-format"); !ok || inputFormat != "stream-json" {
		t.Fatalf("input format command = %#v", commands[0])
	}
	if outputFormat, ok := valueAfter(commands[0], "--output-format"); !ok || outputFormat != "stream-json" {
		t.Fatalf("output format command = %#v", commands[0])
	}
	if resumeSessionID, ok := valueAfter(commands[1], "--resume"); !ok || resumeSessionID != firstSessionID || contains(commands[1], "--session-id") {
		t.Fatalf("resume stream-json command = %#v", commands[1])
	}
	if !contains(commands[0], "--model") || !contains(commands[0], "claude-sonnet") ||
		!contains(commands[0], "--effort") || !contains(commands[0], "high") {
		t.Fatalf("model/effort command = %#v", commands[0])
	}
}

func TestEmbeddedClaudeBackendMissingBinaryReturnsBackendError(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	backend, err := newBackend("claude", root, "slot_0", "Claude Code", root, SlotConfig{})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	_, err = backend.RunTurn(context.Background(), "prompt", TurnOptions{TimeoutSeconds: 1})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "claude") {
		t.Fatalf("missing Claude binary error = %v", err)
	}
}

func TestEmbeddedClaudeBackendTimeoutReturnsPlaceholder(t *testing.T) {
	env := setupClaudeFakeProviders(t)
	backend, err := newBackend("claude", env.relayHome, "slot_0", "Claude Code", env.projectDir, SlotConfig{})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	result, err := backend.RunTurn(context.Background(), "CLAUDE_TIMEOUT", TurnOptions{TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("timeout turn: result = %#v, error = %v", result, err)
	}
	if result.Content != "[Claude Code timed out after 1s]" || !result.TimedOut || !result.ProviderResult.TimedOut || result.Recovered || result.ProviderResult.Recovered {
		t.Fatalf("timeout result = %#v", result)
	}
}

func TestEmbeddedClaudeBackendVisibleAuthFailureIsNotRetriedOrRecovered(t *testing.T) {
	env := setupClaudeFakeProviders(t)
	backend, err := newBackend("claude", env.relayHome, "slot_0", "Claude Code", env.projectDir, SlotConfig{})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	var retries int
	withFakeRetryBackoff(t, func(context.Context, time.Duration) error {
		retries++
		return nil
	})

	result, err := runWithRetryableProviderErrors(context.Background(), backend.Label(), func() (TurnResult, error) {
		return backend.RunTurn(context.Background(), "CLAUDE_AUTH_FAILURE", TurnOptions{TimeoutSeconds: 5})
	})
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("auth error = %T %v, want BackendRunError", err, err)
	}
	var retryableErr RetryableProviderError
	if errors.As(err, &retryableErr) {
		t.Fatalf("auth failure was classified retryable: %v", err)
	}
	if backendErr.Label != "Claude Code" || backendErr.Detail != "Authentication error: token expired" {
		t.Fatalf("backend error = %#v", backendErr)
	}
	if result.Content != "Authentication error: token expired" || result.Recovered || result.ProviderResult.Recovered || result.ProviderResult.RetryableError != "" {
		t.Fatalf("auth result = %#v", result)
	}
	if retries != 0 {
		t.Fatalf("auth failure retried %d times", retries)
	}
	if commands := readClaudeCommands(t, env.homeDir); len(commands) != 1 {
		t.Fatalf("auth failure command count = %d, want one", len(commands))
	}
}

func TestBuildAndRestoreSlotsSupportClaude(t *testing.T) {
	env := setupClaudeFakeProviders(t)
	profiles := map[string]map[string]any{
		"claude-profile": {
			"backend": "claude",
			"model":   "claude-sonnet",
			"effort":  "medium",
		},
	}
	slots, err := buildSlots(
		[]string{"claude-profile", "codex"},
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
	if slots[0].Name() != "claude" || slots[0].Label() != "Claude Code" {
		t.Fatalf("slots = %#v", slots)
	}
	claudeState := slots[0].SessionState()
	if claudeState["session_id"] == "" || claudeState["model"] != "claude-sonnet" || claudeState["effort"] != "medium" {
		t.Fatalf("claude state = %#v", claudeState)
	}

	meta := map[string]any{
		"slots": []any{
			map[string]any{
				"backend": "claude",
				"slot_id": "slot_0",
				"label":   "Claude Code",
				"state": map[string]any{
					"session_id": "claude-existing",
					"started":    true,
					"cwd":        env.projectDir,
					"model":      "old-model",
					"effort":     "old-effort",
				},
			},
			slotEnvelope(slots[1]),
		},
	}
	restored, err := restoreSlots(meta, env.relayHome, []SlotConfig{{Model: "new-model"}, {}}, recipes.RuntimeConfig{}, "", 0, 1)
	if err != nil {
		t.Fatalf("restore slots: %v", err)
	}
	restoredState := restored[0].SessionState()
	if restored[0].Name() != "claude" ||
		restoredState["session_id"] != "claude-existing" ||
		restoredState["started"] != true ||
		restoredState["model"] != "new-model" ||
		restoredState["effort"] != "old-effort" {
		t.Fatalf("restored claude state = %#v", restoredState)
	}
	if err := restored[0].RestoreState(map[string]any{"started": true}, SlotConfig{}); err == nil {
		t.Fatalf("missing session_id state unexpectedly restored")
	}
	for _, sessionID := range []string{".", "..", "../outside", `..\outside`, "nested/session", "session id"} {
		if err := restored[0].RestoreState(map[string]any{"session_id": sessionID}, SlotConfig{}); err == nil || !strings.Contains(err.Error(), "safe path component") {
			t.Fatalf("unsafe session_id %q restore error = %v", sessionID, err)
		}
	}
}

func TestRunResumeAndCleanClaudeSessions(t *testing.T) {
	env := setupClaudeFakeProviders(t)
	cases := []struct {
		name   string
		agents []string
		rounds int
	}{
		{name: "claude-claude", agents: []string{"claude", "claude"}, rounds: 1},
		{name: "claude-codex", agents: []string{"claude", "codex"}, rounds: 1},
		{name: "codex-claude", agents: []string{"codex", "claude"}, rounds: 2},
		{name: "profile-claude-code", agents: []string{"claude-code", "codex"}, rounds: 1},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			sessionDir := filepath.Join(env.relayHome, "sessions", tt.name)
			if _, err := Run(context.Background(), Options{
				SessionDir:     sessionDir,
				Task:           "CLAUDE_SUCCESS",
				Agents:         tt.agents,
				Rounds:         tt.rounds,
				TimeoutSeconds: 5,
				LaunchCWD:      env.projectDir,
			}); err != nil {
				t.Fatalf("run: %v", err)
			}
			meta := mustLoadMeta(t, sessionDir)
			slots := meta["slots"].([]any)
			foundClaude := false
			for _, rawSlot := range slots {
				slot := rawSlot.(map[string]any)
				if slot["backend"] != "claude" {
					continue
				}
				foundClaude = true
				state := slot["state"].(map[string]any)
				if state["session_id"] == "" || state["cwd"] != env.projectDir {
					t.Fatalf("claude slot state = %#v", state)
				}
			}
			if !foundClaude {
				t.Fatalf("session has no claude slot: %#v", slots)
			}
			transcript := mustLoadTranscript(t, sessionDir)
			foundClaudeTurn := false
			for _, entry := range transcript {
				if !strings.HasPrefix(stringFromAny(entry["from"]), "Claude Code") {
					continue
				}
				foundClaudeTurn = true
				providerResult := entry["provider_result"].(map[string]any)
				if providerResult["backend"] != "claude" {
					t.Fatalf("claude provider_result = %#v", providerResult)
				}
			}
			if !foundClaudeTurn {
				t.Fatalf("transcript has no claude turn: %#v", transcript)
			}
			if _, err := inspect.BuildContractsReport(sessionDir, false, "", ""); err != nil {
				t.Fatalf("contracts report: %v", err)
			}
		})
	}

	resumeDir := filepath.Join(env.relayHome, "sessions", "claude-resume-clean")
	if _, err := Run(context.Background(), Options{
		SessionDir:     resumeDir,
		Task:           "CLAUDE_SUCCESS",
		Agents:         []string{"claude", "codex"},
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
	if err != nil || cleanReport["status"] != "deleted" {
		t.Fatalf("clean: %#v, %v", cleanReport, err)
	}
	if _, err := os.Stat(resumeDir); !os.IsNotExist(err) {
		t.Fatalf("cleaned session still exists")
	}
}

func TestRunPersistsClaudeProviderResult(t *testing.T) {
	env := setupClaudeFakeProviders(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "claude-provider-result")
	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "CLAUDE_SUCCESS",
		Agents:         []string{"claude", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if len(transcript) != 1 {
		t.Fatalf("transcript = %#v", transcript)
	}
	providerResult := transcript[0]["provider_result"].(map[string]any)
	if providerResult["backend"] != "claude" ||
		providerResult["timed_out"] != false ||
		providerResult["stalled"] != false ||
		providerResult["recovered"] != false ||
		intFromAny(providerResult["return_code"], -1) != 0 {
		t.Fatalf("provider_result = %#v", providerResult)
	}
}

func setupClaudeFakeProviders(t *testing.T) claudeFakeEnv {
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
	writeEmbeddedProviderFakes(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_CLAUDE_HOME", relayHome)
	t.Setenv("CONVO_RELAY_COMMAND_HOME", homeDir)
	return claudeFakeEnv{relayHome: relayHome, projectDir: projectDir, homeDir: homeDir}
}

func writeEmbeddedProviderFakes(t *testing.T, binDir string) {
	t.Helper()
	for name, script := range map[string]string{
		"codex":  fakeCodexAppServerScript,
		"claude": fakeClaudeStreamJSONScript,
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
}

func writeFakeCodexAppServer(t *testing.T, binDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(fakeCodexAppServerScript), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
}

func readClaudeCommands(t *testing.T, homeDir string) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(homeDir, "claude_commands.jsonl"))
	if err != nil {
		t.Fatalf("read claude commands: %v", err)
	}
	var commands [][]string
	for _, rawLine := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(rawLine) == "" {
			continue
		}
		var command []string
		if err := json.Unmarshal([]byte(rawLine), &command); err != nil {
			t.Fatalf("decode command %q: %v", rawLine, err)
		}
		commands = append(commands, command)
	}
	return commands
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func valueAfter(values []string, flag string) (string, bool) {
	for index, value := range values {
		if value == flag && index+1 < len(values) {
			return values[index+1], true
		}
	}
	return "", false
}

const fakeCodexAppServerScript = `#!/usr/bin/env python3
import json
import os
import sys
import time
from pathlib import Path

def send(value):
    print(json.dumps(value), flush=True)

def response(request, result):
    send({"id": request.get("id"), "result": result})

def prompt_from(params):
    items = params.get("input", [])
    if items and isinstance(items[0], dict):
        return str(items[0].get("text", ""))
    return ""

def text_for(prompt, suffix):
    if "Return the updated ledger as JSON" in prompt and "Persistent contested dynamic" in prompt:
        return '{"settled":[],"contested":["phase risk"],"withdrawn":[]}', ""
    if "Return the updated ledger as JSON" in prompt and "Malformed ledger stall" in prompt:
        return "not a ledger " + ("💥" * 60), ""
    if "Return the updated ledger as JSON" in prompt and ("Explicit empty ledger stall" in prompt or "Empty ledger done convergence" in prompt):
        return '{"settled":[],"contested":[],"withdrawn":[]}', ""
    if "Return the updated ledger as JSON" in prompt:
        return '{"settled":["done"],"contested":[],"withdrawn":[]}', ""
    if "PHASE8_RETRY_ALWAYS" in prompt:
        return "", "API Error: rate limit exceeded"
    if "PHASE8_RETRY_THEN_SUCCESS" in prompt:
        marker = Path(os.environ.get("CODEX_HOME", ".")) / "phase8_retry_count"
        count = int(marker.read_text(encoding="utf-8")) if marker.exists() else 0
        marker.write_text(str(count + 1), encoding="utf-8")
        if count == 0:
            return "", "API Error: rate limit exceeded"
        return "retry succeeded", ""
    if "PHASE8_AUTH_FAILURE" in prompt:
        return "", "Authentication error: token expired"
    if "Empty ledger done convergence" in prompt:
        return "task is complete; no further changes", ""
    if "Explicit empty ledger stall" in prompt:
        return "Explicit empty ledger stall response", ""
    if "Malformed ledger stall" in prompt:
        return "Malformed ledger stall response", ""
    if "Resume context marker" in prompt and "Resume skill marker" in prompt:
        return "Fake Codex saw resume input bundles", ""
    if "Phase 7 steering marker" in prompt:
        return "Fake Codex saw Phase 7 steering marker", ""
    if "Persistent contested dynamic" in prompt:
        return "Persistent contested dynamic phase risk remains unresolved", ""
    lines = prompt.splitlines()
    first = lines[0] if lines else ""
    return f"Fake Codex {suffix}: {first[:80]}", ""

def main():
    if "--version" in sys.argv:
        print("codex fake 1.0")
        return 0
    if len(sys.argv) < 2 or sys.argv[1] != "app-server":
        print("expected codex app-server", file=sys.stderr)
        return 2

    suffix = Path(os.environ.get("CODEX_HOME", "slot")).name or "slot"
    thread_id = f"thread-{suffix}"
    turn_number = 0
    for raw in sys.stdin:
        try:
            request = json.loads(raw)
        except json.JSONDecodeError:
            continue
        method = request.get("method", "")
        params = request.get("params", {}) or {}
        if method == "initialize":
            response(request, {"serverInfo": {"name": "fake-codex"}})
        elif method in ("thread/start", "thread/resume"):
            thread_id = params.get("threadId") or thread_id
            response(request, {"thread": {"id": thread_id}})
        elif method == "model/list":
            response(request, {"data": [{"id": "fake-codex-model", "supportedReasoningEfforts": ["low", "high"]}]})
        elif method == "turn/start":
            turn_number += 1
            turn_id = f"turn-{turn_number}"
            response(request, {"turn": {"id": turn_id}})
            prompt = prompt_from(params)
            if "Slow cancellation check" in prompt:
                time.sleep(30)
                continue
            text, failure = text_for(prompt, suffix)
            if failure:
                send({"method": "turn/completed", "params": {
                    "threadId": thread_id,
                    "turn": {"id": turn_id, "status": "failed", "error": {"message": failure}},
                }})
                continue
            send({"method": "item/completed", "params": {
                "item": {"id": f"item-{turn_number}", "type": "agentMessage", "text": text},
            }})
            send({"method": "turn/completed", "params": {
                "threadId": thread_id,
                "turn": {"id": turn_id, "status": "completed"},
            }})
        elif method == "turn/interrupt":
            response(request, {})
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`

const fakeClaudeStreamJSONScript = `#!/usr/bin/env python3
import json
import os
import sys
import time
from pathlib import Path

def arg_value(flag):
    if flag in sys.argv:
        index = sys.argv.index(flag)
        if index + 1 < len(sys.argv):
            return sys.argv[index + 1]
    return ""

def send(value):
    print(json.dumps(value), flush=True)

def text_for(prompt):
    if "Return the updated ledger as JSON" in prompt:
        return '{"settled":["claude facilitator"],"contested":[],"withdrawn":[]}', ""
    if "CLAUDE_RETRYABLE" in prompt:
        return "", "API Error: rate limit exceeded"
    if "CLAUDE_AUTH_FAILURE" in prompt:
        return "Authentication error: token expired", "Authentication error: token expired"
    if "second" in prompt.lower():
        return "second reply", ""
    if "first" in prompt.lower():
        return "first reply", ""
    return "Fake Claude response", ""

def log_command():
    home = Path(os.environ.get("CONVO_RELAY_COMMAND_HOME") or os.environ.get("HOME", "."))
    home.mkdir(parents=True, exist_ok=True)
    with (home / "claude_commands.jsonl").open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(sys.argv[1:]) + "\n")

def main():
    if "--version" in sys.argv:
        print("2.1.205")
        return 0
    if "--help" in sys.argv:
        print("--effort values (low, medium, high, max)\n--model examples (fake-claude-model)")
        return 0
    log_command()
    init_line = sys.stdin.readline()
    if not init_line:
        return 0
    initialize = json.loads(init_line)
    request_id = initialize.get("request_id", "")
    send({"type": "control_response", "response": {"subtype": "success", "request_id": request_id, "response": {}}})
    user_line = sys.stdin.readline()
    if not user_line:
        return 0
    user = json.loads(user_line)
    prompt = str((user.get("message") or {}).get("content", ""))
    session_id = arg_value("--resume") or f"claude-{os.getpid()}"
    if "CLAUDE_TIMEOUT" in prompt:
        while True:
            time.sleep(1)
    text, failure = text_for(prompt)
    send({"type": "system", "session_id": session_id, "model": arg_value("--model") or "fake-claude-model"})
    if failure:
        if text:
            send({"type": "assistant", "session_id": session_id, "message": {"content": [{"type": "text", "text": text}]}})
        send({"type": "result", "session_id": session_id, "subtype": "error", "is_error": True, "error": failure})
        return 0
    send({"type": "assistant", "session_id": session_id, "message": {"content": [{"type": "text", "text": text}]}})
    send({"type": "result", "session_id": session_id, "subtype": "success", "is_error": False, "result": text})
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`
