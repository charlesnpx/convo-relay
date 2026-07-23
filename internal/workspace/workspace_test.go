package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestResolvePolicyOrderingAndExplicitOverrides(t *testing.T) {
	tests := []struct {
		name               string
		minimum            string
		requested          string
		explicit           bool
		wantEffective      string
		wantAchieved       string
		wantDiagnosticCode string
	}{
		{name: "defaults", wantEffective: PolicyInherited},
		{name: "recipe minimum survives ordinary default", minimum: PolicyReadOnly, requested: PolicyInherited, wantEffective: PolicyReadOnly},
		{name: "explicit strengthening", minimum: PolicyInherited, requested: PolicyReadOnly, explicit: true, wantEffective: PolicyReadOnly},
		{name: "explicit strongest", minimum: PolicyReadOnly, requested: PolicyEphemeral, explicit: true, wantEffective: PolicyEphemeral},
		{name: "same explicit policy", minimum: PolicyReadOnly, requested: PolicyReadOnly, explicit: true, wantEffective: PolicyReadOnly},
		{name: "empty explicit request", minimum: PolicyInherited, requested: "", explicit: true, wantDiagnosticCode: DiagnosticCodeInvalidPolicy},
		{name: "explicit weakening", minimum: PolicyEphemeral, requested: PolicyReadOnly, explicit: true, wantDiagnosticCode: DiagnosticCodePolicyWeakened},
		{name: "invalid minimum", minimum: "shared", wantDiagnosticCode: DiagnosticCodeInvalidPolicy},
		{name: "invalid request", requested: "shared", wantDiagnosticCode: DiagnosticCodeInvalidPolicy},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolvePolicy(test.minimum, test.requested, test.explicit)
			if test.wantDiagnosticCode != "" {
				requireDiagnosticCode(t, err, test.wantDiagnosticCode)
				return
			}
			if err != nil {
				t.Fatalf("ResolvePolicy: %v", err)
			}
			if resolution.Minimum != normalizePolicyDefault(test.minimum) || resolution.Requested != normalizePolicyDefault(test.requested) {
				t.Fatalf("resolution did not preserve normalized inputs: %#v", resolution)
			}
			if resolution.RequestedExplicit != test.explicit || resolution.Effective != test.wantEffective || resolution.Achieved != test.wantAchieved {
				t.Fatalf("resolution = %#v", resolution)
			}
		})
	}
}

func TestPreflightDoesNotClaimAchievedPolicy(t *testing.T) {
	root := newCommittedRepo(t)
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: filepath.Join(t.TempDir(), "session"), MinimumPolicy: PolicyReadOnly})
	if snapshot.Policy().Achieved != "" {
		t.Fatalf("preflight achieved policy = %q", snapshot.Policy().Achieved)
	}
	if _, exists := snapshot.Report()["achieved_policy"]; exists {
		t.Fatalf("preflight report claimed an achieved policy: %#v", snapshot.Report())
	}
}

