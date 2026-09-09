package sessionstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/v2/internal/eventlog"
	"github.com/charlesnpx/convo-relay/v2/internal/plan"
	"github.com/charlesnpx/convo-relay/v2/internal/session"
	"github.com/charlesnpx/convo-relay/v2/internal/workspace"
)

func TestResolveSessionDirUniquePrefixAndExplicitDirectory(t *testing.T) {
	home := t.TempDir()
	sess := createTestSession(t, home, "unique-session", testTime(1), false)

	resolved, err := ResolveSessionDir(home, "", "unique-")
	if err != nil {
		t.Fatalf("resolve unique prefix: %v", err)
	}
	if resolved != sess.Root {
		t.Fatalf("resolved prefix = %q, want %q", resolved, sess.Root)
	}
	explicit := filepath.Join(t.TempDir(), "explicit-session")
	resolved, err = ResolveSessionDir(home, explicit, "unique-")
	if err != nil {
		t.Fatalf("resolve explicit directory: %v", err)
	}
	if resolved != explicit {
		t.Fatalf("resolved explicit directory = %q, want %q", resolved, explicit)
	}
}

func TestResolveSessionDirRejectsAmbiguousPrefix(t *testing.T) {
	home := t.TempDir()
	createTestSession(t, home, "shared-one", testTime(1), false)
	createTestSession(t, home, "shared-two", testTime(2), false)

	_, err := ResolveSessionDir(home, "", "shared-")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix error = %v", err)
	}
}

