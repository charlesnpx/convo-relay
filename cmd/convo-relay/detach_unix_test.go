//go:build darwin || linux

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/v2/internal/relayv2"
	"github.com/charlesnpx/convo-relay/v2/internal/session"
	"github.com/charlesnpx/convo-relay/v2/internal/sessionview"
)

func TestDetachedRunSurvivesLauncherProcessGroupCleanup(t *testing.T) {
	root := t.TempDir()
	launchCWD := filepath.Join(root, "launch")
	if err := os.MkdirAll(launchCWD, 0o700); err != nil {
		t.Fatalf("create launch directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launchCWD, "settings.toml"), nil, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	planValue := suppliedPlanFixture("detached-plan-id", nil)
	planBody, err := json.MarshalIndent(planValue, "", "  ")
	if err != nil {
		t.Fatalf("marshal supplied plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launchCWD, "plan.json"), planBody, 0o600); err != nil {
		t.Fatalf("write supplied plan: %v", err)
	}

	binary := buildDetachTestBinary(t, root)
	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatalf("create fake provider directory: %v", err)
	}
	statePath := filepath.Join(root, "provider-state.txt")
	if err := os.WriteFile(filepath.Join(fakeBin, "codex"), []byte(detachedFakeCodexScript), 0o700); err != nil {
		t.Fatalf("write fake provider: %v", err)
	}
	home := filepath.Join(root, "relay-home")
	sessionDir := filepath.Join(root, "session")
	command := exec.Command(binary,
		"run", "--plan", "plan.json", "--settings", "settings.toml",
		"--home", home, "--session-dir", sessionDir, "--detach", "--json",
	)
	command.Dir = launchCWD
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DETACH_PROVIDER_STATE_FILE="+statePath,
	)
	var stdout strings.Builder
	var stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("launch detached run: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("detached launcher stderr = %q", stderr.String())
	}
	receipt := decodeJSONObject(t, stdout.String())
	if receipt["kind"] != "launch_receipt" || receipt["session_id"] != planValue.SessionID {
		t.Fatalf("detached receipt = %#v", receipt)
	}
	logPath := stringValue(receipt["log_path"])
	if !filepath.IsAbs(logPath) {
		t.Fatalf("receipt log path is not absolute: %q", logPath)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("stat launch log: %v", err)
	}

	providerPID, providerSessionID := waitForDetachedProvider(t, statePath)
	if providerSessionID == command.Process.Pid {
		t.Fatalf("provider session id %d matches launcher process group %d", providerSessionID, command.Process.Pid)
	}
	if err := syscall.Kill(providerPID, 0); err != nil {
		t.Fatalf("fake provider is not still running before launcher group cleanup: %v", err)
	}
	if status, terminal := readDetachedStatus(t, sessionDir); terminal {
		t.Fatalf("session was already terminal before launcher group cleanup: %q", status)
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("clean up launcher process group: %v", err)
	}
	waitForDetachedCompletion(t, sessionDir)

	logBody, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read completed run log: %v", err)
	}
	if !strings.HasPrefix(string(logBody), "Session "+planValue.SessionID+"\n") {
		t.Fatalf("detached log %s does not start with its session launch line:\n%s", logPath, logBody)
	}
}