func TestPreflightInventoriesCleanAndDirtySourceStates(t *testing.T) {
	tests := []struct {
		name             string
		mutate           func(*testing.T, string)
		wantStaged       []string
		wantUnstaged     []string
		wantUntracked    []string
		wantDigestChange bool
	}{
		{name: "clean"},
		{
			name: "staged only",
			mutate: func(t *testing.T, root string) {
				writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("staged\n"), 0o644)
				testGit(t, root, "add", "--", "committed.txt")
			},
			wantStaged: []string{"committed.txt"}, wantDigestChange: true,
		},
		{
			name: "unstaged only",
			mutate: func(t *testing.T, root string) {
				writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("unstaged\n"), 0o644)
			},
			wantUnstaged: []string{"committed.txt"}, wantDigestChange: true,
		},
		{
			name: "staged and unstaged",
			mutate: func(t *testing.T, root string) {
				writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("staged\n"), 0o644)
				testGit(t, root, "add", "--", "committed.txt")
				writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("unstaged after staging\n"), 0o644)
			},
			wantStaged: []string{"committed.txt"}, wantUnstaged: []string{"committed.txt"}, wantDigestChange: true,
		},
		{
			name: "untracked",
			mutate: func(t *testing.T, root string) {
				writeTestFile(t, filepath.Join(root, "untracked-λ.txt"), []byte("untracked\n"), 0o600)
			},
			wantUntracked: []string{"untracked-λ.txt"}, wantDigestChange: true,
		},
		{
			name: "ignored",
			mutate: func(t *testing.T, root string) {
				writeTestFile(t, filepath.Join(root, "ignored.log"), []byte("ignored\n"), 0o644)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newCommittedRepo(t)
			clean := mustPreflight(t, Options{
				LaunchCWD:     root,
				SessionDir:    filepath.Join(t.TempDir(), "clean-session"),
				MinimumPolicy: PolicyInherited,
			})
			if test.mutate != nil {
				test.mutate(t, root)
			}
			snapshot := mustPreflight(t, Options{
				LaunchCWD:     root,
				SessionDir:    filepath.Join(t.TempDir(), "session"),
				MinimumPolicy: PolicyInherited,
			})
			if snapshot.HeadCommit() == "" || snapshot.SourceDigest() == "" || snapshot.GitRoot() != root {
				t.Fatalf("missing Git identity: %#v", snapshot.Report())
			}
			if got := exclusionPaths(t, snapshot.exclusions, "staged"); !slices.Equal(got, test.wantStaged) {
				t.Fatalf("staged exclusions = %#v, want %#v", got, test.wantStaged)
			}
			if got := exclusionPaths(t, snapshot.exclusions, "unstaged"); !slices.Equal(got, test.wantUnstaged) {
				t.Fatalf("unstaged exclusions = %#v, want %#v", got, test.wantUnstaged)
			}
			if got := exclusionPaths(t, snapshot.exclusions, "untracked"); !slices.Equal(got, test.wantUntracked) {
				t.Fatalf("untracked exclusions = %#v, want %#v", got, test.wantUntracked)
			}
			changed := snapshot.SourceDigest() != clean.SourceDigest()
			if changed != test.wantDigestChange {
				t.Fatalf("digest changed = %v, want %v\nclean: %s\nnext:  %s", changed, test.wantDigestChange, clean.SourceDigest(), snapshot.SourceDigest())
			}
			if _, err := os.Stat(snapshot.SessionDir()); !os.IsNotExist(err) {
				t.Fatalf("Preflight created session directory: %v", err)
			}
		})
	}
}

func TestPreflightInventoryIncludesRawSymlinkAndExecutableModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink mode assertions require Unix Git semantics")
	}
	root := newCommittedRepo(t)
	scriptPath := filepath.Join(root, "script.sh")
	writeTestFile(t, scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	if err := os.Symlink("committed.txt", filepath.Join(root, "committed-link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	testGit(t, root, "add", "--", "script.sh", "committed-link")
	testGit(t, root, "commit", "-q", "-m", "add modes")

	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: filepath.Join(t.TempDir(), "session")})
	tracked := inventoryEntries(t, snapshot.sourceReport, "tracked_worktree")
	if got := tracked["script.sh"]["mode"]; got != "100755" {
		t.Fatalf("script mode = %#v", got)
	}
	if got := tracked["committed-link"]["mode"]; got != "120000" {
		t.Fatalf("symlink mode = %#v", got)
	}
	if got := tracked["committed-link"]["raw_digest"]; got != digestBytes([]byte("committed.txt")) {
		t.Fatalf("symlink raw digest = %#v", got)
	}
}

