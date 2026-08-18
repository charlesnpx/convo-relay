package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/namedinputs"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const (
	rootInitializationJournalSchemaVersion       = 4
	rootInitializationLegacyJournalSchemaVersion = 3
)

type rootInitializationOwnedEntry struct {
	Path     string
	Kind     string
	Mode     uint32
	Size     int64
	Digest   string
	Identity string
}

type rootInitializationWriteIntent struct {
	ID                string
	Target            rootInitializationOwnedEntry
	TemporaryPath     string
	TemporaryIdentity string
	Previous          *rootInitializationOwnedEntry
	PreviousPath      string
	Directories       []rootInitializationOwnedEntry
}

type rootInitializationScopeIntent struct {
	Name    string
	Planned []rootInitializationOwnedEntry
}

func (entry rootInitializationOwnedEntry) toMap() map[string]any {
	return map[string]any{
		"path":     entry.Path,
		"kind":     entry.Kind,
		"mode":     int64(entry.Mode),
		"size":     entry.Size,
		"digest":   emptyStringAsNil(entry.Digest),
		"identity": emptyStringAsNil(entry.Identity),
	}
}

func rootInitializationOwnedEntryFromMap(value any) (rootInitializationOwnedEntry, error) {
	item, ok := value.(map[string]any)
	if !ok {
		return rootInitializationOwnedEntry{}, errors.New("initialization ownership entry must be an object")
	}
	entry := rootInitializationOwnedEntry{
		Path:     filepath.Clean(filepath.FromSlash(strings.TrimSpace(stringFromAny(item["path"])))),
		Kind:     strings.TrimSpace(stringFromAny(item["kind"])),
		Mode:     uint32(intFromAny(item["mode"], -1)),
		Size:     int64FromAny(item["size"], -1),
		Digest:   strings.TrimSpace(stringFromAny(item["digest"])),
		Identity: strings.TrimSpace(stringFromAny(item["identity"])),
	}
	if err := validateRootInitializationOwnedEntry(entry); err != nil {
		return rootInitializationOwnedEntry{}, err
	}
	return entry, nil
}

func validateRootInitializationOwnedEntry(entry rootInitializationOwnedEntry) error {
	if entry.Path == "" ||
		entry.Path == "." ||
		filepath.IsAbs(entry.Path) ||
		entry.Path == ".." ||
		strings.HasPrefix(entry.Path, ".."+string(filepath.Separator)) {
		return errors.New("initialization ownership path is invalid")
	}
	if entry.Mode > 0o777 {
		return fmt.Errorf("initialization ownership mode for %s is invalid", entry.Path)
	}
	if !rootInitializationPathAllowed(entry.Path) {
		return fmt.Errorf("initialization ownership path %s is outside the transaction namespace", entry.Path)
	}
	switch entry.Kind {
	case "directory":
		if entry.Size != 0 || entry.Digest != "" {
			return fmt.Errorf("initialization directory ownership for %s has invalid content fields", entry.Path)
		}
	case "regular", "symlink":
		if entry.Size < 0 || !strings.HasPrefix(entry.Digest, contracts.DigestPrefix) {
			return fmt.Errorf("initialization ownership for %s is missing content identity", entry.Path)
		}
	default:
		return fmt.Errorf("initialization ownership for %s has unsupported kind %q", entry.Path, entry.Kind)
	}
	return nil
}

func (intent rootInitializationWriteIntent) toMap() map[string]any {
	payload := map[string]any{
		"id":                 intent.ID,
		"target":             intent.Target.toMap(),
		"temporary_path":     filepath.ToSlash(intent.TemporaryPath),
		"temporary_identity": emptyStringAsNil(intent.TemporaryIdentity),
	}
	directories := make([]any, 0, len(intent.Directories))
	for _, directory := range intent.Directories {
		directories = append(directories, directory.toMap())
	}
	payload["planned_directories"] = directories
	if intent.Previous != nil {
		payload["previous"] = intent.Previous.toMap()
		payload["previous_path"] = filepath.ToSlash(intent.PreviousPath)
	}
	return payload
}

func rootInitializationWriteIntentFromMap(value any) (*rootInitializationWriteIntent, error) {
	if value == nil {
		return nil, nil
	}
	item, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("initialization pending write must be an object")
	}
	target, err := rootInitializationOwnedEntryFromMap(item["target"])
	if err != nil {
		return nil, err
	}
	intent := &rootInitializationWriteIntent{
		ID:                strings.TrimSpace(stringFromAny(item["id"])),
		Target:            target,
		TemporaryPath:     filepath.Clean(filepath.FromSlash(strings.TrimSpace(stringFromAny(item["temporary_path"])))),
		TemporaryIdentity: strings.TrimSpace(stringFromAny(item["temporary_identity"])),
	}
	if intent.ID == "" || intent.TemporaryPath == "" || filepath.IsAbs(intent.TemporaryPath) {
		return nil, errors.New("initialization pending write identity is invalid")
	}
	wantTemporary := filepath.Join(
		filepath.Dir(target.Path),
		"."+filepath.Base(target.Path)+".initialization-"+intent.ID+".tmp",
	)
	if intent.TemporaryPath != wantTemporary || !rootInitializationPathAllowed(intent.TemporaryPath) {
		return nil, errors.New("initialization pending write temporary path is invalid")
	}
	if rawPrevious, exists := item["previous"]; exists && rawPrevious != nil {
		previous, err := rootInitializationOwnedEntryFromMap(rawPrevious)
		if err != nil {
			return nil, err
		}
		if previous.Path != target.Path {
			return nil, errors.New("initialization pending write previous path is invalid")
		}
		intent.Previous = &previous
		intent.PreviousPath = filepath.Clean(filepath.FromSlash(strings.TrimSpace(stringFromAny(item["previous_path"]))))
		wantPreviousPath := filepath.Join(
			filepath.Dir(target.Path),
			"."+filepath.Base(target.Path)+".initialization-"+intent.ID+".previous",
		)
		if intent.PreviousPath != wantPreviousPath ||
			filepath.IsAbs(intent.PreviousPath) ||
			!rootInitializationPathAllowed(intent.PreviousPath) {
			return nil, errors.New("initialization pending write previous staging path is invalid")
		}
	} else if strings.TrimSpace(stringFromAny(item["previous_path"])) != "" {
		return nil, errors.New("initialization pending write without a previous target has a staging path")
	}
	rawDirectories, ok := item["planned_directories"].([]any)
	if !ok {
		return nil, errors.New("initialization pending write planned directories are invalid")
	}
	for _, rawDirectory := range rawDirectories {
		directory, err := rootInitializationOwnedEntryFromMap(rawDirectory)
		if err != nil {
			return nil, err
		}
		if directory.Kind != "directory" ||
			directory.Identity != "" ||
			!rootInitializationPathIsAncestor(directory.Path, target.Path) {
			return nil, errors.New("initialization pending write planned directory is invalid")
		}
		intent.Directories = append(intent.Directories, directory)
	}
	return intent, nil
}

