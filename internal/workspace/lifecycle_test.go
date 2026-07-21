package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestFinalizePersistsSourceAfterAndRetainsIsolatedEvidence(t *testing.T) {
	tests := []struct {
		name        string
		mutate      bool
		wantChanged bool
	}{
		{name: "unchanged"},
		{name: "source mutated", mutate: true, wantChanged: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newCommittedRepo(t)
			sessionDir := filepath.Join(t.TempDir(), "session")
			snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyReadOnly})
			st := store.New(sessionDir)
			materialized, err := Materialize(context.Background(), st, snapshot)
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			registerWorktreeCleanup(t, root, materialized.WorktreePath)
			if err := st.SaveTranscriptItems([]any{map[string]any{"content": "preserved transcript"}}); err != nil {
				t.Fatalf("save transcript evidence: %v", err)
			}
			resultRef, err := st.SaveArtifact("results", "raw", map[string]any{"content": "preserved result"})
			if err != nil {
				t.Fatalf("save result evidence: %v", err)
			}
			if test.mutate {
				writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("changed while running\n"), 0o644)
			}

			finalized, err := Finalize(context.Background(), st)
			if err != nil {
				t.Fatalf("Finalize: %v", err)
			}
			if !finalized.Managed || finalized.SourceBeforeDigest != snapshot.SourceDigest() || finalized.SourceAfterDigest == "" {
				t.Fatalf("finalization = %#v", finalized)
			}
			if finalized.SourceChanged != test.wantChanged || finalized.SourceMutated != test.wantChanged {
				t.Fatalf("source result changed=%v mutated=%v, want %v", finalized.SourceChanged, finalized.SourceMutated, test.wantChanged)
			}
			if test.wantChanged {
				var mutation *SourceMutatedError
				if err := finalized.MutationError(); !errors.As(err, &mutation) || mutation.BeforeDigest != snapshot.SourceDigest() || mutation.AfterDigest != finalized.SourceAfterDigest {
					t.Fatalf("mutation error = %#v, %v", mutation, err)
				}
			} else if err := finalized.MutationError(); err != nil {
				t.Fatalf("unchanged finalization returned mutation error: %v", err)
			}
			if _, err := os.Stat(materialized.WorktreePath); err != nil {
				t.Fatalf("terminal finalization removed worktree: %v", err)
			}
			if registered, err := repositoryWorktreeRegistered(context.Background(), snapshot.repository, materialized.WorktreePath); err != nil || !registered {
				t.Fatalf("terminal finalization registration = %v, %v", registered, err)
			}
			transcript, err := st.LoadTranscript()
			if err != nil || transcript.Len() != 1 {
				t.Fatalf("transcript evidence changed: %#v, %v", transcript, err)
			}
			result, err := st.LoadArtifact(resultRef)
			if err != nil || result["content"] != "preserved result" {
				t.Fatalf("result evidence changed: %#v, %v", result, err)
			}

			if _, _, err := graph.RepairAndSaveFromEvents(st); err != nil {
				t.Fatalf("repair graph after finalization: %v", err)
			}
			repeated, err := Finalize(context.Background(), st)
			if err != nil || repeated.SourceAfterDigest != finalized.SourceAfterDigest || repeated.ArtifactRef["digest"] != finalized.ArtifactRef["digest"] {
				t.Fatalf("idempotent finalization = %#v, %v", repeated, err)
			}
		})
	}
}

func TestFinalizeDeletedLaunchSubdirectoryRecordsMutationAndAllowsCleanup(t *testing.T) {
	root := newCommittedRepo(t)
	launchDir := filepath.Join(root, "nested", "launch")
	writeTestFile(t, filepath.Join(launchDir, "source.txt"), []byte("committed launch source\n"), 0o644)
	testGit(t, root, "add", "--all")
	testGit(t, root, "commit", "-m", "add launch directory")

	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: launchDir, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)
	if err := os.RemoveAll(filepath.Join(root, "nested")); err != nil {
		t.Fatalf("remove source launch directory: %v", err)
	}

	finalized, err := Finalize(context.Background(), st)
	if err != nil {
		t.Fatalf("Finalize deleted launch directory: %v", err)
	}
	if !finalized.SourceChanged || !finalized.SourceMutated || finalized.SourceAfterDigest == "" {
		t.Fatalf("deleted launch directory finalization = %#v", finalized)
	}
	if sourceAfter, _ := finalized.Artifact["source_after"].(map[string]any); sourceAfter["launch_cwd"] != launchDir || sourceAfter["launch_subpath"] != "nested/launch" {
		t.Fatalf("source-after launch identity = %#v", sourceAfter)
	}

	cleanup, err := Cleanup(context.Background(), st)
	if err != nil || !cleanup.WorktreeRemoved || !cleanup.MetadataPruned {
		t.Fatalf("Cleanup deleted launch directory = %#v, %v", cleanup, err)
	}
	requirePathAbsent(t, materialized.WorktreePath)
}

