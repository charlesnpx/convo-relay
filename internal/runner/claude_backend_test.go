package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/recipes"
)

type phase10Env struct {
	relayHome  string
	projectDir string
	homeDir    string
}

func TestClaudeHelpersMatchPythonBehavior(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectDir := claudeProjectDir("/tmp/project dir")
	if projectDir != filepath.Join(home, ".claude", "projects", "-tmp-project-dir") {
		t.Fatalf("claudeProjectDir = %q", projectDir)
	}

	jsonlPath := filepath.Join(home, "main.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("abcd"), 0o644); err != nil {
		t.Fatalf("write main jsonl: %v", err)
	}
	subagentsDir := filepath.Join(home, "main", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subagentsDir, "one.jsonl"), []byte("12345"), 0o644); err != nil {
		t.Fatalf("write subagent one: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subagentsDir, "two.jsonl"), []byte("xy"), 0o644); err != nil {
		t.Fatalf("write subagent two: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subagentsDir, "ignore.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write ignored subagent: %v", err)
	}
	if size := jsonlTotalSize(jsonlPath); size != 11 {
		t.Fatalf("jsonlTotalSize = %d, want 11", size)
	}

	extractPath := filepath.Join(home, "extract.jsonl")
	prefix := []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"old\"}]}}\n")
	body := []byte(strings.Join([]string{
		`{"type":"user","message":{"content":[{"type":"text","text":"skip"}]}}`,
		`not json`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"first"},{"type":"tool_use","text":"skip"},{"type":"text","text":"second"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"   "}]}}`,
	}, "\n"))
	if err := os.WriteFile(extractPath, append(prefix, body...), 0o644); err != nil {
		t.Fatalf("write extract jsonl: %v", err)
	}
	if got := extractClaudeResponse(extractPath, int64(len(prefix))); got != "first\n\nsecond" {
		t.Fatalf("extractClaudeResponse = %q", got)
	}
}

func TestClaudeBackendRunTurnStateAndResumeCommand(t *testing.T) {
	env := setupPhase10FakeProviders(t)
	backend := newClaudeBackend(env.relayHome, "slot_0", "Claude Code", env.projectDir, SlotConfig{
		Model:  "claude-sonnet",
		Effort: "high",
	})

	first, err := backend.RunTurn(context.Background(), "PHASE10_SUCCESS first", TurnOptions{TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("first run turn: %v", err)
	}
	second, err := backend.RunTurn(context.Background(), "PHASE10_SUCCESS second", TurnOptions{TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("second run turn: %v", err)
	}
	if !strings.Contains(first.Content, "first") || !strings.Contains(second.Content, "second") {
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
	if !contains(commands[0], "--session-id") || contains(commands[0], "--resume") {
		t.Fatalf("first command = %#v", commands[0])
	}
	if !contains(commands[1], "--resume") || contains(commands[1], "--session-id") {
		t.Fatalf("second command = %#v", commands[1])
	}
	if !contains(commands[0], "--model") || !contains(commands[0], "claude-sonnet") ||
		!contains(commands[0], "--effort") || !contains(commands[0], "high") {
		t.Fatalf("model/effort command = %#v", commands[0])
	}
}

func TestClaudeBackendLifecycleOutcomes(t *testing.T) {
	withFastClaudePoll(t)
	tests := []struct {
		name           string
		prompt         string
		timeout        int
		stallTimeout   int
		wantContent    string
		wantTimedOut   bool
		wantStalled    bool
		wantRecovered  bool
		wantReturnCode int
		wantSource     string
	}{
		{name: "stdout fallback", prompt: "PHASE10_STDOUT_FALLBACK", timeout: 5, wantContent: "stdout fallback", wantReturnCode: 0},
		{name: "nonzero recovery", prompt: "PHASE10_NONZERO_RECOVERED", timeout: 5, wantContent: "recovered after nonzero", wantRecovered: true, wantReturnCode: 7, wantSource: "jsonl"},
		{name: "timeout recovery", prompt: "PHASE10_TIMEOUT_RECOVERED", timeout: 1, stallTimeout: 10, wantContent: "recovered before timeout", wantTimedOut: true, wantRecovered: true, wantReturnCode: -1, wantSource: "jsonl"},
		{name: "timeout empty", prompt: "PHASE10_TIMEOUT_EMPTY", timeout: 1, stallTimeout: 10, wantContent: "[Claude Code timed out after 1s]", wantTimedOut: true, wantReturnCode: -1},
		{name: "stall recovery", prompt: "PHASE10_STALL_RECOVERED", timeout: 10, stallTimeout: 1, wantContent: "recovered before stall", wantStalled: true, wantRecovered: true, wantReturnCode: -1, wantSource: "jsonl"},
		{name: "stall empty", prompt: "PHASE10_STALL_EMPTY", timeout: 10, stallTimeout: 1, wantContent: "[Claude Code stalled after 1s of no JSONL activity]", wantStalled: true, wantReturnCode: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setupPhase10FakeProviders(t)
			backend := newClaudeBackend(env.relayHome, "slot_0", "Claude Code", env.projectDir, SlotConfig{})

			result, err := backend.RunTurn(context.Background(), tt.prompt, TurnOptions{TimeoutSeconds: tt.timeout, StallTimeoutSeconds: tt.stallTimeout})
			if err != nil {
				t.Fatalf("run turn: %v", err)
			}
			if result.Content != tt.wantContent ||
				result.ProviderResult.TimedOut != tt.wantTimedOut ||
				result.ProviderResult.Stalled != tt.wantStalled ||
				result.ProviderResult.Recovered != tt.wantRecovered ||
				result.ProviderResult.ReturnCode != tt.wantReturnCode ||
				result.ProviderResult.RecoverySource != tt.wantSource {
				t.Fatalf("result = %#v, provider = %#v", result, result.ProviderResult)
			}
		})
	}
}

func TestClaudeBackendRetriesSessionCollisionAndClassifiesErrors(t *testing.T) {
	env := setupPhase10FakeProviders(t)
	backend := newClaudeBackend(env.relayHome, "slot_0", "Claude Code", env.projectDir, SlotConfig{})
	oldSessionID := stringFromAny(backend.SessionState()["session_id"])
	result, err := backend.RunTurn(context.Background(), "PHASE10_SESSION_COLLISION", TurnOptions{TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("session collision run turn: %v", err)
	}
	if result.Content != "fresh response" {
		t.Fatalf("content = %q", result.Content)
	}
	newSessionID := stringFromAny(backend.SessionState()["session_id"])
	if newSessionID == "" || newSessionID == oldSessionID {
		t.Fatalf("session id was not refreshed: old=%q new=%q", oldSessionID, newSessionID)
	}
	commands := readClaudeCommands(t, env.homeDir)
	if len(commands) != 2 || commands[0][argIndex(commands[0], "--session-id")+1] != oldSessionID || commands[1][argIndex(commands[1], "--session-id")+1] != newSessionID {
		t.Fatalf("commands = %#v, old=%q new=%q", commands, oldSessionID, newSessionID)
	}

	backend = newClaudeBackend(env.relayHome, "slot_1", "Claude Code", env.projectDir, SlotConfig{})
	_, err = backend.RunTurn(context.Background(), "PHASE10_RETRYABLE", TurnOptions{TimeoutSeconds: 5})
	var retryable RetryableProviderError
	if !errors.As(err, &retryable) {
		t.Fatalf("error = %T %[1]v, want RetryableProviderError", err)
	}

	backend = newClaudeBackend(env.relayHome, "slot_2", "Claude Code", env.projectDir, SlotConfig{})
	_, err = backend.RunTurn(context.Background(), "PHASE10_API_ERROR_PARTIAL", TurnOptions{TimeoutSeconds: 5})
	if !errors.As(err, &retryable) {
		t.Fatalf("partial api error = %T %[1]v, want RetryableProviderError", err)
	}

	backend = newClaudeBackend(env.relayHome, "slot_3", "Claude Code", env.projectDir, SlotConfig{})
	_, err = backend.RunTurn(context.Background(), "PHASE10_STDERR_ONLY", TurnOptions{TimeoutSeconds: 5})
	if err == nil || errors.As(err, &retryable) || !strings.Contains(err.Error(), "Claude Code failed: boom") {
		t.Fatalf("stderr-only error = %v", err)
	}
}

func TestClaudeBackendAuthTimeoutRecoveryIsNotRecovered(t *testing.T) {
	withFastClaudePoll(t)
	env := setupPhase10FakeProviders(t)
	backend := newClaudeBackend(env.relayHome, "slot_0", "Claude Code", env.projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "PHASE10_TIMEOUT_AUTH", TurnOptions{TimeoutSeconds: 1, StallTimeoutSeconds: 10})
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

func TestClaudeBackendMissingBinaryIsNotRetryable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("HOME", filepath.Join(root, "home"))
	projectDir := filepath.Join(root, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	backend := newClaudeBackend(root, "slot_0", "Claude Code", projectDir, SlotConfig{})

	_, err := backend.RunTurn(context.Background(), "prompt", TurnOptions{TimeoutSeconds: 1})
	var retryable RetryableProviderError
	if err == nil {
		t.Fatalf("missing binary unexpectedly succeeded")
	}
	if errors.As(err, &retryable) {
		t.Fatalf("missing binary was classified retryable: %v", err)
	}
}

func TestBuildAndRestoreSlotsSupportClaude(t *testing.T) {
	env := setupPhase10FakeProviders(t)
	profiles := map[string]map[string]any{
		"phase10-claude": {
			"backend": "claude",
			"model":   "claude-sonnet",
			"effort":  "medium",
		},
	}
	slots, err := buildSlots(
		[]string{"phase10-claude", "codex"},
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
}

func TestRunResumeAndCleanClaudeSessions(t *testing.T) {
	env := setupPhase10FakeProviders(t)
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
				Task:           "PHASE10_SUCCESS",
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
				if slot["backend"] == "claude" {
					foundClaude = true
					state := slot["state"].(map[string]any)
					if state["session_id"] == "" || state["cwd"] != env.projectDir {
						t.Fatalf("claude slot state = %#v", state)
					}
				}
			}
			if !foundClaude {
				t.Fatalf("session has no claude slot: %#v", slots)
			}
			transcript := mustLoadTranscript(t, sessionDir)
			foundClaudeTurn := false
			for _, entry := range transcript {
				if strings.HasPrefix(stringFromAny(entry["from"]), "Claude Code") {
					foundClaudeTurn = true
					providerResult := entry["provider_result"].(map[string]any)
					if providerResult["backend"] != "claude" {
						t.Fatalf("claude provider_result = %#v", providerResult)
					}
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
		Task:           "PHASE10_SUCCESS",
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

	meta := mustLoadMeta(t, resumeDir)
	claudeState := firstClaudeState(t, meta)
	jsonlPath := filepath.Join(claudeProjectDir(stringFromAny(claudeState["cwd"])), stringFromAny(claudeState["session_id"])+".jsonl")
	sessionArtifactDir := filepath.Join(filepath.Dir(jsonlPath), stringFromAny(claudeState["session_id"]))
	if err := os.MkdirAll(sessionArtifactDir, 0o755); err != nil {
		t.Fatalf("mkdir session artifact dir: %v", err)
	}
	unrelated := filepath.Join(filepath.Dir(jsonlPath), "other-session.jsonl")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write unrelated claude history: %v", err)
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
	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Fatalf("claude jsonl was not removed")
	}
	if _, err := os.Stat(sessionArtifactDir); !os.IsNotExist(err) {
		t.Fatalf("claude session artifact dir was not removed")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated claude history should remain: %v", err)
	}
}

func TestRunPersistsClaudeLifecycleProviderResults(t *testing.T) {
	withFastClaudePoll(t)
	tests := []struct {
		name           string
		task           string
		timeout        int
		stallTimeout   int
		wantTimedOut   bool
		wantStalled    bool
		wantRecovered  bool
		wantReturnCode int
	}{
		{name: "stall recovery", task: "PHASE10_STALL_RECOVERED", timeout: 10, stallTimeout: 1, wantStalled: true, wantRecovered: true, wantReturnCode: -1},
		{name: "timeout recovery", task: "PHASE10_TIMEOUT_RECOVERED", timeout: 1, stallTimeout: 10, wantTimedOut: true, wantRecovered: true, wantReturnCode: -1},
		{name: "nonzero recovery", task: "PHASE10_NONZERO_RECOVERED", timeout: 5, stallTimeout: 10, wantRecovered: true, wantReturnCode: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setupPhase10FakeProviders(t)
			sessionDir := filepath.Join(env.relayHome, "sessions", strings.ReplaceAll(tt.name, " ", "-"))

			if _, err := Run(context.Background(), Options{
				SessionDir:          sessionDir,
				Task:                tt.task,
				Agents:              []string{"claude", "codex"},
				Rounds:              1,
				TimeoutSeconds:      tt.timeout,
				StallTimeoutSeconds: tt.stallTimeout,
				LaunchCWD:           env.projectDir,
			}); err != nil {
				t.Fatalf("run: %v", err)
			}
			transcript := mustLoadTranscript(t, sessionDir)
			if len(transcript) != 1 {
				t.Fatalf("transcript = %#v", transcript)
			}
			providerResult := transcript[0]["provider_result"].(map[string]any)
			if providerResult["backend"] != "claude" ||
				providerResult["timed_out"] != tt.wantTimedOut ||
				providerResult["stalled"] != tt.wantStalled ||
				providerResult["recovered"] != tt.wantRecovered ||
				providerResult["recovery_source"] != "jsonl" ||
				intFromAny(providerResult["return_code"], 0) != tt.wantReturnCode {
				t.Fatalf("provider_result = %#v", providerResult)
			}
		})
	}
}

func setupPhase10FakeProviders(t *testing.T) phase10Env {
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
	fakeClaude := `#!/usr/bin/env python3