func (intent rootInitializationScopeIntent) toMap() map[string]any {
	planned := make([]any, 0, len(intent.Planned))
	for _, entry := range intent.Planned {
		planned = append(planned, entry.toMap())
	}
	return map[string]any{"name": intent.Name, "planned": planned}
}

func rootInitializationScopeIntentFromMap(value any) (*rootInitializationScopeIntent, error) {
	if value == nil {
		return nil, nil
	}
	item, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("initialization pending scope must be an object")
	}
	intent := &rootInitializationScopeIntent{Name: strings.TrimSpace(stringFromAny(item["name"]))}
	raw, ok := item["planned"].([]any)
	if intent.Name == "" || !ok || len(raw) == 0 {
		return nil, errors.New("initialization pending scope identity is invalid")
	}
	for _, value := range raw {
		entry, err := rootInitializationOwnedEntryFromMap(value)
		if err != nil {
			return nil, err
		}
		if entry.Kind != "directory" {
			return nil, errors.New("initialization pending scope may plan only directories")
		}
		intent.Planned = append(intent.Planned, entry)
	}
	return intent, nil
}

func (t *rootInitializationTransaction) BeforeFileMutation(
	path string,
	body []byte,
	mode os.FileMode,
) (store.FileMutationPlan, error) {
	if t == nil || t.mutationLock == nil || t.mutationLock.file == nil {
		return store.FileMutationPlan{}, errors.New("root initialization file write requires its mutation lease")
	}
	if t.pendingWrite != nil {
		return store.FileMutationPlan{}, errors.New("root initialization already has a pending file write")
	}
	relative, err := rootInitializationRelativePath(t.sessionRoot, path)
	if err != nil {
		return store.FileMutationPlan{}, err
	}
	if !rootInitializationPathAllowed(relative) {
		return store.FileMutationPlan{}, fmt.Errorf("root initialization write %s is outside its owned namespace", relative)
	}
	t.nextWriteSequence++
	id := fmt.Sprintf("%06d-%s", t.nextWriteSequence, t.token[:12])
	temporaryRelative := filepath.Join(
		filepath.Dir(relative),
		"."+filepath.Base(relative)+".initialization-"+id+".tmp",
	)
	temporaryAbsolute := filepath.Join(t.sessionRoot, temporaryRelative)
	if _, err := os.Lstat(temporaryAbsolute); err == nil {
		return store.FileMutationPlan{}, errors.New("root initialization reserved temporary path already exists")
	} else if !os.IsNotExist(err) {
		return store.FileMutationPlan{}, err
	}
	sum := sha256.Sum256(body)
	target := rootInitializationOwnedEntry{
		Path:   relative,
		Kind:   "regular",
		Mode:   uint32(mode.Perm()),
		Size:   int64(len(body)),
		Digest: contracts.DigestPrefix + hex.EncodeToString(sum[:]),
	}
	var previous *rootInitializationOwnedEntry
	if prior, exists := t.ownedEntries[relative]; exists {
		copy := prior
		previous = &copy
	}
	actualTarget, targetExists, err := rootInitializationMaybeRecordPath(t.sessionRoot, relative)
	if err != nil {
		return store.FileMutationPlan{}, err
	}
	switch {
	case previous == nil && targetExists:
		return store.FileMutationPlan{}, fmt.Errorf("root initialization write target %s is a foreign existing entry", relative)
	case previous != nil && (!targetExists || !rootInitializationEntryMatches(*previous, actualTarget, true)):
		return store.FileMutationPlan{}, fmt.Errorf("root initialization write target %s no longer has its owned identity", relative)
	}
	previousRelative := ""
	if previous != nil {
		previousRelative = filepath.Join(
			filepath.Dir(relative),
			"."+filepath.Base(relative)+".initialization-"+id+".previous",
		)
		previousAbsolute := filepath.Join(t.sessionRoot, previousRelative)
		if _, err := os.Lstat(previousAbsolute); err == nil {
			return store.FileMutationPlan{}, errors.New("root initialization previous-target staging path already exists")
		} else if !os.IsNotExist(err) {
			return store.FileMutationPlan{}, err
		}
	}
	plannedDirectories := make([]rootInitializationOwnedEntry, 0)
	for parent := filepath.Dir(relative); parent != "."; parent = filepath.Dir(parent) {
		if _, exists := t.ownedEntries[parent]; exists {
			continue
		}
		plannedDirectories = append(plannedDirectories, rootInitializationOwnedEntry{
			Path: parent,
			Kind: "directory",
		})
	}
	sort.Slice(plannedDirectories, func(left int, right int) bool {
		return plannedDirectories[left].Path < plannedDirectories[right].Path
	})
	t.pendingWrite = &rootInitializationWriteIntent{
		ID:            id,
		Target:        target,
		TemporaryPath: temporaryRelative,
		Previous:      previous,
		PreviousPath:  previousRelative,
		Directories:   plannedDirectories,
	}
	if err := t.writeJournal(); err != nil {
		t.pendingWrite = nil
		return store.FileMutationPlan{}, err
	}
	if rootInitializationBeforeFileCommit != nil {
		if err := rootInitializationBeforeFileCommit(relative); err != nil {
			return store.FileMutationPlan{}, err
		}
	}
	if err := rejectInitializationSymlinkComponents(filepath.Dir(temporaryAbsolute)); err != nil {
		return store.FileMutationPlan{}, err
	}
	if err := os.MkdirAll(filepath.Dir(temporaryAbsolute), 0o755); err != nil {
		return store.FileMutationPlan{}, err
	}
	if err := rejectInitializationSymlinkComponents(filepath.Dir(temporaryAbsolute)); err != nil {
		return store.FileMutationPlan{}, err
	}
	handle, err := os.OpenFile(temporaryAbsolute, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return store.FileMutationPlan{}, err
	}
	closeErr := handle.Close()
	temporaryInfo, statErr := os.Lstat(temporaryAbsolute)
	if closeErr != nil ||
		statErr != nil ||
		temporaryInfo.Mode()&os.ModeSymlink != 0 ||
		!temporaryInfo.Mode().IsRegular() {
		return store.FileMutationPlan{}, errors.Join(closeErr, statErr, errors.New("root initialization temporary file identity could not be established"))
	}
	t.pendingWrite.TemporaryIdentity = rootInitializationFileIdentity(temporaryAbsolute, temporaryInfo)
	if t.pendingWrite.TemporaryIdentity == "" {
		return store.FileMutationPlan{}, errors.New("root initialization temporary file identity is unavailable")
	}
	if err := t.writeJournal(); err != nil {
		return store.FileMutationPlan{}, err
	}
	if err := t.stagePendingPreviousTarget(); err != nil {
		return store.FileMutationPlan{}, err
	}
	return store.FileMutationPlan{
		ID:                  id,
		TemporaryPath:       temporaryAbsolute,
		RequireAbsentTarget: true,
	}, nil
}

