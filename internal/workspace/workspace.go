package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/recipes"
)

const (
	PolicyInherited = "inherited"
	PolicyReadOnly  = "read_only"
	PolicyEphemeral = "ephemeral"

	SessionPathExplicit  = "explicit"
	SessionPathRelayHome = "relay_home"
	SessionPathResolved  = "resolved"

	DiagnosticCodeInvalidPolicy      = "invalid_workspace_isolation_policy"
	DiagnosticCodePolicyWeakened     = "workspace_isolation_policy_weakened"
	DiagnosticCodeGitRequired        = "workspace_git_repository_required"
	DiagnosticCodeUnbornRepository   = "workspace_unborn_repository"
	DiagnosticCodeInventoryFailed    = "workspace_inventory_failed"
	DiagnosticCodeInventoryLimit     = "workspace_inventory_limit_exceeded"
	DiagnosticCodeInventoryCycle     = "workspace_inventory_cycle_detected"
	DiagnosticCodeInventoryDepth     = "workspace_inventory_depth_exceeded"
	DiagnosticCodeSessionConflict    = "workspace_session_path_conflict"
	DiagnosticCodeLaunchNotCommitted = "workspace_launch_path_not_committed"
	DiagnosticCodeCreationFailed     = "workspace_creation_failed"
	DiagnosticCodeIntegrity          = "workspace_integrity_failed"
	DiagnosticCodeDirtySource        = "workspace_dirty_source"
	DiagnosticCodeDirtyInapplicable  = "workspace_allow_dirty_inapplicable"
)

// Options describes workspace preflight without creating a session or Git
// worktree. RequestedExplicit distinguishes a visited CLI override from its
// ordinary inherited default.
type Options struct {
	LaunchCWD         string
	SessionDir        string
	SessionPathSource string
	MinimumPolicy     string
	RequestedPolicy   string
	RequestedExplicit bool
	AllowDirtySource  bool
	GitBinary         string
	InventoryMaxFiles int64
	InventoryMaxBytes int64
	ArtifactVersion   int
}

// PolicyResolution records the recipe minimum, caller request, effective
// policy, and actual policy after successful materialization. Achieved remains
// empty during pure preflight.
type PolicyResolution struct {
	Minimum           string
	Requested         string
	RequestedExplicit bool
	Effective         string
	Achieved          string
}

// Snapshot is an immutable, pure-preflight inventory. Materialize consumes it
// after the caller has crossed the session-creation boundary.
type Snapshot struct {
	launchCWD         string
	sessionDir        string
	sessionPathSource string
	gitBinary         string
	policy            PolicyResolution
	inventoryLimits   repositoryInventoryLimits
	artifactVersion   int
	allowDirtySource  bool
	sourceChanges     SourceChanges
	repository        *repositorySnapshot
	sourceReport      map[string]any
	exclusions        map[string]any
}

// SourceChanges contains only counts from the captured stable inventory.
type SourceChanges struct {
	Staged    int64
	Unstaged  int64
	Untracked int64
}

func (c SourceChanges) Dirty() bool {
	return c.Staged > 0 || c.Unstaged > 0 || c.Untracked > 0
}

