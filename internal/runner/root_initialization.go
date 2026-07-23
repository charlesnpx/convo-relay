package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const rootInitializationJournalName = "initialization.json"

var rootInitializationAfterStage func(string) error

var rootInitializationOwnedEntries = []string{
	".git",
	".mutation.lock",
	"artifacts",
	"events.jsonl",
	"execution",
	"graph.json",
	rootInitializationJournalName,
	"meta.json",
	"transcript.json",
}

type rootInitializationTransaction struct {
	token           string
	sessionRoot     string
	preExistingRoot bool
	sourceGitRoot   string
	worktreePath    string
	headCommit      string
	headTree        string
	objectFormat    string
	phase           string
	cleanupState    string
	st              *store.Store
}

func beginRootInitialization(preflight *recipePreflight) (*rootInitializationTransaction, error) {
	if preflight == nil || preflight.workspace == nil {
		return nil, errors.New("root initialization requires workspace preflight")
	}
	sessionRoot := filepath.Clean(preflight.sessionDir)
	info, statErr := os.Lstat(sessionRoot)
	preExisting := statErr == nil
	switch {
	case os.IsNotExist(statErr):
		if err := os.MkdirAll(sessionRoot, 0o755); err != nil {
			return nil, err
		}
	case statErr != nil:
		return nil, statErr
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return nil, rootRecipeDiagnostic(diagnosticCodeSessionPathInvalid, contracts.DiagnosticPhasePolicy, "/session_dir", "The root initialization destination must be a real directory.", nil)
	default:
		entries, err := os.ReadDir(sessionRoot)
		if err != nil {
			return nil, err
		}
		if len(entries) != 0 {
			return nil, rootRecipeDiagnostic(diagnosticCodeSessionPathInvalid, contracts.DiagnosticPhasePolicy, "/session_dir", "The root initialization destination must remain empty until it is claimed.", nil)
		}
	}
	canonicalRoot, err := filepath.EvalSymlinks(sessionRoot)
	if err != nil {
		if !preExisting {
			_ = os.Remove(sessionRoot)
		}
		return nil, err
	}
	canonicalRoot, err = filepath.Abs(canonicalRoot)
	if err != nil || filepath.Clean(canonicalRoot) != sessionRoot {
		if !preExisting {
			_ = os.Remove(sessionRoot)
		}
		return nil, rootRecipeDiagnostic(
			diagnosticCodeSessionPathInvalid,
			contracts.DiagnosticPhasePolicy,
			"/session_dir",
			"The root initialization destination changed after preflight.",
			map[string]any{"expected": sessionRoot, "observed": canonicalRoot},
		)
	}
	token, err := rootInitializationToken()
	if err != nil {
		if !preExisting {
			_ = os.Remove(sessionRoot)
		}
		return nil, err
	}
	worktreePath := ""
	if preflight.workspace.Policy().Effective != workspace.PolicyInherited {
		worktreePath = filepath.Join(sessionRoot, "execution", "worktree")
	}
	transaction := &rootInitializationTransaction{
		token:           token,
		sessionRoot:     sessionRoot,
		preExistingRoot: preExisting,
		sourceGitRoot:   preflight.workspace.GitRoot(),
		worktreePath:    worktreePath,
		headCommit:      preflight.workspace.HeadCommit(),
		headTree:        preflight.workspace.HeadTree(),
		objectFormat:    preflight.workspace.ObjectFormat(),
		phase:           "claimed",
		cleanupState:    "armed",
		st:              store.New(sessionRoot),
	}
	if err := transaction.writeJournal(); err != nil {
		if !preExisting {
			_ = os.Remove(sessionRoot)
		}
		return nil, err
	}
	initializingMeta := map[string]any{
		"session_id":                preflight.sessionID,
		"execution_kind":            "recipe",
		"task":                      preflight.options.Task,
		"title":                     makeTitle(preflight.options.Task),
		"status":                    RootInitializationStateInitializing,
		"initialization_state":      RootInitializationStateInitializing,
		"initialization_token":      token,
		"initialization_phase":      transaction.phase,
		"canonical_session_root":    sessionRoot,
		"expected_worktree_path":    emptyStringAsNil(worktreePath),
		"source_git_root":           emptyStringAsNil(transaction.sourceGitRoot),
		"captured_commit":           emptyStringAsNil(transaction.headCommit),
		"captured_tree":             emptyStringAsNil(transaction.headTree),
		"object_format":             emptyStringAsNil(transaction.objectFormat),
		"pre_existing_session_root": preExisting,
		"created_at":                utcNow(),
	}
	if err := transaction.st.SaveMetaMap(initializingMeta); err != nil {
		return nil, errors.Join(err, transaction.compensateWithoutMeta())
	}
	if err := transaction.advance("session_claimed"); err != nil {
		return nil, transaction.Fail(err)
	}
	if err := ensureGitRepo(sessionRoot); err != nil {
		return nil, transaction.Fail(err)
	}
	if err := transaction.advance("session_repository_initialized"); err != nil {
		return nil, transaction.Fail(err)
	}
	return transaction, nil
}