func TestResolveSessionDirRejectsUnreadableCandidateThenResolvesUniquely(t *testing.T) {
	home := t.TempDir()
	readable := createTestSession(t, home, "shared-readable", testTime(1), false)
	unreadable := filepath.Join(home, "sessions", "shared-unreadable")
	if err := os.MkdirAll(unreadable, 0o755); err != nil {
		t.Fatalf("create unreadable candidate: %v", err)
	}

	_, err := ResolveSessionDir(home, "", "shared-")
	if err == nil {
		t.Fatal("resolve contested prefix unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "candidate "+unreadable+" is not a readable v2 session") {
		t.Fatalf("contested prefix error = %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("contested prefix error does not retain its cause: %v", err)
	}
	if err := os.Remove(unreadable); err != nil {
		t.Fatalf("remove unreadable candidate: %v", err)
	}

	resolved, err := ResolveSessionDir(home, "", "shared-")
	if err != nil {
		t.Fatalf("resolve unique prefix after removal: %v", err)
	}
	if resolved != readable.Root {
		t.Fatalf("resolved unique prefix = %q, want %q", resolved, readable.Root)
	}
}

func TestResolveSessionDirRejectsUnknownPrefix(t *testing.T) {
	home := t.TempDir()
	createTestSession(t, home, "known-session", testTime(1), false)

	_, err := ResolveSessionDir(home, "", "missing")
	if err == nil || !strings.Contains(err.Error(), "no session matching") {
		t.Fatalf("unknown prefix error = %v", err)
	}
}

func TestListSessionsListsTwoV2Sessions(t *testing.T) {
	home := t.TempDir()
	older := createEmptyTestSession(t, home, "older-session")
	newer := createTestSession(t, home, "newer-session", testTime(2), true)

	items, err := ListSessions(home, 10)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("listed sessions = %#v, want two", items)
	}
	if items[0]["session_id"] != newer.Plan.SessionID || items[1]["session_id"] != older.Plan.SessionID {
		t.Fatalf("listed ordering = %#v", items)
	}
	if got, want := items[0]["created_at"], testTime(2).Format(time.RFC3339Nano); got != want {
		t.Fatalf("newer created_at = %#v, want %q", got, want)
	}
	if got := items[1]["created_at"]; got != nil {
		t.Fatalf("empty session created_at = %#v, want nil", got)
	}
	if got := items[0]["agents"]; len(got.([]any)) != 2 {
		t.Fatalf("newer agents = %#v", got)
	}
	if got, want := items[0]["title"], newer.Plan.Task; got != want {
		t.Fatalf("newer title = %#v, want %q", got, want)
	}
}

func TestListSessionsOrdersByEventTimeAndSessionID(t *testing.T) {
	home := t.TempDir()
	newest := createTestSession(t, home, "fractional-newer", testTime(1).Add(900*time.Millisecond), false)
	createTestSession(t, home, "whole-second", testTime(1), false)
	createTestSession(t, home, "equal-a", testTime(0), false)
	createTestSession(t, home, "equal-b", testTime(0), false)
	createEmptyTestSession(t, home, "zero-a")
	createEmptyTestSession(t, home, "zero-b")

	limited, err := ListSessions(home, 1)
	if err != nil {
		t.Fatalf("list newest session: %v", err)
	}
	if len(limited) != 1 || limited[0]["session_id"] != newest.Plan.SessionID {
		t.Fatalf("limited listing = %#v, want %q", limited, newest.Plan.SessionID)
	}

	items, err := ListSessions(home, 10)
	if err != nil {
		t.Fatalf("list ordered sessions: %v", err)
	}
	want := []string{"fractional-newer", "whole-second", "equal-a", "equal-b", "zero-a", "zero-b"}
	if len(items) != len(want) {
		t.Fatalf("listed sessions = %#v, want %d", items, len(want))
	}
	for index, sessionID := range want {
		if got := items[index]["session_id"]; got != sessionID {
			t.Fatalf("listed session %d = %#v, want %q; all=%#v", index, got, sessionID, items)
		}
	}
	if items[len(items)-1]["created_at"] != nil || items[len(items)-2]["created_at"] != nil {
		t.Fatalf("zero-event sessions did not remain last: %#v", items)
	}
}

func TestCleanSessionUsesSuppliedRemover(t *testing.T) {
	home := t.TempDir()
	completed := createTestSession(t, home, "completed-session", testTime(1), true)
	removed := []string{}
	remove := func(path string) error {
		removed = append(removed, path)
		return nil
	}

	report, err := cleanSessionWithRemover(completed.Root, remove, false)
	if err != nil {
		t.Fatalf("clean completed session: %v", err)
	}
	if report["status"] != "deleted" || len(removed) != 1 || removed[0] != completed.Root {
		t.Fatalf("clean report = %#v, remover calls = %#v", report, removed)
	}

	orphan := createTestSession(t, home, "orphan-session", testTime(2), false)
	cleanup, err := cleanupSessionsWithRemover(home, 10, false, remove)
	if err != nil {
		t.Fatalf("cleanup orphan session: %v", err)
	}
	if cleanup["orphaned_count"] != 1 || removed[len(removed)-1] != orphan.Root {
		t.Fatalf("cleanup report = %#v, remover calls = %#v", cleanup, removed)
	}

	forceHome := t.TempDir()
	active := createTestSession(t, forceHome, "force-session", testTime(3), false)
	writer, err := active.EventWriter(nil)
	if err != nil {
		t.Fatalf("open force writer: %v", err)
	}
	defer writer.Close()
	forceCalls := []string{}
	forced, err := cleanupSessionsWithRemover(forceHome, 10, true, func(path string) error {
		forceCalls = append(forceCalls, path)
		return nil
	})
	if err != nil {
		t.Fatalf("force cleanup active session: %v", err)
	}
	if forced["orphaned_count"] != 1 || len(forceCalls) != 1 || forceCalls[0] != active.Root {
		t.Fatalf("forced cleanup report = %#v, remover calls = %#v", forced, forceCalls)
	}
}

func TestCleanSessionRefusesActiveWriter(t *testing.T) {
	home := t.TempDir()
	sess := createTestSession(t, home, "active-session", testTime(1), false)
	writer, err := sess.EventWriter(nil)
	if err != nil {
		t.Fatalf("open active writer: %v", err)
	}
	defer writer.Close()

	called := false
	_, err = cleanSessionWithRemover(sess.Root, func(string) error {
		called = true
		return nil
	}, false)
	if !errors.Is(err, errSessionRunning) {
		t.Fatalf("clean active session error = %v", err)
	}
	if called {
		t.Fatal("remover was called for an active session")
	}
}

func TestCleanSessionUnregistersHeadCopyWorktree(t *testing.T) {
	source := t.TempDir()
	runSessionstoreGit(t, source, "init")
	runSessionstoreGit(t, source, "config", "user.email", "test@example.invalid")
	runSessionstoreGit(t, source, "config", "user.name", "sessionstore test")
	if err := os.WriteFile(filepath.Join(source, "value.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSessionstoreGit(t, source, "add", "value.txt")
	runSessionstoreGit(t, source, "commit", "-m", "initial")

	value, err := plan.FromFlags(plan.Flags{
		SessionID: "head-copy-clean", Task: "head-copy cleanup", Agents: "codex,codex", Rounds: 1,
		Workspace: session.Workspace{Mode: workspace.ModeHeadCopy},
	})
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	sess, err := session.CreateWithOptions(session.CreateOptions{
		RelayHome: filepath.Join(t.TempDir(), "sessions"), Prefix: "head-copy-clean-", Plan: value,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	prepared, err := workspace.Prepare(context.Background(), sess, workspace.Options{LaunchCWD: source, Mode: workspace.ModeHeadCopy})
	if err != nil {
		t.Fatalf("prepare head-copy workspace: %v", err)
	}
	if listing := sessionstoreGitOutput(t, source, "worktree", "list", "--porcelain"); !strings.Contains(listing, prepared.WorktreePath) {
		t.Fatalf("prepared worktree is not registered: %s", listing)
	}

	report, err := CleanSession(sess.Root, false)
	if err != nil {
		t.Fatalf("clean head-copy session: %v", err)
	}
	if report["status"] != "deleted" {
		t.Fatalf("clean report = %#v", report)
	}
	if listing := sessionstoreGitOutput(t, source, "worktree", "list", "--porcelain"); strings.Contains(listing, prepared.WorktreePath) {
		t.Fatalf("stale worktree registration remains after clean: %s", listing)
	}
}

func TestListSessionsLeavesWriterOpensUncontended(t *testing.T) {
	home := t.TempDir()
	sess := createTestSession(t, home, "contention-session", testTime(1), false)

	stop := make(chan struct{})
	listErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := ListSessions(home, 10); err != nil {
				listErr <- err
				return
			}
		}
	}()

	writerLocked := 0
	const writerAttempts = 2048
	for attempt := 0; attempt < writerAttempts; attempt++ {
		writer, err := sess.EventWriter(nil)
		if err != nil {
			var locked *eventlog.WriterLockedError
			if errors.As(err, &locked) {
				writerLocked++
				continue
			}
			close(stop)
			<-done
			t.Fatalf("open writer %d: %v", attempt, err)
		}
		if err := writer.Close(); err != nil {
			close(stop)
			<-done
			t.Fatalf("close writer %d: %v", attempt, err)
		}
		runtime.Gosched()
	}
	close(stop)
	<-done
	select {
	case err := <-listErr:
		t.Fatalf("list under contention: %v", err)
	default:
	}
	if writerLocked != 0 {
		t.Fatalf("writer_locked=%d, want 0", writerLocked)
	}
	t.Logf("writer_locked=%d over %d opens", writerLocked, writerAttempts)
}

func createTestSession(t *testing.T, home string, id string, started time.Time, complete bool) *session.Session {
	t.Helper()
	sess := createEmptyTestSession(t, home, id)
	digest, err := session.PlanDigest(sess.Plan)
	if err != nil {
		t.Fatalf("plan digest: %v", err)
	}
	writer, err := sess.EventWriter(nil)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if _, err := writer.Append(eventlog.NewEvent(id+"-started", started, eventlog.SessionStartedPayload{PlanDigest: digest, SessionID: id})); err != nil {
		_ = writer.Close()
		t.Fatalf("append session start: %v", err)
	}
	if complete {
		if _, err := writer.Append(eventlog.NewEvent(id+"-finished", started.Add(time.Second), eventlog.SessionFinishedPayload{Status: "completed", StopReason: "completed"})); err != nil {
			_ = writer.Close()
			t.Fatalf("append session finish: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return sess
}

func createEmptyTestSession(t *testing.T, home string, id string) *session.Session {
	t.Helper()
	value, err := plan.FromFlags(plan.Flags{
		SessionID: id,
		Task:      "task for " + id,
		Agents:    "codex,codex",
		Rounds:    1,
	})
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	sess, err := session.CreateWithOptions(session.CreateOptions{
		RelayHome: filepath.Join(home, "sessions"),
		Prefix:    id + "-",
		Plan:      value,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

func testTime(second int) time.Time {
	return time.Date(2026, time.August, 25, 12, 0, second, 0, time.UTC)
}

func runSessionstoreGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	if output, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func sessionstoreGitOutput(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
