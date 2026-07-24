package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const (
	rootInitializationJournalName = "initialization.json"
	rootInitializationClaimName   = "initialization.claim"
)

var rootInitializationAfterStage func(string) error
var rootInitializationAfterMutation func(string) error
var rootInitializationAfterWorktreeRemoval func() error
var rootInitializationBeforeFileCommit func(string) error

var rootInitializationOwnedEntries = []string{
	".git",
	".mutation.lock",
	"artifacts",
	"events.jsonl",
	"execution",
	"graph.json",
	rootInitializationClaimName,
	rootInitializationJournalName,
	"meta.json",
	"transcript.json",
}

type rootInitializationTransaction struct {
	token             string
	sessionRoot       string
	preExistingRoot   bool
	sourceGitRoot     string
	worktreePath      string
	headCommit        string
	headTree          string
	objectFormat      string
	phase             string
	cleanupState      string
	ownedDigest       string
	claimIdentity     string
	ownedEntries      map[string]rootInitializationOwnedEntry
	pendingWrite      *rootInitializationWriteIntent
	pendingScope      *rootInitializationScopeIntent
	nextWriteSequence uint64
	mutationLock      *sessionMutationLock
	st                *store.Store
}

func beginRootInitialization(preflight *recipePreflight) (*rootInitializationTransaction, error) {
	if preflight == nil || preflight.workspace == nil {
		return nil, errors.New("root initialization requires workspace preflight")
	}
	sessionRoot, err := initializationLexicalPath(preflight.sessionDir)
	if err != nil {
		return nil, err
	}
	token, err := rootInitializationToken()
	if err != nil {
		return nil, err
	}
	preExisting, err := claimRootInitializationDestination(sessionRoot, token)
	if err != nil {
		return nil, err
	}
	mutationLock, err := lockSessionMutation(sessionRoot)
	if err != nil {
		_ = removeRootInitializationClaim(sessionRoot, token)
		if !preExisting {
			_ = os.Remove(sessionRoot)
		}
		return nil, err
	}
	releaseLeaseOnError := true
	var transaction *rootInitializationTransaction
	defer func() {
		if releaseLeaseOnError {
			if transaction != nil {
				_ = transaction.removeBootstrapControls()
				return
			}
			_ = removeRootInitializationClaim(sessionRoot, token)
			_ = mutationLock.Unlock()
			_ = mutationLock.removePathAfterUnlock()
			if !preExisting {
				_ = os.Remove(sessionRoot)
			}
		}
	}()
	if err := rejectInitializationSymlinkComponents(sessionRoot); err != nil {
		return nil, rootRecipeDiagnostic(
			diagnosticCodeSessionPathInvalid,
			contracts.DiagnosticPhasePolicy,
			"/session_dir",
			"The root initialization destination changed after it was claimed.",
			map[string]any{"cause": err.Error()},
		)
	}
	info, statErr := os.Lstat(sessionRoot)
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, rootRecipeDiagnostic(diagnosticCodeSessionPathInvalid, contracts.DiagnosticPhasePolicy, "/session_dir", "The root initialization destination must be a real directory.", nil)
	}
	claimInfo, err := os.Lstat(filepath.Join(sessionRoot, rootInitializationClaimName))
	if err != nil || claimInfo.Mode()&os.ModeSymlink != 0 || !claimInfo.Mode().IsRegular() {
		return nil, errors.New("root initialization claim identity could not be established")
	}
	worktreePath := ""
	if preflight.workspace.Policy().Effective != workspace.PolicyInherited {
		worktreePath = filepath.Join(sessionRoot, "execution", "worktree")
	}
	transaction = &rootInitializationTransaction{
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
		claimIdentity: rootInitializationFileIdentity(
			filepath.Join(sessionRoot, rootInitializationClaimName),
			claimInfo,
		),
		ownedEntries: map[string]rootInitializationOwnedEntry{},
		mutationLock: mutationLock,
	}
	transaction.st = store.NewWithFileMutationObserver(sessionRoot, transaction)
	if err := transaction.refreshOwnershipDigest(); err != nil {
		return nil, err
	}
	if err := mutationLock.writeMarker(transaction.lockMarker("initializing")); err != nil {
		return nil, err
	}
	if err := transaction.writeJournal(); err != nil {
		return nil, err
	}
	initializingMeta := map[string]any{
		"session_id":                            preflight.sessionID,
		"execution_kind":                        "recipe",
		"task":                                  preflight.options.Task,
		"title":                                 makeTitle(preflight.options.Task),
		"status":                                RootInitializationStateInitializing,
		"initialization_state":                  RootInitializationStateInitializing,
		"initialization_token":                  token,
		"initialization_phase":                  transaction.phase,
		"canonical_session_root":                sessionRoot,
		"expected_worktree_path":                emptyStringAsNil(worktreePath),
		"source_git_root":                       emptyStringAsNil(transaction.sourceGitRoot),
		"captured_commit":                       emptyStringAsNil(transaction.headCommit),
		"captured_tree":                         emptyStringAsNil(transaction.headTree),
		"object_format":                         emptyStringAsNil(transaction.objectFormat),
		"initialization_claim":                  rootInitializationClaimName,
		"initialization_owned_inventory_digest": transaction.ownedDigest,
		"pre_existing_session_root":             preExisting,
		"created_at":                            utcNow(),
	}
	if err := transaction.st.SaveMetaMap(initializingMeta); err != nil {
		releaseLeaseOnError = false
		return nil, errors.Join(err, transaction.compensateBootstrap())
	}
	if err := runRootInitializationAfterMutation("initializing_metadata_persisted"); err != nil {
		releaseLeaseOnError = false
		return nil, transaction.Fail(err)
	}
	if err := transaction.advance("session_claimed"); err != nil {
		releaseLeaseOnError = false
		return nil, transaction.Fail(err)
	}
	if err := transaction.beginOwnedScope("session_repository_initialization", ".git"); err != nil {
		releaseLeaseOnError = false
		return nil, transaction.Fail(err)
	}
	if err := ensureGitRepo(sessionRoot); err != nil {
		abortErr := transaction.abortOwnedScope()
		releaseLeaseOnError = false
		return nil, transaction.Fail(errors.Join(err, abortErr))
	}
	if err := validateInitializationSessionRepository(sessionRoot); err != nil {
		releaseLeaseOnError = false
		return nil, transaction.Fail(err)
	}
	if err := transaction.completeOwnedScope(); err != nil {
		releaseLeaseOnError = false
		return nil, transaction.Fail(err)
	}
	if err := runRootInitializationAfterMutation("session_repository_initialized"); err != nil {
		releaseLeaseOnError = false
		return nil, transaction.Fail(err)
	}
	if err := transaction.advance("session_repository_initialized"); err != nil {
		releaseLeaseOnError = false
		return nil, transaction.Fail(err)
	}
	releaseLeaseOnError = false
	return transaction, nil
}

