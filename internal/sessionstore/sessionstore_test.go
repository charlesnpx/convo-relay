package sessionstore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/plan"
	"github.com/charlesnpx/convo-relay/internal/session"
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
}

func TestCleanSessionUsesSuppliedRemover(t *testing.T) {
	home := t.TempDir()
	completed := createTestSession(t, home, "completed-session", testTime(1), true)
	removed := []string{}
	remove := func(path string) error {
		removed = append(removed, path)
		return nil
	}

	report, err := cleanSessionWithRemover(completed.Root, remove)
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
	})
	if !errors.Is(err, errSessionRunning) {
		t.Fatalf("clean active session error = %v", err)
	}
	if called {
		t.Fatal("remover was called for an active session")
	}
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