func TestDetachedRunPreflightFailuresAndSpawnFailure(t *testing.T) {
	root := t.TempDir()
	binary := buildDetachTestBinary(t, root)
	settings := filepath.Join(root, "settings.toml")
	if err := os.WriteFile(settings, nil, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	occupiedSessionDir := filepath.Join(root, "already-exists")
	if err := os.Mkdir(occupiedSessionDir, 0o700); err != nil {
		t.Fatalf("create occupied session directory: %v", err)
	}
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatalf("create fake provider directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "codex"), []byte("#!/bin/sh\nexit 88\n"), 0o700); err != nil {
		t.Fatalf("write fake provider: %v", err)
	}

	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "invalid context",
			args: []string{"--task", "bad context", "--agents", "codex", "--rounds", "1", "--context", filepath.Join(root, "missing-context.md")},
			want: "ctx input",
		},
		{
			name: "bad supplied plan",
			args: []string{"--plan", filepath.Join(root, "missing-plan.json")},
			want: "read supplied plan",
		},
		{
			name: "occupied explicit session directory",
			args: []string{"--task", "occupied directory", "--agents", "codex", "--rounds", "1", "--session-dir", occupiedSessionDir},
			want: "already exists",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-")+"-home")
			args := append([]string{"run"}, test.args...)
			args = append(args, "--settings", settings, "--home", home, "--detach", "--json")
			command := exec.Command(binary, args...)
			command.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("detached preflight unexpectedly succeeded: %s", output)
			}
			if !strings.Contains(string(output), "inspect log:") {
				t.Fatalf("preflight diagnostic has no log location: %s", output)
			}
			if strings.Contains(string(output), `"kind": "launch_receipt"`) {
				t.Fatalf("preflight failure emitted a success receipt: %s", output)
			}
			logs, err := filepath.Glob(filepath.Join(home, "logs", "run-detached-*.log"))
			if err != nil || len(logs) != 1 {
				t.Fatalf("preflight logs = %v, %v", logs, err)
			}
			logBody, err := os.ReadFile(logs[0])
			if err != nil {
				t.Fatalf("read preflight log: %v", err)
			}
			if !strings.Contains(string(logBody), test.want) {
				t.Fatalf("preflight log does not contain %q:\n%s", test.want, logBody)
			}
		})
	}

	if _, err := startDetachedRun(filepath.Join(root, "missing-convo-relay"), []string{"--task", "spawn failure"}, filepath.Join(root, "spawn-home")); err == nil || !strings.Contains(err.Error(), "start detached run child") {
		t.Fatalf("child spawn failure = %v", err)
	}
}

func buildDetachTestBinary(t *testing.T, root string) string {
	t.Helper()
	binary := filepath.Join(root, "convo-relay")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	return binary
}

func waitForDetachedProvider(t *testing.T, statePath string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(statePath)
		if err == nil {
			fields := strings.Fields(string(body))
			if len(fields) == 2 {
				pid, pidErr := strconv.Atoi(fields[0])
				sessionID, sessionErr := strconv.Atoi(fields[1])
				if pidErr == nil && sessionErr == nil {
					return pid, sessionID
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake provider did not start; state file %s is missing", statePath)
	return 0, 0
}

func readDetachedStatus(t *testing.T, sessionDir string) (string, bool) {
	t.Helper()
	sess, err := session.Open(sessionDir)
	if err != nil {
		t.Fatalf("open detached session: %v", err)
	}
	events, err := relayv2.Events(sess)
	if err != nil {
		t.Fatalf("read detached session events: %v", err)
	}
	status := sessionview.Status(sess.Plan, events)
	return status.Status, status.Terminal
}

func waitForDetachedCompletion(t *testing.T, sessionDir string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, terminal := readDetachedStatus(t, sessionDir)
		if terminal {
			if status != "completed" {
				t.Fatalf("detached session terminal status = %q", status)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	status, terminal := readDetachedStatus(t, sessionDir)
	t.Fatalf("detached session did not complete before timeout (status=%q terminal=%t)", status, terminal)
}

var detachedFakeCodexScript = func() string {
	script := strings.Replace(fakeCodexAppServerScript,
		"import sys\n",
		"import sys\nimport time\n",
		1,
	)
	script = strings.Replace(script,
		"def main():\n",
		"def main():\n    state_file = os.environ.get(\"DETACH_PROVIDER_STATE_FILE\", \"\")\n    if state_file:\n        Path(state_file).write_text(f\"{os.getpid()} {os.getsid(0)}\\n\", encoding=\"utf-8\")\n",
		1,
	)
	return strings.Replace(script,
		"            turn_number += 1\n",
		"            turn_number += 1\n            time.sleep(1.5)\n",
		1,
	)
}()