func TestPreflightInventoriesStagedFileDirectoryTransitions(t *testing.T) {
	t.Run("file to directory", func(t *testing.T) {
		root := newCommittedRepo(t)
		writeTestFile(t, filepath.Join(root, "node"), []byte("file\n"), 0o644)
		testGit(t, root, "add", "--", "node")
		testGit(t, root, "commit", "-q", "-m", "add file node")
		if err := os.Remove(filepath.Join(root, "node")); err != nil {
			t.Fatalf("remove file node: %v", err)
		}
		writeTestFile(t, filepath.Join(root, "node", "child.txt"), []byte("child\n"), 0o644)
		testGit(t, root, "add", "--all")

		snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: filepath.Join(t.TempDir(), "session")})
		tracked := inventoryEntries(t, snapshot.sourceReport, "tracked_worktree")
		if tracked["node"]["present"] != false || tracked["node"]["obstruction"] != "directory" {
			t.Fatalf("replaced file inventory = %#v", tracked["node"])
		}
		if tracked["node/child.txt"]["present"] != true {
			t.Fatalf("staged descendant inventory = %#v", tracked["node/child.txt"])
		}
	})

	t.Run("directory to file", func(t *testing.T) {
		root := newCommittedRepo(t)
		writeTestFile(t, filepath.Join(root, "node", "child.txt"), []byte("child\n"), 0o644)
		testGit(t, root, "add", "--", "node/child.txt")
		testGit(t, root, "commit", "-q", "-m", "add directory node")
		if err := os.RemoveAll(filepath.Join(root, "node")); err != nil {
			t.Fatalf("remove directory node: %v", err)
		}
		writeTestFile(t, filepath.Join(root, "node"), []byte("file\n"), 0o644)
		testGit(t, root, "add", "--all")

		snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: filepath.Join(t.TempDir(), "session")})
		tracked := inventoryEntries(t, snapshot.sourceReport, "tracked_worktree")
		if tracked["node"]["present"] != true {
			t.Fatalf("replacement file inventory = %#v", tracked["node"])
		}
		if tracked["node/child.txt"]["present"] != false || tracked["node/child.txt"]["obstruction"] != "ancestor_not_directory" {
			t.Fatalf("replaced descendant inventory = %#v", tracked["node/child.txt"])
		}
	})
}

