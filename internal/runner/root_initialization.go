package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

const (
	rootInitializationJournalName = "initialization.json"
	rootInitializationClaimName   = "initialization.claim"
)

var rootInitializationAfterStage func(string) error

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
	ownedDigest     string
	claimOwned      bool
	st              *store.Store
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
	releaseClaimOnError := true
	defer func() {
		if releaseClaimOnError {
			_ = removeRootInitializationClaim(sessionRoot, token)
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
	ownedDigest, err := rootInitializationOwnedInventoryDigest(sessionRoot)
	if err != nil {
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
		ownedDigest:     ownedDigest,
		claimOwned:      true,
		st:              store.New(sessionRoot),
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
		releaseClaimOnError = false
		return nil, errors.Join(err, transaction.compensateWithoutMeta())
	}
	if err := transaction.advance("session_claimed"); err != nil {
		releaseClaimOnError = false
		return nil, transaction.Fail(err)
	}
	if err := ensureGitRepo(sessionRoot); err != nil {
		releaseClaimOnError = false
		return nil, transaction.Fail(err)
	}
	if err := transaction.advance("session_repository_initialized"); err != nil {
		releaseClaimOnError = false
		return nil, transaction.Fail(err)
	}
	releaseClaimOnError = false
	return transaction, nil
}

func (t *rootInitializationTransaction) advance(phase string) error {
	if t == nil {
		return errors.New("root initialization transaction is required")
	}
	t.phase = strings.TrimSpace(phase)
	if err := t.refreshOwnedInventory(); err != nil {
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
	if rootInitializationAfterStage != nil {
		if err := rootInitializationAfterStage("ready_metadata_persisted"); err != nil {
			return err
		}
	}
	t.phase = "ready"
	t.cleanupState = "disarmed"
	_ = t.writeJournal()
	return removeRootInitializationClaim(t.sessionRoot, t.token)
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
			return removeRootInitializationClaim(t.sessionRoot, t.token)
		}
		return errors.New("initialization compensation refused metadata with a mismatched state or transaction token")
	}
	if t.claimOwned {
		if err := t.refreshOwnedInventory(); err != nil {
			return fmt.Errorf("refresh initialization ownership before compensation: %w", err)
		}
		meta = meta.With("initialization_owned_inventory_digest", t.ownedDigest)
		if err := t.st.SaveMeta(meta); err != nil {
			return fmt.Errorf("persist initialization ownership before compensation: %w", err)
		}
		if err := t.writeJournal(); err != nil {
			return fmt.Errorf("persist initialization journal ownership before compensation: %w", err)
		}
	}
	if meta.String("initialization_owned_inventory_digest") != t.ownedDigest {
		return errors.New("initialization compensation refused mismatched ownership metadata")
	}
	if err := validateRootInitializationClaim(t.sessionRoot, t.token); err != nil {
		return fmt.Errorf("validate initialization claim before compensation: %w", err)
	}
	observedDigest, err := rootInitializationOwnedInventoryDigest(t.sessionRoot)
	if err != nil {
		return err
	}
	if observedDigest != t.ownedDigest {
		t.cleanupState = "blocked_foreign_entries"
		_ = t.writeJournal()
		return errors.New("initialization compensation refused foreign session entries")
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
	cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := workspace.CleanupInitializationWorktree(cleanupContext, t.sourceGitRoot, t.worktreePath, t.headCommit); err != nil {
		t.cleanupState = "failed"
		_ = t.writeJournal()
		return fmt.Errorf("compensate initialization worktree: %w", err)
	}
	if err := rejectInitializationSymlinkComponents(t.sessionRoot); err != nil {
		return fmt.Errorf("validate initialization root before removal: %w", err)
	}
	t.cleanupState = "complete"
	_ = t.writeJournal()
	if t.preExistingRoot {
		for _, name := range rootInitializationOwnedEntries {
			path := filepath.Join(t.sessionRoot, name)
			if err := rejectInitializationSymlinkComponents(path); err != nil {
				return fmt.Errorf("validate initialization-owned entry %s: %w", name, err)
			}
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

func rootInitializationOwnedInventoryDigest(sessionRoot string) (string, error) {
	if err := rejectInitializationSymlinkComponents(sessionRoot); err != nil {
		return "", err
	}
	allowed := make(map[string]bool, len(rootInitializationOwnedEntries))
	for _, name := range rootInitializationOwnedEntries {
		allowed[name] = true
	}
	dynamic := map[string]bool{
		".mutation.lock":              true,
		rootInitializationClaimName:   true,
		rootInitializationJournalName: true,
		"meta.json":                   true,
	}
	records := make([]string, 0)
	err := filepath.WalkDir(sessionRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sessionRoot, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		relative = filepath.Clean(relative)
		topLevel := relative
		if separator := strings.IndexRune(relative, filepath.Separator); separator >= 0 {
			topLevel = relative[:separator]
		}
		if !allowed[topLevel] {
			return fmt.Errorf("foreign session entry %s is outside the initialization ownership set", relative)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if dynamic[relative] {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("initialization control entry %s must be a regular file", relative)
			}
			return nil
		}
		kind := ""
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
		case info.IsDir():
			kind = "directory"
		case info.Mode().IsRegular():
			kind = "regular"
		default:
			return fmt.Errorf("foreign session entry %s has an unsupported file type", relative)
		}
		encodedPath := base64.StdEncoding.EncodeToString([]byte(filepath.ToSlash(relative)))
		records = append(records, encodedPath+"\x00"+kind)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(records)
	canonical, err := contracts.CanonicalJSONBytes(records)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return contracts.DigestPrefix + hex.EncodeToString(sum[:]), nil
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

func (t *rootInitializationTransaction) refreshOwnedInventory() error {
	if t == nil {
		return errors.New("root initialization transaction is required")
	}
	digest, err := rootInitializationOwnedInventoryDigest(t.sessionRoot)
	if err != nil {
		return err
	}
	t.ownedDigest = digest
	return nil
}

func (t *rootInitializationTransaction) writeJournal() error {
	payload := map[string]any{
		"schema_version":            2,
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
		"owned_inventory_digest":    t.ownedDigest,
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
	if intFromAny(payload["schema_version"], 0) != 2 {
		return nil, errors.New("root initialization journal schema version is invalid")
	}
	token := strings.TrimSpace(stringFromAny(payload["transaction_token"]))
	if token == "" || token != strings.TrimSpace(stringFromAny(meta["initialization_token"])) {
		return nil, errors.New("root initialization journal token does not match session metadata")
	}
	recordedRoot := cleanInitializationPath(payload["canonical_session_root"])
	metaRoot := cleanInitializationPath(meta["canonical_session_root"])
	if recordedRoot != absoluteRoot || metaRoot != absoluteRoot {
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
	if strings.TrimSpace(stringFromAny(payload["initialization_claim"])) != rootInitializationClaimName ||
		strings.TrimSpace(stringFromAny(meta["initialization_claim"])) != rootInitializationClaimName {
		return nil, errors.New("root initialization journal claim identity is invalid")
	}
	ownedDigest := strings.TrimSpace(stringFromAny(payload["owned_inventory_digest"]))
	if ownedDigest == "" || ownedDigest != strings.TrimSpace(stringFromAny(meta["initialization_owned_inventory_digest"])) {
		return nil, errors.New("root initialization journal ownership digest does not match session metadata")
	}
	if err := validateRootInitializationClaim(absoluteRoot, token); err != nil {
		return nil, err
	}
	transaction := &rootInitializationTransaction{
		token:           token,
		sessionRoot:     absoluteRoot,
		preExistingRoot: preExisting,
		sourceGitRoot:   sourceGitRoot,
		worktreePath:    worktreePath,
		headCommit:      strings.TrimSpace(stringFromAny(payload["captured_commit"])),
		headTree:        strings.TrimSpace(stringFromAny(payload["captured_tree"])),
		objectFormat:    strings.TrimSpace(stringFromAny(payload["object_format"])),
		phase:           strings.TrimSpace(stringFromAny(payload["initialization_phase"])),
		cleanupState:    strings.TrimSpace(stringFromAny(payload["cleanup_state"])),
		ownedDigest:     ownedDigest,
		st:              store.New(absoluteRoot),
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
