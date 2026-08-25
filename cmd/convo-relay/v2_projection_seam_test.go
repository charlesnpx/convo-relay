package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type u2db3CLIResult struct {
	stdout   string
	stderr   string
	exitCode int
}

type u2db3CLIEnv struct {
	binary       string
	relayHome    string
	workDir      string
	settingsPath string
	startedPath  string
	commandEnv   []string
}

func TestV2CancellationProjectionSeam(t *testing.T) {
	env := newU2DB3CLIEnv(t, "cancel")
	command := env.command(
		"run", "--home", env.relayHome, "--session-id", "cancelled",
		"--task", "cancel a provider call", "--agents", "gemini", "--rounds", "1",
		"--settings", env.settingsPath, "--launch-cwd", env.workDir,
		"--timeout", "30", "--stall-timeout", "30", "--json",
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start cancellation run: %v", err)
	}
	if !u2db3WaitForFile(env.startedPath) {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("provider did not reach the cancellable turn")
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt relay: %v", err)
	}
	runErr := command.Wait()
	if runErr == nil {
		t.Fatal("cancelled run exited successfully")
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("cancelled run error = %v", runErr)
	}
	immediate := u2db3Report(t, u2db3CLIResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: exitErr.ExitCode()})
	if got := u2db3Status(t, immediate); got != "interrupted" {
		t.Fatalf("immediate cancellation status = %q, report=%#v", got, immediate)
	}

	show := env.run(t, "show", "--home", env.relayHome, "--json", "cancelled")
	u2db3RequireExit(t, show, 0)
	if got := u2db3Status(t, u2db3Report(t, show)); got != "interrupted" {
		t.Fatalf("show cancellation status = %q", got)
	}
	resumed := env.run(t, "resume", "--home", env.relayHome, "--timeout", "30", "--stall-timeout", "30", "--json", "cancelled")
	u2db3RequireExit(t, resumed, 0)
	if got := u2db3Status(t, u2db3Report(t, resumed)); got != "completed" {
		t.Fatalf("resume cancellation status = %q", got)
	}
	show = env.run(t, "show", "--home", env.relayHome, "--json", "cancelled")
	u2db3RequireExit(t, show, 0)
	if got := u2db3Status(t, u2db3Report(t, show)); got != "completed" {
		t.Fatalf("show resumed status = %q", got)
	}
}

func TestV2AskModeProjectionSeam(t *testing.T) {
	env := newU2DB3CLIEnv(t, "ask")
	if err := os.WriteFile(env.settingsPath, []byte(u2db3AskSettings), 0o600); err != nil {
		t.Fatalf("write ask-mode settings: %v", err)
	}
	initial := env.run(t,
		"run", "--home", env.relayHome, "--session-id", "ask-mode",
		"--task", "ask for a child decision", "--agents", "gemini", "--rounds", "1", "--dynamic", "ask",
		"--settings", env.settingsPath, "--launch-cwd", env.workDir,
		"--timeout", "30", "--stall-timeout", "30", "--json",
	)
	u2db3RequireExit(t, initial, 0)
	immediate := u2db3Report(t, initial)
	if got := u2db3Status(t, immediate); got != "awaiting_decision" {
		t.Fatalf("immediate ask-mode status = %q, report=%#v", got, immediate)
	}
	if got := u2db3ValidationStatus(t, immediate); got != "pending" {
		t.Fatalf("ask-mode validation status = %q, report=%#v", got, immediate)
	}

	show := env.run(t, "show", "--home", env.relayHome, "--json", "ask-mode")
	u2db3RequireExit(t, show, 0)
	if got := u2db3Status(t, u2db3Report(t, show)); got != "awaiting_decision" {
		t.Fatalf("show ask-mode status = %q", got)
	}
	if got := env.listStatus(t, "ask-mode"); got != "awaiting_decision" {
		t.Fatalf("list ask-mode status = %q", got)
	}

	proposalID := env.proposalID(t, "ask-mode")
	approval := env.run(t,
		"control", "approve", "--home", env.relayHome, "--proposal", proposalID, "--rounds", "1",
		"--timeout", "30", "--stall-timeout", "30", "ask-mode",
	)
	u2db3RequireExit(t, approval, 0)
	resumed := env.run(t, "resume", "--home", env.relayHome, "--timeout", "30", "--stall-timeout", "30", "--json", "ask-mode")
	u2db3RequireExit(t, resumed, 0)
	if got := u2db3Status(t, u2db3Report(t, resumed)); got != "completed" {
		t.Fatalf("resume approved ask-mode status = %q", got)
	}
	show = env.run(t, "show", "--home", env.relayHome, "--json", "ask-mode")
	u2db3RequireExit(t, show, 0)
	if got := u2db3Status(t, u2db3Report(t, show)); got != "completed" {
		t.Fatalf("show approved ask-mode status = %q", got)
	}
	if got := env.listStatus(t, "ask-mode"); got != "completed" {
		t.Fatalf("list approved ask-mode status = %q", got)
	}
}

