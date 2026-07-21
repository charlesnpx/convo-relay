package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestMaterializeRequiredPoliciesCreateVerifiedDetachedWorktreeAndArtifact(t *testing.T) {
	for _, policy := range []string{PolicyReadOnly, PolicyEphemeral} {
		t.Run(policy, func(t *testing.T) {
			root := newCommittedRepo(t)
			originalHead := testGit(t, root, "rev-parse", "HEAD")
			writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("staged\n"), 0o644)
			testGit(t, root, "add", "--", "committed.txt")
			writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("unstaged after staging\n"), 0o644)
			writeTestFile(t, filepath.Join(root, "untracked.txt"), []byte("excluded\n"), 0o644)

			sessionDir := filepath.Join(t.TempDir(), "session")
			launchCWD := filepath.Join(root, "sub", "dir")
			snapshot := mustPreflight(t, Options{
				LaunchCWD:         launchCWD,
				SessionDir:        sessionDir,
				SessionPathSource: SessionPathExplicit,
				MinimumPolicy:     policy,
			})
			if snapshot.HeadCommit() != originalHead {
				t.Fatalf("snapshot HEAD = %s, want %s", snapshot.HeadCommit(), originalHead)
			}

			st := store.New(sessionDir)
			materialized, err := Materialize(context.Background(), st, snapshot)
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			registerWorktreeCleanup(t, root, materialized.WorktreePath)

			wantWorktree := filepath.Join(snapshot.SessionDir(), "execution", "worktree")
			wantCWD := filepath.Join(wantWorktree, "sub", "dir")
			if materialized.WorktreePath != wantWorktree || materialized.ExecutionCWD != wantCWD {
				t.Fatalf("materialized paths = worktree %q cwd %q", materialized.WorktreePath, materialized.ExecutionCWD)
			}
			if got := testGit(t, materialized.WorktreePath, "rev-parse", "HEAD"); got != originalHead {
				t.Fatalf("worktree HEAD = %s, want %s", got, originalHead)
			}
			if got := testGit(t, materialized.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
				t.Fatalf("worktree branch = %s", got)
			}
			if got := testGit(t, materialized.WorktreePath, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
				t.Fatalf("worktree status = %q", got)
			}
			committed, err := os.ReadFile(filepath.Join(materialized.WorktreePath, "committed.txt"))
			if err != nil {
				t.Fatalf("read committed worktree file: %v", err)
			}
			if string(committed) != "committed\n" {
				t.Fatalf("worktree included dirty bytes: %q", committed)
			}
			requirePathAbsent(t, filepath.Join(materialized.WorktreePath, "untracked.txt"))

			registered, err := repositoryWorktreeRegistered(context.Background(), snapshot.repository, materialized.WorktreePath)
			if err != nil || !registered {
				t.Fatalf("registered = %v, err = %v", registered, err)
			}
			policyRecord := requireObjectField(t, materialized.Artifact, "policy")
			if policyRecord["minimum"] != policy || policyRecord["effective"] != policy || policyRecord["achieved"] != PolicyEphemeral {
				t.Fatalf("artifact policy = %#v", policyRecord)
			}
			registration := requireObjectField(t, materialized.Artifact, "registration")
			if registration["mode"] != "detached_worktree" || registration["registered"] != true || registration["detached"] != true || registration["writable"] != true {
				t.Fatalf("artifact registration = %#v", registration)
			}
			identity := requireObjectField(t, materialized.Artifact, "identity")
			if identity["source_git_root"] != snapshot.GitRoot() || identity["launch_subpath"] != "sub/dir" || identity["execution_cwd"] != wantCWD {
				t.Fatalf("artifact identity = %#v", identity)
			}
			if materialized.Artifact["source_before_digest"] != snapshot.SourceDigest() {
				t.Fatalf("source_before_digest = %#v", materialized.Artifact["source_before_digest"])
			}
			if got := exclusionPaths(t, requireObjectField(t, materialized.Artifact, "exclusions"), "staged"); len(got) != 1 || got[0] != "committed.txt" {
				t.Fatalf("staged exclusions = %#v", got)
			}
			if got := exclusionPaths(t, requireObjectField(t, materialized.Artifact, "exclusions"), "unstaged"); len(got) != 1 || got[0] != "committed.txt" {
				t.Fatalf("unstaged exclusions = %#v", got)
			}
			if got := exclusionPaths(t, requireObjectField(t, materialized.Artifact, "exclusions"), "untracked"); len(got) != 1 || got[0] != "untracked.txt" {
				t.Fatalf("untracked exclusions = %#v", got)
			}

			persisted, err := st.LoadArtifactPayloadRaw(materialized.ArtifactRef)
			if err != nil {
				t.Fatalf("load workspace artifact: %v", err)
			}
			if _, err := contracts.ValidateRootArtifactRef(materialized.ArtifactRef, contracts.RootArtifactKindExecutionWorkspace, 0, persisted); err != nil {
				t.Fatalf("validate workspace artifact ref: %v", err)
			}
			if err := verifyWorkspaceIdentity(persisted); err != nil {
				t.Fatalf("verify workspace identity: %v", err)
			}
			if materialized.ArtifactRef["id"] != "execution_workspace:selected" {
				t.Fatalf("artifact ref = %#v", materialized.ArtifactRef)
			}

			// Returned maps are snapshots rather than aliases of store state.
			materialized.Artifact["source_before_digest"] = "changed"
			reloaded, err := st.LoadArtifactPayloadRaw(materialized.ArtifactRef)
			if err != nil || reloaded["source_before_digest"] != snapshot.SourceDigest() {
				t.Fatalf("persisted artifact changed through returned map: %#v, %v", reloaded, err)
			}
		})
	}
}

