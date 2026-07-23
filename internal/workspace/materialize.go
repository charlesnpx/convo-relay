package workspace

import (
	"bytes"
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

const executionWorkspaceCategory = contracts.RootArtifactKindExecutionWorkspace

// Materialized is the verified workspace boundary consumed by execution.
// Artifact and ArtifactRef are cloned so callers cannot mutate persisted state.
type Materialized struct {
	ExecutionCWD string
	WorktreePath string
	Artifact     map[string]any
	ArtifactRef  map[string]any
}

type worktreeRegistration struct {
	Path     string
	Head     string
	Branch   string
	Detached bool
	Bare     bool
	Locked   bool
	Prunable bool
}

// Materialize crosses the session mutation boundary. Required isolation is
// achieved by a detached worktree at the exact commit captured by Preflight;
// inherited execution persists the same policy/source record without creating
// a worktree.
func Materialize(ctx context.Context, st *store.Store, snapshot *Snapshot) (*Materialized, error) {
	if st == nil || strings.TrimSpace(st.Root) == "" {
		return nil, workspaceError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, "/session_dir", "Workspace materialization requires a session store.", nil)
	}
	if snapshot == nil {
		return nil, workspaceError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, "", "Workspace materialization requires a preflight snapshot.", nil)
	}
	storeRoot, err := canonicalPathAllowMissing(st.Root)
	if err != nil || storeRoot != snapshot.sessionDir {
		return nil, workspaceError(
			err,
			DiagnosticCodeSessionConflict,
			contracts.DiagnosticPhasePolicy,
			"/session_dir",
			"The session store does not match the preflight session directory.",
			map[string]any{"preflight_session_dir": snapshot.sessionDir, "store_root": storeRoot},
		)
	}

	executionCWD := snapshot.launchCWD
	worktreePath := ""
	registration := inheritedRegistration()
	createdWorktree := false
	var rawDescriptor *rawExportDescriptor

	if snapshot.policy.Effective != PolicyInherited {
		if snapshot.repository == nil {
			return nil, workspaceError(nil, DiagnosticCodeGitRequired, contracts.DiagnosticPhasePreflight, "/launch_cwd", "Required workspace isolation needs a committed Git working tree.", nil)
		}
		if pathsOverlap(snapshot.repository.root, storeRoot) {
			return nil, workspaceError(
				nil,
				DiagnosticCodeSessionConflict,
				contracts.DiagnosticPhasePolicy,
				"/session_dir",
				"Required workspace isolation cannot place the session inside, equal to, or above the source Git worktree.",
				map[string]any{"git_root": snapshot.repository.root, "session_dir": storeRoot, "session_path_source": snapshot.sessionPathSource},
			)
		}

		executionRoot := filepath.Join(storeRoot, "execution")
		worktreePath = filepath.Join(executionRoot, "worktree")
		registered, err := repositoryWorktreeRegistered(ctx, snapshot.repository, worktreePath)
		if err != nil {
			return nil, workspaceError(err, DiagnosticCodeCreationFailed, contracts.DiagnosticPhasePreflight, "/session_dir", "The target Git worktree registration could not be inspected.", map[string]any{"worktree_path": worktreePath})
		}
		if registered {
			return nil, workspaceError(nil, DiagnosticCodeSessionConflict, contracts.DiagnosticPhasePolicy, "/session_dir", "The target execution worktree is already registered.", map[string]any{"worktree_path": worktreePath})
		}
		if _, err := os.Lstat(worktreePath); err == nil {
			return nil, workspaceError(nil, DiagnosticCodeCreationFailed, contracts.DiagnosticPhasePreflight, "/session_dir", "The target execution worktree path already exists.", map[string]any{"worktree_path": worktreePath})
		} else if !os.IsNotExist(err) {
			return nil, workspaceError(err, DiagnosticCodeCreationFailed, contracts.DiagnosticPhasePreflight, "/session_dir", "The target execution worktree path could not be inspected.", map[string]any{"worktree_path": worktreePath})
		}
		if err := os.MkdirAll(executionRoot, 0o755); err != nil {
			return nil, workspaceError(err, DiagnosticCodeCreationFailed, contracts.DiagnosticPhasePreflight, "/session_dir", "The session execution directory could not be created.", map[string]any{"execution_root": executionRoot})
		}
		canonicalExecutionRoot, err := canonicalExistingDirectory(executionRoot)
		if err != nil || canonicalExecutionRoot != executionRoot {
			return nil, workspaceError(err, DiagnosticCodeSessionConflict, contracts.DiagnosticPhasePolicy, "/session_dir", "The session execution directory changed after preflight.", map[string]any{"execution_root": executionRoot, "resolved_execution_root": canonicalExecutionRoot})
		}

		if _, err := runGit(
			ctx,
			snapshot.gitBinary,
			snapshot.repository.root,
			"worktree", "add", "--detach", "--no-checkout", worktreePath, snapshot.repository.headCommit,
		); err != nil {
			if rollbackErr := rollbackMaterializedWorktreeAfterFailure(snapshot.repository, worktreePath); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("partial worktree rollback failed: %w", rollbackErr))
			}
			return nil, workspaceError(err, DiagnosticCodeCreationFailed, contracts.DiagnosticPhasePreflight, "/session_dir", "The detached execution worktree could not be created.", map[string]any{"worktree_path": worktreePath, "head_commit": snapshot.repository.headCommit})
		}
		createdWorktree = true

		if _, err := runGit(
			ctx,
			snapshot.gitBinary,
			worktreePath,
			"read-tree", "--reset", snapshot.repository.headTree,
		); err != nil {
			return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The detached execution worktree index could not be initialized.")
		}
		rawDescriptor, err = exportCapturedTree(ctx, snapshot.repository, worktreePath, snapshot.inventoryLimits)
		if err != nil {
			return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The captured Git tree could not be exported from raw objects.")
		}
		executionCWD, registration, err = verifyMaterializedWorktree(ctx, snapshot.repository, worktreePath, rawDescriptor)
		if err != nil {
			return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The detached execution worktree failed its integrity checks.")
		}
	}

	artifact, err := workspaceArtifact(snapshot, storeRoot, worktreePath, executionCWD, registration, rawDescriptor)
	if err != nil {
		return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The execution workspace artifact could not be constructed.")
	}
	identity, err := contracts.RootArtifactIdentityFor(contracts.RootArtifactKindExecutionWorkspace, 0)
	if err != nil {
		return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The execution workspace artifact identity is invalid.")
	}
	ref, err := st.SaveContractArtifact(executionWorkspaceCategory, identity.ArtifactID, artifact, identity.RefID)
	if err != nil {
		return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The execution workspace artifact could not be persisted.")
	}
	persisted, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The persisted execution workspace artifact could not be loaded.")
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, contracts.RootArtifactKindExecutionWorkspace, 0, persisted); err != nil {
		return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The persisted execution workspace artifact ref failed validation.")
	}
	if err := verifyWorkspaceIdentity(persisted); err != nil {
		return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The persisted execution workspace identity failed validation.")
	}
	want, err := contracts.CanonicalJSONBytes(artifact)
	if err != nil {
		return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The execution workspace artifact could not be verified.")
	}
	got, err := contracts.CanonicalJSONBytes(persisted)
	if err != nil || !bytes.Equal(got, want) {
		if err == nil {
			err = errors.New("persisted workspace payload changed")
		}
		return nil, materializationFailure(snapshot.repository, worktreePath, createdWorktree, err, "The persisted execution workspace artifact changed during persistence.")
	}

	return &Materialized{
		ExecutionCWD: executionCWD,
		WorktreePath: worktreePath,
		Artifact:     cloneMap(persisted),
		ArtifactRef:  cloneMap(ref),
	}, nil
}

