package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const StopReasonSourceMutated = "source_mutated"

// SourceMutatedError reports that the original source changed while an
// isolated execution was active. The execution result and transcript remain
// persisted; callers use this error to make the terminal failure observable.
type SourceMutatedError struct {
	BeforeDigest string
	AfterDigest  string
}

func (e *SourceMutatedError) Error() string {
	if e == nil {
		return "source workspace mutated during isolated execution"
	}
	return fmt.Sprintf("source workspace mutated during isolated execution (%s -> %s)", e.BeforeDigest, e.AfterDigest)
}

// Finalization is the durable source-integrity result for a persisted
// execution workspace. Managed is false for ordinary sessions that have no
// execution_workspace artifact.
type Finalization struct {
	Managed            bool
	SourceBeforeDigest string
	SourceAfterDigest  string
	SourceChanged      bool
	SourceMutated      bool
	EffectivePolicy    string
	AchievedPolicy     string
	Artifact           map[string]any
	ArtifactRef        map[string]any
}

func (f *Finalization) MutationError() error {
	if f == nil || !f.SourceMutated {
		return nil
	}
	return &SourceMutatedError{BeforeDigest: f.SourceBeforeDigest, AfterDigest: f.SourceAfterDigest}
}

// CleanupResult describes Git cleanup performed before the caller deletes a
// session. Cleanup never removes the session directory itself.
type CleanupResult struct {
	Managed           bool
	WorktreePath      string
	RegistrationFound bool
	WorktreeRemoved   bool
	MetadataPruned    bool
}