func TestPreflightInventoriesInitializedDirtyAndDeinitializedSubmodules(t *testing.T) {
	submoduleRoot := newCommittedRepo(t)
	root := newCommittedRepo(t)
	testGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", submoduleRoot, "vendor/module")
	testGit(t, root, "commit", "-q", "-m", "add submodule")

	clean := mustPreflight(t, Options{LaunchCWD: root, SessionDir: filepath.Join(t.TempDir(), "clean-session")})
	cleanEntry := inventoryEntries(t, clean.sourceReport, "tracked_worktree")["vendor/module"]
	if cleanEntry["present"] != true || cleanEntry["mode"] != "160000" || cleanEntry["gitlink_state"] != "initialized" {
		t.Fatalf("initialized submodule inventory = %#v", cleanEntry)
	}
	if cleanEntry["submodule_head"] != testGit(t, submoduleRoot, "rev-parse", "HEAD") || cleanEntry["submodule_source_digest"] == "" {
		t.Fatalf("initialized submodule identity = %#v", cleanEntry)
	}
	cleanInventory := clean.sourceReport["inventory"].(map[string]any)
	totalFiles := intFromWorkspaceAny(cleanInventory["file_count"])
	totalBytes := intFromWorkspaceAny(cleanInventory["byte_count"])
	submoduleFiles := intFromWorkspaceAny(cleanEntry["submodule_inventory_files"])
	submoduleBytes := intFromWorkspaceAny(cleanEntry["submodule_inventory_bytes"])
	directFiles := int64(len(inventoryEntries(t, clean.sourceReport, "tracked_worktree")) + len(inventoryEntries(t, clean.sourceReport, "untracked_worktree")))
	directBytes := inventoryEntryBytes(inventoryEntries(t, clean.sourceReport, "tracked_worktree")) +
		inventoryEntryBytes(inventoryEntries(t, clean.sourceReport, "untracked_worktree"))
	if submoduleFiles < 1 || submoduleBytes < 1 ||
		totalFiles != directFiles+submoduleFiles ||
		totalBytes != directBytes+submoduleBytes {
		t.Fatalf(
			"shared submodule accounting total=(%d,%d) direct=(%d,%d) nested=(%d,%d)",
			totalFiles,
			totalBytes,
			directFiles,
			directBytes,
			submoduleFiles,
			submoduleBytes,
		)
	}
	if _, err := Preflight(context.Background(), Options{
		LaunchCWD:         root,
		SessionDir:        filepath.Join(t.TempDir(), "exact-budget"),
		InventoryMaxFiles: totalFiles,
		InventoryMaxBytes: totalBytes,
	}); err != nil {
		t.Fatalf("exact shared submodule budget rejected: %v", err)
	}
	_, err := Preflight(context.Background(), Options{
		LaunchCWD:         root,
		SessionDir:        filepath.Join(t.TempDir(), "file-budget"),
		InventoryMaxFiles: totalFiles - 1,
		InventoryMaxBytes: totalBytes,
	})
	requireDiagnosticCode(t, err, contracts.DiagnosticCodeRepositoryInventoryMaxFiles)
	_, err = Preflight(context.Background(), Options{
		LaunchCWD:         root,
		SessionDir:        filepath.Join(t.TempDir(), "byte-budget"),
		InventoryMaxFiles: totalFiles,
		InventoryMaxBytes: totalBytes - 1,
	})
	requireDiagnosticCode(t, err, contracts.DiagnosticCodeRepositoryInventoryMaxBytes)

	moduleWorktree := filepath.Join(root, "vendor", "module")
	writeTestFile(t, filepath.Join(moduleWorktree, "committed.txt"), []byte("staged submodule\n"), 0o644)
	testGit(t, moduleWorktree, "add", "--", "committed.txt")
	writeTestFile(t, filepath.Join(moduleWorktree, "committed.txt"), []byte("unstaged submodule\n"), 0o644)
	writeTestFile(t, filepath.Join(moduleWorktree, "untracked.txt"), []byte("untracked submodule\n"), 0o644)
	dirtySession := filepath.Join(t.TempDir(), "dirty-session")
	dirty := mustPreflight(t, Options{LaunchCWD: root, SessionDir: dirtySession})
	dirtyEntry := inventoryEntries(t, dirty.sourceReport, "tracked_worktree")["vendor/module"]
	if dirty.SourceDigest() == clean.SourceDigest() || dirtyEntry["submodule_source_digest"] == cleanEntry["submodule_source_digest"] {
		t.Fatalf("dirty submodule did not change inventory: clean=%#v dirty=%#v", cleanEntry, dirtyEntry)
	}
	if changes := dirty.SourceChanges(); changes.Unstaged != 1 {
		t.Fatalf("parent dirtiness did not include initialized submodule: %#v", changes)
	}
	dirtySubmoduleSource, ok := dirtyEntry["submodule_source"].(map[string]any)
	if !ok {
		t.Fatalf("dirty submodule source report = %#v", dirtyEntry["submodule_source"])
	}
	dirtySubmoduleExclusions, ok := dirtySubmoduleSource["exclusions"].(map[string]any)
	if !ok ||
		len(exclusionPaths(t, dirtySubmoduleExclusions, "staged")) != 1 ||
		len(exclusionPaths(t, dirtySubmoduleExclusions, "unstaged")) != 1 ||
		len(exclusionPaths(t, dirtySubmoduleExclusions, "untracked")) != 1 {
		t.Fatalf("dirty submodule exclusions = %#v", dirtySubmoduleSource["exclusions"])
	}
	materialized, err := Materialize(context.Background(), store.New(dirtySession), dirty)
	if err != nil {
		t.Fatalf("persist dirty initialized-submodule inventory: %v", err)
	}
	persistedSource, ok := materialized.Artifact["source"].(map[string]any)
	if !ok {
		t.Fatalf("persisted source report = %#v", materialized.Artifact["source"])
	}
	persistedEntry := inventoryEntries(t, persistedSource, "tracked_worktree")["vendor/module"]
	persistedSubmoduleSource, ok := persistedEntry["submodule_source"].(map[string]any)
	if !ok || persistedSubmoduleSource["exclusions"] == nil {
		t.Fatalf("persisted submodule dirtiness = %#v", persistedEntry)
	}

	testGit(t, root, "submodule", "deinit", "-q", "-f", "--", "vendor/module")
	deinitialized := mustPreflight(t, Options{LaunchCWD: root, SessionDir: filepath.Join(t.TempDir(), "deinitialized-session")})
	deinitializedEntry := inventoryEntries(t, deinitialized.sourceReport, "tracked_worktree")["vendor/module"]
	if deinitializedEntry["present"] != false || deinitializedEntry["gitlink_state"] != "uninitialized" {
		t.Fatalf("deinitialized submodule inventory = %#v", deinitializedEntry)
	}
	if _, exists := deinitializedEntry["submodule_head"]; exists {
		t.Fatalf("deinitialized submodule claimed a HEAD: %#v", deinitializedEntry)
	}
}