func (t *rootInitializationTransaction) advance(phase string) error {
	if t == nil {
		return errors.New("root initialization transaction is required")
	}
	t.phase = strings.TrimSpace(phase)
	if err := t.refreshOwnershipDigest(); err != nil {
		return err
	}
	meta, err := t.st.LoadMeta()
	if err != nil {
		return err
	}
	if meta.String("initialization_token") != t.token || meta.String("status") != RootInitializationStateInitializing {
		return errors.New("root initialization metadata no longer matches its transaction")
	}
	meta = meta.
		With("initialization_phase", t.phase).
		With("initialization_owned_inventory_digest", t.ownedDigest)
	if err := t.st.SaveMeta(meta); err != nil {
		return err
	}
	if err := t.writeJournal(); err != nil {
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
	t.phase = "ready"
	t.cleanupState = "disarmed"
	if rootInitializationAfterStage != nil {
		if err := rootInitializationAfterStage("ready_metadata_persisted"); err != nil {
			return err
		}
	}
	_ = t.writeJournal()
	if err := removeRootInitializationClaim(t.sessionRoot, t.token); err != nil {
		return err
	}
	t.st.SetFileMutationObserver(nil)
	return t.releaseMutationLock()
}

func (t *rootInitializationTransaction) Fail(primary error) error {
	if t == nil {
		return primary
	}
	cleanupErr := t.compensate()
	return errors.Join(primary, cleanupErr)
}

func (t *rootInitializationTransaction) compensate() error {
	defer func() {
		_ = t.releaseMutationLock()
	}()
	if err := t.resolvePendingWrite(); err != nil {
		return fmt.Errorf("resolve pending initialization write: %w", err)
	}
	if err := t.resolvePendingScope(); err != nil {
		return fmt.Errorf("resolve pending initialization mutation scope: %w", err)
	}
	meta, err := t.st.LoadMeta()
	if err != nil {
		return fmt.Errorf("read initialization metadata before compensation: %w", err)
	}
	status := meta.String("status")
	if meta.String("initialization_token") != t.token ||
		(status != RootInitializationStateInitializing && status != RootInitializationStateFailed) {
		if status == RootInitializationStateReady && meta.String("initialization_token") == t.token {
			t.st.SetFileMutationObserver(nil)
			return removeRootInitializationClaim(t.sessionRoot, t.token)
		}
		return errors.New("initialization compensation refused metadata with a mismatched state or transaction token")
	}
	if err := validateRootInitializationClaim(t.sessionRoot, t.token); err != nil {
		return fmt.Errorf("validate initialization claim before compensation: %w", err)
	}
	if err := t.validateClaimIdentity(); err != nil {
		return err
	}
	if err := t.validateOwnedInventory(); err != nil {
		t.cleanupState = "blocked_foreign_entries"
		_ = t.writeJournal()
		return err
	}
	failed := meta.
		WithStatus(RootInitializationStateFailed).
		With("initialization_state", RootInitializationStateFailed).
		With("initialization_phase", t.phase).
		With("initialization_owned_inventory_digest", t.ownedDigest).
		With("initialization_failed_at", utcNow())
	if err := t.st.SaveMeta(failed); err != nil {
		return fmt.Errorf("persist initialization failure metadata: %w", err)
	}
	t.cleanupState = "running"
	if err := t.writeJournal(); err != nil {
		return err
	}
	if err := t.validateOwnedInventory(); err != nil {
		t.cleanupState = "blocked_foreign_entries"
		_ = t.writeJournal()
		return err
	}
	cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := workspace.CleanupInitializationWorktree(cleanupContext, t.sourceGitRoot, t.worktreePath, t.headCommit); err != nil {
		t.cleanupState = "failed"
		_ = t.writeJournal()
		return fmt.Errorf("compensate initialization worktree: %w", err)
	}
	if rootInitializationAfterWorktreeRemoval != nil {
		if err := rootInitializationAfterWorktreeRemoval(); err != nil {
			return err
		}
	}
	if err := rejectInitializationSymlinkComponents(t.sessionRoot); err != nil {
		return fmt.Errorf("validate initialization root before removal: %w", err)
	}
	t.cleanupState = "worktree_removed"
	if err := t.writeJournal(); err != nil {
		return err
	}
	if err := t.removeOwnedEntriesExact(); err != nil {
		t.cleanupState = "failed"
		_ = t.writeJournal()
		return err
	}
	t.cleanupState = "entries_removed"
	if err := t.writeJournal(); err != nil {
		return err
	}
	return t.finalizeInitializationControls()
}

func (t *rootInitializationTransaction) compensateWithoutMeta() error {
	return t.compensateBootstrap()
}

func claimRootInitializationDestination(sessionRoot string, token string) (bool, error) {
	info, statErr := os.Lstat(sessionRoot)
	preExisting := statErr == nil
	switch {
	case os.IsNotExist(statErr):
		if err := os.MkdirAll(filepath.Dir(sessionRoot), 0o755); err != nil {
			return false, err
		}
		if err := os.Mkdir(sessionRoot, 0o755); err != nil {
			return false, rootRecipeDiagnostic(
				diagnosticCodeSessionPathInvalid,
				contracts.DiagnosticPhasePolicy,
				"/session_dir",
				"The root initialization destination could not be claimed exclusively.",
				map[string]any{"cause": err.Error()},
			)
		}
	case statErr != nil:
		return false, statErr
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return false, rootRecipeDiagnostic(diagnosticCodeSessionPathInvalid, contracts.DiagnosticPhasePolicy, "/session_dir", "The root initialization destination must be a real directory.", nil)
	default:
		entries, err := os.ReadDir(sessionRoot)
		if err != nil {
			return false, err
		}
		if len(entries) != 0 {
			return false, rootRecipeDiagnostic(diagnosticCodeSessionPathInvalid, contracts.DiagnosticPhasePolicy, "/session_dir", "The root initialization destination must remain empty until it is claimed.", nil)
		}
	}
	if err := rejectInitializationSymlinkComponents(sessionRoot); err != nil {
		if !preExisting {
			_ = os.Remove(sessionRoot)
		}
		return false, err
	}
	if err := createRootInitializationClaim(sessionRoot, token); err != nil {
		if !preExisting {
			_ = os.Remove(sessionRoot)
		}
		return false, rootRecipeDiagnostic(
			diagnosticCodeSessionPathInvalid,
			contracts.DiagnosticPhasePolicy,
			"/session_dir",
			"The root initialization destination could not be claimed exclusively.",
			map[string]any{"cause": err.Error()},
		)
	}
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		_ = removeRootInitializationClaim(sessionRoot, token)
		if !preExisting {
			_ = os.Remove(sessionRoot)
		}
		return false, err
	}
	if len(entries) != 1 || entries[0].Name() != rootInitializationClaimName {
		_ = removeRootInitializationClaim(sessionRoot, token)
		if !preExisting {
			_ = os.Remove(sessionRoot)
		}
		return false, rootRecipeDiagnostic(
			diagnosticCodeSessionPathInvalid,
			contracts.DiagnosticPhasePolicy,
			"/session_dir",
			"The root initialization destination changed while it was being claimed.",
			nil,
		)
	}
	return preExisting, nil
}

