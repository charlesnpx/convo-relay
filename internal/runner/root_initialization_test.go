package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

func TestRootInitializationFailuresCompensateEveryPreReadyStage(t *testing.T) {
	stages := []struct {
		name       string
		namedInput bool
	}{
		{name: "session_claimed"},
		{name: "session_repository_initialized"},
		{name: "workspace_materialized"},
		{name: "inputs_materialized", namedInput: true},
		{name: "retained_inputs_verified"},
		{name: "checkpoint_1_persisted"},
		{name: "transcript_persisted"},
		{name: "start_event_persisted"},
		{name: "graph_persisted"},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			launchRoot := newRootInitializationRepository(t)
			sessionDir := filepath.Join(t.TempDir(), "session")
			injected := errors.New("injected initialization interruption")
			rootInitializationAfterStage = func(observed string) error {
				if observed == stage.name {
					return injected
				}
				return nil
			}
			defer func() { rootInitializationAfterStage = nil }()

			options := rootInitializationOptions(launchRoot, sessionDir)
			if stage.namedInput {
				inputPath := filepath.Join(launchRoot, "payload.json")
				if err := os.WriteFile(inputPath, []byte(`{"value":"stable"}`), 0o644); err != nil {
					t.Fatalf("write named input: %v", err)
				}
				rootInitializationGit(t, launchRoot, "add", "--", "payload.json")
				rootInitializationGit(t, launchRoot, "commit", "-m", "add input")
				options.IntegrationBundle = decodeRootRecipeTestBundle(t, rootRecipeTestBundle)
				options.InputBindings = []string{"payload=payload.json"}
				options.RuntimeConfig = rootRecipeRuntimeConfig("neutral/contract-v1")
			}
			_, err := RunRecipe(context.Background(), options)
			if !errors.Is(err, injected) {
				t.Fatalf("initialization error = %v, want injected failure", err)
			}
			if _, err := os.Lstat(sessionDir); !os.IsNotExist(err) {
				t.Fatalf("compensated session remained at stage %s: %v", stage.name, err)
			}
			if output := rootInitializationGit(t, launchRoot, "worktree", "list", "--porcelain"); strings.Contains(output, sessionDir) {
				t.Fatalf("worktree registration remained at stage %s:\n%s", stage.name, output)
			}
		})
	}
}

func TestRootInitializationReadyMetadataDisarmsCompensation(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	injected := errors.New("interrupt immediately after ready metadata")
	rootInitializationAfterStage = func(stage string) error {
		if stage == "ready_metadata_persisted" {
			return injected
		}
		return nil
	}
	defer func() { rootInitializationAfterStage = nil }()

	_, err := RunRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
	if !errors.Is(err, injected) {
		t.Fatalf("ready-boundary error = %v", err)
	}
	meta, loadErr := store.New(sessionDir).LoadMeta()
	if loadErr != nil {
		t.Fatalf("load ready metadata: %v", loadErr)
	}
	if meta.String("status") != RootInitializationStateReady ||
		meta.String("initialization_state") != RootInitializationStateReady {
		t.Fatalf("ready metadata = %#v", meta.ToMap())
	}
	worktreePath := filepath.Join(sessionDir, "execution", "worktree")
	if info, statErr := os.Stat(worktreePath); statErr != nil || !info.IsDir() {
		t.Fatalf("ready worktree was compensated: %v", statErr)
	}
	rootInitializationAfterStage = nil
	if report, cleanErr := CleanSession(sessionDir); cleanErr != nil || report["status"] != "deleted" {
		t.Fatalf("clean ready interrupted session = %#v, %v", report, cleanErr)
	}
}

func TestEnsureGitRepoInitializesTheExactSessionRootInsideAnAncestorRepository(t *testing.T) {
	ancestor := newRootInitializationRepository(t)
	sessionDir := filepath.Join(ancestor, "nested", "session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("create nested session directory: %v", err)
	}
	if pathHasGitRepo(sessionDir) {
		t.Fatal("nested directory was mistaken for its ancestor Git root")
	}
	if !pathWithinGitRepo(sessionDir) {
		t.Fatal("nested directory was not recognized as residing in a Git repository")
	}
	if err := ensureGitRepo(sessionDir); err != nil {
		t.Fatalf("initialize exact session repository: %v", err)
	}
	canonicalSession, err := filepath.EvalSymlinks(sessionDir)
	if err != nil {
		t.Fatalf("canonical session directory: %v", err)
	}
	if got := rootInitializationGit(t, sessionDir, "rev-parse", "--show-toplevel"); got != canonicalSession {
		t.Fatalf("session Git root = %q, want %q", got, canonicalSession)
	}
	if !pathHasGitRepo(sessionDir) {
		t.Fatal("initialized session directory was not recognized as its own Git root")
	}
}