func TestRepositoryInventoryRejectsInitializedSubmoduleCycles(t *testing.T) {
	root := t.TempDir()
	testGit(t, root, "init", "-q")
	testGit(t, root, "config", "user.name", "Workspace Test")
	testGit(t, root, "config", "user.email", "workspace@example.invalid")
	testGit(t, root, "commit", "--allow-empty", "-q", "-m", "initial")
	head := testGit(t, root, "rev-parse", "HEAD")
	testGit(t, root, "update-index", "--add", "--cacheinfo", "160000", head, "loop")
	testGit(t, root, "commit", "-q", "-m", "add repository cycle")
	loopWorktree := filepath.Join(root, "loop")
	testGit(t, root, "worktree", "add", "--detach", "--no-checkout", loopWorktree, "HEAD")
	t.Cleanup(func() {
		command := exec.Command("git", "-C", root, "worktree", "remove", "--force", loopWorktree)
		_ = command.Run()
	})
	testGit(t, loopWorktree, "read-tree", "--reset", "HEAD")
	canonicalRoot, err := canonicalExistingDirectory(root)
	if err != nil {
		t.Fatalf("canonical cycle root: %v", err)
	}

	_, err = inspectRepositoryWithLimits(
		context.Background(),
		"git",
		canonicalRoot,
		repositoryInventoryLimits{maxFiles: 1_000, maxBytes: 1_000_000},
	)
	if err == nil || !strings.Contains(err.Error(), "repository inventory cycle detected") {
		t.Fatalf("repository cycle error = %v", err)
	}
}

func TestRepositoryInventoryAllowsDepthEightAndRejectsDepthNine(t *testing.T) {
	root := newInitializedSubmoduleChain(t, 9)
	canonicalRoot, err := canonicalExistingDirectory(root)
	if err != nil {
		t.Fatalf("canonical submodule-chain root: %v", err)
	}
	root = canonicalRoot
	depthEightRoot := filepath.Join(root, "nested")
	if _, err := inspectRepositoryWithLimits(
		context.Background(),
		"git",
		depthEightRoot,
		repositoryInventoryLimits{maxFiles: 10_000, maxBytes: 1_000_000_000},
	); err != nil {
		t.Fatalf("depth-eight initialized submodule chain rejected: %v", err)
	}
	_, err = inspectRepositoryWithLimits(
		context.Background(),
		"git",
		root,
		repositoryInventoryLimits{maxFiles: 10_000, maxBytes: 1_000_000_000},
	)
	if err == nil || !strings.Contains(err.Error(), "repository inventory depth exceeds 8") {
		t.Fatalf("depth-nine initialized submodule chain error = %v", err)
	}
}

func TestPreflightIgnoresAmbientGitRepositoryRouting(t *testing.T) {
	root := newCommittedRepo(t)
	wantHead := testGit(t, root, "rev-parse", "HEAD")
	redirected := newCommittedRepo(t)
	writeTestFile(t, filepath.Join(redirected, "redirected.txt"), []byte("wrong repository\n"), 0o644)
	testGit(t, redirected, "add", "--", "redirected.txt")
	testGit(t, redirected, "commit", "-q", "-m", "redirected state")

	t.Run("Git directory and work tree", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(redirected, ".git"))
		t.Setenv("GIT_WORK_TREE", redirected)

		snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: filepath.Join(t.TempDir(), "session")})
		if snapshot.GitRoot() != root {
			t.Fatalf("Git root = %q, want %q", snapshot.GitRoot(), root)
		}
		if snapshot.HeadCommit() != wantHead {
			t.Fatalf("HEAD was redirected: %s", snapshot.HeadCommit())
		}
	})

	t.Run("alternate index", func(t *testing.T) {
		t.Setenv("GIT_INDEX_FILE", filepath.Join(redirected, ".git", "index"))

		snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: filepath.Join(t.TempDir(), "session")})
		entries := inventoryEntries(t, snapshot.sourceReport, "index_entries")
		if _, exists := entries["redirected.txt"]; exists {
			t.Fatalf("inventory used ambient alternate index: %#v", entries["redirected.txt"])
		}
		if _, exists := entries["committed.txt"]; !exists {
			t.Fatalf("inventory omitted intended index entries: %#v", entries)
		}
	})
}

