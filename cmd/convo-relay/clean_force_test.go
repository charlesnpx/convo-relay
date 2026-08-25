package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/plan"
	"github.com/charlesnpx/convo-relay/internal/session"
)

func TestCleanSingleSessionForceFlag(t *testing.T) {
	home := t.TempDir()
	binary := buildCleanForceTestBinary(t)

	unforced := createCleanForceTestSession(t, home, "clean-unforced")
	unforcedWriter, err := unforced.EventWriter(nil)
	if err != nil {
		t.Fatalf("open unforced writer: %v", err)
	}
	defer func() { _ = unforcedWriter.Close() }()

	command := exec.Command(binary, "clean", unforced.Plan.SessionID, "--home", home, "--json")
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("unforced clean error = %v, output = %s", err, output)
	}
	if !strings.Contains(string(output), "session is running") {
		t.Fatalf("unforced clean output = %s", output)
	}
	if _, err := os.Stat(unforced.Root); err != nil {
		t.Fatalf("unforced clean removed active session: %v", err)
	}

	forced := createCleanForceTestSession(t, home, "clean-forced")
	forcedWriter, err := forced.EventWriter(nil)
	if err != nil {
		t.Fatalf("open forced writer: %v", err)
	}
	defer func() { _ = forcedWriter.Close() }()

	command = exec.Command(binary, "clean", forced.Plan.SessionID, "--home", home, "--force", "--json")
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("forced clean: %v, output = %s", err, output)
	}
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("decode forced clean report: %v, output = %s", err, output)
	}
	if report["status"] != "deleted" || report["session_id"] != forced.Plan.SessionID {
		t.Fatalf("forced clean report = %#v", report)
	}
	if _, err := os.Stat(forced.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("forced clean left session directory: %v", err)
	}
}

func buildCleanForceTestBinary(t *testing.T) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get command directory: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "convo-relay")
	command := exec.Command("go", "build", "-o", binary, ".")
	command.Dir = workingDir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	return binary
}

func createCleanForceTestSession(t *testing.T, home string, sessionID string) *session.Session {
	t.Helper()
	value, err := plan.FromFlags(plan.Flags{
		SessionID: sessionID,
		Task:      "clean force test",
		Agents:    "codex,codex",
		Rounds:    1,
	})
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	sess, err := session.CreateWithOptions(session.CreateOptions{
		RelayHome: filepath.Join(home, "sessions"),
		Prefix:    sessionID + "-",
		Plan:      value,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}
