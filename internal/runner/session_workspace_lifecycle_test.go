package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

func TestCleanSessionPersistsSourceMutationAndRetryCheckpoints(t *testing.T) {
	fixture := newIsolatedSessionFixture(t, "completed")
	st := store.New(fixture.sessionDir)
	if err := st.SaveTranscriptItems([]any{map[string]any{"content": "preserve transcript"}}); err != nil {
		t.Fatalf("save transcript: %v", err)
	}
	resultRef, err := st.SaveArtifact("results", "raw", map[string]any{"content": "preserve result"})
	if err != nil {
		t.Fatalf("save result: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.sourceRoot, "source.txt"), []byte("mutated during execution\n"), 0o644); err != nil {
		t.Fatalf("mutate source: %v", err)
	}
	deleteErr := errors.New("injected session deletion failure")
	if report, err := cleanSessionWithRemover(fixture.sessionDir, func(string) error { return deleteErr }); report != nil || !errors.Is(err, deleteErr) {
		t.Fatalf("failed session delete = %#v, %v", report, err)
	}

	meta := mustLoadMeta(t, fixture.sessionDir)
	if meta["status"] != "failed" || meta["stop_reason"] != workspace.StopReasonSourceMutated || meta["source_mutated"] != true || stringFromAny(meta["source_after_digest"]) == "" {
		t.Fatalf("source mutation meta = %#v", meta)
	}
	checkpoint := meta["workspace_cleanup"].(map[string]any)
	if checkpoint["status"] != "failed" || checkpoint["stage"] != "session_delete" || intFromAny(checkpoint["attempt"], 0) != 1 {
		t.Fatalf("cleanup checkpoint = %#v", checkpoint)
	}
	history := checkpoint["history"].([]any)
	if !cleanupHistoryContains(history, "pending", "source_finalization") || !cleanupHistoryContains(history, "complete", "ready_to_delete") || !cleanupHistoryContains(history, "failed", "session_delete") {
		t.Fatalf("cleanup history = %#v", history)
	}
	if _, err := os.Stat(fixture.sessionDir); err != nil {
		t.Fatalf("failed deletion lost session evidence: %v", err)
	}
	if _, err := os.Stat(fixture.worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed before session deletion, err = %v", err)
	}
	transcript, err := st.LoadTranscript()
	if err != nil || transcript.Len() != 1 {
		t.Fatalf("transcript not preserved: %#v, %v", transcript, err)
	}
	result, err := st.LoadArtifact(resultRef)
	if err != nil || result["content"] != "preserve result" {
		t.Fatalf("result not preserved: %#v, %v", result, err)
	}

	retryDeleteErr := errors.New("injected retry deletion failure")
	if report, err := cleanSessionWithRemover(fixture.sessionDir, func(string) error { return retryDeleteErr }); report != nil || !errors.Is(err, retryDeleteErr) {
		t.Fatalf("failed cleanup retry = %#v, %v", report, err)
	}
	retriedMeta := mustLoadMeta(t, fixture.sessionDir)
	if retriedMeta["terminal_status_before_source_check"] != "completed" {
		t.Fatalf("cleanup retry replaced original terminal status: %#v", retriedMeta)
	}

	report, err := CleanSession(fixture.sessionDir)
	if err != nil || report["status"] != "deleted" {
		t.Fatalf("cleanup retry = %#v, %v", report, err)
	}
	if _, err := os.Stat(fixture.sessionDir); !os.IsNotExist(err) {
		t.Fatalf("retry did not delete session: %v", err)
	}
}

func TestRunStateCompletionGivesSourceMutationOperationalPrecedence(t *testing.T) {
	fixture := newIsolatedSessionFixture(t, "running")
	st := store.New(fixture.sessionDir)
	if err := os.WriteFile(filepath.Join(fixture.sourceRoot, "source.txt"), []byte("mutated before terminal checkpoint\n"), 0o644); err != nil {
		t.Fatalf("mutate source: %v", err)
	}
	meta, err := st.LoadMeta()
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	state := &runState{
		st:              st,
		sessionDir:      fixture.sessionDir,
		sessionID:       filepath.Base(fixture.sessionDir),
		meta:            meta,
		transcript:      model.EmptyTranscript(),
		mode:            "adversarial",
		facilitatorName: "codex",
		startedAt:       time.Now(),
	}
	result, err := state.markCompleted("fixed_rounds")
	var mutation *workspace.SourceMutatedError
	if !errors.As(err, &mutation) || result["status"] != "failed" || result["stop_reason"] != workspace.StopReasonSourceMutated {
		t.Fatalf("terminal mutation result = %#v, %v", result, err)
	}
	persisted := mustLoadMeta(t, fixture.sessionDir)
	if persisted["status"] != "failed" || persisted["terminal_status_before_source_check"] != "completed" || persisted["source_mutated"] != true {
		t.Fatalf("terminal mutation meta = %#v", persisted)
	}
	if _, err := os.Stat(fixture.worktreePath); err != nil {
		t.Fatalf("terminal override removed worktree: %v", err)
	}
}

func TestStopKilledAndCrashedSessionsRetainWorktreeUntilClean(t *testing.T) {
	fixture := newIsolatedSessionFixture(t, "running")
	process := exec.Command("sleep", "30")
	if err := process.Start(); err != nil {
		t.Fatalf("start tracked process: %v", err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	if err := os.WriteFile(filepath.Join(fixture.sessionDir, "relay.pid"), []byte(fmt.Sprint(process.Process.Pid)), 0o644); err != nil {
		t.Fatalf("write relay pid: %v", err)
	}
	report, err := Stop(fixture.sessionDir, StopOptions{ForceKill: true})
	if err != nil || report["status"] != "killed" {
		t.Fatalf("kill report = %#v, %v", report, err)
	}
	_ = process.Wait()
	if _, err := os.Stat(fixture.worktreePath); err != nil {
		t.Fatalf("killed session lost worktree: %v", err)
	}
	killedMeta := mustLoadMeta(t, fixture.sessionDir)
	if killedMeta["status"] != "killed" || stringFromAny(killedMeta["source_after_digest"]) == "" {
		t.Fatalf("killed terminal meta = %#v", killedMeta)
	}

	if err := os.WriteFile(filepath.Join(fixture.sessionDir, "relay.pid"), []byte("99999999"), 0o644); err != nil {
		t.Fatalf("write crashed relay pid: %v", err)
	}
	report, err = Stop(fixture.sessionDir, StopOptions{})
	if err != nil || report["status"] != "orphaned" {
		t.Fatalf("crashed report = %#v, %v", report, err)
	}
	if _, err := os.Stat(fixture.worktreePath); err != nil {
		t.Fatalf("crashed session lost worktree: %v", err)
	}
}

func TestCleanSessionWorktreeRemovalFailureIsActionableAndRetryable(t *testing.T) {
	fixture := newIsolatedSessionFixture(t, "killed")
	runTestGit(t, fixture.sourceRoot, "worktree", "lock", "--reason", "test retry", fixture.worktreePath)

	if report, err := CleanSession(fixture.sessionDir); report != nil || err == nil || !strings.Contains(err.Error(), "remove execution worktree") {
		t.Fatalf("locked clean = %#v, %v", report, err)
	}
	meta := mustLoadMeta(t, fixture.sessionDir)
	checkpoint := meta["workspace_cleanup"].(map[string]any)
	if checkpoint["status"] != "failed" || checkpoint["stage"] != "execution_workspace" || intFromAny(checkpoint["attempt"], 0) != 1 {
		t.Fatalf("locked cleanup checkpoint = %#v", checkpoint)
	}
	if _, err := os.Stat(fixture.worktreePath); err != nil {
		t.Fatalf("failed cleanup removed worktree: %v", err)
	}

	runTestGit(t, fixture.sourceRoot, "worktree", "unlock", fixture.worktreePath)
	report, err := CleanSession(fixture.sessionDir)
	if err != nil || report["status"] != "deleted" {
		t.Fatalf("cleanup retry = %#v, %v", report, err)
	}
}

func TestCleanSessionProviderFailureRetainsWorktreeAndSessionForRetry(t *testing.T) {
	fixture := newIsolatedSessionFixture(t, "invalid")
	st := store.New(fixture.sessionDir)
	meta := mustLoadMeta(t, fixture.sessionDir)
	meta["facilitator_provider_state"] = map[string]any{
		"backend": "claude",
		"slot_id": "facilitator",
		"state":   map[string]any{"cwd": fixture.executionCWD, "started": true},
	}
	if err := st.SaveMetaMap(meta); err != nil {
		t.Fatalf("save invalid facilitator state: %v", err)
	}

	if report, err := CleanSession(fixture.sessionDir); report != nil || err == nil || !strings.Contains(err.Error(), "provider_artifacts") && !strings.Contains(err.Error(), "session_id") {
		t.Fatalf("invalid provider clean = %#v, %v", report, err)
	}
	meta = mustLoadMeta(t, fixture.sessionDir)
	checkpoint := meta["workspace_cleanup"].(map[string]any)
	if checkpoint["status"] != "failed" || checkpoint["stage"] != "provider_artifacts" {
		t.Fatalf("provider failure checkpoint = %#v", checkpoint)
	}
	if _, err := os.Stat(fixture.worktreePath); err != nil {
		t.Fatalf("provider failure removed worktree: %v", err)
	}

	valid := claudeCleanupEnvelope("fixed-facilitator-session", "facilitator", fixture.executionCWD)
	meta["facilitator_provider_state"] = valid
	if err := st.SaveMetaMap(meta); err != nil {
		t.Fatalf("repair facilitator state: %v", err)
	}
	report, err := CleanSession(fixture.sessionDir)
	if err != nil || report["status"] != "deleted" {
		t.Fatalf("provider cleanup retry = %#v, %v", report, err)
	}
}

func TestCleanSessionRemovesLegacyClaudeFacilitatorArtifacts(t *testing.T) {
	fixture := newIsolatedSessionFixture(t, "completed")
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	legacyArtifacts := claudeProjectDir(fixture.sessionDir)
	if err := os.MkdirAll(legacyArtifacts, 0o755); err != nil {
		t.Fatalf("create legacy Claude project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyArtifacts, "legacy.jsonl"), []byte("legacy transcript\n"), 0o644); err != nil {
		t.Fatalf("write legacy Claude transcript: %v", err)
	}

	st := store.New(fixture.sessionDir)
	meta := mustLoadMeta(t, fixture.sessionDir)
	meta["facilitator_backend"] = "claude"
	if err := st.SaveMetaMap(meta); err != nil {
		t.Fatalf("save legacy Claude facilitator metadata: %v", err)
	}

	report, err := CleanSession(fixture.sessionDir)
	if err != nil || report["status"] != "deleted" {
		t.Fatalf("clean legacy Claude session = %#v, %v", report, err)
	}
	if _, err := os.Stat(legacyArtifacts); !os.IsNotExist(err) {
		t.Fatalf("legacy Claude project still exists: %v", err)
	}
}

func TestCleanSessionLegacyClaudeProjectDirEncodesPunctuation(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	const sessionDir = "/tmp/convo-relay/legacy.session_name"
	const expectedEncodedName = "-tmp-convo-relay-legacy-session-name"
	want := filepath.Join(homeDir, ".claude", "projects", expectedEncodedName)
	if got := claudeProjectDir(sessionDir); got != want {
		t.Fatalf("legacy Claude project path = %q, want %q", got, want)
	}
}

func TestCleanSessionRemovesLegacyClaudeFacilitatorArtifactsByDefault(t *testing.T) {
	fixture := newIsolatedSessionFixture(t, "completed")
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	legacyArtifacts := claudeProjectDir(fixture.sessionDir)
	if err := os.MkdirAll(legacyArtifacts, 0o755); err != nil {
		t.Fatalf("create legacy Claude project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyArtifacts, "legacy.jsonl"), []byte("legacy transcript\n"), 0o644); err != nil {
		t.Fatalf("write legacy Claude transcript: %v", err)
	}

	st := store.New(fixture.sessionDir)
	meta := mustLoadMeta(t, fixture.sessionDir)
	delete(meta, "facilitator_backend")
	delete(meta, "slots")
	if _, hasFacilitatorBackend := meta["facilitator_backend"]; hasFacilitatorBackend {
		t.Fatal("legacy metadata unexpectedly has facilitator_backend")
	}
	if _, hasSlots := meta["slots"]; hasSlots {
		t.Fatal("legacy metadata unexpectedly has slots")
	}
	if err := st.SaveMetaMap(meta); err != nil {
		t.Fatalf("save legacy facilitator metadata: %v", err)
	}

	report, err := CleanSession(fixture.sessionDir)
	if err != nil || report["status"] != "deleted" {
		t.Fatalf("clean default legacy Claude session = %#v, %v", report, err)
	}
	if _, err := os.Stat(legacyArtifacts); !os.IsNotExist(err) {
		t.Fatalf("default legacy Claude project still exists: %v", err)
	}
}

func TestCleanSessionRejectsUnsafeClaudeSessionIDsForEveryProviderRole(t *testing.T) {
	for _, role := range []string{"participant", "facilitator", "reducer"} {
		t.Run(role, func(t *testing.T) {
			fixture := newIsolatedSessionFixture(t, "crashed")
			st := store.New(fixture.sessionDir)

			envelope := claudeCleanupEnvelope("../outside-"+role, role, fixture.executionCWD)
			meta := mustLoadMeta(t, fixture.sessionDir)
			switch role {
			case "participant":
				meta["slots"] = []any{envelope}
			case "facilitator":
				meta["facilitator_provider_state"] = envelope
			case "reducer":
				meta["reducer_provider_state"] = envelope
			}
			if err := st.SaveMetaMap(meta); err != nil {
				t.Fatalf("save adversarial %s state: %v", role, err)
			}

			report, err := CleanSession(fixture.sessionDir)
			if report != nil || err == nil || !strings.Contains(err.Error(), "safe path component") {
				t.Fatalf("adversarial %s cleanup = %#v, %v", role, report, err)
			}
			persisted := mustLoadMeta(t, fixture.sessionDir)
			checkpoint := persisted["workspace_cleanup"].(map[string]any)
			if checkpoint["status"] != "failed" || checkpoint["stage"] != "provider_artifacts" {
				t.Fatalf("adversarial cleanup checkpoint = %#v", checkpoint)
			}
			if _, err := os.Stat(fixture.worktreePath); err != nil {
				t.Fatalf("adversarial cleanup removed retryable worktree: %v", err)
			}
			if _, err := os.Stat(fixture.sessionDir); err != nil {
				t.Fatalf("adversarial cleanup removed session evidence: %v", err)
			}
		})
	}
}

func TestCleanSessionRejectsForeignClaudeCWDForEveryProviderRole(t *testing.T) {
	for _, role := range []string{"participant", "facilitator", "reducer"} {
		t.Run(role, func(t *testing.T) {
			fixture := newIsolatedSessionFixture(t, "crashed")
			st := store.New(fixture.sessionDir)
			foreignCWD := filepath.Join(t.TempDir(), "foreign-project")
			if err := os.MkdirAll(foreignCWD, 0o755); err != nil {
				t.Fatalf("create foreign cwd: %v", err)
			}
			sessionID := "safe-foreign-" + role
			envelope := claudeCleanupEnvelope(sessionID, role, foreignCWD)
			meta := mustLoadMeta(t, fixture.sessionDir)
			switch role {
			case "participant":
				meta["slots"] = []any{envelope}
			case "facilitator":
				meta["facilitator_provider_state"] = envelope
			case "reducer":
				meta["reducer_provider_state"] = envelope
			}
			if err := st.SaveMetaMap(meta); err != nil {
				t.Fatalf("save foreign %s state: %v", role, err)
			}

			report, err := CleanSession(fixture.sessionDir)
			if report != nil || err == nil || !strings.Contains(err.Error(), "cleanup cwd") {
				t.Fatalf("foreign %s cleanup = %#v, %v", role, report, err)
			}
			persisted := mustLoadMeta(t, fixture.sessionDir)
			checkpoint := persisted["workspace_cleanup"].(map[string]any)
			if checkpoint["status"] != "failed" || checkpoint["stage"] != "provider_artifacts" {
				t.Fatalf("foreign cleanup checkpoint = %#v", checkpoint)
			}
			if _, err := os.Stat(fixture.worktreePath); err != nil {
				t.Fatalf("foreign cleanup removed retryable worktree: %v", err)
			}
			if _, err := os.Stat(fixture.sessionDir); err != nil {
				t.Fatalf("foreign cleanup removed session evidence: %v", err)
			}
		})
	}
}

type isolatedSessionFixture struct {
	sourceRoot   string
	sessionDir   string
	worktreePath string
	executionCWD string
}

func newIsolatedSessionFixture(t *testing.T, status string) isolatedSessionFixture {
	t.Helper()
	sourceRoot := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatalf("create source root: %v", err)
	}
	runTestGit(t, sourceRoot, "init", "-q")
	runTestGit(t, sourceRoot, "config", "user.name", "Runner Workspace Test")
	runTestGit(t, sourceRoot, "config", "user.email", "runner-workspace@example.invalid")
	if err := os.WriteFile(filepath.Join(sourceRoot, "source.txt"), []byte("committed source\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	runTestGit(t, sourceRoot, "add", "--all")
	runTestGit(t, sourceRoot, "commit", "-q", "-m", "initial")
	canonicalRoot, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		t.Fatalf("resolve source root: %v", err)
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot, err := workspace.Preflight(context.Background(), workspace.Options{
		LaunchCWD:     canonicalRoot,
		SessionDir:    sessionDir,
		MinimumPolicy: workspace.PolicyEphemeral,
	})
	if err != nil {
		t.Fatalf("workspace preflight: %v", err)
	}
	st := store.New(sessionDir)
	materialized, err := workspace.Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("workspace materialize: %v", err)
	}
	if err := st.SaveMetaMap(map[string]any{
		"session_id":              filepath.Base(sessionDir),
		"status":                  status,
		"task":                    "workspace lifecycle test",
		"launch_cwd":              canonicalRoot,
		"execution_cwd":           materialized.ExecutionCWD,
		"execution_workspace_ref": materialized.ArtifactRef,
	}); err != nil {
		t.Fatalf("save session meta: %v", err)
	}
	t.Cleanup(func() {
		command := exec.Command("git", "-C", canonicalRoot, "worktree", "remove", "--force", materialized.WorktreePath)
		command.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
		_ = command.Run()
	})
	return isolatedSessionFixture{
		sourceRoot:   canonicalRoot,
		sessionDir:   sessionDir,
		worktreePath: materialized.WorktreePath,
		executionCWD: materialized.ExecutionCWD,
	}
}

func runTestGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	command.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func cleanupHistoryContains(history []any, status string, stage string) bool {
	for _, raw := range history {
		entry, _ := raw.(map[string]any)
		if entry["status"] == status && entry["stage"] == stage {
			return true
		}
	}
	return false
}

func claudeCleanupEnvelope(sessionID string, slotID string, cwd string) map[string]any {
	return map[string]any{
		"backend": "claude",
		"slot_id": slotID,
		"label":   slotID,
		"state": map[string]any{
			"session_id": sessionID,
			"started":    true,
			"cwd":        cwd,
		},
	}
}