func (t *rootInitializationTransaction) advance(phase string) error {
	if t == nil {
		return errors.New("root initialization transaction is required")
	}
	t.phase = strings.TrimSpace(phase)
	if err := t.writeJournal(); err != nil {
		return err
	}
	meta, err := t.st.LoadMeta()
	if err != nil {
		return err
	}
	if meta.String("initialization_token") != t.token || meta.String("status") != RootInitializationStateInitializing {
		return errors.New("root initialization metadata no longer matches its transaction")
	}
	if err := t.st.SaveMeta(meta.With("initialization_phase", t.phase)); err != nil {
		return err
	}
	if rootInitializationAfterStage != nil {
		return rootInitializationAfterStage(t.phase)
	}
	return nil
}

func (t *rootInitializationTransaction) markReady(meta map[string]any) error {
	if t == nil {
		return errors.New("root initialization transaction is required")
	}
	ready := make(map[string]any, len(meta)+4)
	for key, value := range meta {
		ready[key] = value
	}
	ready["status"] = RootInitializationStateReady
	ready["initialization_state"] = RootInitializationStateReady
	ready["initialization_token"] = t.token
	ready["initialization_phase"] = "ready"
	if strings.TrimSpace(stringFromAny(ready["initialization_ready_at"])) == "" {
		ready["initialization_ready_at"] = utcNow()
	}
	if err := t.st.SaveMetaMap(ready); err != nil {
		return err
	}
	if rootInitializationAfterStage != nil {
		if err := rootInitializationAfterStage("ready_metadata_persisted"); err != nil {
			return err
		}
	}
	t.phase = "ready"
	t.cleanupState = "disarmed"
	_ = t.writeJournal()
	return nil
}

func (t *rootInitializationTransaction) Fail(primary error) error {
	if t == nil {
		return primary
	}
	cleanupErr := t.compensate()
	return errors.Join(primary, cleanupErr)
}