// CleanupInitializationWorktree removes only the exact detached registration
// captured by an initialization journal. A mismatched live registration is
// never removed.
func CleanupInitializationWorktree(
	ctx context.Context,
	sourceGitRoot string,
	worktreePath string,
	expectedHead string,
) error {
	if strings.TrimSpace(sourceGitRoot) == "" || strings.TrimSpace(worktreePath) == "" {
		return nil
	}
	repository, err := repositoryForCleanup(ctx, sourceGitRoot)
	if err != nil {
		return err
	}
	target, err := canonicalPathAllowMissing(worktreePath)
	if err != nil {
		return err
	}
	record, registered, err := repositoryWorktreeRegistration(ctx, repository, target)
	if err != nil {
		return err
	}
	if registered {
		if record.Bare || (!record.Prunable && (!record.Detached || record.Branch != "" || strings.TrimSpace(record.Head) != strings.TrimSpace(expectedHead))) {
			return contracts.NewValidationError("initialization worktree registration does not match its journal")
		}
		if !record.Prunable {
			if _, err := runGit(ctx, repository.gitBinary, repository.root, "worktree", "remove", "--force", target); err != nil {
				return err
			}
		}
	}
	if _, err := runGit(ctx, repository.gitBinary, repository.root, "worktree", "prune", "--expire", "now"); err != nil {
		return err
	}
	if stillRegistered, err := repositoryWorktreeRegistered(ctx, repository, target); err != nil {
		return err
	} else if stillRegistered {
		return contracts.NewValidationError("initialization worktree registration remained after cleanup")
	}
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return contracts.NewValidationError("initialization worktree path became a symlink")
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Recover validates the persisted workspace boundary without changing it.
// Isolated execution requires the exact detached worktree captured at launch;
// inherited execution requires its recorded launch directory. The returned
// Materialized value is reconstructed only from digest-checked session state.
func Recover(ctx context.Context, st *store.Store) (*Materialized, error) {
	artifact, ref, found, err := loadExecutionWorkspace(st)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, contracts.NewValidationError("root recovery requires an execution_workspace artifact")
	}
	paths, err := validatePersistedWorkspace(st, artifact)
	if err != nil {
		return nil, err
	}
	policy, err := workspacePolicy(artifact)
	if err != nil {
		return nil, err
	}
	identity, _ := artifact["identity"].(map[string]any)
	recordedExecutionCWD := strings.TrimSpace(stringValue(identity["execution_cwd"]))
	if recordedExecutionCWD == "" {
		return nil, contracts.NewValidationError("execution_workspace identity is missing execution_cwd")
	}

	if policy.achieved == PolicyInherited {
		if paths.worktreePath != "" {
			return nil, contracts.NewValidationError("inherited execution_workspace unexpectedly records a worktree path")
		}
		executionCWD, err := canonicalExistingDirectory(recordedExecutionCWD)
		if err != nil {
			return nil, fmt.Errorf("resolve inherited recovery directory: %w", err)
		}
		if paths.sourceLaunchCWD != "" && !pathsEquivalent(executionCWD, paths.sourceLaunchCWD) {
			return nil, contracts.NewValidationError("inherited execution_workspace recovery directory changed")
		}
		return &Materialized{
			ExecutionCWD: executionCWD,
			Artifact:     cloneMap(artifact),
			ArtifactRef:  cloneMap(ref),
		}, nil
	}

	if paths.sourceGitRoot == "" || paths.worktreePath == "" {
		return nil, contracts.NewValidationError("isolated root recovery requires its retained worktree")
	}
	info, err := os.Lstat(paths.worktreePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, contracts.NewValidationError("required retained execution worktree is absent")
		}
		return nil, fmt.Errorf("inspect retained execution worktree: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, contracts.NewValidationError("required retained execution worktree must be a real directory")
	}
	expectedHead, err := persistedDetachedWorktreeHead(artifact)
	if err != nil {
		return nil, err
	}
	repository, err := repositoryForCleanup(ctx, paths.sourceGitRoot)
	if err != nil {
		return nil, err
	}
	record, registered, err := repositoryWorktreeRegistration(ctx, repository, paths.worktreePath)
	if err != nil {
		return nil, fmt.Errorf("inspect retained execution worktree registration: %w", err)
	}
	if !registered || record.Prunable || record.Bare || !record.Detached || record.Branch != "" || strings.TrimSpace(record.Head) != expectedHead {
		return nil, contracts.NewValidationError("required retained execution worktree registration no longer matches the persisted workspace")
	}
	worktreeRoot, err := canonicalExistingDirectory(paths.worktreePath)
	if err != nil || !pathsEquivalent(worktreeRoot, paths.worktreePath) {
		return nil, contracts.NewValidationError("required retained execution worktree path changed")
	}
	rawDescriptor, hasRawDescriptor, err := rawExportDescriptorFromArtifact(artifact)
	if err != nil {
		return nil, contracts.NewValidationError("execution_workspace committed export descriptor is invalid: %v", err)
	}
	if hasRawDescriptor {
		if err := verifyRawExport(ctx, worktreeRoot, rawDescriptor); err != nil {
			return nil, contracts.NewValidationError("required retained execution worktree content changed: %v", err)
		}
	}
	headOutput, err := runGit(ctx, repository.gitBinary, worktreeRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(headOutput)) != expectedHead {
		return nil, contracts.NewValidationError("required retained execution worktree HEAD changed")
	}
	if base, ok := artifact["base"].(map[string]any); ok {
		expectedTree := strings.TrimSpace(stringValue(base["head_tree"]))
		if expectedTree != "" {
			treeOutput, treeErr := runGit(ctx, repository.gitBinary, worktreeRoot, "rev-parse", "--verify", "HEAD^{tree}")
			if treeErr != nil || strings.TrimSpace(string(treeOutput)) != expectedTree {
				return nil, contracts.NewValidationError("required retained execution worktree tree changed")
			}
		}
	}
	executionCWD, err := canonicalExistingDirectory(recordedExecutionCWD)
	if err != nil || !pathContains(worktreeRoot, executionCWD) {
		return nil, contracts.NewValidationError("root recovery execution directory is not inside the retained worktree")
	}
	return &Materialized{
		ExecutionCWD: executionCWD,
		WorktreePath: worktreeRoot,
		Artifact:     cloneMap(artifact),
		ArtifactRef:  cloneMap(ref),
	}, nil
}

