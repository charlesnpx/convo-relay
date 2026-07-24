package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestRepositoryInventoryBudgetsAcceptExactBoundariesAndRejectOneOver(t *testing.T) {
	root := newCommittedRepo(t)
	if runtime.GOOS != "windows" {
		if err := os.Symlink("committed.txt", filepath.Join(root, "budget-link")); err != nil {
			t.Fatalf("create budget symlink: %v", err)
		}
		testGit(t, root, "add", "--", "budget-link")
		testGit(t, root, "commit", "-q", "-m", "add budget symlink")
	}
	head := testGit(t, root, "rev-parse", "HEAD")
	testGit(t, root, "update-index", "--add", "--cacheinfo", "160000", head, "vendor/budget-module")
	testGit(t, root, "commit", "-q", "-m", "add budget gitlink")
	baseline := mustPreflight(t, Options{
		LaunchCWD:         root,
		SessionDir:        filepath.Join(t.TempDir(), "baseline"),
		MinimumPolicy:     PolicyInherited,
		InventoryMaxFiles: 1_000,
		InventoryMaxBytes: 1_000_000,
	})
	inventory := baseline.sourceReport["inventory"].(map[string]any)
	files := intFromWorkspaceAny(inventory["file_count"])
	bytes := intFromWorkspaceAny(inventory["byte_count"])
	if files < 1 || bytes < 1 {
		t.Fatalf("baseline inventory accounting = %#v", inventory)
	}
	tracked := inventoryEntries(t, baseline.sourceReport, "tracked_worktree")
	if gitlink := tracked["vendor/budget-module"]; gitlink["present"] != false || gitlink["gitlink_state"] != "uninitialized" {
		t.Fatalf("missing gitlink accounting = %#v", gitlink)
	}
	if runtime.GOOS != "windows" {
		if symlink := tracked["budget-link"]; symlink["mode"] != "120000" || intFromWorkspaceAny(symlink["size_bytes"]) != int64(len("committed.txt")) {
			t.Fatalf("symlink target-byte accounting = %#v", symlink)
		}
	}
	if _, err := Preflight(context.Background(), Options{
		LaunchCWD:         root,
		SessionDir:        filepath.Join(t.TempDir(), "exact"),
		MinimumPolicy:     PolicyInherited,
		InventoryMaxFiles: files,
		InventoryMaxBytes: bytes,
	}); err != nil {
		t.Fatalf("exact inventory boundary rejected: %v", err)
	}
	_, err := Preflight(context.Background(), Options{
		LaunchCWD:         root,
		SessionDir:        filepath.Join(t.TempDir(), "file-over"),
		MinimumPolicy:     PolicyInherited,
		InventoryMaxFiles: files - 1,
		InventoryMaxBytes: bytes,
	})
	requireDiagnosticCode(t, err, contracts.DiagnosticCodeRepositoryInventoryMaxFiles)
	_, err = Preflight(context.Background(), Options{
		LaunchCWD:         root,
		SessionDir:        filepath.Join(t.TempDir(), "byte-over"),
		MinimumPolicy:     PolicyInherited,
		InventoryMaxFiles: files,
		InventoryMaxBytes: bytes - 1,
	})
	requireDiagnosticCode(t, err, contracts.DiagnosticCodeRepositoryInventoryMaxBytes)

	if err := os.Remove(filepath.Join(root, "sub", "dir", "nested.txt")); err != nil {
		t.Fatalf("remove tracked file: %v", err)
	}
	missing := mustPreflight(t, Options{
		LaunchCWD:         root,
		SessionDir:        filepath.Join(t.TempDir(), "missing"),
		MinimumPolicy:     PolicyInherited,
		InventoryMaxFiles: files,
		InventoryMaxBytes: bytes,
	})
	missingInventory := missing.sourceReport["inventory"].(map[string]any)
	if intFromWorkspaceAny(missingInventory["file_count"]) != files ||
		intFromWorkspaceAny(missingInventory["byte_count"]) >= bytes {
		t.Fatalf("missing tracked path accounting = %#v, baseline=%#v", missingInventory, inventory)
	}
}