func (t *rootInitializationTransaction) AfterFileMutation(plan store.FileMutationPlan, _ bool) error {
	if t == nil || t.pendingWrite == nil || t.pendingWrite.ID != plan.ID {
		return errors.New("root initialization file write completion does not match its persisted intent")
	}
	// Completion is recovered from exact filesystem identities rather than the
	// caller's Boolean alone. That also covers a process failure after a
	// no-replace publication but before Store returned success.
	return t.resolvePendingWrite()
}

func (t *rootInitializationTransaction) resolvePendingWrite() error {
	if t == nil || t.pendingWrite == nil {
		return nil
	}
	intent := *t.pendingWrite
	for _, planned := range intent.Directories {
		if _, exists := t.ownedEntries[planned.Path]; !exists {
			t.ownedEntries[planned.Path] = planned
		}
	}
	target, targetExists, err := rootInitializationMaybeRecordPath(t.sessionRoot, intent.Target.Path)
	if err != nil {
		return err
	}
	previous, previousExists, err := t.pendingPreviousRecord(intent)
	if err != nil {
		return err
	}
	if previousExists &&
		(intent.Previous == nil ||
			!rootInitializationEntryMatches(
				*intent.Previous,
				rootInitializationEntryAtTargetPath(previous, intent.Target.Path),
				true,
			)) {
		return fmt.Errorf("pending initialization previous target %s has a foreign file identity", intent.PreviousPath)
	}
	switch {
	case targetExists && rootInitializationEntryMatches(intent.Target, target, false):
		if intent.TemporaryIdentity == "" || target.Identity != intent.TemporaryIdentity {
			return fmt.Errorf("pending initialization write target %s has a foreign file identity", intent.Target.Path)
		}
		t.addOwnedEntryWithParents(target)
	case targetExists && intent.Previous != nil && rootInitializationEntryMatches(*intent.Previous, target, true):
		t.ownedEntries[target.Path] = target
	case targetExists:
		return fmt.Errorf("pending initialization write target %s was replaced by foreign content", intent.Target.Path)
	case intent.Previous != nil:
		if !previousExists {
			// The previously owned inode was removed outside the transaction.
			// Once its replacement is also absent there is no foreign entry to
			// delete, so recovery can forget the lost owned intermediate and
			// continue exact cleanup.
			delete(t.ownedEntries, intent.Target.Path)
			break
		}
		if err := restoreInitializationStagedEntry(
			filepath.Join(t.sessionRoot, intent.PreviousPath),
			filepath.Join(t.sessionRoot, intent.Target.Path),
			previous,
		); err != nil {
			return fmt.Errorf("restore pending initialization previous target %s: %w", intent.Target.Path, err)
		}
		target, err = rootInitializationRecordPath(t.sessionRoot, intent.Target.Path)
		if err != nil || !rootInitializationEntryMatches(*intent.Previous, target, true) {
			return errors.Join(err, fmt.Errorf("restored pending initialization target %s changed identity", intent.Target.Path))
		}
		t.ownedEntries[target.Path] = target
	}
	if err := t.removePendingPrevious(intent); err != nil {
		return err
	}
	if err := t.removePendingTemporary(intent); err != nil {
		return err
	}
	t.pendingWrite = nil
	return t.writeJournal()
}