func ResolvePolicy(minimum string, requested string, requestedExplicit bool) (PolicyResolution, error) {
	if requestedExplicit && strings.TrimSpace(requested) == "" {
		return PolicyResolution{}, workspaceError(
			nil,
			DiagnosticCodeInvalidPolicy,
			contracts.DiagnosticPhasePolicy,
			"/workspace_isolation",
			"An explicit workspace isolation override must be inherited, read_only, or ephemeral.",
			map[string]any{"minimum": normalizePolicyDefault(minimum), "requested": requested},
		)
	}
	minimum = normalizePolicyDefault(minimum)
	requested = normalizePolicyDefault(requested)
	minimumRank, minimumOK := policyRank(minimum)
	requestedRank, requestedOK := policyRank(requested)
	if !minimumOK || !requestedOK {
		return PolicyResolution{}, workspaceError(
			nil,
			DiagnosticCodeInvalidPolicy,
			contracts.DiagnosticPhasePolicy,
			"/workspace_isolation",
			"Workspace isolation must be inherited, read_only, or ephemeral.",
			map[string]any{"minimum": minimum, "requested": requested},
		)
	}
	if requestedExplicit && requestedRank < minimumRank {
		return PolicyResolution{}, workspaceError(
			nil,
			DiagnosticCodePolicyWeakened,
			contracts.DiagnosticPhasePolicy,
			"/workspace_isolation",
			"An explicit workspace isolation override cannot weaken the recipe minimum.",
			map[string]any{"minimum": minimum, "requested": requested},
		)
	}
	effective := minimum
	if requestedRank > minimumRank {
		effective = requested
	}
	return PolicyResolution{
		Minimum:           minimum,
		Requested:         requested,
		RequestedExplicit: requestedExplicit,
		Effective:         effective,
	}, nil
}