func TestPreflightRequiredIsolationRejectsUnavailableGitStatesAndPathsWithoutMutation(t *testing.T) {
	t.Run("non Git", func(t *testing.T) {
		launch := t.TempDir()
		session := filepath.Join(t.TempDir(), "session")
		_, err := Preflight(context.Background(), Options{LaunchCWD: launch, SessionDir: session, MinimumPolicy: PolicyReadOnly})
		requireDiagnosticCode(t, err, DiagnosticCodeGitRequired)
		requirePathAbsent(t, session)

		inherited := mustPreflight(t, Options{LaunchCWD: launch, SessionDir: session})
		if inherited.sourceReport["repository_state"] != "non_git" || inherited.repository != nil {
			t.Fatalf("inherited non-Git report = %#v", inherited.Report())
		}
	})

	t.Run("unavailable Git executable", func(t *testing.T) {
		launch := t.TempDir()
		session := filepath.Join(t.TempDir(), "session")
		_, err := Preflight(context.Background(), Options{
			LaunchCWD:     launch,
			SessionDir:    session,
			MinimumPolicy: PolicyReadOnly,
			GitBinary:     filepath.Join(t.TempDir(), "missing-git"),
		})
		requireDiagnosticCode(t, err, DiagnosticCodeInventoryFailed)
		requirePathAbsent(t, session)
	})

	t.Run("session below a file", func(t *testing.T) {
		root := newCommittedRepo(t)
		container := t.TempDir()
		file := filepath.Join(container, "not-a-directory")
		writeTestFile(t, file, []byte("data"), 0o644)
		_, err := Preflight(context.Background(), Options{
			LaunchCWD:     root,
			SessionDir:    filepath.Join(file, "session"),
			MinimumPolicy: PolicyReadOnly,
		})
		requireDiagnosticCode(t, err, DiagnosticCodeSessionConflict)
	})

	t.Run("unborn repository", func(t *testing.T) {
		root := t.TempDir()
		testGit(t, root, "init", "-q")
		session := filepath.Join(t.TempDir(), "session")
		_, err := Preflight(context.Background(), Options{LaunchCWD: root, SessionDir: session, MinimumPolicy: PolicyEphemeral})
		requireDiagnosticCode(t, err, DiagnosticCodeUnbornRepository)
		requirePathAbsent(t, session)

		inherited := mustPreflight(t, Options{LaunchCWD: root, SessionDir: session})
		if inherited.sourceReport["repository_state"] != "unborn" {
			t.Fatalf("inherited unborn report = %#v", inherited.Report())
		}
	})

	for _, source := range []string{SessionPathExplicit, SessionPathRelayHome} {
		t.Run("session inside source via "+source, func(t *testing.T) {
			root := newCommittedRepo(t)
			session := filepath.Join(root, "sessions", "candidate")
			_, err := Preflight(context.Background(), Options{
				LaunchCWD:         root,
				SessionDir:        session,
				SessionPathSource: source,
				MinimumPolicy:     PolicyReadOnly,
			})
			requireDiagnosticCode(t, err, DiagnosticCodeSessionConflict)
			requirePathAbsent(t, session)
		})
	}

	t.Run("case-variant session inside source", func(t *testing.T) {
		root := newCommittedRepo(t)
		caseVariantRoot := strings.ToUpper(root)
		rootInfo, rootErr := os.Stat(root)
		variantInfo, variantErr := os.Stat(caseVariantRoot)
		if caseVariantRoot == root || rootErr != nil || variantErr != nil || !os.SameFile(rootInfo, variantInfo) {
			t.Skip("filesystem is case-sensitive")
		}
		for _, source := range []string{SessionPathExplicit, SessionPathRelayHome} {
			session := filepath.Join(caseVariantRoot, "sessions", source)
			_, err := Preflight(context.Background(), Options{
				LaunchCWD:         root,
				SessionDir:        session,
				SessionPathSource: source,
				MinimumPolicy:     PolicyReadOnly,
			})
			requireDiagnosticCode(t, err, DiagnosticCodeSessionConflict)
			requirePathAbsent(t, session)
		}
	})

	t.Run("session above source", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "source")
		initCommittedRepoAt(t, root)
		_, err := Preflight(context.Background(), Options{LaunchCWD: root, SessionDir: base, MinimumPolicy: PolicyReadOnly})
		requireDiagnosticCode(t, err, DiagnosticCodeSessionConflict)
	})

	t.Run("launch directory absent from HEAD", func(t *testing.T) {
		root := newCommittedRepo(t)
		launch := filepath.Join(root, "untracked-directory")
		writeTestFile(t, filepath.Join(launch, "only-untracked.txt"), []byte("data"), 0o644)
		session := filepath.Join(t.TempDir(), "session")
		_, err := Preflight(context.Background(), Options{LaunchCWD: launch, SessionDir: session, MinimumPolicy: PolicyReadOnly})
		requireDiagnosticCode(t, err, DiagnosticCodeLaunchNotCommitted)
		requirePathAbsent(t, session)
	})
}

