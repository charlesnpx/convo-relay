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
	DiagnosticCodeSessionConflict    = "workspace_session_path_conflict"
	DiagnosticCodeLaunchNotCommitted = "workspace_launch_path_not_committed"
	DiagnosticCodeCreationFailed     = "workspace_creation_failed"
	DiagnosticCodeIntegrity          = "workspace_integrity_failed"
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
	GitBinary         string
}

// PolicyResolution records the recipe minimum, caller request, effective
// policy, and actual policy that the workspace implementation will achieve.
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
	repository        *repositorySnapshot
	sourceReport      map[string]any
	exclusions        map[string]any
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
	achieved := PolicyInherited
	if effective != PolicyInherited {
		// Version 1 implements both required policies with a detached writable
		// worktree, which is the stronger ephemeral behavior.
		achieved = PolicyEphemeral
	}
	return PolicyResolution{
		Minimum:           minimum,
		Requested:         requested,
		RequestedExplicit: requestedExplicit,
		Effective:         effective,
		Achieved:          achieved,
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

	snapshot := &Snapshot{
		launchCWD:         launchCWD,
		sessionDir:        sessionDir,
		sessionPathSource: sessionPathSource,
		gitBinary:         gitBinary,
		policy:            policy,
		exclusions:        emptyExclusions(),
	}
	repository, err := inspectRepository(ctx, gitBinary, launchCWD)
	if err != nil {
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
		return snapshot, nil
	}
	snapshot.repository = repository
	snapshot.sourceReport = cloneMap(repository.sourceReport)
	snapshot.exclusions = cloneMap(repository.exclusions)

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
		worktreePath := filepath.Join(sessionDir, "execution", "worktree")
		if registered, err := repositoryWorktreeRegistered(ctx, repository, worktreePath); err != nil {
			return nil, workspaceError(err, DiagnosticCodeInventoryFailed, contracts.DiagnosticPhasePreflight, "/session_dir", "Git worktree registration could not be inspected.", nil)
		} else if registered {
			return nil, workspaceError(nil, DiagnosticCodeSessionConflict, contracts.DiagnosticPhasePolicy, "/session_dir", "The target execution worktree is already registered.", map[string]any{"worktree_path": worktreePath})
		}
	}
	return snapshot, nil
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
	return map[string]any{
		"minimum_policy":      s.policy.Minimum,
		"requested_policy":    s.policy.Requested,
		"requested_explicit":  s.policy.RequestedExplicit,
		"effective_policy":    s.policy.Effective,
		"achieved_policy":     s.policy.Achieved,
		"session_dir":         s.sessionDir,
		"session_path_source": s.sessionPathSource,
		"source":              cloneMap(s.sourceReport),
		"exclusions":          cloneMap(s.exclusions),
	}
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

func pathsOverlap(left string, right string) bool {
	return pathContains(left, right) || pathContains(right, left)
}

func pathContains(parent string, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
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