// Preflight resolves policy and paths and inventories Git state without
// creating a session, artifact, directory, or worktree.
func Preflight(ctx context.Context, options Options) (*Snapshot, error) {
	policy, err := ResolvePolicy(options.MinimumPolicy, options.RequestedPolicy, options.RequestedExplicit)
	if err != nil {
		return nil, err
	}
	launchCWD, err := canonicalExistingDirectory(options.LaunchCWD)
	if err != nil {
		return nil, workspaceError(err, DiagnosticCodeInventoryFailed, contracts.DiagnosticPhasePreflight, "/launch_cwd", "Launch CWD must resolve to a readable directory.", nil)
	}
	sessionDir, err := canonicalPathAllowMissing(options.SessionDir)
	if err != nil || strings.TrimSpace(options.SessionDir) == "" {
		return nil, workspaceError(err, DiagnosticCodeSessionConflict, contracts.DiagnosticPhasePolicy, "/session_dir", "A resolved session directory is required for workspace preflight.", nil)
	}
	sessionPathSource := strings.TrimSpace(options.SessionPathSource)
	if sessionPathSource == "" {
		sessionPathSource = SessionPathResolved
	}
	if sessionPathSource != SessionPathExplicit && sessionPathSource != SessionPathRelayHome && sessionPathSource != SessionPathResolved {
		return nil, workspaceError(nil, DiagnosticCodeSessionConflict, contracts.DiagnosticPhasePolicy, "/session_path_source", "Session path source must be explicit, relay_home, or resolved.", map[string]any{"source": sessionPathSource})
	}
	gitBinary := strings.TrimSpace(options.GitBinary)
	if gitBinary == "" {
		gitBinary = "git"
	}
	inventoryLimits := repositoryInventoryLimits{
		maxFiles: options.InventoryMaxFiles,
		maxBytes: options.InventoryMaxBytes,
	}
	artifactVersion := options.ArtifactVersion
	if artifactVersion == 0 {
		artifactVersion = contracts.RootArtifactSchemaVersion
	}
	if artifactVersion != contracts.RootArtifactSchemaVersion && artifactVersion != contracts.RootArtifactSchemaVersionV2 {
		return nil, contracts.NewValidationError("execution workspace artifact version must be 1 or 2")
	}
	if inventoryLimits.maxFiles == 0 {
		inventoryLimits.maxFiles = recipes.DefaultRepositoryInventoryMaxFiles
	}
	if inventoryLimits.maxBytes == 0 {
		inventoryLimits.maxBytes = recipes.DefaultRepositoryInventoryMaxBytes
	}
	if inventoryLimits.maxFiles < 0 || inventoryLimits.maxBytes < 0 {
		return nil, workspaceError(
			nil,
			DiagnosticCodeInventoryFailed,
			contracts.DiagnosticPhasePreflight,
			"/runtime_config/limits",
			"Repository inventory limits must be positive integers.",
			map[string]any{
				"repository_inventory_max_files": inventoryLimits.maxFiles,
				"repository_inventory_max_bytes": inventoryLimits.maxBytes,
			},
		)
	}

	snapshot := &Snapshot{
		launchCWD:         launchCWD,
		sessionDir:        sessionDir,
		sessionPathSource: sessionPathSource,
		gitBinary:         gitBinary,
		policy:            policy,
		inventoryLimits:   inventoryLimits,
		artifactVersion:   artifactVersion,
		allowDirtySource:  options.AllowDirtySource,
		exclusions:        emptyExclusions(),
	}
	repository, err := inspectRepositoryWithLimits(ctx, gitBinary, launchCWD, inventoryLimits)
	if err != nil {
		var topologyErr *repositoryTopologyError
		if errors.As(err, &topologyErr) {
			details := map[string]any{
				"repository_depth": topologyErr.repositoryDepth,
			}
			message := "The source workspace repository topology contains a cycle."
			if topologyErr.code == DiagnosticCodeInventoryDepth {
				message = "The source workspace repository topology exceeds the supported depth."
				details["max_repository_depth"] = topologyErr.maxRepositoryDepth
			} else {
				details["repository_root"] = topologyErr.repositoryRoot
			}
			return nil, workspaceError(
				err,
				topologyErr.code,
				contracts.DiagnosticPhasePreflight,
				"/launch_cwd",
				message,
				details,
			)
		}
		var limitErr *contracts.ResourceLimitError
		if errors.As(err, &limitErr) {
			return nil, workspaceError(
				err,
				DiagnosticCodeInventoryLimit,
				contracts.DiagnosticPhasePreflight,
				"/runtime_config/limits",
				"The source repository exceeds its configured inventory budget.",
				resourceLimitDetails(limitErr),
			)
		}
		notGit := errors.Is(err, errNotGitRepository)
		unborn := errors.Is(err, errUnbornRepository)
		if policy.Effective != PolicyInherited && (notGit || unborn) {
			code := DiagnosticCodeGitRequired
			message := "Required workspace isolation needs a Git working tree."
			if unborn {
				code = DiagnosticCodeUnbornRepository
				message = "Required workspace isolation needs a repository with a committed HEAD."
			}
			return nil, workspaceError(err, code, contracts.DiagnosticPhasePreflight, "/launch_cwd", message, map[string]any{"launch_cwd": launchCWD})
		}
		if !notGit && !unborn {
			return nil, workspaceError(err, DiagnosticCodeInventoryFailed, contracts.DiagnosticPhasePreflight, "/launch_cwd", "The source workspace could not be inventoried.", map[string]any{"launch_cwd": launchCWD})
		}
		snapshot.sourceReport = map[string]any{
			"repository_state": repositoryStateForError(err),
			"launch_cwd":       launchCWD,
		}
		if options.AllowDirtySource {
			return nil, workspaceError(
				nil,
				DiagnosticCodeDirtyInapplicable,
				contracts.DiagnosticPhasePolicy,
				"/allow_dirty_source",
				"Dirty-source override applies only to isolated Git execution.",
				map[string]any{"repository_state": repositoryStateForError(err), "effective_policy": policy.Effective},
			)
		}
		return snapshot, nil
	}
	snapshot.repository = repository
	snapshot.sourceReport = cloneMap(repository.sourceReport)
	snapshot.exclusions = cloneMap(repository.exclusions)
	snapshot.sourceChanges = sourceChangeCounts(repository.exclusions)

	if policy.Effective == PolicyInherited && options.AllowDirtySource {
		return nil, workspaceError(
			nil,
			DiagnosticCodeDirtyInapplicable,
			contracts.DiagnosticPhasePolicy,
			"/allow_dirty_source",
			"Dirty-source override is inapplicable to inherited execution.",
			map[string]any{"effective_policy": policy.Effective},
		)
	}

	if policy.Effective != PolicyInherited {
		if pathsOverlap(repository.root, sessionDir) {
			return nil, workspaceError(
				nil,
				DiagnosticCodeSessionConflict,
				contracts.DiagnosticPhasePolicy,
				"/session_dir",
				"Required workspace isolation cannot place the session inside, equal to, or above the source Git worktree.",
				map[string]any{"git_root": repository.root, "session_dir": sessionDir, "session_path_source": sessionPathSource},
			)
		}
		if err := verifyCommittedLaunchSubpath(ctx, repository); err != nil {
			return nil, workspaceError(err, DiagnosticCodeLaunchNotCommitted, contracts.DiagnosticPhasePreflight, "/launch_cwd", "The launch directory is not present as a directory in committed HEAD.", map[string]any{"launch_subpath": repository.launchSubpath})
		}
		if snapshot.sourceChanges.Dirty() && !options.AllowDirtySource {
			return nil, workspaceError(
				nil,
				DiagnosticCodeDirtySource,
				contracts.DiagnosticPhasePolicy,
				"/allow_dirty_source",
				"Isolated execution requires a clean source or an explicit dirty-source override.",
				map[string]any{
					"staged_changes":    snapshot.sourceChanges.Staged,
					"unstaged_changes":  snapshot.sourceChanges.Unstaged,
					"untracked_changes": snapshot.sourceChanges.Untracked,
				},
			)
		}
		worktreePath := filepath.Join(sessionDir, "execution", "worktree")
		if registered, err := repositoryWorktreeRegistered(ctx, repository, worktreePath); err != nil {
			return nil, workspaceError(err, DiagnosticCodeInventoryFailed, contracts.DiagnosticPhasePreflight, "/session_dir", "Git worktree registration could not be inspected.", nil)
		} else if registered {
			return nil, workspaceError(nil, DiagnosticCodeSessionConflict, contracts.DiagnosticPhasePolicy, "/session_dir", "The target execution worktree is already registered.", map[string]any{"worktree_path": worktreePath})
		}
	}
	return snapshot, nil
}

