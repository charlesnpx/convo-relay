package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/eventlog"
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

func TestPrepareCrashGapRecovery(t *testing.T) {
	t.Run("current state repairs its missing event exactly once", func(t *testing.T) {
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
		wantCWD, err := absoluteDirectory(launchCWD)
		if err != nil {
			t.Fatalf("resolve launch CWD: %v", err)
		}
		compiled, err := plan.FromFlags(plan.Flags{
			SessionID: "workspace-current-crash-gap",
			Task:      "test",
			Agents:    "codex,codex",
			Rounds:    1,
			Workspace: session.Workspace{Mode: ModeCurrent},
		})
		if err != nil {
			t.Fatalf("compile plan: %v", err)
		}
		sess, err := session.Create(t.TempDir(), compiled)
		if err != nil {
			t.Fatalf("create session: %v", err)
		}

		eventWriteFailure := errors.New("simulated event write failure")
		_, err = prepareWithHooks(
			context.Background(),
			sess,
			Options{LaunchCWD: launchCWD, Mode: ModeCurrent},
			save,
			func(*session.Session, *Materialized) error { return eventWriteFailure },
		)
		if !errors.Is(err, eventWriteFailure) {
			t.Fatalf("prepare crash gap error = %v", err)
		}
		if _, err := os.Stat(statePath(sess)); err != nil {
			t.Fatalf("runtime state after simulated crash: %v", err)
		}
		if _, found, err := preparedEvent(sess); err != nil || found {
			t.Fatalf("prepared event after simulated crash: found=%t err=%v", found, err)
		}

		recovered, err := Recover(context.Background(), sess)
		if err != nil {
			t.Fatalf("repair current workspace: %v", err)
		}
		if recovered.Mode != ModeCurrent || recovered.ExecutionCWD != wantCWD {
			t.Fatalf("repaired workspace = %#v", recovered)
		}
		if got := preparedEventCount(t, sess); got != 1 {
			t.Fatalf("workspace.prepared count after repair = %d, want 1", got)
		}
		prepared, found, err := preparedEvent(sess)
		if err != nil || !found || prepared.Mode != ModeCurrent || prepared.Commit == "" || prepared.TreeHash == "" || prepared.RelativePath != "nested" {
			t.Fatalf("repaired current workspace.prepared = %#v found=%t err=%v", prepared, found, err)
		}
		if _, err := Recover(context.Background(), sess); err != nil {
			t.Fatalf("repeat current recovery: %v", err)
		}
		if got := preparedEventCount(t, sess); got != 1 {
			t.Fatalf("workspace.prepared count after repeat repair = %d, want 1", got)
		}
		if err := os.Remove(statePath(sess)); err != nil {
			t.Fatalf("remove current runtime state: %v", err)
		}
		if _, err := Recover(context.Background(), sess); err == nil || !strings.Contains(err.Error(), "runtime/workspace.json is missing for current workspace; it may have been deleted") {
			t.Fatalf("missing current runtime state error = %v", err)
		}
		if err := os.WriteFile(statePath(sess), []byte(`{"mode":"current"`), 0o600); err != nil {
			t.Fatalf("write torn current runtime state: %v", err)
		}
		if _, err := Recover(context.Background(), sess); err == nil || !strings.Contains(err.Error(), "decode workspace state") {
			t.Fatalf("torn current runtime state error = %v", err)
		}
	})

	t.Run("head-copy event precedes its registered worktree", func(t *testing.T) {
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

		worktreePath := filepath.Join(sess.Root, "runtime", "workspace")
		eventWriteFailure := errors.New("simulated event write failure")
		_, err = prepareWithHooks(
			context.Background(),
			sess,
			Options{LaunchCWD: launchCWD, Mode: ModeHeadCopy},
			save,
			func(*session.Session, *Materialized) error { return eventWriteFailure },
		)
		if !errors.Is(err, eventWriteFailure) {
			t.Fatalf("head-copy event failure = %v", err)
		}
		if _, err := os.Lstat(worktreePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worktree after failed event append = %v, want absent", err)
		}
		listing, err := gitOutput(context.Background(), repository, "worktree", "list", "--porcelain")
		if err != nil {
			t.Fatalf("list source worktrees: %v", err)
		}
		if strings.Contains(listing, worktreePath) {
			t.Fatalf("source registered worktree after failed event append: %s", listing)
		}

		eventSawNoWorktree := false
		preparedBefore, err := prepareWithHooks(
			context.Background(),
			sess,
			Options{LaunchCWD: launchCWD, Mode: ModeHeadCopy},
			save,
			func(appended *session.Session, materialized *Materialized) error {
				if materialized.Commit == "" || materialized.TreeHash == "" || materialized.RelativePath != "nested" {
					t.Fatalf("head-copy facts before event = %#v", materialized)
				}
				if _, err := os.Lstat(worktreePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("worktree at event append = %v, want absent", err)
				}
				eventSawNoWorktree = true
				return appendPrepared(appended, materialized)
			},
		)
		if err != nil {
			t.Fatalf("prepare head-copy workspace: %v", err)
		}
		if !eventSawNoWorktree {
			t.Fatal("head-copy event append did not observe the absent worktree")
		}
		t.Cleanup(func() { _ = Cleanup(context.Background(), sess) })
		if _, err := os.Stat(statePath(sess)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("head-copy runtime state = %v, want absent", err)
		}
		prepared, found, err := preparedEvent(sess)
		if err != nil || !found || prepared.Mode != ModeHeadCopy || prepared.Commit == "" || prepared.TreeHash == "" || prepared.RelativePath != "nested" {
			t.Fatalf("durable workspace event = %#v found=%t err=%v", prepared, found, err)
		}

		recovered, err := Recover(context.Background(), sess)
		if err != nil {
			t.Fatalf("recover from workspace.prepared: %v", err)
		}
		if recovered.Mode != ModeHeadCopy || recovered.ExecutionCWD != filepath.Join(sess.Root, "runtime", "workspace", "nested") || recovered.ExecutionCWD != preparedBefore.ExecutionCWD || recovered.Commit != prepared.Commit || recovered.TreeHash != prepared.TreeHash {
			t.Fatalf("rebuilt workspace = %#v, event = %#v", recovered, prepared)
		}
		if _, err := os.Stat(statePath(sess)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("head-copy rebuild wrote runtime state: %v", err)
		}
	})
}

func preparedEventCount(t *testing.T, sess *session.Session) int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(sess.Root, eventlog.EventsFilename))
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	events, err := eventlog.Replay(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("replay events: %v", err)
	}
	count := 0
	for _, event := range events {
		if event.Type == eventlog.WorkspacePrepared {
			count++
		}
	}
	return count
}

func runGitTest(t *testing.T, cwd string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}