func TestTrackedInventoryRejectsSymlinkedAncestorWithoutFollowingOutsideRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := newCommittedRepo(t)
	outside := filepath.Join(t.TempDir(), "outside")
	outsideTarget := filepath.Join(outside, "dir", "nested.txt")
	writeTestFile(t, outsideTarget, []byte("outside bytes must not be inventoried\n"), 0o644)
	if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
		t.Fatalf("remove tracked directory: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
		t.Fatalf("replace tracked ancestor with symlink: %v", err)
	}

	snapshot, err := inspectRepository(context.Background(), "git", root)
	if err != nil {
		t.Fatalf("inspect repository with symlinked tracked ancestor: %v", err)
	}
	tracked := snapshot.sourceReport["inventory"].(map[string]any)["tracked_worktree"].([]any)
	var observed map[string]any
	for _, raw := range tracked {
		entry := raw.(map[string]any)
		if entry["path"] == "sub/dir/nested.txt" {
			observed = entry
			break
		}
	}
	if observed == nil ||
		observed["present"] != false ||
		observed["obstruction"] != "ancestor_symlink" ||
		observed["raw_digest"] != nil {
		t.Fatalf("tracked entry through outside symlink = %#v", observed)
	}
}

func TestMaterializePreservesRawExportRepositoryBudgetDiagnostic(t *testing.T) {
	root := t.TempDir()
	testGit(t, root, "init", "-q")
	testGit(t, root, "config", "user.name", "Workspace Test")
	testGit(t, root, "config", "user.email", "workspace@example.invalid")
	writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("committed bytes exceed the export budget\n"), 0o644)
	testGit(t, root, "add", "--", "committed.txt")
	testGit(t, root, "commit", "-q", "-m", "initial")
	if err := os.Remove(filepath.Join(root, "committed.txt")); err != nil {
		t.Fatalf("remove tracked source file: %v", err)
	}

	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{
		LaunchCWD:         root,
		SessionDir:        sessionDir,
		MinimumPolicy:     PolicyEphemeral,
		AllowDirtySource:  true,
		InventoryMaxFiles: 1,
		InventoryMaxBytes: 1,
	})
	_, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	requireDiagnosticCode(t, err, contracts.DiagnosticCodeRepositoryInventoryMaxBytes)
	var diagnosticErr *contracts.DiagnosticError
	if !errors.As(err, &diagnosticErr) || diagnosticErr.Diagnostics[0].Path != "/runtime_config/limits" {
		t.Fatalf("raw-export limit diagnostic = %#v, %v", diagnosticErr, err)
	}
	worktreePath := filepath.Join(sessionDir, "execution", "worktree")
	if _, statErr := os.Lstat(worktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("failed raw export left a worktree path: %v", statErr)
	}
	if output := testGit(t, root, "worktree", "list", "--porcelain"); strings.Contains(output, worktreePath) {
		t.Fatalf("failed raw export left a worktree registration:\n%s", output)
	}
}

func TestMaterializeRejectsOversizedCommittedBlobWithoutBlockingBatchClose(t *testing.T) {
	root := t.TempDir()
	testGit(t, root, "init", "-q")
	testGit(t, root, "config", "user.name", "Workspace Test")
	testGit(t, root, "config", "user.email", "workspace@example.invalid")
	writeTestFile(t, filepath.Join(root, "large.bin"), bytes.Repeat([]byte("x"), 2*1024*1024), 0o644)
	testGit(t, root, "add", "--", "large.bin")
	testGit(t, root, "commit", "-q", "-m", "large blob")
	if err := os.Remove(filepath.Join(root, "large.bin")); err != nil {
		t.Fatalf("remove tracked large blob: %v", err)
	}

	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{
		LaunchCWD:         root,
		SessionDir:        sessionDir,
		MinimumPolicy:     PolicyEphemeral,
		AllowDirtySource:  true,
		InventoryMaxFiles: 1,
		InventoryMaxBytes: 1,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	_, err := Materialize(ctx, store.New(sessionDir), snapshot)
	requireDiagnosticCode(t, err, contracts.DiagnosticCodeRepositoryInventoryMaxBytes)
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("oversized raw export waited for cat-file batch shutdown: %s", elapsed)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("oversized raw export required context cancellation: %v", err)
	}
}