func (s *Snapshot) SourceChanges() SourceChanges {
	if s == nil {
		return SourceChanges{}
	}
	return s.sourceChanges
}

func (s *Snapshot) AllowDirtySource() bool {
	return s != nil && s.allowDirtySource
}

func sourceChangeCounts(exclusions map[string]any) SourceChanges {
	return SourceChanges{
		Staged:    int64(len(anySlice(exclusions["staged"]))),
		Unstaged:  int64(len(anySlice(exclusions["unstaged"]))),
		Untracked: int64(len(anySlice(exclusions["untracked"]))),
	}
}

func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}

func (s *Snapshot) Policy() PolicyResolution {
	if s == nil {
		return PolicyResolution{}
	}
	return s.policy
}

func (s *Snapshot) LaunchCWD() string {
	if s == nil {
		return ""
	}
	return s.launchCWD
}

func (s *Snapshot) SessionDir() string {
	if s == nil {
		return ""
	}
	return s.sessionDir
}

func (s *Snapshot) GitRoot() string {
	if s == nil || s.repository == nil {
		return ""
	}
	return s.repository.root
}

func (s *Snapshot) HeadCommit() string {
	if s == nil || s.repository == nil {
		return ""
	}
	return s.repository.headCommit
}

func (s *Snapshot) HeadTree() string {
	if s == nil || s.repository == nil {
		return ""
	}
	return s.repository.headTree
}

func (s *Snapshot) ObjectFormat() string {
	if s == nil || s.repository == nil {
		return ""
	}
	return s.repository.objectFormat
}

func (s *Snapshot) SourceDigest() string {
	if s == nil || s.repository == nil {
		return ""
	}
	return s.repository.sourceDigest
}