func (t *rootInitializationTransaction) compensate() error {
	meta, err := t.st.LoadMeta()
	if err != nil {
		return fmt.Errorf("read initialization metadata before compensation: %w", err)
	}
	status := meta.String("status")
	if meta.String("initialization_token") != t.token ||
		(status != RootInitializationStateInitializing && status != RootInitializationStateFailed) {
		if status == RootInitializationStateReady && meta.String("initialization_token") == t.token {
			return nil
		}
		return errors.New("initialization compensation refused metadata with a mismatched state or transaction token")
	}
	failed := meta.
		WithStatus(RootInitializationStateFailed).
		With("initialization_state", RootInitializationStateFailed).
		With("initialization_phase", t.phase).
		With("initialization_failed_at", utcNow())
	if err := t.st.SaveMeta(failed); err != nil {
		return fmt.Errorf("persist initialization failure metadata: %w", err)
	}
	t.cleanupState = "running"
	if err := t.writeJournal(); err != nil {
		return err
	}
	cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := workspace.CleanupInitializationWorktree(cleanupContext, t.sourceGitRoot, t.worktreePath, t.headCommit); err != nil {
		t.cleanupState = "failed"
		_ = t.writeJournal()
		return fmt.Errorf("compensate initialization worktree: %w", err)
	}
	if foreign, err := t.foreignEntries(); err != nil {
		return err
	} else if len(foreign) > 0 {
		t.cleanupState = "blocked_foreign_entries"
		_ = t.writeJournal()
		return fmt.Errorf("initialization compensation refused foreign session entries: %s", strings.Join(foreign, ", "))
	}
	t.cleanupState = "complete"
	_ = t.writeJournal()
	if t.preExistingRoot {
		for _, name := range rootInitializationOwnedEntries {
			path := filepath.Join(t.sessionRoot, name)
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove initialization-owned entry %s: %w", name, err)
			}
		}
		return nil
	}
	if err := os.RemoveAll(t.sessionRoot); err != nil {
		return fmt.Errorf("remove initialization-owned session root: %w", err)
	}
	return nil
}

func (t *rootInitializationTransaction) compensateWithoutMeta() error {
	if t == nil {
		return nil
	}
	// The journal exists, but cleanup is deliberately refused when matching
	// initializing metadata was never durably established.
	t.cleanupState = "blocked_missing_metadata"
	return t.writeJournal()
}

func (t *rootInitializationTransaction) foreignEntries() ([]string, error) {
	entries, err := os.ReadDir(t.sessionRoot)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, name := range rootInitializationOwnedEntries {
		allowed[name] = true
	}
	foreign := make([]string, 0)
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			foreign = append(foreign, entry.Name())
		}
	}
	sort.Strings(foreign)
	return foreign, nil
}

func (t *rootInitializationTransaction) writeJournal() error {
	payload := map[string]any{
		"schema_version":            1,
		"transaction_token":         t.token,
		"canonical_session_root":    t.sessionRoot,
		"pre_existing_session_root": t.preExistingRoot,
		"source_git_root":           emptyStringAsNil(t.sourceGitRoot),
		"expected_worktree_path":    emptyStringAsNil(t.worktreePath),
		"captured_commit":           emptyStringAsNil(t.headCommit),
		"captured_tree":             emptyStringAsNil(t.headTree),
		"object_format":             emptyStringAsNil(t.objectFormat),
		"initialization_phase":      t.phase,
		"cleanup_state":             t.cleanupState,
		"owned_top_level_entries":   append([]string(nil), rootInitializationOwnedEntries...),
		"updated_at":                utcNow(),
	}
	data, err := contracts.CanonicalJSONBytes(payload)
	if err != nil {
		return err
	}
	return store.AtomicWriteFile(filepath.Join(t.sessionRoot, rootInitializationJournalName), data)
}

func rootInitializationToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func loadRootInitializationTransaction(
	sessionDir string,
	meta map[string]any,
) (*rootInitializationTransaction, error) {
	data, err := os.ReadFile(filepath.Join(sessionDir, rootInitializationJournalName))
	if err != nil {
		return nil, fmt.Errorf("read root initialization journal: %w", err)
	}
	payload, err := contracts.DecodeJSONObjectBytes(data)
	if err != nil {
		return nil, fmt.Errorf("decode root initialization journal: %w", err)
	}
	if intFromAny(payload["schema_version"], 0) != 1 {
		return nil, errors.New("root initialization journal schema version is invalid")
	}
	token := strings.TrimSpace(stringFromAny(payload["transaction_token"]))
	if token == "" || token != strings.TrimSpace(stringFromAny(meta["initialization_token"])) {
		return nil, errors.New("root initialization journal token does not match session metadata")
	}
	absoluteRoot, err := filepath.Abs(filepath.Clean(sessionDir))
	if err != nil {
		return nil, err
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, err
	}
	canonicalRoot = filepath.Clean(canonicalRoot)
	recordedRoot := cleanInitializationPath(payload["canonical_session_root"])
	metaRoot := cleanInitializationPath(meta["canonical_session_root"])
	if recordedRoot != canonicalRoot || metaRoot != canonicalRoot {
		return nil, errors.New("root initialization journal session identity is invalid")
	}
	sourceGitRoot := cleanInitializationPath(payload["source_git_root"])
	metaSourceGitRoot := cleanInitializationPath(meta["source_git_root"])
	if sourceGitRoot != metaSourceGitRoot || (sourceGitRoot != "" && !filepath.IsAbs(sourceGitRoot)) {
		return nil, errors.New("root initialization journal source repository identity is invalid")
	}
	worktreePath := cleanInitializationPath(payload["expected_worktree_path"])
	metaWorktreePath := cleanInitializationPath(meta["expected_worktree_path"])
	if worktreePath != metaWorktreePath {
		return nil, errors.New("root initialization journal worktree identity does not match session metadata")
	}
	expectedWorktree := filepath.Join(canonicalRoot, "execution", "worktree")
	if worktreePath != "" && worktreePath != expectedWorktree {
		return nil, errors.New("root initialization journal worktree path is not the fixed session worktree")
	}
	if worktreePath != "" && sourceGitRoot == "" {
		return nil, errors.New("root initialization journal worktree is missing its source repository")
	}
	preExisting, ok := payload["pre_existing_session_root"].(bool)
	if !ok {
		return nil, errors.New("root initialization journal ownership is invalid")
	}
	metaPreExisting, metaPreExistingOK := meta["pre_existing_session_root"].(bool)
	if !metaPreExistingOK || metaPreExisting != preExisting {
		return nil, errors.New("root initialization journal ownership does not match session metadata")
	}
	for _, field := range []string{"captured_commit", "captured_tree", "object_format"} {
		if strings.TrimSpace(stringFromAny(payload[field])) != strings.TrimSpace(stringFromAny(meta[field])) {
			return nil, fmt.Errorf("root initialization journal captured identity field %s does not match session metadata", field)
		}
	}
	if err := validateRootInitializationOwnedEntries(payload["owned_top_level_entries"]); err != nil {
		return nil, err
	}
	transaction := &rootInitializationTransaction{
		token:           token,
		sessionRoot:     canonicalRoot,
		preExistingRoot: preExisting,
		sourceGitRoot:   sourceGitRoot,
		worktreePath:    worktreePath,
		headCommit:      strings.TrimSpace(stringFromAny(payload["captured_commit"])),
		headTree:        strings.TrimSpace(stringFromAny(payload["captured_tree"])),
		objectFormat:    strings.TrimSpace(stringFromAny(payload["object_format"])),
		phase:           strings.TrimSpace(stringFromAny(payload["initialization_phase"])),
		cleanupState:    strings.TrimSpace(stringFromAny(payload["cleanup_state"])),
		st:              store.New(canonicalRoot),
	}
	if transaction.phase == "" || transaction.cleanupState == "" {
		return nil, errors.New("root initialization journal lifecycle fields are invalid")
	}
	return transaction, nil
}

func cleanInitializationPath(value any) string {
	raw := strings.TrimSpace(stringFromAny(value))
	if raw == "" {
		return ""
	}
	return filepath.Clean(raw)
}

func validateRootInitializationOwnedEntries(value any) error {
	raw, ok := value.([]any)
	if !ok || len(raw) != len(rootInitializationOwnedEntries) {
		return errors.New("root initialization journal owned-entry identity is invalid")
	}
	for index, expected := range rootInitializationOwnedEntries {
		actual, ok := raw[index].(string)
		if !ok || actual != expected {
			return errors.New("root initialization journal owned-entry identity is invalid")
		}
	}
	return nil
}