func TestRootInitializationVerifiesRetainedInputsBeforeCheckpointOne(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	inputPath := filepath.Join(launchRoot, "payload.json")
	if err := os.WriteFile(inputPath, []byte(`{"value":"stable"}`), 0o644); err != nil {
		t.Fatalf("write named input: %v", err)
	}
	rootInitializationGit(t, launchRoot, "add", "--", "payload.json")
	rootInitializationGit(t, launchRoot, "commit", "-m", "add input")
	sessionDir := filepath.Join(t.TempDir(), "session")
	rootInitializationAfterStage = func(stage string) error {
		if stage == "inputs_materialized" {
			target := filepath.Join(sessionDir, "execution", "inputs", "000001")
			if err := os.Chmod(target, 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	defer func() { rootInitializationAfterStage = nil }()
	options := rootInitializationOptions(launchRoot, sessionDir)
	options.IntegrationBundle = decodeRootRecipeTestBundle(t, rootRecipeTestBundle)
	options.InputBindings = []string{"payload=payload.json"}
	options.RuntimeConfig = rootRecipeRuntimeConfig("neutral/contract-v1")
	recorder := &rootBackendRecorder{}
	options.backendFactory = recorder.factory()

	_, err := RunRecipe(context.Background(), options)
	if err == nil {
		t.Fatal("tampered retained input unexpectedly reached checkpoint 1")
	}
	if len(recorder.snapshotCalls()) != 0 {
		t.Fatalf("provider constructed or called after initialization mismatch: %#v", recorder.snapshotCalls())
	}
	recorder.mu.Lock()
	constructed := recorder.nextID
	recorder.mu.Unlock()
	if constructed != 0 {
		t.Fatalf("providers constructed after initialization mismatch: %d", constructed)
	}
	if _, statErr := os.Lstat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("initialization mismatch was not compensated: %v", statErr)
	}
}

func TestCleanCompletesStaleInitializingTransactionAndRejectsForeignEntries(t *testing.T) {
	t.Run("stale initialization", func(t *testing.T) {
		launchRoot := newRootInitializationRepository(t)
		sessionDir := filepath.Join(t.TempDir(), "session")
		preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		transaction, err := beginRootInitialization(preflight)
		if err != nil {
			t.Fatalf("begin initialization: %v", err)
		}
		if _, err := transaction.materializeWorkspace(context.Background(), preflight.workspace); err != nil {
			t.Fatalf("materialize interrupted worktree: %v", err)
		}
		if err := transaction.advance("workspace_materialized"); err != nil {
			t.Fatalf("record materialized ownership: %v", err)
		}
		abandonRootInitializationLease(t, transaction)

		report, err := CleanSession(sessionDir)
		if err != nil || report["status"] != "deleted" {
			t.Fatalf("clean stale initialization = %#v, %v", report, err)
		}
		if _, err := os.Lstat(sessionDir); !os.IsNotExist(err) {
			t.Fatalf("stale initialization root remained: %v", err)
		}
	})

	t.Run("foreign entry", func(t *testing.T) {
		launchRoot := newRootInitializationRepository(t)
		sessionDir := filepath.Join(t.TempDir(), "session")
		if err := os.MkdirAll(sessionDir, 0o755); err != nil {
			t.Fatalf("create explicit empty session root: %v", err)
		}
		preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		transaction, err := beginRootInitialization(preflight)
		if err != nil {
			t.Fatalf("begin initialization: %v", err)
		}
		if _, err := transaction.materializeWorkspace(context.Background(), preflight.workspace); err != nil {
			t.Fatalf("materialize interrupted worktree: %v", err)
		}
		if err := transaction.advance("workspace_materialized"); err != nil {
			t.Fatalf("record materialized ownership: %v", err)
		}
		foreignPath := filepath.Join(sessionDir, "foreign.txt")
		if err := os.WriteFile(foreignPath, []byte("foreign"), 0o644); err != nil {
			t.Fatalf("write foreign entry: %v", err)
		}
		abandonRootInitializationLease(t, transaction)
		if report, err := CleanSession(sessionDir); err == nil || report != nil || !strings.Contains(err.Error(), "foreign") {
			t.Fatalf("foreign-entry clean = %#v, %v", report, err)
		}
		if _, err := os.Stat(foreignPath); err != nil {
			t.Fatalf("foreign entry was removed: %v", err)
		}
		_ = os.Remove(foreignPath)
		if report, err := CleanSession(sessionDir); err != nil || report["status"] != "deleted" {
			t.Fatalf("cleanup after removing foreign entry = %#v, %v", report, err)
		}
	})
}

func TestCleanWaitsForLiveRootInitializerMutationLease(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	transaction, err := beginRootInitialization(preflight)
	if err != nil {
		t.Fatalf("begin initialization: %v", err)
	}

	type cleanResult struct {
		report map[string]any
		err    error
	}
	result := make(chan cleanResult, 1)
	go func() {
		report, err := CleanSession(sessionDir)
		result <- cleanResult{report: report, err: err}
	}()
	select {
	case observed := <-result:
		t.Fatalf("clean crossed a live initializer lease: %#v, %v", observed.report, observed.err)
	case <-time.After(150 * time.Millisecond):
	}

	// Releasing the process-owned lease simulates process death. The waiting
	// cleaner may then load the durable journal and complete compensation.
	abandonRootInitializationLease(t, transaction)
	select {
	case observed := <-result:
		if observed.err != nil || observed.report["status"] != "deleted" {
			t.Fatalf("stale clean after lease release = %#v, %v", observed.report, observed.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clean did not resume after the initializer lease was released")
	}
}

func TestMutationLockWaiterRejectsUnlinkedOldInode(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("create session directory: %v", err)
	}
	first, err := lockSessionMutation(sessionDir)
	if err != nil {
		t.Fatalf("acquire first mutation lock: %v", err)
	}
	defer func() {
		sessionMutationLockAfterOpen = nil
		_ = first.Unlock()
	}()

	opened := make(chan struct{})
	sessionMutationLockAfterOpen = func(string) {
		sessionMutationLockAfterOpen = nil
		close(opened)
	}
	type lockResult struct {
		lock *sessionMutationLock
		err  error
	}
	waiter := make(chan lockResult, 1)
	go func() {
		lock, err := lockSessionMutation(sessionDir)
		waiter <- lockResult{lock: lock, err: err}
	}()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("waiting lock did not open the original lock inode")
	}

	if err := os.Remove(first.path); err != nil {
		t.Fatalf("unlink original mutation lock path: %v", err)
	}
	replacement, err := lockSessionMutation(sessionDir)
	if err != nil {
		t.Fatalf("acquire replacement mutation lock: %v", err)
	}
	defer func() { _ = replacement.Unlock() }()
	if err := first.Unlock(); err != nil {
		t.Fatalf("release original mutation lock: %v", err)
	}
	select {
	case observed := <-waiter:
		if observed.lock != nil {
			_ = observed.lock.Unlock()
		}
		if observed.err == nil || !strings.Contains(observed.err.Error(), "changed while waiting") {
			t.Fatalf("old-inode waiter = %#v, %v", observed.lock, observed.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old-inode waiter did not return after original lock release")
	}
}

func TestInitializationPendingFileIntentRecoversCommitBeforeCompletionRecord(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	transaction, err := beginRootInitialization(preflight)
	if err != nil {
		t.Fatalf("begin initialization: %v", err)
	}

	body := []byte("durable write before phase advance\n")
	target := filepath.Join(transaction.sessionRoot, "artifacts", "interrupted", "record.json")
	plan, err := transaction.BeforeFileMutation(target, body, 0o600)
	if err != nil {
		t.Fatalf("persist file mutation intent: %v", err)
	}
	if plan.TemporaryPath == "" {
		t.Fatal("file mutation intent omitted its exact temporary path")
	}
	// Simulate process death after the target rename but before Store can call
	// AfterFileMutation or the runner can advance its phase.
	commitPendingInitializationWrite(t, plan, target, body, 0o600)
	abandonRootInitializationLease(t, transaction)

	report, err := CleanSession(sessionDir)
	if err != nil || report["status"] != "deleted" {
		t.Fatalf("recover pending committed write = %#v, %v", report, err)
	}
}

func TestInitializationPendingFileIntentRejectsSameContentReplacement(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	transaction, err := beginRootInitialization(preflight)
	if err != nil {
		t.Fatalf("begin initialization: %v", err)
	}

	body := []byte("same bytes, different inode\n")
	target := filepath.Join(transaction.sessionRoot, "artifacts", "interrupted", "record.json")
	plan, err := transaction.BeforeFileMutation(target, body, 0o600)
	if err != nil {
		t.Fatalf("persist file mutation intent: %v", err)
	}
	commitPendingInitializationWrite(t, plan, target, body, 0o600)
	committedInfo, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("stat committed pending target: %v", err)
	}
	if err := store.AtomicWriteFile(target, body); err != nil {
		t.Fatalf("replace pending target with identical content: %v", err)
	}
	replacementInfo, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("stat same-content replacement: %v", err)
	}
	if rootInitializationFileIdentity(target, committedInfo) == rootInitializationFileIdentity(target, replacementInfo) {
		t.Fatal("same-content replacement retained the committed temporary identity")
	}
	abandonRootInitializationLease(t, transaction)

	if report, err := CleanSession(sessionDir); err == nil || report != nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("same-content pending replacement clean = %#v, %v", report, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != string(body) {
		t.Fatalf("same-content pending replacement changed: %q, %v", data, err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove same-content pending replacement: %v", err)
	}
	if report, err := CleanSession(sessionDir); err != nil || report["status"] != "deleted" {
		t.Fatalf("cleanup after pending replacement removal = %#v, %v", report, err)
	}
}

func TestInitializationPendingWorkspaceScopeRecoversVerifiedMaterialization(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	transaction, err := beginRootInitialization(preflight)
	if err != nil {
		t.Fatalf("begin initialization: %v", err)
	}
	if err := transaction.beginOwnedScope("workspace_materialization", "execution"); err != nil {
		t.Fatalf("persist workspace operation intent: %v", err)
	}
	if _, err := workspace.Materialize(context.Background(), transaction.st, preflight.workspace); err != nil {
		t.Fatalf("materialize workspace before simulated process death: %v", err)
	}
	// Do not call completeOwnedScope or advance: this is the precise crash
	// window after the direct mutation returns and before phase persistence.
	abandonRootInitializationLease(t, transaction)

	report, err := CleanSession(sessionDir)
	if err != nil || report["status"] != "deleted" {
		t.Fatalf("recover verified pending workspace = %#v, %v", report, err)
	}
	worktreePath := filepath.Join(sessionDir, "execution", "worktree")
	if output := rootInitializationGit(t, launchRoot, "worktree", "list", "--porcelain"); strings.Contains(output, worktreePath) {
		t.Fatalf("pending workspace recovery retained registration:\n%s", output)
	}
}

func TestInitializationPendingWorkspaceScopeRejectsForeignSiblingBeforeOwnershipCapture(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	transaction, err := beginRootInitialization(preflight)
	if err != nil {
		t.Fatalf("begin initialization: %v", err)
	}
	if err := transaction.beginOwnedScope("workspace_materialization", "execution"); err != nil {
		t.Fatalf("persist workspace operation intent: %v", err)
	}
	if _, err := workspace.Materialize(context.Background(), transaction.st, preflight.workspace); err != nil {
		t.Fatalf("materialize workspace before simulated process death: %v", err)
	}
	foreignPath := filepath.Join(sessionDir, "execution", "foreign.db")
	if err := os.WriteFile(foreignPath, []byte("preserve foreign sibling\n"), 0o644); err != nil {
		t.Fatalf("write foreign execution sibling: %v", err)
	}
	abandonRootInitializationLease(t, transaction)

	if report, err := CleanSession(sessionDir); err == nil || report != nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("pending workspace foreign-sibling clean = %#v, %v", report, err)
	}
	if data, err := os.ReadFile(foreignPath); err != nil || string(data) != "preserve foreign sibling\n" {
		t.Fatalf("foreign execution sibling changed: %q, %v", data, err)
	}
	worktreePath := filepath.Join(sessionDir, "execution", "worktree")
	if info, err := os.Stat(worktreePath); err != nil || !info.IsDir() {
		t.Fatalf("foreign-sibling rejection removed worktree: %v", err)
	}
	if output := rootInitializationGit(t, launchRoot, "worktree", "list", "--porcelain"); !strings.Contains(output, worktreePath) {
		t.Fatalf("foreign-sibling rejection removed worktree registration:\n%s", output)
	}

	if err := os.Remove(foreignPath); err != nil {
		t.Fatalf("remove foreign execution sibling: %v", err)
	}
	if report, err := CleanSession(sessionDir); err != nil || report["status"] != "deleted" {
		t.Fatalf("cleanup after foreign execution sibling removal = %#v, %v", report, err)
	}
}

func TestInitialMetadataWriteFailureReleasesClaimJournalAndLease(t *testing.T) {
	for _, preExisting := range []bool{false, true} {
		name := "new root"
		if preExisting {
			name = "pre-existing root"
		}
		t.Run(name, func(t *testing.T) {
			launchRoot := newRootInitializationRepository(t)
			sessionDir := filepath.Join(t.TempDir(), "session")
			if preExisting {
				if err := os.MkdirAll(sessionDir, 0o755); err != nil {
					t.Fatalf("create pre-existing root: %v", err)
				}
			}
			preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
			if err != nil {
				t.Fatalf("preflight: %v", err)
			}
			injected := errors.New("reject initial metadata commit")
			rootInitializationBeforeFileCommit = func(relative string) error {
				if relative == "meta.json" {
					return injected
				}
				return nil
			}
			defer func() { rootInitializationBeforeFileCommit = nil }()
			transaction, err := beginRootInitialization(preflight)
			if transaction != nil || !errors.Is(err, injected) {
				t.Fatalf("initial metadata failure = %#v, %v", transaction, err)
			}
			rootInitializationBeforeFileCommit = nil

			if preExisting {
				entries, err := os.ReadDir(sessionDir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("pre-existing root after bootstrap cleanup = %#v, %v", entries, err)
				}
			} else if _, err := os.Lstat(sessionDir); !os.IsNotExist(err) {
				t.Fatalf("new root remained after bootstrap cleanup: %v", err)
			}

			retryPreflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
			if err != nil {
				t.Fatalf("retry preflight: %v", err)
			}
			retry, err := beginRootInitialization(retryPreflight)
			if err != nil {
				t.Fatalf("retry initialization after metadata failure: %v", err)
			}
			if err := retry.compensate(); err != nil {
				t.Fatalf("compensate retry initialization: %v", err)
			}
		})
	}
}

func TestInitializationOwnershipRejectsSamePathRegularFileReplacement(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	transaction, err := beginRootInitialization(preflight)
	if err != nil {
		t.Fatalf("begin initialization: %v", err)
	}
	target := filepath.Join(transaction.sessionRoot, "transcript.json")
	body := []byte("owned transcript bytes\n")
	if err := transaction.st.WriteFileAtomically(target, body, 0o600); err != nil {
		t.Fatalf("write transaction-owned file: %v", err)
	}
	before, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("stat owned file: %v", err)
	}
	if err := store.AtomicWriteFile(target, body); err != nil {
		t.Fatalf("replace owned file at the same lexical path: %v", err)
	}
	after, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("stat replacement file: %v", err)
	}
	if rootInitializationFileIdentity(target, before) == rootInitializationFileIdentity(target, after) {
		t.Fatal("test replacement retained the original file identity")
	}
	abandonRootInitializationLease(t, transaction)

	if report, err := CleanSession(sessionDir); err == nil || report != nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("same-path replacement clean = %#v, %v", report, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != string(body) {
		t.Fatalf("same-path replacement was changed: %q, %v", data, err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove replacement after preservation proof: %v", err)
	}
	if report, err := CleanSession(sessionDir); err != nil || report["status"] != "deleted" {
		t.Fatalf("cleanup after replacement removal = %#v, %v", report, err)
	}
}

func TestInitializationOwnedScopeRejectsReplacementBeforeCapture(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	transaction, err := beginRootInitialization(preflight)
	if err != nil {
		t.Fatalf("begin initialization: %v", err)
	}
	if err := transaction.beginOwnedScope(
		"retained_input_materialization",
		filepath.Join("execution", "inputs"),
	); err != nil {
		t.Fatalf("begin retained-input ownership scope: %v", err)
	}
	target := filepath.Join(transaction.sessionRoot, "execution", "inputs", "000001")
	body := []byte("transaction-owned input\n")
	if err := transaction.st.WriteFileAtomically(target, body, 0o444); err != nil {
		t.Fatalf("write transaction-owned input: %v", err)
	}
	before, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("stat transaction-owned input: %v", err)
	}
	if err := store.AtomicWriteFile(target, body); err != nil {
		t.Fatalf("replace transaction-owned input: %v", err)
	}
	after, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("stat replacement input: %v", err)
	}
	if rootInitializationFileIdentity(target, before) == rootInitializationFileIdentity(target, after) {
		t.Fatal("test replacement retained the original file identity")
	}
	if err := transaction.completeOwnedScope(); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("scope completion accepted replacement: %v", err)
	}
	abandonRootInitializationLease(t, transaction)

	if report, err := CleanSession(sessionDir); err == nil || report != nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("pre-capture replacement clean = %#v, %v", report, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != string(body) {
		t.Fatalf("pre-capture replacement changed: %q, %v", data, err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove preserved replacement: %v", err)
	}
	if report, err := CleanSession(sessionDir); err != nil || report["status"] != "deleted" {
		t.Fatalf("cleanup after pre-capture replacement removal = %#v, %v", report, err)
	}
}

func TestInitializationCleanupRetriesAfterInterruptionFollowingWorktreeRemoval(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	transaction, err := beginRootInitialization(preflight)
	if err != nil {
		t.Fatalf("begin initialization: %v", err)
	}
	if _, err := transaction.materializeWorkspace(context.Background(), preflight.workspace); err != nil {
		t.Fatalf("materialize interrupted worktree: %v", err)
	}
	if err := transaction.advance("workspace_materialized"); err != nil {
		t.Fatalf("record materialized workspace: %v", err)
	}
	injected := errors.New("interrupt after exact worktree removal")
	rootInitializationAfterWorktreeRemoval = func() error { return injected }
	defer func() { rootInitializationAfterWorktreeRemoval = nil }()
	if err := transaction.compensate(); !errors.Is(err, injected) {
		t.Fatalf("first compensation error = %v, want injected interruption", err)
	}
	rootInitializationAfterWorktreeRemoval = nil

	worktreePath := filepath.Join(sessionDir, "execution", "worktree")
	if _, err := os.Lstat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("interrupted compensation retained worktree path: %v", err)
	}
	if output := rootInitializationGit(t, launchRoot, "worktree", "list", "--porcelain"); strings.Contains(output, worktreePath) {
		t.Fatalf("interrupted compensation retained worktree registration:\n%s", output)
	}
	report, err := CleanSession(sessionDir)
	if err != nil || report["status"] != "deleted" {
		t.Fatalf("retry compensation after worktree removal = %#v, %v", report, err)
	}
}

func TestCleanRejectsNestedForeignInitializationEntriesBeforeWorktreeRemoval(t *testing.T) {
	tests := []struct {
		name         string
		relativePath string
	}{
		{name: "inside execution directory", relativePath: filepath.Join("execution", "foreign.db")},
		{name: "inside registered worktree", relativePath: filepath.Join("execution", "worktree", "foreign.db")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			launchRoot := newRootInitializationRepository(t)
			sessionDir := filepath.Join(t.TempDir(), "session")
			if err := os.MkdirAll(sessionDir, 0o755); err != nil {
				t.Fatalf("create explicit empty session root: %v", err)
			}
			preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
			if err != nil {
				t.Fatalf("preflight: %v", err)
			}
			transaction, err := beginRootInitialization(preflight)
			if err != nil {
				t.Fatalf("begin initialization: %v", err)
			}
			if _, err := transaction.materializeWorkspace(context.Background(), preflight.workspace); err != nil {
				t.Fatalf("materialize interrupted worktree: %v", err)
			}
			if err := transaction.advance("workspace_materialized"); err != nil {
				t.Fatalf("record materialized ownership: %v", err)
			}
			foreignPath := filepath.Join(sessionDir, test.relativePath)
			if err := os.WriteFile(foreignPath, []byte("foreign"), 0o644); err != nil {
				t.Fatalf("write nested foreign entry: %v", err)
			}
			abandonRootInitializationLease(t, transaction)

			if report, err := CleanSession(sessionDir); err == nil || report != nil || !strings.Contains(err.Error(), "foreign") {
				t.Fatalf("nested-foreign clean = %#v, %v", report, err)
			}
			if _, err := os.Stat(foreignPath); err != nil {
				t.Fatalf("nested foreign entry was removed: %v", err)
			}
			worktreePath := filepath.Join(sessionDir, "execution", "worktree")
			if info, err := os.Stat(worktreePath); err != nil || !info.IsDir() {
				t.Fatalf("foreign-entry rejection removed worktree: %v", err)
			}
			if output := rootInitializationGit(t, launchRoot, "worktree", "list", "--porcelain"); !strings.Contains(output, worktreePath) {
				t.Fatalf("foreign-entry rejection removed worktree registration:\n%s", output)
			}

			if err := os.Remove(foreignPath); err != nil {
				t.Fatalf("remove nested foreign entry: %v", err)
			}
			if report, err := CleanSession(sessionDir); err != nil || report["status"] != "deleted" {
				t.Fatalf("cleanup after removing nested foreign entry = %#v, %v", report, err)
			}
		})
	}
}