func (s *Snapshot) Report() map[string]any {
	if s == nil {
		return nil
	}
	report := map[string]any{
		"minimum_policy":      s.policy.Minimum,
		"requested_policy":    s.policy.Requested,
		"requested_explicit":  s.policy.RequestedExplicit,
		"effective_policy":    s.policy.Effective,
		"session_dir":         s.sessionDir,
		"session_path_source": s.sessionPathSource,
		"source":              cloneMap(s.sourceReport),
		"exclusions":          cloneMap(s.exclusions),
	}
	if s.policy.Achieved != "" {
		report["achieved_policy"] = s.policy.Achieved
	}
	return report
}

func normalizePolicyDefault(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return PolicyInherited
	}
	return value
}

func policyRank(value string) (int, bool) {
	switch value {
	case PolicyInherited:
		return 0, true
	case PolicyReadOnly:
		return 1, true
	case PolicyEphemeral:
		return 2, true
	default:
		return 0, false
	}
}

func emptyExclusions() map[string]any {
	return map[string]any{"staged": []any{}, "unstaged": []any{}, "untracked": []any{}}
}

func canonicalExistingDirectory(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		value = "."
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func canonicalPathAllowMissing(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("path is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	missing := make([]string, 0)
	for {
		if _, err := os.Lstat(current); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %s", absolute)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("existing path ancestor %s is not a directory", resolved)
	}
	for index := len(missing) - 1; index >= 0; index-- {
		resolved = filepath.Join(resolved, missing[index])
	}
	return filepath.Clean(resolved), nil
}

func lexicalAbsolutePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("path is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

// rejectSymlinkPathComponents validates a lexical absolute path without
// resolving it. Missing suffixes are allowed, but every existing component
// must be a real directory except for an existing leaf.
func rejectSymlinkPathComponents(value string) error {
	target, err := lexicalAbsolutePath(value)
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
			return fmt.Errorf("inspect path component %s: %w", current, err)
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

func pathsOverlap(left string, right string) bool {
	return pathContains(left, right) || pathContains(right, left)
}

func pathContains(parent string, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	if err == nil && !filepath.IsAbs(relative) && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
		return true
	}
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return false
	}
	current := filepath.Clean(candidate)
	for {
		if candidateInfo, statErr := os.Stat(current); statErr == nil && os.SameFile(parentInfo, candidateInfo) {
			return true
		}
		next := filepath.Dir(current)
		if next == current {
			return false
		}
		current = next
	}
}

func pathsEquivalent(left string, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func semanticDigest(value any) (string, error) {
	canonical, err := contracts.CanonicalJSONBytes(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return contracts.DigestPrefix + hex.EncodeToString(sum[:]), nil
}

func pathMap(path string) map[string]any {
	result := map[string]any{"path_bytes_base64": base64.StdEncoding.EncodeToString([]byte(path))}
	if utf8.ValidString(path) {
		result["path"] = path
	}
	return result
}

func pathRecords(paths []string) []any {
	copied := append([]string(nil), paths...)
	sort.Slice(copied, func(left int, right int) bool { return copied[left] < copied[right] })
	result := make([]any, 0, len(copied))
	for _, path := range copied {
		result = append(result, pathMap(path))
	}
	return result
}

func cloneMap(value map[string]any) map[string]any {
	cloned, _ := contracts.Materialize(value).(map[string]any)
	return cloned
}

func workspaceError(cause error, code string, phase string, path string, message string, details map[string]any) error {
	diagnostic := contracts.NewDiagnostic(code, phase, path, message, details)
	return contracts.WrapDiagnosticError(cause, message, diagnostic)
}

func resourceLimitDetails(limitErr *contracts.ResourceLimitError) map[string]any {
	if limitErr == nil {
		return map[string]any{}
	}
	return map[string]any{
		"resource":  limitErr.Resource,
		"limit":     limitErr.Limit,
		"observed":  limitErr.Observed,
		"current":   limitErr.Current,
		"increment": limitErr.Increment,
	}
}
