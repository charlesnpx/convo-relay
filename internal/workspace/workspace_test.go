package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/plan"
	"github.com/charlesnpx/convo-relay/internal/session"
)

func TestCommittedHeadExecution(t *testing.T) {
	repository := t.TempDir()
	runGitTest(t, repository, "init")
	runGitTest(t, repository, "config", "user.email", "test@example.invalid")
	runGitTest(t, repository, "config", "user.name", "workspace test")
	filename := filepath.Join(repository, "value.txt")
	if err := os.WriteFile(filename, []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repository, "add", "value.txt")
	runGitTest(t, repository, "commit", "-m", "initial")
	if err := os.WriteFile(filename, []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compiled, err := plan.FromFlags(plan.Flags{SessionID: "workspace-test", Task: "test", Agents: "codex,codex", Rounds: 1, Workspace: session.Workspace{Mode: ModeHeadCopy}})
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	sess, err := session.Create(t.TempDir(), compiled)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	prepared, err := Prepare(context.Background(), sess, Options{LaunchCWD: repository, Mode: ModeHeadCopy})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Cleanup(func() { _ = Cleanup(context.Background(), sess) })
	body, err := os.ReadFile(filepath.Join(prepared.ExecutionCWD, "value.txt"))
	if err != nil {
		t.Fatalf("read head copy: %v", err)
	}
	if string(body) != "committed\n" {
		t.Fatalf("head-copy content = %q, want committed HEAD", body)
	}
	projection, err := Projection(sess)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if projection[WorkspaceContentSourceKey] != WorkspaceContentSourceCommittedHead || projection[WorkingTreeChangesIncludedKey] != false {
		t.Fatalf("workspace projection = %#v", projection)
	}
}

func TestPrepareCrashGapRebuildsHeadCopyStateFromPreparedEvent(t *testing.T) {
	repository := t.TempDir()
	runGitTest(t, repository, "init")
	runGitTest(t, repository, "config", "user.email", "test@example.invalid")
	runGitTest(t, repository, "config", "user.name", "workspace test")
	launchCWD := filepath.Join(repository, "nested")
	if err := os.Mkdir(launchCWD, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(launchCWD, "value.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repository, "add", "nested/value.txt")
	runGitTest(t, repository, "commit", "-m", "initial")
	compiled, err := plan.FromFlags(plan.Flags{SessionID: "workspace-crash-gap", Task: "test", Agents: "codex,codex", Rounds: 1, Workspace: session.Workspace{Mode: ModeHeadCopy}})
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	sess, err := session.Create(t.TempDir(), compiled)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	stateWriteFailure := errors.New("simulated state write failure")
	_, err = prepareWithStateSaver(context.Background(), sess, Options{LaunchCWD: launchCWD, Mode: ModeHeadCopy}, func(*session.Session, Materialized) error {
		return stateWriteFailure
	})
	if !errors.Is(err, stateWriteFailure) {
		t.Fatalf("prepare crash gap error = %v", err)
	}
	if _, err := os.Stat(statePath(sess)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace state after simulated crash = %v, want absent", err)
	}
	prepared, found, err := preparedEvent(sess)
	if err != nil || !found || prepared.Mode != ModeHeadCopy || prepared.Commit == "" || prepared.TreeHash == "" || prepared.RelativePath != "nested" {
		t.Fatalf("durable workspace event = %#v found=%t err=%v", prepared, found, err)
	}

	recovered, err := Recover(context.Background(), sess)
	if err != nil {
		t.Fatalf("recover from workspace.prepared: %v", err)
	}
	t.Cleanup(func() { _ = Cleanup(context.Background(), sess) })
	if recovered.Mode != ModeHeadCopy || recovered.ExecutionCWD != filepath.Join(sess.Root, "runtime", "workspace", "nested") || recovered.Commit != prepared.Commit || recovered.TreeHash != prepared.TreeHash {
		t.Fatalf("rebuilt workspace = %#v, event = %#v", recovered, prepared)
	}
	if _, err := os.Stat(statePath(sess)); err != nil {
		t.Fatalf("rebuilt workspace state: %v", err)
	}
}

func runGitTest(t *testing.T, cwd string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}