func TestMaterializeUsesRecordedHeadEvenWhenSourceBranchAdvances(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	recordedHead := snapshot.HeadCommit()

	writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("new branch head\n"), 0o644)
	testGit(t, root, "add", "--", "committed.txt")
	testGit(t, root, "commit", "-q", "-m", "advance source")
	if current := testGit(t, root, "rev-parse", "HEAD"); current == recordedHead {
		t.Fatal("source branch did not advance")
	}

	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)
	if got := testGit(t, materialized.WorktreePath, "rev-parse", "HEAD"); got != recordedHead {
		t.Fatalf("worktree HEAD = %s, want recorded %s", got, recordedHead)
	}
	content, err := os.ReadFile(filepath.Join(materialized.WorktreePath, "committed.txt"))
	if err != nil || string(content) != "committed\n" {
		t.Fatalf("worktree content = %q, err = %v", content, err)
	}
}

func TestMaterializeInheritedPersistsOrdinaryWorkspaceWithoutWorktree(t *testing.T) {
	root := newCommittedRepo(t)
	writeTestFile(t, filepath.Join(root, "committed.txt"), []byte("ordinary dirty bytes\n"), 0o644)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir})

	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if materialized.ExecutionCWD != snapshot.LaunchCWD() || materialized.WorktreePath != "" {
		t.Fatalf("inherited materialized paths = %#v", materialized)
	}
	requirePathAbsent(t, filepath.Join(sessionDir, "execution"))
	policy := requireObjectField(t, materialized.Artifact, "policy")
	if policy["effective"] != PolicyInherited || policy["achieved"] != PolicyInherited {
		t.Fatalf("inherited artifact policy = %#v", policy)
	}
	registration := requireObjectField(t, materialized.Artifact, "registration")
	if registration["mode"] != PolicyInherited || registration["registered"] != false || registration["writable"] != false {
		t.Fatalf("inherited registration = %#v", registration)
	}
	if err := verifyWorkspaceIdentity(materialized.Artifact); err != nil {
		t.Fatalf("verify inherited identity: %v", err)
	}
}

func TestMaterializeInheritedAllowsNonGitWorkspace(t *testing.T) {
	launch := t.TempDir()
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: launch, SessionDir: sessionDir})
	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if materialized.ExecutionCWD != snapshot.LaunchCWD() || materialized.Artifact["source_before_digest"] != nil {
		t.Fatalf("non-Git materialization = %#v", materialized)
	}
	base := requireObjectField(t, materialized.Artifact, "base")
	if base["repository_state"] != "non_git" {
		t.Fatalf("non-Git base = %#v", base)
	}
}

func TestMaterializeCreationFailureDoesNotDowngradeOrReplaceExistingPath(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyReadOnly})
	target := filepath.Join(sessionDir, "execution", "worktree")
	writeTestFile(t, filepath.Join(target, "preserve.txt"), []byte("preserve"), 0o644)

	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if materialized != nil {
		t.Fatalf("creation failure returned materialized workspace: %#v", materialized)
	}
	requireDiagnosticCode(t, err, DiagnosticCodeCreationFailed)
	content, readErr := os.ReadFile(filepath.Join(target, "preserve.txt"))
	if readErr != nil || string(content) != "preserve" {
		t.Fatalf("existing target changed: %q, %v", content, readErr)
	}
	registered, inspectErr := repositoryWorktreeRegistered(context.Background(), snapshot.repository, target)
	if inspectErr != nil || registered {
		t.Fatalf("failed target registration = %v, %v", registered, inspectErr)
	}
	requirePathAbsent(t, filepath.Join(sessionDir, "artifacts"))
}