func (t *rootInitializationTransaction) stagePendingPreviousTarget() error {
	if t == nil || t.pendingWrite == nil || t.pendingWrite.Previous == nil {
		return nil
	}
	intent := *t.pendingWrite
	targetPath := filepath.Join(t.sessionRoot, intent.Target.Path)
	previousPath := filepath.Join(t.sessionRoot, intent.PreviousPath)
	if _, err := os.Lstat(previousPath); err == nil {
		return errors.New("root initialization previous-target staging path already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(targetPath, previousPath); err != nil {
		return fmt.Errorf("stage root initialization previous target: %w", err)
	}
	moved, err := rootInitializationRecordPath(t.sessionRoot, intent.PreviousPath)
	if err == nil {
		moved.Path = intent.Target.Path
	}
	if err == nil && rootInitializationEntryMatches(*intent.Previous, moved, true) {
		return nil
	}
	staged, stagedErr := rootInitializationRecordPath(t.sessionRoot, intent.PreviousPath)
	if stagedErr == nil {
		staged.Path = intent.PreviousPath
	}
	restoreErr := stagedErr
	if restoreErr == nil {
		restoreErr = restoreInitializationStagedEntry(previousPath, targetPath, staged)
	}
	return errors.Join(
		err,
		restoreErr,
		fmt.Errorf("root initialization write target %s changed identity before staging", intent.Target.Path),
	)
}

func (t *rootInitializationTransaction) pendingPreviousRecord(
	intent rootInitializationWriteIntent,
) (rootInitializationOwnedEntry, bool, error) {
	if intent.Previous == nil || strings.TrimSpace(intent.PreviousPath) == "" {
		return rootInitializationOwnedEntry{}, false, nil
	}
	record, exists, err := rootInitializationMaybeRecordPath(t.sessionRoot, intent.PreviousPath)
	if err != nil || !exists {
		return record, exists, err
	}
	return record, true, nil
}

func (t *rootInitializationTransaction) removePendingPrevious(intent rootInitializationWriteIntent) error {
	if intent.Previous == nil || strings.TrimSpace(intent.PreviousPath) == "" {
		return nil
	}
	previous, exists, err := t.pendingPreviousRecord(intent)
	if err != nil || !exists {
		return err
	}
	if !rootInitializationEntryMatches(*intent.Previous, rootInitializationEntryAtTargetPath(previous, intent.Target.Path), true) {
		return fmt.Errorf("pending initialization previous target %s has a foreign file identity", intent.PreviousPath)
	}
	return os.Remove(filepath.Join(t.sessionRoot, intent.PreviousPath))
}

func restoreInitializationStagedEntry(
	stagedPath string,
	targetPath string,
	staged rootInitializationOwnedEntry,
) error {
	if staged.Kind != "regular" {
		if _, err := os.Lstat(targetPath); err == nil {
			return errors.New("initialization target was populated before staged entry restoration")
		} else if !os.IsNotExist(err) {
			return err
		}
		return os.Rename(stagedPath, targetPath)
	}
	if err := os.Link(stagedPath, targetPath); err != nil {
		return fmt.Errorf("publish staged initialization entry without replacement: %w", err)
	}
	targetInfo, targetErr := os.Lstat(targetPath)
	stagedInfo, stagedErr := os.Lstat(stagedPath)
	if targetErr != nil ||
		stagedErr != nil ||
		targetInfo.Mode()&os.ModeSymlink != 0 ||
		!targetInfo.Mode().IsRegular() ||
		!os.SameFile(stagedInfo, targetInfo) {
		return errors.Join(targetErr, stagedErr, errors.New("restored initialization entry changed identity"))
	}
	return os.Remove(stagedPath)
}

func rootInitializationEntryAtTargetPath(
	entry rootInitializationOwnedEntry,
	targetPath string,
) rootInitializationOwnedEntry {
	entry.Path = targetPath
	return entry
}

func (t *rootInitializationTransaction) removePendingTemporary(intent rootInitializationWriteIntent) error {
	if strings.TrimSpace(intent.TemporaryPath) == "" {
		return nil
	}
	temporary, exists, err := rootInitializationMaybeRecordPath(t.sessionRoot, intent.TemporaryPath)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if intent.TemporaryIdentity == "" || temporary.Identity != intent.TemporaryIdentity {
		return fmt.Errorf("pending initialization temporary file %s has a foreign file identity", intent.TemporaryPath)
	}
	return os.Remove(filepath.Join(t.sessionRoot, intent.TemporaryPath))
}

func (t *rootInitializationTransaction) beginOwnedScope(name string, roots ...string) error {
	if t == nil || t.pendingScope != nil {
		return errors.New("root initialization already has a pending mutation scope")
	}
	intent := &rootInitializationScopeIntent{Name: strings.TrimSpace(name)}
	if intent.Name == "" || len(roots) == 0 {
		return errors.New("root initialization mutation scope requires a name and root")
	}
	for _, root := range roots {
		relative := filepath.Clean(filepath.FromSlash(root))
		entry := rootInitializationOwnedEntry{Path: relative, Kind: "directory"}
		if err := validateRootInitializationOwnedEntry(entry); err != nil {
			return err
		}
		intent.Planned = append(intent.Planned, entry)
		if _, exists := t.ownedEntries[relative]; !exists {
			t.ownedEntries[relative] = entry
		}
	}
	t.pendingScope = intent
	return t.writeJournal()
}

func (t *rootInitializationTransaction) completeOwnedScope() error {
	if t == nil || t.pendingScope == nil {
		return errors.New("root initialization has no pending mutation scope")
	}
	for _, planned := range t.pendingScope.Planned {
		records, err := rootInitializationCaptureSubtree(t.sessionRoot, planned.Path)
		if err != nil {
			return err
		}
		for _, record := range records {
			if existing, exists := t.ownedEntries[record.Path]; exists && existing.Identity != "" {
				if !rootInitializationEntryMatches(existing, record, true) {
					return fmt.Errorf("initialization-owned entry %s was replaced before scope completion", record.Path)
				}
				continue
			}
			t.ownedEntries[record.Path] = record
		}
	}
	t.pendingScope = nil
	return t.writeJournal()
}

func (t *rootInitializationTransaction) abortOwnedScope() error {
	if t == nil || t.pendingScope == nil {
		return nil
	}
	if err := t.validateOwnedInventory(); err != nil {
		return err
	}
	t.pendingScope = nil
	return t.writeJournal()
}

func (t *rootInitializationTransaction) resolvePendingScope() error {
	if t == nil || t.pendingScope == nil {
		return nil
	}
	switch t.pendingScope.Name {
	case "session_repository_initialization":
		if err := validateInitializationSessionRepository(t.sessionRoot); err != nil {
			return t.abortOwnedScope()
		}
		return t.completeOwnedScope()
	case "workspace_materialization":
		materialized, err := workspace.Recover(context.Background(), t.st)
		if err != nil {
			// An operation that failed before its authoritative artifact was
			// complete may still have created only its predeclared directories.
			// That narrow case can be aborted without observing new ownership.
			return t.abortOwnedScope()
		}
		if err := t.validateWorkspaceScope(materialized); err != nil {
			return err
		}
		return t.completeOwnedScope()
	case "retained_input_materialization":
		identity, err := contracts.RootArtifactIdentityFor(contracts.RootArtifactKindRetainedInputs, 0)
		if err != nil {
			return err
		}
		ref, err := t.st.ResolveArtifactRef(identity.RefID, "")
		if err != nil {
			return t.abortOwnedScope()
		}
		if err := namedinputs.VerifyRetained(
			context.Background(),
			t.st,
			ref,
			"initialization_recovery",
			namedinputs.IntegrityBoundaryInitialization,
			nil,
		); err != nil {
			return err
		}
		return t.completeOwnedScope()
	default:
		return t.abortOwnedScope()
	}
}

func (t *rootInitializationTransaction) materializeWorkspace(
	ctx context.Context,
	snapshot *workspace.Snapshot,
) (*workspace.Materialized, error) {
	if err := t.beginOwnedScope("workspace_materialization", "execution"); err != nil {
		return nil, err
	}
	materialized, err := workspace.Materialize(ctx, t.st, snapshot)
	if err != nil {
		return nil, errors.Join(err, t.abortOwnedScope())
	}
	if err := t.validateWorkspaceScope(materialized); err != nil {
		return nil, err
	}
	if err := t.completeOwnedScope(); err != nil {
		return nil, err
	}
	return materialized, nil
}

// validateWorkspaceScope keeps workspace recovery from turning an unexpected
// sibling into transaction ownership. workspace.Recover proves the exact
// worktree contents and registration; this additional check proves that the
// enclosing execution namespace contains only that worktree (or remains empty
// for inherited execution) before the scope is captured.
func (t *rootInitializationTransaction) validateWorkspaceScope(materialized *workspace.Materialized) error {
	if t == nil || materialized == nil {
		return errors.New("root initialization workspace scope requires recovered materialization")
	}
	executionRoot := filepath.Join(t.sessionRoot, "execution")
	info, err := os.Lstat(executionRoot)
	if os.IsNotExist(err) {
		if strings.TrimSpace(materialized.WorktreePath) != "" {
			return errors.New("recovered initialization workspace is missing its execution directory")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("recovered initialization execution path has a foreign type")
	}
	entries, err := os.ReadDir(executionRoot)
	if err != nil {
		return err
	}
	if strings.TrimSpace(materialized.WorktreePath) == "" {
		if len(entries) != 0 {
			return errors.New("recovered inherited initialization workspace contains foreign execution entries")
		}
		return nil
	}
	expectedWorktree := filepath.Join(executionRoot, "worktree")
	if filepath.Clean(materialized.WorktreePath) != expectedWorktree {
		return errors.New("recovered initialization worktree is outside its fixed execution path")
	}
	if len(entries) != 1 || entries[0].Name() != "worktree" {
		return errors.New("recovered initialization workspace contains foreign execution entries")
	}
	worktreeInfo, err := os.Lstat(expectedWorktree)
	if err != nil {
		return err
	}
	if worktreeInfo.Mode()&os.ModeSymlink != 0 || !worktreeInfo.IsDir() {
		return errors.New("recovered initialization worktree has a foreign type")
	}
	return nil
}

func (t *rootInitializationTransaction) initializationWorktreeRootIdentity() (os.FileInfo, error) {
	if t == nil || strings.TrimSpace(t.worktreePath) == "" {
		return nil, nil
	}
	info, err := os.Lstat(t.worktreePath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("initialization worktree root changed type before cleanup")
	}
	relative, err := rootInitializationRelativePath(t.sessionRoot, t.worktreePath)
	if err != nil {
		return nil, err
	}
	expected, exists := t.ownedEntries[relative]
	actual := rootInitializationOwnedEntry{
		Path:     relative,
		Kind:     "directory",
		Mode:     uint32(info.Mode().Perm()),
		Identity: rootInitializationFileIdentity(t.worktreePath, info),
	}
	if !exists ||
		expected.Identity == "" ||
		!rootInitializationEntryMatches(expected, actual, true) {
		return nil, errors.New("initialization worktree root changed identity before cleanup")
	}
	return info, nil
}

func (t *rootInitializationTransaction) addOwnedEntryWithParents(entry rootInitializationOwnedEntry) {
	if t.ownedEntries == nil {
		t.ownedEntries = map[string]rootInitializationOwnedEntry{}
	}
	t.ownedEntries[entry.Path] = entry
	for parent := filepath.Dir(entry.Path); parent != "."; parent = filepath.Dir(parent) {
		if _, exists := t.ownedEntries[parent]; exists {
			continue
		}
		if record, err := rootInitializationRecordPath(t.sessionRoot, parent); err == nil {
			t.ownedEntries[parent] = record
		}
	}
}

func rootInitializationCaptureSubtree(
	sessionRoot string,
	relativeRoot string,
) ([]rootInitializationOwnedEntry, error) {
	absoluteRoot := filepath.Join(sessionRoot, relativeRoot)
	if _, err := os.Lstat(absoluteRoot); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	records := make([]rootInitializationOwnedEntry, 0)
	err := filepath.WalkDir(absoluteRoot, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := rootInitializationRelativePath(sessionRoot, path)
		if err != nil {
			return err
		}
		record, err := rootInitializationRecordPath(sessionRoot, relative)
		if err != nil {
			return err
		}
		records = append(records, record)
		if item.Type()&os.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(left int, right int) bool { return records[left].Path < records[right].Path })
	return records, nil
}

func rootInitializationRecordPath(sessionRoot string, relative string) (rootInitializationOwnedEntry, error) {
	relative = filepath.Clean(relative)
	if err := validateRootInitializationOwnedEntry(rootInitializationOwnedEntry{
		Path: relative,
		Kind: "directory",
	}); err != nil {
		return rootInitializationOwnedEntry{}, err
	}
	path := filepath.Join(sessionRoot, relative)
	if err := rejectInitializationSymlinkComponents(filepath.Dir(path)); err != nil {
		return rootInitializationOwnedEntry{}, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return rootInitializationOwnedEntry{}, err
	}
	entry := rootInitializationOwnedEntry{
		Path:     relative,
		Mode:     uint32(before.Mode().Perm()),
		Identity: rootInitializationFileIdentity(path, before),
	}
	switch {
	case before.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return rootInitializationOwnedEntry{}, err
		}
		entry.Kind = "symlink"
		entry.Size = int64(len([]byte(target)))
		entry.Digest = rootInitializationDigestBytes([]byte(target))
	case before.IsDir():
		entry.Kind = "directory"
	case before.Mode().IsRegular():
		entry.Kind = "regular"
		entry.Size = before.Size()
		handle, err := os.Open(path)
		if err != nil {
			return rootInitializationOwnedEntry{}, err
		}
		hasher := sha256.New()
		written, readErr := io.CopyN(hasher, handle, entry.Size)
		var extra [1]byte
		extraCount, extraErr := handle.Read(extra[:])
		closeErr := handle.Close()
		if extraErr != nil && !errors.Is(extraErr, io.EOF) {
			readErr = errors.Join(readErr, extraErr)
		}
		if readErr != nil || closeErr != nil || written != entry.Size || extraCount != 0 {
			return rootInitializationOwnedEntry{}, errors.Join(readErr, closeErr, errors.New("initialization ownership file changed while it was read"))
		}
		entry.Digest = contracts.DigestPrefix + hex.EncodeToString(hasher.Sum(nil))
	default:
		return rootInitializationOwnedEntry{}, fmt.Errorf("initialization path %s has unsupported mode %s", relative, before.Mode())
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() {
		return rootInitializationOwnedEntry{}, errors.New("initialization ownership path changed while it was inventoried")
	}
	entry.Identity = rootInitializationFileIdentity(path, after)
	return entry, nil
}

func rootInitializationMaybeRecordPath(
	sessionRoot string,
	relative string,
) (rootInitializationOwnedEntry, bool, error) {
	path := filepath.Join(sessionRoot, filepath.Clean(relative))
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return rootInitializationOwnedEntry{}, false, nil
	} else if err != nil {
		return rootInitializationOwnedEntry{}, false, err
	}
	entry, err := rootInitializationRecordPath(sessionRoot, relative)
	return entry, err == nil, err
}

func rootInitializationEntryMatches(
	expected rootInitializationOwnedEntry,
	actual rootInitializationOwnedEntry,
	requireIdentity bool,
) bool {
	if expected.Path != actual.Path || expected.Kind != actual.Kind {
		return false
	}
	if expected.Identity != "" {
		// Once a write has completed, the durable file identity is the
		// ownership proof. In-place mode or content tampering still changes
		// transaction-owned storage and may be removed; replacement at the
		// same lexical path has a different identity and is preserved.
		return expected.Identity == actual.Identity
	}
	if requireIdentity {
		return false
	}
	return (expected.Mode == 0 || expected.Mode == actual.Mode) &&
		expected.Size == actual.Size &&
		expected.Digest == actual.Digest
}

func (t *rootInitializationTransaction) validateOwnedInventory() error {
	if t == nil {
		return errors.New("root initialization transaction is required")
	}
	if err := rejectInitializationSymlinkComponents(t.sessionRoot); err != nil {
		return err
	}
	actual, err := rootInitializationCaptureSessionInventory(t)
	if err != nil {
		return err
	}
	for path, record := range actual {
		expected, exists := t.ownedEntries[path]
		if exists && rootInitializationEntryMatches(expected, record, expected.Identity != "") {
			continue
		}
		if t.pendingWrite != nil {
			if path == t.pendingWrite.Target.Path &&
				((t.pendingWrite.TemporaryIdentity != "" &&
					record.Identity == t.pendingWrite.TemporaryIdentity &&
					rootInitializationEntryMatches(t.pendingWrite.Target, record, false)) ||
					(t.pendingWrite.Previous != nil && rootInitializationEntryMatches(*t.pendingWrite.Previous, record, true))) {
				continue
			}
			if path == t.pendingWrite.TemporaryPath &&
				t.pendingWrite.TemporaryIdentity != "" &&
				record.Identity == t.pendingWrite.TemporaryIdentity {
				continue
			}
			if t.pendingWrite.Previous != nil &&
				path == t.pendingWrite.PreviousPath &&
				rootInitializationEntryMatches(
					*t.pendingWrite.Previous,
					rootInitializationEntryAtTargetPath(record, t.pendingWrite.Target.Path),
					true,
				) {
				continue
			}
			plannedDirectory := false
			for _, directory := range t.pendingWrite.Directories {
				if path == directory.Path && record.Kind == "directory" {
					plannedDirectory = true
					break
				}
			}
			if plannedDirectory {
				continue
			}
		}
		if t.pendingScope != nil {
			planned := false
			for _, candidate := range t.pendingScope.Planned {
				if path == candidate.Path && record.Kind == "directory" {
					planned = true
					break
				}
			}
			if planned {
				continue
			}
		}
		return fmt.Errorf("foreign session entry %s does not match persisted initialization ownership", path)
	}
	return nil
}

func rootInitializationCaptureSessionInventory(
	t *rootInitializationTransaction,
) (map[string]rootInitializationOwnedEntry, error) {
	result := map[string]rootInitializationOwnedEntry{}
	err := filepath.WalkDir(t.sessionRoot, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(t.sessionRoot, path)
		if err != nil {
			return err
		}
		relative = filepath.Clean(relative)
		if relative == "." {
			return nil
		}
		if t.isInitializationControlPath(relative) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		record, err := rootInitializationRecordPath(t.sessionRoot, relative)
		if err != nil {
			return err
		}
		result[relative] = record
		if item.Type()&os.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		return nil
	})
	return result, err
}

