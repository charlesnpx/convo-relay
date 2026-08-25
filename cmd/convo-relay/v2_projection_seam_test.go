package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/relayv2"
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
	sessionDir := u2db3SessionDir(t, env.relayHome, "cancelled")
	beforeCancel, err := os.ReadFile(filepath.Join(sessionDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events before control cancel: %v", err)
	}
	cancel := env.run(t, "control", "cancel", "--home", env.relayHome, "--json", "cancelled")
	if cancel.exitCode != 1 || !strings.Contains(cancel.stderr, "interrupt its relay process directly") {
		t.Fatalf("control cancel = exit %d stdout=%q stderr=%q", cancel.exitCode, cancel.stdout, cancel.stderr)
	}
	afterCancel, err := os.ReadFile(filepath.Join(sessionDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events after control cancel: %v", err)
	}
	if !bytes.Equal(afterCancel, beforeCancel) {
		t.Fatalf("control cancel appended durable state: before=%s after=%s", beforeCancel, afterCancel)
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
	beforeStrandedCancel, err := os.ReadFile(filepath.Join(sessionDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events before stranded control cancel: %v", err)
	}
	stranded := env.run(t, "control", "cancel", "--home", env.relayHome, "--json", "cancelled")
	if stranded.exitCode != 1 || !strings.Contains(stranded.stderr, "no owning process is active; the session is not running") {
		t.Fatalf("stranded control cancel = exit %d stdout=%q stderr=%q", stranded.exitCode, stranded.stdout, stranded.stderr)
	}
	afterStrandedCancel, err := os.ReadFile(filepath.Join(sessionDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events after stranded control cancel: %v", err)
	}
	if !bytes.Equal(afterStrandedCancel, beforeStrandedCancel) {
		t.Fatalf("stranded control cancel appended durable state: before=%s after=%s", beforeStrandedCancel, afterStrandedCancel)
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
	beforeTerminalCancel, err := os.ReadFile(filepath.Join(sessionDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events before terminal control cancel: %v", err)
	}
	terminal := env.run(t, "control", "cancel", "--home", env.relayHome, "--json", "cancelled")
	u2db3RequireExit(t, terminal, 0)
	if got := u2db3Status(t, u2db3Report(t, terminal)); got != "completed" {
		t.Fatalf("terminal control cancel status = %q", got)
	}
	afterTerminalCancel, err := os.ReadFile(filepath.Join(sessionDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events after terminal control cancel: %v", err)
	}
	if !bytes.Equal(afterTerminalCancel, beforeTerminalCancel) {
		t.Fatalf("terminal control cancel appended durable state: before=%s after=%s", beforeTerminalCancel, afterTerminalCancel)
	}
}

func TestV2ControlApproveRetiredFlagsRejected(t *testing.T) {
	env := newU2DB3CLIEnv(t, "approve-flags")
	for _, name := range []string{"rounds", "timeout", "stall-timeout", "settings"} {
		t.Run(name, func(t *testing.T) {
			rejected := env.run(t, "control", "approve", "--"+name, "1")
			if rejected.exitCode != 2 || !strings.Contains(rejected.stderr, "flag provided but not defined: -"+name) {
				t.Fatalf("control approve --%s = exit %d stdout=%q stderr=%q", name, rejected.exitCode, rejected.stdout, rejected.stderr)
			}
		})
	}
}

func TestV2ControlSteerQueuesOneBlobBackedEvent(t *testing.T) {
	env := newU2DB3CLIEnv(t, "steer")
	seed := env.run(t,
		"run", "--home", env.relayHome, "--session-id", "steer-queue",
		"--task", "queue a later direction", "--agents", "gemini", "--rounds", "1",
		"--settings", env.settingsPath, "--launch-cwd", env.workDir,
		"--timeout", "30", "--stall-timeout", "30", "--json",
	)
	u2db3RequireExit(t, seed, 0)
	sessionDir := u2db3SessionDir(t, env.relayHome, "steer-queue")
	sess, found, err := relayv2.Open(sessionDir)
	if err != nil || !found {
		t.Fatalf("open seeded v2 session: session=%#v found=%v err=%v", sess, found, err)
	}
	before, err := relayv2.Events(sess)
	if err != nil {
		t.Fatalf("read events before steering: %v", err)
	}
	queued := env.run(t, "control", "steer", "--home", env.relayHome, "--json", "steer-queue", "steer exactly once")
	u2db3RequireExit(t, queued, 0)
	after, err := relayv2.Events(sess)
	if err != nil {
		t.Fatalf("read events after steering: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("steering event count = %d, want %d; events=%#v", len(after), len(before)+1, after)
	}
	steering, ok := after[len(after)-1].Payload.(eventlog.SteeringQueuedPayload)
	if !ok || after[len(after)-1].Type != eventlog.SteeringQueued {
		t.Fatalf("last event = %#v, want steering.queued", after[len(after)-1])
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		t.Fatalf("open blobs: %v", err)
	}
	reader, err := blobs.Open(steering.Prompt)
	if err != nil {
		t.Fatalf("open steering prompt blob: %v", err)
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read steering prompt blob: read=%v close=%v", readErr, closeErr)
	}
	if string(body) != "steer exactly once" {
		t.Fatalf("steering prompt blob = %q", body)
	}
}

func u2db3SessionDir(t *testing.T, relayHome string, sessionID string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(relayHome, "sessions"))
	if err != nil {
		t.Fatalf("read session root: %v", err)
	}
	matches := []string{}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), sessionID+"-") {
			matches = append(matches, filepath.Join(relayHome, "sessions", entry.Name()))
		}
	}
	if len(matches) != 1 {
		t.Fatalf("session %q directories = %#v", sessionID, matches)
	}
	return matches[0]
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