func newU2DB3CLIEnv(t *testing.T, mode string) *u2db3CLIEnv {
	t.Helper()
	root := t.TempDir()
	currentDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get command directory: %v", err)
	}
	env := &u2db3CLIEnv{
		binary:       filepath.Join(root, "convo-relay"),
		relayHome:    filepath.Join(root, "relay-home"),
		workDir:      filepath.Join(root, "work"),
		settingsPath: filepath.Join(root, "settings.toml"),
		startedPath:  filepath.Join(root, "provider-started"),
	}
	binDir := filepath.Join(root, "bin")
	homeDir := filepath.Join(root, "home")
	statePath := filepath.Join(root, "provider-state")
	for _, path := range []string{binDir, homeDir, env.relayHome, env.workDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}
	if err := os.WriteFile(env.settingsPath, nil, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gemini"), []byte(u2db3GeminiScript), 0o755); err != nil {
		t.Fatalf("write fake gemini: %v", err)
	}
	build := exec.Command("go", "build", "-o", env.binary, ".")
	build.Dir = currentDir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	env.commandEnv = u2db3ReplaceEnv(os.Environ(), map[string]string{
		"PATH":          binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME":          homeDir,
		"U2DB3_MODE":    mode,
		"U2DB3_STATE":   statePath,
		"U2DB3_STARTED": env.startedPath,
	})
	return env
}

func (env *u2db3CLIEnv) command(args ...string) *exec.Cmd {
	command := exec.Command(env.binary, args...)
	command.Dir = env.workDir
	command.Env = env.commandEnv
	return command
}

func (env *u2db3CLIEnv) run(t *testing.T, args ...string) u2db3CLIResult {
	t.Helper()
	command := env.command(args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	result := u2db3CLIResult{}
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("start %q: %v", strings.Join(args, " "), err)
		}
		result.exitCode = exitErr.ExitCode()
	}
	result.stdout = stdout.String()
	result.stderr = stderr.String()
	return result
}

func (env *u2db3CLIEnv) listStatus(t *testing.T, sessionID string) string {
	t.Helper()
	result := env.run(t, "list", "--home", env.relayHome, "--json")
	u2db3RequireExit(t, result, 0)
	report := u2db3Report(t, result)
	sessions, ok := report["sessions"].([]any)
	if !ok {
		t.Fatalf("list sessions = %#v", report)
	}
	for _, raw := range sessions {
		item, ok := raw.(map[string]any)
		if !ok || item["session_id"] != sessionID {
			continue
		}
		return u2db3Status(t, item)
	}
	t.Fatalf("session %q not listed: %#v", sessionID, report)
	return ""
}

func (env *u2db3CLIEnv) proposalID(t *testing.T, sessionID string) string {
	t.Helper()
	result := env.run(t, "show", "--graph", "--home", env.relayHome, "--json", sessionID)
	u2db3RequireExit(t, result, 0)
	report := u2db3Report(t, result)
	graph, ok := report["graph"].(map[string]any)
	if !ok {
		t.Fatalf("graph report = %#v", report)
	}
	proposals, ok := graph["proposals"].(map[string]any)
	if !ok || len(proposals) != 1 {
		t.Fatalf("graph proposals = %#v", graph)
	}
	for proposalID := range proposals {
		return proposalID
	}
	t.Fatal("missing graph proposal id")
	return ""
}

func u2db3ReplaceEnv(base []string, replacements map[string]string) []string {
	result := make([]string, 0, len(base)+len(replacements))
	for _, item := range base {
		name, _, found := strings.Cut(item, "=")
		if found {
			if _, replaced := replacements[name]; replaced {
				continue
			}
		}
		result = append(result, item)
	}
	for name, value := range replacements {
		result = append(result, name+"="+value)
	}
	return result
}

func u2db3Report(t *testing.T, result u2db3CLIResult) map[string]any {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
		t.Fatalf("decode JSON output (exit %d): %v\nstdout:\n%s\nstderr:\n%s", result.exitCode, err, result.stdout, result.stderr)
	}
	return report
}

func u2db3Status(t *testing.T, report map[string]any) string {
	t.Helper()
	status, ok := report["status"].(string)
	if !ok {
		t.Fatalf("status missing from %#v", report)
	}
	return status
}

func u2db3ValidationStatus(t *testing.T, report map[string]any) string {
	t.Helper()
	status, ok := report["validation_status"].(string)
	if !ok {
		t.Fatalf("validation status missing from %#v", report)
	}
	return status
}

func u2db3RequireExit(t *testing.T, result u2db3CLIResult, want int) {
	t.Helper()
	if result.exitCode != want {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", result.exitCode, want, result.stdout, result.stderr)
	}
}

func u2db3WaitForFile(path string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

const u2db3GeminiScript = `#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys
import time

state = Path(os.environ["U2DB3_STATE"])
try:
    call = int(state.read_text(encoding="utf-8"))
except FileNotFoundError:
    call = 0
state.write_text(str(call + 1), encoding="utf-8")

prompt = sys.stdin.read()
if os.environ.get("U2DB3_MODE") == "cancel" and call == 0:
    Path(os.environ["U2DB3_STARTED"]).write_text("started", encoding="utf-8")
    while True:
        time.sleep(0.05)

if os.environ.get("U2DB3_MODE") == "ask" and call == 1:
    text = '{"settled":[],"contested":["seam_contested"],"withdrawn":[]}'
else:
    text = "seam participant reply"
print(json.dumps({"response": text, "session_id": "u2db3-" + str(call + 1)}), flush=True)
`

const u2db3AskSettings = `
[backend_profiles.seam-gemini]
backend = "gemini"
model = "seam-gemini"
effort = "low"
capabilities = []

[relay_recipes.seam-child]
purpose = "Resolve the ask-mode seam item."
participants = ["seam-gemini", "seam-gemini"]
facilitator = "seam-gemini"
reducer = "seam-gemini"
mode = "cooperative"
participant_turns = 1
max_rounds = 1
result_source = "last_turn"
provider_retry = "allow"
max_depth = 1
auto_approval = "never"
match_keywords = ["seam_contested"]

[relay_recipes.seam-child.lifecycle]
resume = "allow"
steering = "allow"
dynamic = "forbid"
workspace_isolation = "inherited"
`