// Finalize recomputes the original source inventory once and persists the
// source-after digest in a new execution_workspace artifact revision. A
// changed digest only becomes source_mutated for isolated execution;
// inherited execution records the observation without changing semantics.
func Finalize(ctx context.Context, st *store.Store) (*Finalization, error) {
	artifact, ref, found, err := loadExecutionWorkspace(st)
	if err != nil {
		return nil, err
	}
	if !found {
		return &Finalization{}, nil
	}
	paths, err := validatePersistedWorkspace(st, artifact)
	if err != nil {
		return nil, err
	}
	policy, err := workspacePolicy(artifact)
	if err != nil {
		return nil, err
	}
	before, _ := artifact["source_before_digest"].(string)
	if existing, ok := artifact["source_after_digest"].(string); ok && strings.TrimSpace(existing) != "" {
		return finalizationFromArtifact(artifact, ref, policy, before, existing), nil
	}
	if before == "" && artifact["source_check"] == "not_applicable" {
		return finalizationFromArtifact(artifact, ref, policy, "", ""), nil
	}

	next := cloneMap(artifact)
	next["source_checked_at"] = lifecycleTimestamp()
	if before == "" {
		next["source_check"] = "not_applicable"
		next["source_changed"] = false
		next["source_mutated"] = false
		updated, updatedRef, err := persistExecutionWorkspace(st, next)
		if err != nil {
			return nil, err
		}
		return finalizationFromArtifact(updated, updatedRef, policy, "", ""), nil
	}
	if paths.sourceLaunchCWD == "" || paths.sourceGitRoot == "" {
		return nil, contracts.NewValidationError("execution_workspace with source_before_digest requires persisted source Git paths")
	}

	// The source digest covers the complete repository and does not depend on
	// the launch subdirectory. Inventory from the stable Git root so deleting
	// or replacing that subdirectory is classified as source mutation instead
	// of making terminal finalization impossible.
	inventoryLimits, err := persistedRepositoryInventoryLimits(artifact)
	if err != nil {
		return nil, err
	}
	repository, err := inspectRepositoryWithLimits(ctx, "git", paths.sourceGitRoot, inventoryLimits)
	if err != nil {
		return nil, fmt.Errorf("recompute source workspace inventory: %w", err)
	}
	if !pathsEquivalent(repository.root, paths.sourceGitRoot) {
		return nil, contracts.NewValidationError("execution_workspace source Git root changed from %s to %s", paths.sourceGitRoot, repository.root)
	}
	repository.sourceReport["launch_cwd"] = paths.sourceLaunchCWD
	repository.sourceReport["launch_subpath"] = paths.launchSubpath
	after := repository.sourceDigest
	changed := before != after
	mutated := changed && policy.achieved != PolicyInherited
	next["source_after_digest"] = after
	next["source_after"] = cloneMap(repository.sourceReport)
	next["source_changed"] = changed
	next["source_mutated"] = mutated
	next["source_check"] = "complete"
	updated, updatedRef, err := persistExecutionWorkspace(st, next)
	if err != nil {
		return nil, err
	}
	return finalizationFromArtifact(updated, updatedRef, policy, before, after), nil
}

func persistedRepositoryInventoryLimits(artifact map[string]any) (repositoryInventoryLimits, error) {
	defaults := repositoryInventoryLimits{
		maxFiles: 100_000,
		maxBytes: 2 * 1024 * 1024 * 1024,
	}
	raw, exists := artifact["repository_inventory_limits"]
	if !exists {
		return defaults, nil
	}
	payload, ok := raw.(map[string]any)
	if !ok {
		return repositoryInventoryLimits{}, contracts.NewValidationError("execution_workspace repository inventory limits must be an object")
	}
	maxFiles, filesOK := nonnegativeInt64(payload["max_files"])
	maxBytes, bytesOK := nonnegativeInt64(payload["max_bytes"])
	if !filesOK || !bytesOK || maxFiles < 1 || maxBytes < 1 {
		return repositoryInventoryLimits{}, contracts.NewValidationError("execution_workspace repository inventory limits must be positive")
	}
	return repositoryInventoryLimits{maxFiles: maxFiles, maxBytes: maxBytes}, nil
}