func TestRepositoryInventoryAndFinalizationObserveCancellation(t *testing.T) {
	root := newCommittedRepo(t)
	canceledPreflight, cancelPreflight := context.WithCancel(context.Background())
	cancelPreflight()
	sessionDir := filepath.Join(t.TempDir(), "session")
	if _, err := Preflight(canceledPreflight, Options{
		LaunchCWD:     root,
		SessionDir:    sessionDir,
		MinimumPolicy: PolicyInherited,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled repository preflight = %v", err)
	}
	if _, err := os.Lstat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("canceled repository preflight claimed a session: %v", err)
	}

	snapshot := mustPreflight(t, Options{
		LaunchCWD:     root,
		SessionDir:    sessionDir,
		MinimumPolicy: PolicyInherited,
	})
	st := store.New(sessionDir)
	if _, err := Materialize(context.Background(), st, snapshot); err != nil {
		t.Fatalf("materialize cancellation fixture: %v", err)
	}
	canceledFinalization, cancelFinalization := context.WithCancel(context.Background())
	cancelFinalization()
	if _, err := Finalize(canceledFinalization, st); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled final source inventory = %v", err)
	}
}

func TestWorkspaceContextReaderObservesCancellationDuringRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &contextReader{
		ctx: ctx,
		reader: workspaceCancelingReader{
			cancel: cancel,
		},
	}
	buffer := make([]byte, 8)
	count, err := reader.Read(buffer)
	if count != len("content") || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceling context read = %d, %v", count, err)
	}
}

func TestDirtyIsolatedLaunchRequiresExplicitCommittedHeadOverride(t *testing.T) {
	root := newCommittedRepo(t)
	writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("staged\n"), 0o644)
	testGit(t, root, "add", "--", "committed.txt")
	writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("unstaged\n"), 0o644)
	writeTestFile(t, filepath.Join(root, "untracked.txt"), []byte("untracked\n"), 0o644)
	sessionDir := filepath.Join(t.TempDir(), "session")

	_, err := Preflight(context.Background(), Options{
		LaunchCWD:     root,
		SessionDir:    sessionDir,
		MinimumPolicy: PolicyEphemeral,
	})
	requireDiagnosticCode(t, err, DiagnosticCodeDirtySource)
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("dirty rejection claimed a session: %v", statErr)
	}

	snapshot := mustPreflight(t, Options{
		LaunchCWD:        root,
		SessionDir:       sessionDir,
		MinimumPolicy:    PolicyEphemeral,
		AllowDirtySource: true,
	})
	changes := snapshot.SourceChanges()
	if changes.Staged != 1 || changes.Unstaged != 1 || changes.Untracked != 1 {
		t.Fatalf("dirty source counts = %#v", changes)
	}
	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if err != nil {
		t.Fatalf("materialize dirty override: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)
	facts, err := LaunchFactsFromArtifact(materialized.Artifact)
	if err != nil {
		t.Fatalf("launch facts: %v", err)
	}
	if !facts.Recorded || !facts.AllowDirtySourceRequested ||
		facts.StagedChanges != 1 || facts.UnstagedChanges != 1 || facts.UnignoredUntrackedChanges != 1 {
		t.Fatalf("authoritative dirty launch facts = %#v", facts)
	}
	projection := facts.Projection()
	if _, err := ValidateLaunchFactsProjection(projection, materialized.Artifact); err != nil {
		t.Fatalf("valid dirty-source projection: %v", err)
	}
	projection[SourceUnstagedChangesKey] = int64(2)
	if _, err := ValidateLaunchFactsProjection(projection, materialized.Artifact); err == nil {
		t.Fatal("disagreeing dirty-source projection was accepted")
	}
}

func TestDirtyIsolatedLaunchDoesNotHonorIndexWorktreeHints(t *testing.T) {
	for _, test := range []struct {
		name string
		flag string
	}{
		{name: "skip worktree", flag: "--skip-worktree"},
		{name: "assume unchanged", flag: "--assume-unchanged"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newCommittedRepo(t)
			writeTestFile(t, filepath.Join(root, ".gitattributes"), []byte("committed.txt filter=danger\n"), 0o644)
			testGit(t, root, "add", "--", ".gitattributes")
			testGit(t, root, "commit", "-q", "-m", "add filter attribute")
			filterMarker := filepath.Join(t.TempDir(), "filter-ran")
			filterScript := filepath.Join(t.TempDir(), "filter")
			writeExecutableTestScript(t, filterScript, filterMarker)
			testGit(t, root, "update-index", test.flag, "--", "committed.txt")
			testGit(t, root, "config", "filter.danger.process", filterScript)
			testGit(t, root, "config", "filter.danger.required", "true")
			writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("hint-hidden change\n"), 0o644)
			sessionDir := filepath.Join(t.TempDir(), "session")

			_, err := Preflight(context.Background(), Options{
				LaunchCWD:     root,
				SessionDir:    sessionDir,
				MinimumPolicy: PolicyEphemeral,
			})
			requireDiagnosticCode(t, err, DiagnosticCodeDirtySource)
			if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
				t.Fatalf("hint-hidden dirty rejection claimed a session: %v", statErr)
			}

			snapshot := mustPreflight(t, Options{
				LaunchCWD:        root,
				SessionDir:       sessionDir,
				MinimumPolicy:    PolicyEphemeral,
				AllowDirtySource: true,
			})
			if changes := snapshot.SourceChanges(); changes.Staged != 0 || changes.Unstaged != 1 || changes.Untracked != 0 {
				t.Fatalf("hint-hidden dirty source counts = %#v", changes)
			}
			if _, err := os.Stat(filterMarker); !os.IsNotExist(err) {
				t.Fatalf("hint-resistant dirtiness executed process filter: %v", err)
			}
		})
	}
}