func TestCleanupAllowsLaunchSubdirectoryRemovalAfterFinalization(t *testing.T) {
	root := newCommittedRepo(t)
	launchDir := filepath.Join(root, "nested", "launch")
	writeTestFile(t, filepath.Join(launchDir, "source.txt"), []byte("committed launch source\n"), 0o644)
	testGit(t, root, "add", "--all")
	testGit(t, root, "commit", "-m", "add launch directory")

	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: launchDir, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)
	finalized, err := Finalize(context.Background(), st)
	if err != nil || finalized.SourceChanged {
		t.Fatalf("initial Finalize = %#v, %v", finalized, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "nested")); err != nil {
		t.Fatalf("remove finalized launch directory: %v", err)
	}

	cleanup, err := Cleanup(context.Background(), st)
	if err != nil || !cleanup.WorktreeRemoved || !cleanup.MetadataPruned {
		t.Fatalf("Cleanup after finalized launch removal = %#v, %v", cleanup, err)
	}
	requirePathAbsent(t, materialized.WorktreePath)
}

func TestFinalizeInheritedRecordsChangeWithoutSourceMutationOverride(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyInherited})
	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("ordinary inherited edit\n"), 0o644)

	finalized, err := Finalize(context.Background(), store.New(sessionDir))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !finalized.SourceChanged || finalized.SourceMutated || finalized.MutationError() != nil {
		t.Fatalf("inherited finalization = %#v", finalized)
	}
	if finalized.AchievedPolicy != PolicyInherited || materialized.WorktreePath != "" {
		t.Fatalf("inherited workspace = %#v, %#v", materialized, finalized)
	}
}

func TestFinalizeNonGitInheritedWorkspaceIsIdempotent(t *testing.T) {
	launch := t.TempDir()
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: launch, SessionDir: sessionDir, MinimumPolicy: PolicyInherited})
	if _, err := Materialize(context.Background(), store.New(sessionDir), snapshot); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	first, err := Finalize(context.Background(), store.New(sessionDir))
	if err != nil || first.SourceAfterDigest != "" || first.SourceMutated {
		t.Fatalf("first non-Git finalization = %#v, %v", first, err)
	}
	second, err := Finalize(context.Background(), store.New(sessionDir))
	if err != nil || second.ArtifactRef["digest"] != first.ArtifactRef["digest"] {
		t.Fatalf("repeated non-Git finalization = %#v, %v", second, err)
	}
}

func TestTerminalStatusesRetainWorktreeUntilCleanup(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)

	for _, status := range []string{"completed", "failed", "invalid", "interrupted", "killed", "crashed", "orphaned"} {
		if err := st.SaveMetaMap(map[string]any{"status": status}); err != nil {
			t.Fatalf("save %s meta: %v", status, err)
		}
		if _, err := Finalize(context.Background(), st); err != nil {
			t.Fatalf("finalize %s: %v", status, err)
		}
		if _, err := os.Stat(materialized.WorktreePath); err != nil {
			t.Fatalf("%s finalization removed worktree: %v", status, err)
		}
	}
}

func TestCleanupRemovesWorktreeAndPrunesStaleMetadata(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)

	stalePath := filepath.Join(t.TempDir(), "stale-worktree")
	testGit(t, root, "worktree", "add", "--detach", stalePath, "HEAD")
	if err := os.RemoveAll(stalePath); err != nil {
		t.Fatalf("remove stale worktree directory: %v", err)
	}

	result, err := Cleanup(context.Background(), st)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if !result.Managed || !result.RegistrationFound || !result.WorktreeRemoved || !result.MetadataPruned {
		t.Fatalf("cleanup result = %#v", result)
	}
	requirePathAbsent(t, materialized.WorktreePath)
	registrations := testGit(t, root, "worktree", "list", "--porcelain")
	if strings.Contains(registrations, materialized.WorktreePath) || strings.Contains(registrations, stalePath) {
		t.Fatalf("stale registration survived prune:\n%s", registrations)
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("workspace cleanup deleted session evidence: %v", err)
	}

	repeated, err := Cleanup(context.Background(), st)
	if err != nil || !repeated.Managed || repeated.RegistrationFound || !repeated.MetadataPruned {
		t.Fatalf("idempotent cleanup = %#v, %v", repeated, err)
	}
}