// Cleanup removes a persisted detached worktree registration with
// `git worktree remove --force`, prunes stale metadata, and verifies the
// registration is gone. It is safe to retry after a partial failure.
func Cleanup(ctx context.Context, st *store.Store) (*CleanupResult, error) {
	artifact, _, found, err := loadExecutionWorkspace(st)
	if err != nil {
		return nil, err
	}
	if !found {
		return &CleanupResult{}, nil
	}
	paths, err := validatePersistedWorkspace(st, artifact)
	if err != nil {
		return nil, err
	}
	policy, err := workspacePolicy(artifact)
	if err != nil {
		return nil, err
	}
	result := &CleanupResult{Managed: true, WorktreePath: paths.worktreePath}
	if policy.achieved == PolicyInherited {
		if paths.worktreePath != "" {
			return nil, contracts.NewValidationError("inherited execution_workspace unexpectedly records a worktree path")
		}
		return result, nil
	}
	if paths.sourceGitRoot == "" || paths.worktreePath == "" {
		return nil, contracts.NewValidationError("isolated execution_workspace requires source Git root and worktree paths")
	}
	expectedHead, err := persistedDetachedWorktreeHead(artifact)
	if err != nil {
		return nil, err
	}

	repository, err := repositoryForCleanup(ctx, paths.sourceGitRoot)
	if err != nil {
		return nil, err
	}
	record, registered, err := repositoryWorktreeRegistration(ctx, repository, paths.worktreePath)
	if err != nil {
		return nil, fmt.Errorf("inspect execution worktree registration: %w", err)
	}
	result.RegistrationFound = registered
	if registered {
		if record.Bare {
			return nil, contracts.NewValidationError("execution workspace registration unexpectedly identifies a bare worktree")
		}
		if !record.Prunable {
			if !record.Detached || record.Branch != "" || strings.TrimSpace(record.Head) != expectedHead {
				return nil, contracts.NewValidationError("execution worktree registration no longer matches the persisted detached workspace")
			}
			if _, err := runGit(ctx, repository.gitBinary, repository.root, "worktree", "remove", "--force", paths.worktreePath); err != nil {
				return nil, fmt.Errorf("remove execution worktree: %w", err)
			}
			result.WorktreeRemoved = true
		}
	}
	if _, err := runGit(ctx, repository.gitBinary, repository.root, "worktree", "prune", "--expire", "now"); err != nil {
		return nil, fmt.Errorf("prune execution worktree metadata: %w", err)
	}
	result.MetadataPruned = true
	if stillRegistered, err := repositoryWorktreeRegistered(ctx, repository, paths.worktreePath); err != nil {
		return nil, fmt.Errorf("verify execution worktree cleanup: %w", err)
	} else if stillRegistered {
		return nil, errors.New("execution worktree remains registered after cleanup")
	}
	return result, nil
}

func persistedDetachedWorktreeHead(artifact map[string]any) (string, error) {
	registration, ok := artifact["registration"].(map[string]any)
	if !ok {
		return "", contracts.NewValidationError("isolated execution_workspace registration must be an object")
	}
	registered, registeredOK := registration["registered"].(bool)
	detached, detachedOK := registration["detached"].(bool)
	writable, writableOK := registration["writable"].(bool)
	head := strings.TrimSpace(stringValue(registration["head_commit"]))
	if registration["mode"] != "detached_worktree" || !registeredOK || !registered || !detachedOK || !detached || !writableOK || !writable || head == "" {
		return "", contracts.NewValidationError("isolated execution_workspace registration is inconsistent with a detached worktree")
	}
	base, ok := artifact["base"].(map[string]any)
	if !ok || strings.TrimSpace(stringValue(base["head_commit"])) != head {
		return "", contracts.NewValidationError("execution_workspace registration HEAD does not match its persisted base")
	}
	return head, nil
}