func (t *rootInitializationTransaction) isInitializationControlPath(relative string) bool {
	relative = filepath.Clean(relative)
	switch relative {
	case ".mutation.lock", rootInitializationClaimName, rootInitializationJournalName:
		return true
	}
	if relative == "."+rootInitializationJournalName+"."+t.token+".tmp" {
		return true
	}
	return false
}

func (t *rootInitializationTransaction) removeOwnedEntriesExact() error {
	paths := make([]string, 0, len(t.ownedEntries))
	for path := range t.ownedEntries {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(left int, right int) bool {
		leftDepth := strings.Count(paths[left], string(filepath.Separator))
		rightDepth := strings.Count(paths[right], string(filepath.Separator))
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return paths[left] > paths[right]
	})
	for _, relative := range paths {
		if relative == rootInitializationOriginName {
			continue
		}
		expected := t.ownedEntries[relative]
		actual, exists, err := rootInitializationMaybeRecordPath(t.sessionRoot, relative)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if !rootInitializationEntryMatches(expected, actual, expected.Identity != "") {
			return fmt.Errorf("initialization-owned entry %s changed before exact removal", relative)
		}
		if err := os.Remove(filepath.Join(t.sessionRoot, relative)); err != nil {
			return fmt.Errorf("remove initialization-owned entry %s: %w", relative, err)
		}
	}
	return nil
}