func TestRootInitializationDestinationClaimIsExclusive(t *testing.T) {
	for _, preExisting := range []bool{false, true} {
		name := "new destination"
		if preExisting {
			name = "pre-existing empty destination"
		}
		t.Run(name, func(t *testing.T) {
			launchRoot := newRootInitializationRepository(t)
			sessionDir := filepath.Join(t.TempDir(), "session")
			if preExisting {
				if err := os.MkdirAll(sessionDir, 0o755); err != nil {
					t.Fatalf("create explicit empty session root: %v", err)
				}
			}
			first, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
			if err != nil {
				t.Fatalf("first preflight: %v", err)
			}
			second, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
			if err != nil {
				t.Fatalf("second preflight: %v", err)
			}
			type result struct {
				transaction *rootInitializationTransaction
				err         error
			}
			start := make(chan struct{})
			results := make(chan result, 2)
			for _, candidate := range []*recipePreflight{first, second} {
				candidate := candidate
				go func() {
					<-start
					transaction, err := beginRootInitialization(candidate)
					results <- result{transaction: transaction, err: err}
				}()
			}
			close(start)
			observed := []result{<-results, <-results}
			var winner *rootInitializationTransaction
			failures := 0
			for _, item := range observed {
				if item.err != nil {
					failures++
					continue
				}
				if winner != nil {
					t.Fatal("two initializers acquired the same session destination")
				}
				winner = item.transaction
			}
			if winner == nil || failures != 1 {
				t.Fatalf("exclusive initialization results = %#v", observed)
			}
			if err := validateRootInitializationClaim(winner.sessionRoot, winner.token); err != nil {
				t.Fatalf("winner claim is not durable: %v", err)
			}
			if err := winner.compensate(); err != nil {
				t.Fatalf("compensate winning initialization: %v", err)
			}
			if preExisting {
				entries, err := os.ReadDir(sessionDir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("pre-existing destination cleanup = %#v, %v", entries, err)
				}
			} else if _, err := os.Lstat(sessionDir); !os.IsNotExist(err) {
				t.Fatalf("new destination remained after compensation: %v", err)
			}
		})
	}
}