func mustPreflight(t *testing.T, options Options) *Snapshot {
	t.Helper()
	snapshot, err := Preflight(context.Background(), options)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	return snapshot
}

func newCommittedRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	initCommittedRepoAt(t, root)
	canonical, err := canonicalExistingDirectory(root)
	if err != nil {
		t.Fatalf("canonical repo root: %v", err)
	}
	return canonical
}

func initCommittedRepoAt(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create repo root: %v", err)
	}
	testGit(t, root, "init", "-q")
	testGit(t, root, "config", "user.name", "Workspace Test")
	testGit(t, root, "config", "user.email", "workspace@example.invalid")
	writeTestFile(t, filepath.Join(root, ".gitignore"), []byte("ignored.log\n"), 0o644)
	writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("committed\n"), 0o644)
	writeTestFile(t, filepath.Join(root, "sub", "dir", "nested.txt"), []byte("nested\n"), 0o644)
	testGit(t, root, "add", "--all")
	testGit(t, root, "commit", "-q", "-m", "initial")
}

func testGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for workspace tests")
	}
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	command.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func requireDiagnosticCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected diagnostic %s", code)
	}
	var diagnosticError *contracts.DiagnosticError
	if !errors.As(err, &diagnosticError) {
		t.Fatalf("error is not a DiagnosticError: %T %v", err, err)
	}
	if len(diagnosticError.Diagnostics) != 1 || diagnosticError.Diagnostics[0].Code != code {
		t.Fatalf("diagnostics = %#v, want code %s", diagnosticError.Diagnostics, code)
	}
}

func requirePathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("path %s exists or cannot be inspected: %v", path, err)
	}
}

func exclusionPaths(t *testing.T, exclusions map[string]any, category string) []string {
	t.Helper()
	items, ok := exclusions[category].([]any)
	if !ok {
		t.Fatalf("exclusion category %s = %#v", category, exclusions[category])
	}
	paths := make([]string, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("exclusion item = %#v", raw)
		}
		path, ok := item["path"].(string)
		if !ok {
			t.Fatalf("exclusion item has no UTF-8 path: %#v", item)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func inventoryEntries(t *testing.T, source map[string]any, category string) map[string]map[string]any {
	t.Helper()
	inventory, ok := source["inventory"].(map[string]any)
	if !ok {
		t.Fatalf("source inventory = %#v", source["inventory"])
	}
	items, ok := inventory[category].([]any)
	if !ok {
		t.Fatalf("inventory category %s = %#v", category, inventory[category])
	}
	result := map[string]map[string]any{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("inventory item = %#v", raw)
		}
		if path, ok := item["path"].(string); ok {
			result[path] = item
		}
	}
	return result
}

func inventoryEntryBytes(entries map[string]map[string]any) int64 {
	var total int64
	for _, entry := range entries {
		total += intFromWorkspaceAny(entry["size_bytes"])
	}
	return total
}

func newInitializedSubmoduleChain(t *testing.T, depth int) string {
	t.Helper()
	child := newCommittedRepo(t)
	for level := 0; level < depth; level++ {
		parent := newCommittedRepo(t)
		testGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", child, "nested")
		testGit(t, parent, "commit", "-q", "-m", "add nested submodule")
		child = parent
	}
	testGit(t, child, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	return child
}