func TestCleanupPrunesMissingManagedWorktreeRegistration(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)
	if err := os.RemoveAll(materialized.WorktreePath); err != nil {
		t.Fatalf("remove managed worktree directory: %v", err)
	}
	record, registered, err := repositoryWorktreeRegistration(context.Background(), snapshot.repository, materialized.WorktreePath)
	if err != nil || !registered || !record.Prunable {
		t.Fatalf("missing managed worktree registration = %#v, %v, %v", record, registered, err)
	}

	result, err := Cleanup(context.Background(), st)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if !result.Managed || !result.RegistrationFound || result.WorktreeRemoved || !result.MetadataPruned {
		t.Fatalf("cleanup result = %#v", result)
	}
	if registered, err := repositoryWorktreeRegistered(context.Background(), snapshot.repository, materialized.WorktreePath); err != nil || registered {
		t.Fatalf("prunable managed registration = %v, %v", registered, err)
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("workspace cleanup deleted session evidence: %v", err)
	}
}

func TestCleanupRejectsDirtyReplacementWorktree(t *testing.T) {
	root := newCommittedRepo(t)
	writeTestFile(t, filepath.Join(root, "second.txt"), []byte("second commit\n"), 0o644)
	testGit(t, root, "add", "--all")
	testGit(t, root, "commit", "-m", "second commit")
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)
	testGit(t, root, "worktree", "remove", "--force", materialized.WorktreePath)
	testGit(t, root, "worktree", "add", "-b", "replacement-worktree", materialized.WorktreePath, "HEAD~1")
	sentinel := filepath.Join(materialized.WorktreePath, "preserve-replacement.txt")
	writeTestFile(t, sentinel, []byte("do not delete replacement\n"), 0o644)

	if result, err := Cleanup(context.Background(), st); result != nil || err == nil || !strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("replacement cleanup = %#v, %v", result, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "do not delete replacement\n" {
		t.Fatalf("replacement sentinel changed: %q, %v", data, err)
	}
	record, registered, err := repositoryWorktreeRegistration(context.Background(), snapshot.repository, materialized.WorktreePath)
	if err != nil || !registered || record.Detached || record.Branch == "" || record.Head == snapshot.repository.headCommit {
		t.Fatalf("replacement registration changed = %#v, %v, %v", record, registered, err)
	}
}

func TestCleanupFailureRetainsRegistrationForIdempotentRetry(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyReadOnly})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)
	testGit(t, root, "worktree", "lock", "--reason", "exercise retry", materialized.WorktreePath)

	if result, err := Cleanup(context.Background(), st); err == nil || result != nil || !strings.Contains(err.Error(), "remove execution worktree") {
		t.Fatalf("locked cleanup = %#v, %v", result, err)
	}
	if _, err := os.Stat(materialized.WorktreePath); err != nil {
		t.Fatalf("failed cleanup removed worktree: %v", err)
	}
	if registered, err := repositoryWorktreeRegistered(context.Background(), snapshot.repository, materialized.WorktreePath); err != nil || !registered {
		t.Fatalf("failed cleanup registration = %v, %v", registered, err)
	}

	testGit(t, root, "worktree", "unlock", materialized.WorktreePath)
	result, err := Cleanup(context.Background(), st)
	if err != nil || !result.WorktreeRemoved || !result.MetadataPruned {
		t.Fatalf("retry cleanup = %#v, %v", result, err)
	}
}