import json
import os
import re
import sys
import time
from pathlib import Path

def arg_value(flag):
    if flag in sys.argv:
        idx = sys.argv.index(flag)
        if idx + 1 < len(sys.argv):
            return sys.argv[idx + 1]
    return ""

def session_id():
    return arg_value("--session-id") or arg_value("--resume")

def project_dir():
    cwd = os.environ.get("CONVO_RELAY_FAKE_CWD") or os.getcwd()
    encoded = re.sub(r"[^a-zA-Z0-9-]", "-", cwd)
    return Path.home() / ".claude" / "projects" / encoded

def append_response(text):
    sid = session_id()
    root = project_dir()
    root.mkdir(parents=True, exist_ok=True)
    (root / sid).mkdir(exist_ok=True)
    path = root / f"{sid}.jsonl"
    record = {"type": "assistant", "message": {"content": [{"type": "text", "text": text}]}}
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(record) + "\n")

def log_command():
    path = Path.home() / "claude_commands.jsonl"
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(sys.argv[1:]) + "\n")

def main():
    prompt = sys.stdin.read()
    log_command()
    if "PHASE10_SESSION_COLLISION" in prompt:
        marker = Path.home() / ".claude" / "collision_seen"
        marker.parent.mkdir(parents=True, exist_ok=True)
        if not marker.exists():
            marker.write_text("seen", encoding="utf-8")
            print(f"Error: Session ID {session_id()} is already in use.", file=sys.stderr, flush=True)
            return 1
        append_response("fresh response")
        return 0
    if "PHASE10_STDOUT_FALLBACK" in prompt:
        print("stdout fallback", flush=True)
        return 0
    if "PHASE10_TIMEOUT_EMPTY" in prompt:
        time.sleep(5)
        return 0
    if "PHASE10_STALL_EMPTY" in prompt:
        time.sleep(5)
        return 0
    if "PHASE10_TIMEOUT_RECOVERED" in prompt:
        append_response("recovered before timeout")
        time.sleep(5)
        return 0
    if "PHASE10_TIMEOUT_AUTH" in prompt:
        append_response("Authentication error: token expired")
        time.sleep(5)
        return 0
    if "PHASE10_STALL_RECOVERED" in prompt:
        append_response("recovered before stall")
        time.sleep(5)
        return 0
    if "PHASE10_NONZERO_RECOVERED" in prompt:
        append_response("recovered after nonzero")
        print("backend exited after partial output", file=sys.stderr, flush=True)
        return 7
    if "PHASE10_RETRYABLE" in prompt:
        print("API Error: rate limit exceeded", file=sys.stderr, flush=True)
        return 1
    if "PHASE10_API_ERROR_PARTIAL" in prompt:
        append_response("API Error: rate limit exceeded")
        return 1
    if "PHASE10_STDERR_ONLY" in prompt:
        print("boom", file=sys.stderr, flush=True)
        return 1
    if "second" in prompt.lower():
        append_response("second reply")
    elif "first" in prompt.lower():
        append_response("first reply")
    else:
        append_response("Fake Claude response")
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(fakeCodex), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_CLAUDE_HOME", relayHome)
	t.Setenv("CONVO_RELAY_FAKE_CWD", projectDir)
	return phase10Env{relayHome: relayHome, projectDir: projectDir, homeDir: homeDir}
}

func withFastClaudePoll(t *testing.T) {
	t.Helper()
	previous := defaultClaudePollInterval
	defaultClaudePollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		defaultClaudePollInterval = previous
	})
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

func contains(values []string, target string) bool {
	return argIndex(values, target) >= 0
}

func argIndex(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func firstClaudeState(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()
	slots, _ := meta["slots"].([]any)
	for _, rawSlot := range slots {
		slot, _ := rawSlot.(map[string]any)
		if slot["backend"] == "claude" {
			state, _ := slot["state"].(map[string]any)
			return state
		}
	}
	t.Fatalf("no claude slot in meta: %#v", meta)
	return nil
}