func TestMaterializeRollsBackWorktreeWhenArtifactPersistenceFails(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("create session: %v", err)
	}
	writeTestFile(t, filepath.Join(sessionDir, "artifacts"), []byte("blocks artifact directory"), 0o644)
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	target := filepath.Join(sessionDir, "execution", "worktree")

	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if materialized != nil {
		t.Fatalf("persistence failure returned materialized workspace: %#v", materialized)
	}
	requireDiagnosticCode(t, err, DiagnosticCodeIntegrity)
	requirePathAbsent(t, target)
	registered, inspectErr := repositoryWorktreeRegistered(context.Background(), snapshot.repository, target)
	if inspectErr != nil || registered {
		t.Fatalf("rolled back registration = %v, %v", registered, inspectErr)
	}
}

func TestMaterializeCompensatesArtifactStateWhenFinalGraphWriteFails(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	graphPath := filepath.Join(sessionDir, store.GraphFilename)
	if err := os.MkdirAll(graphPath, 0o755); err != nil {
		t.Fatalf("create graph write blocker: %v", err)
	}
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	target := filepath.Join(sessionDir, "execution", "worktree")

	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if materialized != nil {
		t.Fatalf("late persistence failure returned materialized workspace: %#v", materialized)
	}
	requireDiagnosticCode(t, err, DiagnosticCodeIntegrity)
	requirePathAbsent(t, target)
	requirePathAbsent(t, filepath.Join(sessionDir, "artifacts", executionWorkspaceCategory, "selected.json"))
	requirePathAbsent(t, filepath.Join(sessionDir, "artifacts", store.ArtifactIndexFilename))
	if info, statErr := os.Stat(graphPath); statErr != nil || !info.IsDir() {
		t.Fatalf("graph write blocker changed: %v, %v", info, statErr)
	}
	registered, inspectErr := repositoryWorktreeRegistered(context.Background(), snapshot.repository, target)
	if inspectErr != nil || registered {
		t.Fatalf("rolled back registration = %v, %v", registered, inspectErr)
	}
}

func TestWorktreeRegistrationMatchesCaseVariantPath(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "case-registration-session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyEphemeral})
	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)

	caseVariantPath := strings.ToUpper(materialized.WorktreePath)
	originalInfo, originalErr := os.Stat(materialized.WorktreePath)
	variantInfo, variantErr := os.Stat(caseVariantPath)
	if caseVariantPath == materialized.WorktreePath || originalErr != nil || variantErr != nil || !os.SameFile(originalInfo, variantInfo) {
		t.Skip("filesystem is case-sensitive")
	}
	registered, inspectErr := repositoryWorktreeRegistered(context.Background(), snapshot.repository, caseVariantPath)
	if inspectErr != nil || !registered {
		t.Fatalf("case-variant registration = %v, %v", registered, inspectErr)
	}
}

func TestMaterializeRejectsStoreThatDiffersFromPreflight(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session-one")
	otherSession := filepath.Join(t.TempDir(), "session-two")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir, MinimumPolicy: PolicyReadOnly})
	materialized, err := Materialize(context.Background(), store.New(otherSession), snapshot)
	if materialized != nil {
		t.Fatalf("mismatched store returned materialized workspace: %#v", materialized)
	}
	requireDiagnosticCode(t, err, DiagnosticCodeSessionConflict)
	requirePathAbsent(t, sessionDir)
	requirePathAbsent(t, otherSession)
}

func TestWorkspaceIdentityDigestCoversContractDigestExcludedPaths(t *testing.T) {
	root := newCommittedRepo(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{LaunchCWD: root, SessionDir: sessionDir})
	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	tampered := cloneMap(materialized.Artifact)
	requireObjectField(t, tampered, "identity")["session_dir"] = filepath.Join(t.TempDir(), "different-session")
	originalDigest, err := contracts.ContractDigest(materialized.Artifact)
	if err != nil {
		t.Fatalf("original contract digest: %v", err)
	}
	tamperedDigest, err := contracts.ContractDigest(tampered)
	if err != nil {
		t.Fatalf("tampered contract digest: %v", err)
	}
	if originalDigest != tamperedDigest {
		t.Fatalf("test path is no longer excluded by ContractDigest: %s != %s", originalDigest, tamperedDigest)
	}
	if err := verifyWorkspaceIdentity(tampered); err == nil || !strings.Contains(err.Error(), "identity digest mismatch") {
		t.Fatalf("tampered identity error = %v", err)
	}
}

func requireObjectField(t *testing.T, object map[string]any, field string) map[string]any {
	t.Helper()
	value, ok := object[field].(map[string]any)
	if !ok {
		t.Fatalf("field %s = %#v", field, object[field])
	}
	return value
}

func registerWorktreeCleanup(t *testing.T, root string, worktreePath string) {
	t.Helper()
	t.Cleanup(func() {
		command := exec.Command("git", "-C", root, "worktree", "remove", "--force", worktreePath)
		command.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
		_ = command.Run()
	})
}