func verifyMaterializedWorktree(
	ctx context.Context,
	repository *repositorySnapshot,
	worktreePath string,
	rawDescriptor *rawExportDescriptor,
) (string, worktreeRegistration, error) {
	canonicalWorktree, err := canonicalExistingDirectory(worktreePath)
	if err != nil {
		return "", worktreeRegistration{}, err
	}
	if canonicalWorktree != filepath.Clean(worktreePath) {
		return "", worktreeRegistration{}, fmt.Errorf("worktree resolved to %s", canonicalWorktree)
	}
	record, found, err := repositoryWorktreeRegistration(ctx, repository, canonicalWorktree)
	if err != nil {
		return "", worktreeRegistration{}, err
	}
	if !found {
		return "", worktreeRegistration{}, errors.New("worktree is not registered")
	}
	if !record.Detached || record.Branch != "" || record.Bare || record.Prunable {
		return "", worktreeRegistration{}, fmt.Errorf("worktree registration is not a live detached worktree")
	}
	if record.Head != repository.headCommit {
		return "", worktreeRegistration{}, fmt.Errorf("registered worktree HEAD is %s, want %s", record.Head, repository.headCommit)
	}

	rootOutput, err := runGit(ctx, repository.gitBinary, canonicalWorktree, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", worktreeRegistration{}, err
	}
	root, err := canonicalExistingDirectory(strings.TrimSpace(string(rootOutput)))
	if err != nil || root != canonicalWorktree {
		return "", worktreeRegistration{}, fmt.Errorf("worktree root mismatch: %s", strings.TrimSpace(string(rootOutput)))
	}
	headOutput, err := runGit(ctx, repository.gitBinary, canonicalWorktree, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(headOutput)) != repository.headCommit {
		return "", worktreeRegistration{}, fmt.Errorf("worktree HEAD does not match recorded commit")
	}
	treeOutput, err := runGit(ctx, repository.gitBinary, canonicalWorktree, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil || strings.TrimSpace(string(treeOutput)) != repository.headTree {
		return "", worktreeRegistration{}, fmt.Errorf("worktree tree does not match recorded tree")
	}
	branchOutput, err := runGit(ctx, repository.gitBinary, canonicalWorktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || strings.TrimSpace(string(branchOutput)) != "HEAD" {
		return "", worktreeRegistration{}, errors.New("worktree HEAD is attached to a branch")
	}

	mappedCWD := canonicalWorktree
	if repository.launchSubpath != "." {
		mappedCWD = filepath.Join(canonicalWorktree, filepath.FromSlash(repository.launchSubpath))
	}
	mappedCWD, err = canonicalExistingDirectory(mappedCWD)
	if err != nil || !pathContains(canonicalWorktree, mappedCWD) {
		return "", worktreeRegistration{}, fmt.Errorf("mapped launch CWD is not a directory inside the worktree")
	}

	probe, err := os.CreateTemp(canonicalWorktree, "convo-relay-write-probe-")
	if err != nil {
		return "", worktreeRegistration{}, fmt.Errorf("worktree is not writable: %w", err)
	}
	probePath := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(probePath)
		return "", worktreeRegistration{}, fmt.Errorf("worktree write probe could not be closed: %w", closeErr)
	}
	if err := os.Remove(probePath); err != nil {
		return "", worktreeRegistration{}, fmt.Errorf("worktree write probe could not be removed: %w", err)
	}
	if err := verifyRawExport(ctx, canonicalWorktree, rawDescriptor); err != nil {
		return "", worktreeRegistration{}, fmt.Errorf("raw export inventory mismatch: %w", err)
	}

	record.Path = canonicalWorktree
	return mappedCWD, record, nil
}

func workspaceArtifact(
	snapshot *Snapshot,
	sessionDir string,
	worktreePath string,
	executionCWD string,
	registration worktreeRegistration,
	rawDescriptor *rawExportDescriptor,
) (map[string]any, error) {
	achievedPolicy := PolicyInherited
	if worktreePath != "" {
		// Version 1 implements both required policies with a detached writable
		// worktree, which is the stronger ephemeral behavior.
		achievedPolicy = PolicyEphemeral
	}
	identity := map[string]any{
		"session_dir":         sessionDir,
		"session_path_source": snapshot.sessionPathSource,
		"source_launch_cwd":   snapshot.launchCWD,
		"execution_cwd":       executionCWD,
	}
	if snapshot.repository != nil {
		identity["source_git_root"] = snapshot.repository.root
		identity["launch_subpath"] = snapshot.repository.launchSubpath
	}
	if worktreePath != "" {
		identity["execution_root"] = filepath.Dir(worktreePath)
		identity["worktree_path"] = worktreePath
	}
	identityDigest, err := semanticDigest(identity)
	if err != nil {
		return nil, err
	}
	identity["identity_digest"] = identityDigest

	base := map[string]any{"repository_state": "non_git"}
	if state, ok := snapshot.sourceReport["repository_state"]; ok {
		base["repository_state"] = state
	}
	if snapshot.repository != nil {
		base["head_commit"] = snapshot.repository.headCommit
		base["head_tree"] = snapshot.repository.headTree
		base["object_format"] = snapshot.repository.objectFormat
	}
	registrationPayload := map[string]any{
		"mode":       PolicyInherited,
		"registered": false,
		"detached":   false,
		"writable":   false,
	}
	if worktreePath != "" {
		registrationPayload = map[string]any{
			"mode":        "detached_worktree",
			"registered":  true,
			"detached":    registration.Detached,
			"writable":    true,
			"head_commit": registration.Head,
			"locked":      registration.Locked,
		}
	}
	fields := map[string]any{
		"policy": map[string]any{
			"minimum":            snapshot.policy.Minimum,
			"requested":          snapshot.policy.Requested,
			"requested_explicit": snapshot.policy.RequestedExplicit,
			"effective":          snapshot.policy.Effective,
			"achieved":           achievedPolicy,
		},
		"identity":                         identity,
		"registration":                     registrationPayload,
		"base":                             base,
		"source":                           cloneMap(snapshot.sourceReport),
		"exclusions":                       cloneMap(snapshot.exclusions),
		SourceStagedChangesKey:             snapshot.sourceChanges.Staged,
		SourceUnstagedChangesKey:           snapshot.sourceChanges.Unstaged,
		SourceUnignoredUntrackedChangesKey: snapshot.sourceChanges.Untracked,
		AllowDirtySourceRequestedKey:       snapshot.allowDirtySource,
		"repository_inventory_limits": map[string]any{
			"max_files": snapshot.inventoryLimits.maxFiles,
			"max_bytes": snapshot.inventoryLimits.maxBytes,
		},
	}
	provenance := provenanceForAchievedPolicy(achievedPolicy)
	fields[WorkspaceContentSourceKey] = provenance.WorkspaceContentSource
	fields[WorkingTreeChangesIncludedKey] = provenance.WorkingTreeChangesIncluded
	if snapshot.repository != nil {
		fields["source_before_digest"] = snapshot.repository.sourceDigest
	}
	if rawDescriptor != nil {
		fields["committed_export"] = rawExportArtifactMap(rawDescriptor)
	}
	return contracts.NormalizeRootArtifact(contracts.RootArtifactKindExecutionWorkspace, fields)
}

func verifyWorkspaceIdentity(artifact map[string]any) error {
	validated, err := contracts.ValidateRootArtifact(artifact, contracts.RootArtifactKindExecutionWorkspace)
	if err != nil {
		return err
	}
	identity, ok := validated["identity"].(map[string]any)
	if !ok {
		return contracts.NewValidationError("execution_workspace identity must be an object")
	}
	digest, ok := identity["identity_digest"].(string)
	if !ok || strings.TrimSpace(digest) == "" {
		return contracts.NewValidationError("execution_workspace identity_digest is required")
	}
	material := cloneMap(identity)
	delete(material, "identity_digest")
	want, err := semanticDigest(material)
	if err != nil {
		return err
	}
	if digest != want {
		return contracts.NewValidationError("execution_workspace identity digest mismatch")
	}
	return nil
}

func inheritedRegistration() worktreeRegistration {
	return worktreeRegistration{}
}

func repositoryWorktreeRegistered(ctx context.Context, repository *repositorySnapshot, path string) (bool, error) {
	_, found, err := repositoryWorktreeRegistration(ctx, repository, path)
	return found, err
}

func repositoryWorktreeRegistration(ctx context.Context, repository *repositorySnapshot, path string) (worktreeRegistration, bool, error) {
	if repository == nil {
		return worktreeRegistration{}, false, errors.New("repository snapshot is required")
	}
	target, err := canonicalPathAllowMissing(path)
	if err != nil {
		return worktreeRegistration{}, false, err
	}
	output, err := runGit(ctx, repository.gitBinary, repository.root, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return worktreeRegistration{}, false, err
	}
	records, err := parseWorktreeRegistrations(output)
	if err != nil {
		return worktreeRegistration{}, false, err
	}
	for _, record := range records {
		candidate, err := canonicalPathAllowMissing(record.Path)
		if err != nil {
			return worktreeRegistration{}, false, err
		}
		if pathsEquivalent(candidate, target) {
			record.Path = candidate
			return record, true, nil
		}
	}
	return worktreeRegistration{}, false, nil
}

func parseWorktreeRegistrations(data []byte) ([]worktreeRegistration, error) {
	if len(data) == 0 {
		return []worktreeRegistration{}, nil
	}
	rawRecords := bytes.Split(data, []byte{0, 0})
	records := make([]worktreeRegistration, 0, len(rawRecords))
	for _, rawRecord := range rawRecords {
		if len(rawRecord) == 0 {
			continue
		}
		fields := bytes.Split(rawRecord, []byte{0})
		record := worktreeRegistration{}
		for _, rawField := range fields {
			field := string(rawField)
			switch {
			case strings.HasPrefix(field, "worktree "):
				record.Path = strings.TrimPrefix(field, "worktree ")
			case strings.HasPrefix(field, "HEAD "):
				record.Head = strings.TrimPrefix(field, "HEAD ")
			case strings.HasPrefix(field, "branch "):
				record.Branch = strings.TrimPrefix(field, "branch ")
			case field == "detached":
				record.Detached = true
			case field == "bare":
				record.Bare = true
			case field == "locked" || strings.HasPrefix(field, "locked "):
				record.Locked = true
			case field == "prunable" || strings.HasPrefix(field, "prunable "):
				record.Prunable = true
			}
		}
		if record.Path == "" {
			return nil, errors.New("Git worktree record is missing its path")
		}
		records = append(records, record)
	}
	return records, nil
}

func materializationFailure(repository *repositorySnapshot, worktreePath string, created bool, cause error, message string) error {
	if created {
		if rollbackErr := rollbackMaterializedWorktreeAfterFailure(repository, worktreePath); rollbackErr != nil {
			cause = errors.Join(cause, fmt.Errorf("worktree rollback failed: %w", rollbackErr))
		}
	}
	code := DiagnosticCodeIntegrity
	path := "/session_dir"
	details := map[string]any{"worktree_path": worktreePath}
	var limitErr *contracts.ResourceLimitError
	if errors.As(cause, &limitErr) {
		code = limitErr.Code
		path = "/runtime_config/limits"
		details = resourceLimitDetails(limitErr)
		details["worktree_path"] = worktreePath
	}
	return workspaceError(cause, code, contracts.DiagnosticPhasePreflight, path, message, details)
}

func rollbackMaterializedWorktreeAfterFailure(repository *repositorySnapshot, worktreePath string) error {
	cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return rollbackMaterializedWorktree(cleanupContext, repository, worktreePath)
}

func rollbackMaterializedWorktree(ctx context.Context, repository *repositorySnapshot, worktreePath string) error {
	if repository == nil || strings.TrimSpace(worktreePath) == "" {
		return nil
	}
	var failures []error
	registered, err := repositoryWorktreeRegistered(ctx, repository, worktreePath)
	if err != nil {
		failures = append(failures, err)
	} else if registered {
		if _, err := runGit(ctx, repository.gitBinary, repository.root, "worktree", "remove", "--force", worktreePath); err != nil {
			failures = append(failures, err)
		}
	}
	if _, err := os.Lstat(worktreePath); err == nil {
		if removeErr := os.RemoveAll(worktreePath); removeErr != nil {
			failures = append(failures, removeErr)
		}
	} else if !os.IsNotExist(err) {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