func createRootInitializationClaim(sessionRoot string, token string) (err error) {
	path := filepath.Join(sessionRoot, rootInitializationClaimName)
	payload, err := contracts.CanonicalJSONBytes(map[string]any{
		"schema_version":         1,
		"transaction_token":      token,
		"canonical_session_root": sessionRoot,
	})
	if err != nil {
		return err
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := handle.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = handle.Write(payload); err != nil {
		return err
	}
	return handle.Sync()
}

func validateRootInitializationClaim(sessionRoot string, token string) error {
	path := filepath.Join(sessionRoot, rootInitializationClaimName)
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return errors.New("root initialization claim must be a regular file")
	}
	data, err := contracts.ReadFileBytesLimited(path, 8*1024)
	if err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
		return errors.New("root initialization claim changed while it was read")
	}
	payload, err := contracts.DecodeJSONObjectBytes(data)
	if err != nil {
		return err
	}
	if intFromAny(payload["schema_version"], 0) != 1 ||
		strings.TrimSpace(stringFromAny(payload["transaction_token"])) != token ||
		cleanInitializationPath(payload["canonical_session_root"]) != sessionRoot {
		return errors.New("root initialization claim does not match its transaction")
	}
	return nil
}

func removeRootInitializationClaim(sessionRoot string, token string) error {
	path := filepath.Join(sessionRoot, rootInitializationClaimName)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := validateRootInitializationClaim(sessionRoot, token); err != nil {
		return err
	}
	return os.Remove(path)
}