func (t *rootInitializationTransaction) ownershipDigest() (string, error) {
	paths := make([]string, 0, len(t.ownedEntries))
	for path := range t.ownedEntries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	records := make([]any, 0, len(paths))
	for _, path := range paths {
		records = append(records, t.ownedEntries[path].toMap())
	}
	canonical, err := contracts.CanonicalJSONBytes(records)
	if err != nil {
		return "", err
	}
	return rootInitializationDigestBytes(canonical), nil
}

func (t *rootInitializationTransaction) refreshOwnershipDigest() error {
	if t == nil {
		return errors.New("root initialization transaction is required")
	}
	digest, err := t.ownershipDigest()
	if err != nil {
		return err
	}
	t.ownedDigest = digest
	return nil
}

func (t *rootInitializationTransaction) validateClaimIdentity() error {
	if t == nil || t.claimIdentity == "" {
		return errors.New("root initialization claim identity is missing")
	}
	info, err := os.Lstat(filepath.Join(t.sessionRoot, rootInitializationClaimName))
	if err != nil {
		return err
	}
	claimPath := filepath.Join(t.sessionRoot, rootInitializationClaimName)
	if info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() ||
		rootInitializationFileIdentity(claimPath, info) != t.claimIdentity {
		return errors.New("root initialization claim file identity changed")
	}
	return nil
}