func TestDirtyIsolatedLaunchDoesNotTrustRepositoryStatCache(t *testing.T) {
	root := newCommittedRepo(t)
	path := filepath.Join(root, "committed.txt")
	testGit(t, root, "update-index", "--refresh")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat committed file: %v", err)
	}
	testGit(t, root, "config", "core.trustctime", "false")
	testGit(t, root, "config", "core.checkStat", "minimal")
	writeTestFile(t, path, []byte("tampered!\n"), 0o644)
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("restore committed file mtime: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("adversarial stat-cache fixture changed metadata: before=%v after=%v err=%v", before, after, err)
	}
	sessionDir := filepath.Join(t.TempDir(), "session")

	_, err = Preflight(context.Background(), Options{
		LaunchCWD:     root,
		SessionDir:    sessionDir,
		MinimumPolicy: PolicyEphemeral,
	})
	requireDiagnosticCode(t, err, DiagnosticCodeDirtySource)
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("stat-cache-hidden dirty rejection claimed a session: %v", statErr)
	}

	snapshot := mustPreflight(t, Options{
		LaunchCWD:        root,
		SessionDir:       sessionDir,
		MinimumPolicy:    PolicyEphemeral,
		AllowDirtySource: true,
	})
	if changes := snapshot.SourceChanges(); changes.Staged != 0 || changes.Unstaged != 1 || changes.Untracked != 0 {
		t.Fatalf("stat-cache-resistant dirty source counts = %#v", changes)
	}
}

func TestDirtySourceOverrideRejectsInheritedAndNonGitExecution(t *testing.T) {
	root := newCommittedRepo(t)
	_, err := Preflight(context.Background(), Options{
		LaunchCWD:        root,
		SessionDir:       filepath.Join(t.TempDir(), "inherited"),
		MinimumPolicy:    PolicyInherited,
		AllowDirtySource: true,
	})
	requireDiagnosticCode(t, err, DiagnosticCodeDirtyInapplicable)

	nonGit := t.TempDir()
	_, err = Preflight(context.Background(), Options{
		LaunchCWD:        nonGit,
		SessionDir:       filepath.Join(t.TempDir(), "non-git"),
		MinimumPolicy:    PolicyInherited,
		AllowDirtySource: true,
	})
	requireDiagnosticCode(t, err, DiagnosticCodeDirtyInapplicable)
}

type workspaceCancelingReader struct {
	cancel context.CancelFunc
}

func (r workspaceCancelingReader) Read(buffer []byte) (int, error) {
	count := copy(buffer, []byte("content"))
	r.cancel()
	return count, nil
}