func TestCleanRejectsTamperedInitializationJournalIdentities(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value func(sessionDir string, outsidePath string) any
	}{
		{
			name:  "transaction token",
			field: "transaction_token",
			value: func(string, string) any { return strings.Repeat("0", 64) },
		},
		{
			name:  "session identity",
			field: "canonical_session_root",
			value: func(sessionDir string, _ string) any { return filepath.Join(sessionDir, "other-session") },
		},
		{
			name:  "worktree path",
			field: "expected_worktree_path",
			value: func(_ string, outsidePath string) any { return outsidePath },
		},
		{
			name:  "source repository",
			field: "source_git_root",
			value: func(_ string, outsidePath string) any { return outsidePath },
		},
		{
			name:  "captured commit",
			field: "captured_commit",
			value: func(string, string) any { return strings.Repeat("1", 40) },
		},
		{
			name:  "captured tree",
			field: "captured_tree",
			value: func(string, string) any { return strings.Repeat("2", 40) },
		},
		{
			name:  "object format",
			field: "object_format",
			value: func(string, string) any { return "sha256" },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			launchRoot := newRootInitializationRepository(t)
			sessionDir := filepath.Join(t.TempDir(), "session")
			preflight, err := preflightRecipe(context.Background(), rootInitializationOptions(launchRoot, sessionDir))
			if err != nil {
				t.Fatalf("preflight: %v", err)
			}
			transaction, err := beginRootInitialization(preflight)
			if err != nil {
				t.Fatalf("begin initialization: %v", err)
			}
			if _, err := transaction.materializeWorkspace(context.Background(), preflight.workspace); err != nil {
				t.Fatalf("materialize interrupted worktree: %v", err)
			}
			if err := transaction.advance("workspace_materialized"); err != nil {
				t.Fatalf("record materialized ownership: %v", err)
			}

			outsidePath := filepath.Join(t.TempDir(), "outside-target")
			if err := os.MkdirAll(outsidePath, 0o755); err != nil {
				t.Fatalf("create outside target: %v", err)
			}
			sentinelPath := filepath.Join(outsidePath, "sentinel.txt")
			if err := os.WriteFile(sentinelPath, []byte("foreign"), 0o644); err != nil {
				t.Fatalf("write outside sentinel: %v", err)
			}
			journalPath := filepath.Join(sessionDir, rootInitializationJournalName)
			original := mutateRootInitializationJournal(t, journalPath, func(payload map[string]any) {
				payload[test.field] = test.value(sessionDir, outsidePath)
			})
			abandonRootInitializationLease(t, transaction)

			if report, err := CleanSession(sessionDir); err == nil || report != nil {
				t.Fatalf("tampered %s cleanup = %#v, %v", test.field, report, err)
			}
			worktreePath := filepath.Join(sessionDir, "execution", "worktree")
			if info, err := os.Stat(worktreePath); err != nil || !info.IsDir() {
				t.Fatalf("tampered journal removed registered worktree: %v", err)
			}
			if _, err := os.Stat(sentinelPath); err != nil {
				t.Fatalf("tampered journal targeted outside content: %v", err)
			}
			if output := rootInitializationGit(t, launchRoot, "worktree", "list", "--porcelain"); !strings.Contains(output, worktreePath) {
				t.Fatalf("tampered journal removed worktree registration:\n%s", output)
			}

			if err := store.AtomicWriteFile(journalPath, original); err != nil {
				t.Fatalf("restore initialization journal: %v", err)
			}
			if report, err := CleanSession(sessionDir); err != nil || report["status"] != "deleted" {
				t.Fatalf("clean restored initialization = %#v, %v", report, err)
			}
		})
	}
}