func (t *rootInitializationTransaction) lockMarker(state string) map[string]any {
	return map[string]any{
		"schema_version":          1,
		"transaction_token":       t.token,
		"canonical_session_root":  t.sessionRoot,
		"pre_existing_root":       t.preExistingRoot,
		"initialization_state":    state,
		"initialization_claim_id": t.claimIdentity,
	}
}

func (t *rootInitializationTransaction) releaseMutationLock() error {
	if t == nil || t.mutationLock == nil {
		return nil
	}
	return t.mutationLock.Unlock()
}

func runRootInitializationAfterMutation(stage string) error {
	if rootInitializationAfterMutation == nil {
		return nil
	}
	return rootInitializationAfterMutation(stage)
}

func (t *rootInitializationTransaction) compensateBootstrap() error {
	if t == nil {
		return nil
	}
	defer func() {
		_ = t.releaseMutationLock()
	}()
	if err := t.resolvePendingWrite(); err != nil {
		return err
	}
	if err := t.resolvePendingScope(); err != nil {
		return err
	}
	if err := validateRootInitializationClaim(t.sessionRoot, t.token); err != nil {
		return err
	}
	if err := t.validateClaimIdentity(); err != nil {
		return err
	}
	if err := t.validateOwnedInventory(); err != nil {
		t.cleanupState = "blocked_foreign_entries"
		_ = t.writeJournal()
		return err
	}
	worktreeRoot, err := t.initializationWorktreeRootIdentity()
	if err != nil {
		return err
	}
	cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := workspace.CleanupInitializationWorktree(
		cleanupContext,
		t.sourceGitRoot,
		t.worktreePath,
		t.headCommit,
		worktreeRoot,
	); err != nil {
		return err
	}
	if err := t.removeOwnedEntriesExact(); err != nil {
		return err
	}
	t.cleanupState = "entries_removed"
	if err := t.writeJournal(); err != nil {
		return err
	}
	return t.finalizeInitializationControls()
}