type persistedWorkspacePaths struct {
	sessionDir      string
	sourceGitRoot   string
	sourceLaunchCWD string
	launchSubpath   string
	worktreePath    string
}

type persistedWorkspacePolicy struct {
	effective string
	achieved  string
}

func loadExecutionWorkspace(st *store.Store) (map[string]any, map[string]any, bool, error) {
	if st == nil || strings.TrimSpace(st.Root) == "" {
		return nil, nil, false, contracts.NewValidationError("execution workspace lifecycle requires a session store")
	}
	present, err := executionWorkspaceStatePresent(st)
	if err != nil {
		return nil, nil, false, err
	}
	if !present {
		return nil, nil, false, nil
	}
	ref, found, err := latestExecutionWorkspaceRef(st)
	if err != nil {
		return nil, nil, false, err
	}
	if !found {
		return nil, nil, false, contracts.NewValidationError("session has execution workspace state but no execution_workspace artifact ref")
	}
	payload, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, nil, false, fmt.Errorf("load execution_workspace artifact: %w", err)
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, contracts.RootArtifactKindExecutionWorkspace, 0, payload); err != nil {
		return nil, nil, false, err
	}
	if err := verifyWorkspaceIdentity(payload); err != nil {
		return nil, nil, false, err
	}
	return cloneMap(payload), cloneMap(ref), true, nil
}

func executionWorkspaceStatePresent(st *store.Store) (bool, error) {
	for _, path := range []string{
		filepath.Join(st.Root, "execution", "worktree"),
		filepath.Join(st.Root, "artifacts", executionWorkspaceCategory),
	} {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("inspect execution workspace state: %w", err)
		}
	}
	if meta, err := st.LoadMeta(); err == nil && meta.Get("execution_workspace_ref") != nil {
		return true, nil
	}
	graph := st.LoadGraph()
	if artifacts, ok := graph["artifacts"].(map[string]any); ok {
		_, exists := artifacts[executionWorkspaceCategory+"/selected"]
		return exists, nil
	}
	return false, nil
}

func latestExecutionWorkspaceRef(st *store.Store) (map[string]any, bool, error) {
	index, indexExists, err := strictLifecycleArtifactIndex(st)
	if err != nil {
		return nil, false, err
	}
	entries, _ := index["entries"].([]any)
	var latest map[string]any
	for _, rawEntry := range entries {
		entry, _ := rawEntry.(map[string]any)
		candidate, _ := entry["ref"].(map[string]any)
		if candidate["id"] != "execution_workspace:selected" {
			continue
		}
		validated, err := contracts.ValidateArtifactRef(candidate)
		if err != nil {
			return nil, false, err
		}
		latest = validated
	}
	if latest != nil {
		return latest, true, nil
	}

	graph := st.LoadGraph()
	if artifacts, ok := graph["artifacts"].(map[string]any); ok {
		if rawEntry, exists := artifacts[executionWorkspaceCategory+"/selected"]; exists {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				return nil, false, contracts.NewValidationError("execution_workspace graph entry must be an object")
			}
			ref, ok := entry["ref"].(map[string]any)
			if !ok {
				return nil, false, contracts.NewValidationError("execution_workspace graph entry is missing its artifact ref")
			}
			validated, err := contracts.ValidateArtifactRef(ref)
			return validated, err == nil, err
		}
	}
	if meta, err := st.LoadMeta(); err == nil {
		if rawRef := meta.Get("execution_workspace_ref"); rawRef != nil {
			ref, ok := rawRef.(map[string]any)
			if !ok {
				return nil, false, contracts.NewValidationError("execution_workspace_ref must be an artifact ref")
			}
			validated, err := contracts.ValidateArtifactRef(ref)
			return validated, err == nil, err
		}
	}
	if indexExists {
		return nil, false, nil
	}

	return nil, false, nil
}

