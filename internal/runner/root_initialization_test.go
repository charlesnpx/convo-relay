package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
		if _, err := workspace.Materialize(context.Background(), transaction.st, preflight.workspace); err != nil {
			t.Fatalf("materialize interrupted worktree: %v", err)
		}

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
		if _, err := workspace.Materialize(context.Background(), transaction.st, preflight.workspace); err != nil {
			t.Fatalf("materialize interrupted worktree: %v", err)
		}
		foreignPath := filepath.Join(sessionDir, "foreign.txt")
		if err := os.WriteFile(foreignPath, []byte("foreign"), 0o644); err != nil {
			t.Fatalf("write foreign entry: %v", err)
		}
		if report, err := CleanSession(sessionDir); err == nil || report != nil || !strings.Contains(err.Error(), "foreign") {
			t.Fatalf("foreign-entry clean = %#v, %v", report, err)
		}
		if _, err := os.Stat(foreignPath); err != nil {
			t.Fatalf("foreign entry was removed: %v", err)
		}
		_ = os.Remove(foreignPath)
		meta, _ := transaction.st.LoadMeta()
		transaction.token = meta.String("initialization_token")
		if err := transaction.compensate(); err != nil {
			t.Fatalf("cleanup after removing foreign entry: %v", err)
		}
	})
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
			if _, err := workspace.Materialize(context.Background(), transaction.st, preflight.workspace); err != nil {
				t.Fatalf("materialize interrupted worktree: %v", err)
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