func (t *rootInitializationTransaction) finalizeInitializationControls() error {
	if t == nil || t.mutationLock == nil || t.mutationLock.file == nil {
		return errors.New("root initialization control cleanup requires its mutation lease")
	}
	if err := t.validateOwnedInventory(); err != nil {
		return err
	}
	if err := t.mutationLock.writeMarker(t.lockMarker("cleanup_complete")); err != nil {
		return err
	}
	t.st.SetFileMutationObserver(nil)
	if !t.preExistingRoot {
		if _, hasOrigin := t.ownedEntries[rootInitializationOriginName]; !hasOrigin {
			return t.finalizeLegacyToolCreatedRoot()
		}
	}
	if err := removeRootInitializationClaim(t.sessionRoot, t.token); err != nil {
		return err
	}
	journalPath := filepath.Join(t.sessionRoot, rootInitializationJournalName)
	if info, err := os.Lstat(journalPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("root initialization journal changed before final removal")
		}
		if err := os.Remove(journalPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	journalTemporaryPath := filepath.Join(t.sessionRoot, "."+rootInitializationJournalName+"."+t.token+".tmp")
	if info, err := os.Lstat(journalTemporaryPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("root initialization journal temporary path changed before final removal")
		}
		if err := os.Remove(journalTemporaryPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	lock := t.mutationLock
	if err := lock.Unlock(); err != nil {
		return err
	}
	if err := lock.removePathAfterUnlock(); err != nil {
		return err
	}
	entries, err := os.ReadDir(t.sessionRoot)
	if err != nil {
		return err
	}
	if t.preExistingRoot {
		if len(entries) != 0 {
			return errors.New("root initialization session contains foreign entries after exact cleanup")
		}
		return nil
	}
	if !rootInitializationDestinationEntriesMatch(entries, rootInitializationOriginName) {
		return errors.New("root initialization session lost its origin marker before final removal")
	}
	if rootInitializationBeforeRootRemoval != nil {
		if err := rootInitializationBeforeRootRemoval(); err != nil {
			return err
		}
	}
	return t.removeToolCreatedRoot()
}

func (t *rootInitializationTransaction) removeBootstrapControls() error {
	if t == nil {
		return nil
	}
	t.st.SetFileMutationObserver(nil)
	var failures []error
	for _, path := range []string{
		filepath.Join(t.sessionRoot, "meta.json"),
		filepath.Join(t.sessionRoot, rootInitializationJournalName),
		filepath.Join(t.sessionRoot, "."+rootInitializationJournalName+"."+t.token+".tmp"),
	} {
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
				failures = append(failures, fmt.Errorf("bootstrap control path %s changed type", path))
				continue
			}
			failures = append(failures, os.Remove(path))
		} else if !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	failures = append(failures, removeRootInitializationClaim(t.sessionRoot, t.token))
	if t.mutationLock != nil {
		failures = append(failures, t.mutationLock.Unlock())
		failures = append(failures, t.mutationLock.removePathAfterUnlock())
	}
	if !t.preExistingRoot {
		failures = append(failures, t.removeToolCreatedRoot())
	}
	return errors.Join(failures...)
}

func (t *rootInitializationTransaction) removeToolCreatedRoot() error {
	if t == nil || t.preExistingRoot {
		return errors.New("root initialization destination origin is not available for removal")
	}
	origin, exists := t.ownedEntries[rootInitializationOriginName]
	if !exists {
		return errors.New("root initialization destination origin is missing from the ownership inventory")
	}
	return removeRootInitializationDestinationOriginAndRoot(t.sessionRoot, origin)
}

func (t *rootInitializationTransaction) finalizeLegacyToolCreatedRoot() error {
	if t == nil || t.mutationLock == nil || t.mutationLock.file == nil {
		return errors.New("legacy root initialization cleanup requires its mutation lease")
	}
	lock := t.mutationLock
	if err := lock.Unlock(); err != nil {
		return err
	}
	parent := filepath.Dir(t.sessionRoot)
	holdingRoot, err := os.MkdirTemp(parent, "relay-initialization-cleanup-")
	if err != nil {
		return err
	}
	removeHoldingRoot := true
	defer func() {
		if removeHoldingRoot {
			_ = os.Remove(holdingRoot)
		}
	}()
	if err := syncRootInitializationDirectory(parent); err != nil {
		return err
	}
	movedRoot := filepath.Join(holdingRoot, "session-root")
	if err := os.Rename(t.sessionRoot, movedRoot); err != nil {
		return err
	}
	removeHoldingRoot = false
	if err := syncRootInitializationDirectory(holdingRoot); err != nil {
		return err
	}
	if err := syncRootInitializationDirectory(parent); err != nil {
		return err
	}
	t.sessionRoot = movedRoot
	lock.sessionRoot = movedRoot
	lock.path = filepath.Join(movedRoot, ".mutation.lock")
	if err := removeRootInitializationClaim(movedRoot, t.token); err != nil {
		return err
	}
	journalPath := filepath.Join(movedRoot, rootInitializationJournalName)
	if info, err := os.Lstat(journalPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("root initialization journal changed before legacy final removal")
		}
		if err := os.Remove(journalPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	journalTemporaryPath := filepath.Join(movedRoot, "."+rootInitializationJournalName+"."+t.token+".tmp")
	if info, err := os.Lstat(journalTemporaryPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("root initialization journal temporary path changed before legacy final removal")
		}
		if err := os.Remove(journalTemporaryPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := lock.removePathAfterUnlock(); err != nil {
		return err
	}
	entries, err := os.ReadDir(movedRoot)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("legacy root initialization session contains foreign entries after exact cleanup")
	}
	if err := os.Remove(movedRoot); err != nil {
		return err
	}
	if err := syncRootInitializationDirectory(holdingRoot); err != nil {
		return err
	}
	if err := os.Remove(holdingRoot); err != nil {
		return err
	}
	return syncRootInitializationDirectory(parent)
}

func (t *rootInitializationTransaction) ownedEntriesPayload() []any {
	paths := make([]string, 0, len(t.ownedEntries))
	for path := range t.ownedEntries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	result := make([]any, 0, len(paths))
	for _, path := range paths {
		result = append(result, t.ownedEntries[path].toMap())
	}
	return result
}

func parseRootInitializationOwnedEntries(value any) (map[string]rootInitializationOwnedEntry, error) {
	raw, ok := value.([]any)
	if !ok {
		return nil, errors.New("initialization ownership manifest must be an array")
	}
	result := make(map[string]rootInitializationOwnedEntry, len(raw))
	for _, item := range raw {
		entry, err := rootInitializationOwnedEntryFromMap(item)
		if err != nil {
			return nil, err
		}
		if _, exists := result[entry.Path]; exists {
			return nil, fmt.Errorf("initialization ownership path %s is duplicated", entry.Path)
		}
		result[entry.Path] = entry
	}
	return result, nil
}

func rootInitializationRelativePath(sessionRoot string, path string) (string, error) {
	absolute, err := initializationLexicalPath(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(sessionRoot, absolute)
	if err != nil ||
		relative == "." ||
		filepath.IsAbs(relative) ||
		relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("initialization mutation path escapes the session root")
	}
	return filepath.Clean(relative), nil
}

func rootInitializationPathIsAncestor(ancestor string, descendant string) bool {
	relative, err := filepath.Rel(filepath.Clean(ancestor), filepath.Clean(descendant))
	return err == nil &&
		relative != "." &&
		!filepath.IsAbs(relative) &&
		relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func rootInitializationPathAllowed(relative string) bool {
	relative = filepath.Clean(relative)
	top := relative
	if separator := strings.IndexRune(relative, filepath.Separator); separator >= 0 {
		top = relative[:separator]
	}
	for _, allowed := range rootInitializationOwnedEntries {
		if top == allowed {
			return true
		}
	}
	if strings.HasPrefix(top, ".") &&
		strings.Contains(top, ".initialization-") &&
		(strings.HasSuffix(top, ".tmp") || strings.HasSuffix(top, ".previous")) {
		base := strings.TrimPrefix(strings.SplitN(top, ".initialization-", 2)[0], ".")
		for _, allowed := range rootInitializationOwnedEntries {
			if base == allowed {
				return true
			}
		}
	}
	return false
}

func rootInitializationDigestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return contracts.DigestPrefix + hex.EncodeToString(sum[:])
}

func rootInitializationOwnershipDigestForPayload(value any) (string, error) {
	entries, err := parseRootInitializationOwnedEntries(value)
	if err != nil {
		return "", err
	}
	transaction := &rootInitializationTransaction{ownedEntries: entries}
	return transaction.ownershipDigest()
}

func int64FromAny(value any, fallback int64) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		if typed >= 0 && typed == float64(int64(typed)) {
			return int64(typed)
		}
	case string:
		if parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil {
			return parsed
		}
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed
		}
	}
	return fallback
}

func equalRootInitializationEntry(left, right rootInitializationOwnedEntry) bool {
	leftBytes, _ := contracts.CanonicalJSONBytes(left.toMap())
	rightBytes, _ := contracts.CanonicalJSONBytes(right.toMap())
	return bytes.Equal(leftBytes, rightBytes)
}