func initializationLexicalPath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("root initialization path is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func rejectInitializationSymlinkComponents(value string) error {
	target, err := initializationLexicalPath(value)
	if err != nil {
		return err
	}
	root := filepath.VolumeName(target) + string(filepath.Separator)
	if filepath.VolumeName(target) == "" {
		root = string(filepath.Separator)
	}
	relative := strings.TrimPrefix(target, root)
	current := filepath.Clean(root)
	components := strings.Split(relative, string(filepath.Separator))
	for index, component := range components {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %s is a symlink", current)
		}
		if index < len(components)-1 && !info.IsDir() {
			return fmt.Errorf("path ancestor %s is not a directory", current)
		}
	}
	return nil
}

func (t *rootInitializationTransaction) writeJournal() error {
	if err := t.refreshOwnershipDigest(); err != nil {
		return err
	}
	payload := map[string]any{
		"schema_version":            rootInitializationJournalSchemaVersion,
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
		"initialization_claim":      rootInitializationClaimName,
		"initialization_claim_id":   t.claimIdentity,
		"owned_inventory_digest":    t.ownedDigest,
		"owned_top_level_entries":   append([]string(nil), rootInitializationOwnedEntries...),
		"owned_entries":             t.ownedEntriesPayload(),
		"next_write_sequence":       int64(t.nextWriteSequence),
		"pending_file_write":        nil,
		"pending_mutation_scope":    nil,
		"updated_at":                utcNow(),
	}
	if t.pendingWrite != nil {
		payload["pending_file_write"] = t.pendingWrite.toMap()
	}
	if t.pendingScope != nil {
		payload["pending_mutation_scope"] = t.pendingScope.toMap()
	}
	data, err := contracts.CanonicalJSONBytes(payload)
	if err != nil {
		return err
	}
	journalPath := filepath.Join(t.sessionRoot, rootInitializationJournalName)
	temporaryPath := filepath.Join(t.sessionRoot, "."+rootInitializationJournalName+"."+t.token+".tmp")
	if _, err := os.Lstat(temporaryPath); err == nil {
		if removeErr := os.Remove(temporaryPath); removeErr != nil {
			return removeErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	handle, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, err := handle.Write(data); err != nil {
		writeErr = err
	}
	if writeErr == nil {
		writeErr = handle.Sync()
	}
	closeErr := handle.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(temporaryPath)
		return writeErr
	}
	if err := os.Rename(temporaryPath, journalPath); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return nil
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
	mutationLock *sessionMutationLock,
) (*rootInitializationTransaction, error) {
	if mutationLock == nil || mutationLock.file == nil {
		return nil, errors.New("root initialization recovery requires the session mutation lease")
	}
	inputRoot, err := initializationLexicalPath(sessionDir)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(inputRoot); err != nil {
		return nil, err
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("root initialization session path must name a real directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(inputRoot)
	if err != nil {
		return nil, err
	}
	absoluteRoot, err := initializationLexicalPath(resolvedRoot)
	if err != nil {
		return nil, err
	}
	if err := rejectInitializationSymlinkComponents(absoluteRoot); err != nil {
		return nil, fmt.Errorf("validate root initialization session path: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(absoluteRoot, rootInitializationJournalName))
	if err != nil {
		return nil, fmt.Errorf("read root initialization journal: %w", err)
	}
	payload, err := contracts.DecodeJSONObjectBytes(data)
	if err != nil {
		return nil, fmt.Errorf("decode root initialization journal: %w", err)
	}
	if intFromAny(payload["schema_version"], 0) != rootInitializationJournalSchemaVersion {
		return nil, errors.New("root initialization journal schema version is invalid")
	}
	hasMeta := len(meta) != 0
	token := strings.TrimSpace(stringFromAny(payload["transaction_token"]))
	tokenBytes, tokenErr := hex.DecodeString(token)
	if tokenErr != nil || len(tokenBytes) != 32 ||
		(hasMeta && token != strings.TrimSpace(stringFromAny(meta["initialization_token"]))) {
		return nil, errors.New("root initialization journal token does not match session metadata")
	}
	recordedRoot := cleanInitializationPath(payload["canonical_session_root"])
	metaRoot := cleanInitializationPath(meta["canonical_session_root"])
	if recordedRoot != absoluteRoot || (hasMeta && metaRoot != absoluteRoot) {
		return nil, errors.New("root initialization journal session identity is invalid")
	}
	sourceGitRoot := cleanInitializationPath(payload["source_git_root"])
	metaSourceGitRoot := cleanInitializationPath(meta["source_git_root"])
	if (hasMeta && sourceGitRoot != metaSourceGitRoot) || (sourceGitRoot != "" && !filepath.IsAbs(sourceGitRoot)) {
		return nil, errors.New("root initialization journal source repository identity is invalid")
	}
	worktreePath := cleanInitializationPath(payload["expected_worktree_path"])
	metaWorktreePath := cleanInitializationPath(meta["expected_worktree_path"])
	if hasMeta && worktreePath != metaWorktreePath {
		return nil, errors.New("root initialization journal worktree identity does not match session metadata")
	}
	expectedWorktree := filepath.Join(absoluteRoot, "execution", "worktree")
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
	if hasMeta && (!metaPreExistingOK || metaPreExisting != preExisting) {
		return nil, errors.New("root initialization journal ownership does not match session metadata")
	}
	for _, field := range []string{"captured_commit", "captured_tree", "object_format"} {
		if hasMeta && strings.TrimSpace(stringFromAny(payload[field])) != strings.TrimSpace(stringFromAny(meta[field])) {
			return nil, fmt.Errorf("root initialization journal captured identity field %s does not match session metadata", field)
		}
	}
	if err := validateRootInitializationOwnedEntries(payload["owned_top_level_entries"]); err != nil {
		return nil, err
	}
	if strings.TrimSpace(stringFromAny(payload["initialization_claim"])) != rootInitializationClaimName ||
		(hasMeta && strings.TrimSpace(stringFromAny(meta["initialization_claim"])) != rootInitializationClaimName) {
		return nil, errors.New("root initialization journal claim identity is invalid")
	}
	claimIdentity := strings.TrimSpace(stringFromAny(payload["initialization_claim_id"]))
	if claimIdentity == "" {
		return nil, errors.New("root initialization journal claim file identity is invalid")
	}
	ownedEntries, err := parseRootInitializationOwnedEntries(payload["owned_entries"])
	if err != nil {
		return nil, err
	}
	ownedDigest := strings.TrimSpace(stringFromAny(payload["owned_inventory_digest"]))
	computedDigest, err := rootInitializationOwnershipDigestForPayload(payload["owned_entries"])
	if err != nil || ownedDigest == "" || ownedDigest != computedDigest {
		return nil, errors.New("root initialization journal ownership digest is invalid")
	}
	pendingWrite, err := rootInitializationWriteIntentFromMap(payload["pending_file_write"])
	if err != nil {
		return nil, err
	}
	pendingScope, err := rootInitializationScopeIntentFromMap(payload["pending_mutation_scope"])
	if err != nil {
		return nil, err
	}
	nextWriteSequence := int64FromAny(payload["next_write_sequence"], -1)
	if nextWriteSequence < 0 {
		return nil, errors.New("root initialization journal write sequence is invalid")
	}
	if err := validateRootInitializationClaim(absoluteRoot, token); err != nil {
		return nil, err
	}
	transaction := &rootInitializationTransaction{
		token:             token,
		sessionRoot:       absoluteRoot,
		preExistingRoot:   preExisting,
		sourceGitRoot:     sourceGitRoot,
		worktreePath:      worktreePath,
		headCommit:        strings.TrimSpace(stringFromAny(payload["captured_commit"])),
		headTree:          strings.TrimSpace(stringFromAny(payload["captured_tree"])),
		objectFormat:      strings.TrimSpace(stringFromAny(payload["object_format"])),
		phase:             strings.TrimSpace(stringFromAny(payload["initialization_phase"])),
		cleanupState:      strings.TrimSpace(stringFromAny(payload["cleanup_state"])),
		ownedDigest:       ownedDigest,
		claimIdentity:     claimIdentity,
		ownedEntries:      ownedEntries,
		pendingWrite:      pendingWrite,
		pendingScope:      pendingScope,
		nextWriteSequence: uint64(nextWriteSequence),
		mutationLock:      mutationLock,
	}
	transaction.st = store.NewWithFileMutationObserver(absoluteRoot, transaction)
	if transaction.phase == "" || transaction.cleanupState == "" {
		return nil, errors.New("root initialization journal lifecycle fields are invalid")
	}
	if err := transaction.validateClaimIdentity(); err != nil {
		return nil, err
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