func TestCleanupRejectsRehashedWorktreePathOutsideSession(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)

	outside := filepath.Join(t.TempDir(), "do-not-remove")
	writeTestFile(t, filepath.Join(outside, "marker"), []byte("preserve"), 0o644)
	tampered := cloneMap(materialized.Artifact)
	identity := requireObjectField(t, tampered, "identity")
	identity["worktree_path"] = outside
	identity["execution_root"] = filepath.Dir(outside)
	material := cloneMap(identity)
	delete(material, "identity_digest")
	identityDigest, err := semanticDigest(material)
	if err != nil {
		t.Fatalf("recompute identity digest: %v", err)
	}
	identity["identity_digest"] = identityDigest
	if _, _, err := persistExecutionWorkspace(st, tampered); err != nil {
		t.Fatalf("persist adversarial workspace artifact: %v", err)
	}

	if result, err := Cleanup(context.Background(), st); err == nil || result != nil || !strings.Contains(err.Error(), "fixed session location") {
		t.Fatalf("tampered cleanup = %#v, %v", result, err)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "marker")); err != nil || string(data) != "preserve" {
		t.Fatalf("outside path changed: %q, %v", data, err)
	}
}

func TestCleanupRejectsCorruptArtifactIndexAndRetainsWorktree(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)
	if err := os.WriteFile(filepath.Join(sessionDir, "artifacts", store.ArtifactIndexFilename), []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("corrupt artifact index: %v", err)
	}

	if result, err := Cleanup(context.Background(), st); err == nil || result != nil || !strings.Contains(err.Error(), "artifact index") {
		t.Fatalf("corrupt-index cleanup = %#v, %v", result, err)
	}
	if _, err := os.Stat(materialized.WorktreePath); err != nil {
		t.Fatalf("corrupt-index cleanup removed worktree: %v", err)
	}
}

func TestCleanupRejectsSymlinkedWorktreeWithoutTouchingTarget(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	st := store.New(sessionDir)
	materialized, err := Materialize(context.Background(), st, snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	testGit(t, root, "worktree", "remove", "--force", materialized.WorktreePath)
	target := filepath.Join(t.TempDir(), "outside")
	writeTestFile(t, filepath.Join(target, "marker"), []byte("preserve"), 0o644)
	if err := os.Symlink(target, materialized.WorktreePath); err != nil {
		t.Fatalf("replace worktree with symlink: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(materialized.WorktreePath) })

	if result, err := Cleanup(context.Background(), st); err == nil || result != nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("symlink cleanup = %#v, %v", result, err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "marker")); err != nil || string(data) != "preserve" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}

func TestCleanupRejectsUntrackedExecutionWorktree(t *testing.T) {
	sessionDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sessionDir, "execution", "worktree"), 0o755); err != nil {
		t.Fatalf("create untracked worktree path: %v", err)
	}
	if result, err := Cleanup(context.Background(), store.New(sessionDir)); err == nil || result != nil || !strings.Contains(err.Error(), "no execution_workspace artifact") {
		t.Fatalf("untracked worktree cleanup = %#v, %v", result, err)
	}
}

func TestFinalizeOrdinarySessionIsNoOp(t *testing.T) {
	st := store.New(t.TempDir())
	if err := st.SaveMetaMap(map[string]any{"status": "completed"}); err != nil {
		t.Fatalf("save ordinary session: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(st.Root, "artifacts"), 0o755); err != nil {
		t.Fatalf("create ordinary artifacts directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(st.Root, "artifacts", store.ArtifactIndexFilename), []byte("ordinary-corrupt-index\n"), 0o644); err != nil {
		t.Fatalf("write unrelated corrupt index: %v", err)
	}
	finalized, err := Finalize(context.Background(), st)
	if err != nil || finalized.Managed {
		t.Fatalf("ordinary finalization = %#v, %v", finalized, err)
	}
	cleanup, err := Cleanup(context.Background(), st)
	if err != nil || cleanup.Managed {
		t.Fatalf("ordinary cleanup = %#v, %v", cleanup, err)
	}
}

func TestFinalizeRejectsInvalidLatestWorkspaceRef(t *testing.T) {
	st := store.New(t.TempDir())
	if err := st.SaveMetaMap(map[string]any{
		"execution_workspace_ref": map[string]any{"kind": "artifact_ref", "schema_version": 1, "id": "wrong", "digest": contracts.DigestPrefix + strings.Repeat("0", 64)},
	}); err != nil {
		t.Fatalf("save invalid ref: %v", err)
	}
	if finalized, err := Finalize(context.Background(), st); err == nil || finalized != nil {
		t.Fatalf("invalid latest ref = %#v, %v", finalized, err)
	}
}