func strictLifecycleArtifactIndex(st *store.Store) (map[string]any, bool, error) {
	path := filepath.Join(st.Root, "artifacts", store.ArtifactIndexFilename)
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{"entries": []any{}}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read artifact index for execution workspace lifecycle: %w", err)
	}
	value, err := contracts.DecodeJSONBytes(body)
	if err != nil {
		return nil, false, fmt.Errorf("decode artifact index for execution workspace lifecycle: %w", err)
	}
	index, err := contracts.ValidateArtifactIndex(value)
	if err != nil {
		return nil, false, fmt.Errorf("validate artifact index for execution workspace lifecycle: %w", err)
	}
	return index, true, nil
}

func validatePersistedWorkspace(st *store.Store, artifact map[string]any) (persistedWorkspacePaths, error) {
	identity, ok := artifact["identity"].(map[string]any)
	if !ok {
		return persistedWorkspacePaths{}, contracts.NewValidationError("execution_workspace identity must be an object")
	}
	if info, err := os.Lstat(st.Root); err != nil {
		return persistedWorkspacePaths{}, fmt.Errorf("inspect session store: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return persistedWorkspacePaths{}, contracts.NewValidationError("execution_workspace session store must be a real directory")
	}
	storeRoot, err := canonicalExistingDirectory(st.Root)
	if err != nil {
		return persistedWorkspacePaths{}, fmt.Errorf("resolve session store: %w", err)
	}
	storedSession, _ := identity["session_dir"].(string)
	canonicalSession, err := canonicalExistingDirectory(storedSession)
	if err != nil || !pathsEquivalent(canonicalSession, storeRoot) {
		return persistedWorkspacePaths{}, contracts.NewValidationError("execution_workspace session directory does not match its store")
	}
	paths := persistedWorkspacePaths{
		sessionDir:      storeRoot,
		sourceGitRoot:   strings.TrimSpace(stringValue(identity["source_git_root"])),
		sourceLaunchCWD: strings.TrimSpace(stringValue(identity["source_launch_cwd"])),
		launchSubpath:   strings.TrimSpace(stringValue(identity["launch_subpath"])),
		worktreePath:    strings.TrimSpace(stringValue(identity["worktree_path"])),
	}
	if paths.worktreePath != "" {
		if err := rejectSymlinksBelow(storeRoot, "execution", "worktree"); err != nil {
			return persistedWorkspacePaths{}, err
		}
		canonicalWorktree, err := canonicalPathAllowMissing(paths.worktreePath)
		if err != nil {
			return persistedWorkspacePaths{}, fmt.Errorf("resolve persisted execution worktree: %w", err)
		}
		expectedWorktree, err := canonicalPathAllowMissing(filepath.Join(storeRoot, "execution", "worktree"))
		if err != nil || canonicalWorktree != expectedWorktree {
			return persistedWorkspacePaths{}, contracts.NewValidationError("execution_workspace worktree path is outside its fixed session location")
		}
		paths.worktreePath = canonicalWorktree
	}
	if paths.sourceGitRoot != "" {
		if info, err := os.Lstat(paths.sourceGitRoot); err != nil {
			return persistedWorkspacePaths{}, fmt.Errorf("inspect persisted source Git root: %w", err)
		} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return persistedWorkspacePaths{}, contracts.NewValidationError("execution_workspace source Git root must be a real directory")
		}
		canonicalRoot, err := canonicalExistingDirectory(paths.sourceGitRoot)
		if err != nil {
			return persistedWorkspacePaths{}, fmt.Errorf("resolve persisted source Git root: %w", err)
		}
		paths.sourceGitRoot = canonicalRoot
	}
	if paths.sourceLaunchCWD != "" && paths.sourceGitRoot != "" {
		relative := filepath.Clean(filepath.FromSlash(paths.launchSubpath))
		if paths.launchSubpath == "" || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return persistedWorkspacePaths{}, contracts.NewValidationError("execution_workspace launch subpath is outside its Git root")
		}
		expectedLaunch := filepath.Clean(filepath.Join(paths.sourceGitRoot, relative))
		storedLaunch, err := filepath.Abs(paths.sourceLaunchCWD)
		if err != nil || !pathsEquivalent(filepath.Clean(storedLaunch), expectedLaunch) {
			return persistedWorkspacePaths{}, contracts.NewValidationError("execution_workspace launch subpath does not match persisted source paths")
		}
		paths.sourceLaunchCWD = filepath.Clean(storedLaunch)
	}
	return paths, nil
}