func TestDirtySourceWarningPrecedesClaimAndProvenanceSkipsFacilitator(t *testing.T) {
	launchRoot := newRootInitializationRepository(t)
	if err := os.WriteFile(filepath.Join(launchRoot, "committed.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("dirty source: %v", err)
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	config := rootRecipeRuntimeConfig("")
	config.RelayRecipes["neutral-root"]["result_source"] = "reducer"
	recorder := &rootBackendRecorder{sessionStateExtra: map[string]any{"thread_id": "test-thread"}}
	var warnings []RecipeWarning
	callbackObservedSession := false
	options := rootInitializationOptions(launchRoot, sessionDir)
	options.RuntimeConfig = config
	options.WarningCallback = func(warning RecipeWarning) {
		warnings = append(warnings, warning)
		_, err := os.Stat(sessionDir)
		callbackObservedSession = err == nil
	}
	options.AllowDirtySource = true
	options.backendFactory = recorder.factory()

	result, err := RunRecipe(context.Background(), options)
	if err != nil || result["status"] != "completed" {
		t.Fatalf("dirty override run = %#v, %v", result, err)
	}
	if len(warnings) != 1 || callbackObservedSession ||
		warnings[0].Code != RecipeWarningCodeDirtySourceCommittedHead ||
		warnings[0].UnstagedChanges != 1 ||
		warnings[0].WorkspaceContentSource != workspace.WorkspaceContentSourceCommittedHead ||
		warnings[0].WorkingTreeChangesIncluded {
		t.Fatalf("dirty warning = %#v, observed_session=%v", warnings, callbackObservedSession)
	}
	meta, err := store.New(sessionDir).LoadMeta()
	if err != nil {
		t.Fatalf("load dirty override metadata: %v", err)
	}
	if meta.Get(workspace.AllowDirtySourceRequestedKey) != true ||
		intFromAny(meta.Get(workspace.SourceUnstagedChangesKey), 0) != 1 ||
		meta.String(workspace.WorkspaceContentSourceKey) != workspace.WorkspaceContentSourceCommittedHead {
		t.Fatalf("dirty source metadata projection = %#v", meta.ToMap())
	}
	for _, call := range recorder.snapshotCalls() {
		hasProvenance := strings.Contains(call.Prompt, "--- Execution Workspace Provenance ---")
		if call.SlotID == "facilitator" && hasProvenance {
			t.Fatalf("facilitator prompt received provenance:\n%s", call.Prompt)
		}
		if call.SlotID != "facilitator" && !hasProvenance {
			t.Fatalf("%s prompt omitted provenance:\n%s", call.SlotID, call.Prompt)
		}
	}
	if report, err := CleanSession(sessionDir); err != nil || report["status"] != "deleted" {
		t.Fatalf("clean dirty override session = %#v, %v", report, err)
	}

	blockedSession := filepath.Join(t.TempDir(), "blocked-session")
	if err := os.MkdirAll(blockedSession, 0o755); err != nil {
		t.Fatalf("create blocked destination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blockedSession, "foreign"), []byte("foreign"), 0o644); err != nil {
		t.Fatalf("populate blocked destination: %v", err)
	}
	callbackCount := 0
	blockedOptions := rootInitializationOptions(launchRoot, blockedSession)
	blockedOptions.AllowDirtySource = true
	blockedOptions.WarningCallback = func(RecipeWarning) { callbackCount++ }
	if _, err := RunRecipe(context.Background(), blockedOptions); err == nil {
		t.Fatal("nonempty destination unexpectedly passed pure preflight")
	}
	if callbackCount != 0 {
		t.Fatalf("warning callback ran before pure preflight completed: %d", callbackCount)
	}
}

func rootInitializationOptions(launchRoot string, sessionDir string) RecipeOptions {
	return RecipeOptions{
		SessionDir:         sessionDir,
		Task:               "Exercise transactional root initialization",
		RecipeID:           "neutral-root",
		LaunchCWD:          launchRoot,
		WorkspaceIsolation: workspace.PolicyEphemeral,
		WorkspaceExplicit:  true,
		RuntimeConfig:      rootRecipeRuntimeConfig(""),
		ReadinessCheck:     readyRootRecipeCheck,
		backendFactory:     successfulRootBackendFactory(),
	}
}

func abandonRootInitializationLease(t *testing.T, transaction *rootInitializationTransaction) {
	t.Helper()
	if transaction == nil {
		t.Fatal("root initialization transaction is required")
	}
	if err := transaction.releaseMutationLock(); err != nil {
		t.Fatalf("release root initialization mutation lease: %v", err)
	}
}

func commitPendingInitializationWrite(
	t *testing.T,
	plan store.FileMutationPlan,
	target string,
	body []byte,
	mode os.FileMode,
) {
	t.Helper()
	handle, err := os.OpenFile(plan.TemporaryPath, os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		t.Fatalf("open pending temporary file: %v", err)
	}
	if err := handle.Chmod(mode.Perm()); err != nil {
		_ = handle.Close()
		t.Fatalf("chmod pending temporary file: %v", err)
	}
	if _, err := handle.Write(body); err != nil {
		_ = handle.Close()
		t.Fatalf("write pending temporary file: %v", err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		t.Fatalf("sync pending temporary file: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close pending temporary file: %v", err)
	}
	if err := os.Rename(plan.TemporaryPath, target); err != nil {
		t.Fatalf("commit pending target write: %v", err)
	}
}

func newRootInitializationRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	rootInitializationGit(t, root, "init")
	rootInitializationGit(t, root, "config", "user.name", "Relay Test")
	rootInitializationGit(t, root, "config", "user.email", "relay@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "committed.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatalf("write committed file: %v", err)
	}
	rootInitializationGit(t, root, "add", "--", "committed.txt")
	rootInitializationGit(t, root, "commit", "-m", "initial")
	return root
}

func rootInitializationGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func mutateRootInitializationJournal(
	t *testing.T,
	path string,
	mutate func(map[string]any),
) []byte {
	t.Helper()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read initialization journal: %v", err)
	}
	payload, err := contracts.DecodeJSONObjectBytes(original)
	if err != nil {
		t.Fatalf("decode initialization journal: %v", err)
	}
	mutate(payload)
	updated, err := contracts.CanonicalJSONBytes(payload)
	if err != nil {
		t.Fatalf("encode initialization journal: %v", err)
	}
	if err := store.AtomicWriteFile(path, updated); err != nil {
		t.Fatalf("write initialization journal: %v", err)
	}
	return original
}