func rejectSymlinksBelow(root string, components ...string) error {
	current := root
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect execution workspace path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return contracts.NewValidationError("execution workspace path %s must not be a symlink", current)
		}
		if !info.IsDir() {
			return contracts.NewValidationError("execution workspace path %s must be a directory", current)
		}
	}
	return nil
}

func workspacePolicy(artifact map[string]any) (persistedWorkspacePolicy, error) {
	payload, ok := artifact["policy"].(map[string]any)
	if !ok {
		return persistedWorkspacePolicy{}, contracts.NewValidationError("execution_workspace policy must be an object")
	}
	policy := persistedWorkspacePolicy{
		effective: strings.TrimSpace(stringValue(payload["effective"])),
		achieved:  strings.TrimSpace(stringValue(payload["achieved"])),
	}
	effectiveRank, effectiveOK := policyRank(policy.effective)
	if !effectiveOK {
		return persistedWorkspacePolicy{}, contracts.NewValidationError("execution_workspace effective policy is invalid")
	}
	achievedRank, achievedOK := policyRank(policy.achieved)
	if !achievedOK {
		return persistedWorkspacePolicy{}, contracts.NewValidationError("execution_workspace achieved policy is invalid")
	}
	if policy.achieved != PolicyInherited && policy.achieved != PolicyEphemeral {
		return persistedWorkspacePolicy{}, contracts.NewValidationError("execution_workspace achieved policy must be inherited or ephemeral")
	}
	if achievedRank < effectiveRank {
		return persistedWorkspacePolicy{}, contracts.NewValidationError("execution_workspace achieved policy cannot be weaker than its effective policy")
	}
	return policy, nil
}

func repositoryForCleanup(ctx context.Context, root string) (*repositorySnapshot, error) {
	output, err := runGit(ctx, "git", root, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("validate persisted source Git root: %w", err)
	}
	discovered, err := canonicalExistingDirectory(strings.TrimSpace(string(output)))
	if err != nil || !pathsEquivalent(discovered, root) {
		return nil, contracts.NewValidationError("persisted source Git root no longer identifies the same repository")
	}
	return &repositorySnapshot{gitBinary: "git", root: discovered}, nil
}

func persistExecutionWorkspace(st *store.Store, artifact map[string]any) (map[string]any, map[string]any, error) {
	normalized, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindExecutionWorkspace, artifact)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyWorkspaceIdentity(normalized); err != nil {
		return nil, nil, err
	}
	identity, err := contracts.RootArtifactIdentityFor(contracts.RootArtifactKindExecutionWorkspace, 0)
	if err != nil {
		return nil, nil, err
	}
	ref, err := st.SaveContractArtifact(executionWorkspaceCategory, identity.ArtifactID, normalized, identity.RefID)
	if err != nil {
		return nil, nil, err
	}
	persisted, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, nil, err
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, contracts.RootArtifactKindExecutionWorkspace, 0, persisted); err != nil {
		return nil, nil, err
	}
	if err := verifyWorkspaceIdentity(persisted); err != nil {
		return nil, nil, err
	}
	return cloneMap(persisted), cloneMap(ref), nil
}

func finalizationFromArtifact(artifact map[string]any, ref map[string]any, policy persistedWorkspacePolicy, before string, after string) *Finalization {
	changed := before != "" && after != "" && before != after
	mutated := changed && policy.achieved != PolicyInherited
	return &Finalization{
		Managed:            true,
		SourceBeforeDigest: before,
		SourceAfterDigest:  after,
		SourceChanged:      changed,
		SourceMutated:      mutated,
		EffectivePolicy:    policy.effective,
		AchievedPolicy:     policy.achieved,
		Artifact:           cloneMap(artifact),
		ArtifactRef:        cloneMap(ref),
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func lifecycleTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}
