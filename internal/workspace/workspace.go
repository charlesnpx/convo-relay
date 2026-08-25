// Package workspace prepares the one execution directory used by a v2 session.
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/session"
)

const (
	ModeCurrent  = "current"
	ModeHeadCopy = "head-copy"

	WorkspaceContentSourceWorkingTree   = "working_tree"
	WorkspaceContentSourceCommittedHead = "committed_head"

	WorkspaceContentSourceKey     = "workspace_content_source"
	WorkingTreeChangesIncludedKey = "working_tree_changes_included"
)

const stateFilename = "workspace.json"

// Options identifies the supplied directory and the only execution mode.
type Options struct {
	LaunchCWD string
	Mode      string
}

// Materialized is the durable execution boundary. Current-mode execution
// needs its local directory from runtime state; head-copy execution is
// reconstructed from workspace.prepared.
type Materialized struct {
	Mode         string
	ExecutionCWD string
	WorktreePath string
	SourceRoot   string
	Commit       string
	TreeHash     string
	RelativePath string
}

type persistedState struct {
	Mode         string `json:"mode"`
	ExecutionCWD string `json:"execution_cwd"`
}

// Prepare records one workspace decision. Current intentionally sees the
// supplied working tree. Head-copy intentionally sees exactly the HEAD tree.
func Prepare(ctx context.Context, sess *session.Session, options Options) (*Materialized, error) {
	return prepareWithStateSaver(ctx, sess, options, save)
}

func prepareWithStateSaver(ctx context.Context, sess *session.Session, options Options, saveState func(*session.Session, Materialized) error) (*Materialized, error) {
	return prepareWithHooks(ctx, sess, options, saveState, appendPrepared)
}

func prepareWithHooks(
	ctx context.Context,
	sess *session.Session,
	options Options,
	saveState func(*session.Session, Materialized) error,
	appendState func(*session.Session, *Materialized) error,
) (*Materialized, error) {
	if sess == nil || strings.TrimSpace(sess.Root) == "" {
		return nil, errors.New("session is required")
	}
	if saveState == nil {
		return nil, errors.New("workspace state saver is required")
	}
	if appendState == nil {
		return nil, errors.New("workspace event appender is required")
	}
	if _, err := os.Stat(statePath(sess)); err == nil {
		return Recover(ctx, sess)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect workspace state: %w", err)
	}
	if _, found, err := preparedEvent(sess); err != nil {
		return nil, err
	} else if found {
		return Recover(ctx, sess)
	}
	mode := strings.TrimSpace(options.Mode)
	if mode == "" {
		mode = sess.Plan.Workspace.Mode
	}
	if mode != ModeCurrent && mode != ModeHeadCopy {
		return nil, fmt.Errorf("workspace mode must be %s or %s", ModeCurrent, ModeHeadCopy)
	}
	launchCWD, err := absoluteDirectory(options.LaunchCWD)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace directory: %w", err)
	}

	materialized := &Materialized{Mode: mode, ExecutionCWD: launchCWD}
	if mode == ModeHeadCopy {
		if err := describeHeadCopy(ctx, sess, launchCWD, materialized); err != nil {
			return nil, err
		}
		// The event contains every portable fact needed to identify this
		// head-copy before Git registers its worktree. Make it durable first so
		// every registered worktree is owned by workspace.prepared.
		if err := appendState(sess, materialized); err != nil {
			return nil, err
		}
		if err := materializeHeadCopy(ctx, materialized); err != nil {
			return nil, err
		}
		return materialized, nil
	} else if root, commit, tree, found, gitErr := gitFacts(ctx, launchCWD); gitErr != nil {
		return nil, gitErr
	} else if found {
		relativePath, err := repositoryRelativePath(root, launchCWD)
		if err != nil {
			return nil, err
		}
		materialized.SourceRoot = root
		materialized.Commit = commit
		materialized.TreeHash = tree
		materialized.RelativePath = relativePath
	}
	// Only current mode needs local runtime state after preparation. Save it
	// before the canonical event: an interruption can then leave only the
	// repairable state-without-event prefix, never event-without-state.
	if mode == ModeCurrent {
		if err := saveState(sess, *materialized); err != nil {
			return nil, err
		}
	}
	if err := appendState(sess, materialized); err != nil {
		return nil, err
	}
	return materialized, nil
}

func describeHeadCopy(ctx context.Context, sess *session.Session, launchCWD string, materialized *Materialized) error {
	sourceRoot, commit, tree, found, err := gitFacts(ctx, launchCWD)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("head-copy workspace requires a Git repository")
	}
	relativePath, err := repositoryRelativePath(sourceRoot, launchCWD)
	if err != nil {
		return err
	}
	worktreePath := filepath.Join(sess.Root, "runtime", "workspace")
	if _, err := os.Lstat(worktreePath); err == nil {
		return fmt.Errorf("head-copy worktree path already exists: %s", worktreePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	materialized.WorktreePath = worktreePath
	materialized.SourceRoot = sourceRoot
	materialized.Commit = commit
	materialized.TreeHash = tree
	materialized.RelativePath = relativePath
	return nil
}

func materializeHeadCopy(ctx context.Context, materialized *Materialized) error {
	if _, err := runGit(ctx, materialized.SourceRoot, "worktree", "add", "--detach", materialized.WorktreePath, materialized.Commit); err != nil {
		return fmt.Errorf("create detached head-copy worktree: %w", err)
	}
	executionCWD := filepath.Join(materialized.WorktreePath, filepath.FromSlash(materialized.RelativePath))
	if _, err := absoluteDirectory(executionCWD); err != nil {
		_ = removeWorktree(ctx, materialized.SourceRoot, materialized.WorktreePath)
		return fmt.Errorf("recorded head-copy subdirectory is unavailable: %w", err)
	}
	materialized.ExecutionCWD = executionCWD
	return nil
}

func repositoryRelativePath(root, launchCWD string) (string, error) {
	relative, err := filepath.Rel(root, launchCWD)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("workspace directory is outside its Git root")
	}
	return filepath.ToSlash(relative), nil
}

// Recover returns the recorded execution boundary without source inventories
// or re-digests. Current-mode state repairs a missing workspace.prepared event
// before callers can execute; a head-copy is rebuilt solely from the durable
// event and detached worktree.
func Recover(ctx context.Context, sess *session.Session) (*Materialized, error) {
	prepared, eventPresent, err := preparedEvent(sess)
	if err != nil {
		return nil, err
	}
	state, stateErr := load(sess)
	if stateErr == nil {
		if state.Mode != ModeCurrent {
			if !eventPresent {
				return nil, errors.New("head-copy workspace state has no workspace.prepared event")
			}
			return rebuildHeadCopy(ctx, sess, prepared)
		}
		materialized, err := materializeCurrent(ctx, state)
		if err != nil {
			return nil, err
		}
		if !eventPresent {
			if err := appendPrepared(sess, materialized); err != nil {
				return nil, fmt.Errorf("repair workspace.prepared from runtime/workspace.json: %w", err)
			}
			return materialized, nil
		}
		if prepared.Mode != ModeCurrent {
			return nil, errors.New("runtime/workspace.json does not match workspace.prepared")
		}
		return materialized, nil
	}
	if !errors.Is(stateErr, os.ErrNotExist) {
		return nil, stateErr
	}
	if !eventPresent {
		return nil, errors.New("workspace runtime state is unavailable and no workspace.prepared event exists")
	}
	if prepared.Mode == ModeCurrent {
		return nil, errors.New("runtime/workspace.json is missing for current workspace; it may have been deleted")
	}
	return rebuildHeadCopy(ctx, sess, prepared)
}

func rebuildHeadCopy(ctx context.Context, sess *session.Session, prepared eventlog.WorkspacePreparedPayload) (*Materialized, error) {
	if prepared.Mode != ModeHeadCopy {
		return nil, errors.New("workspace.prepared is not a head-copy workspace")
	}
	worktreePath := filepath.Join(sess.Root, "runtime", "workspace")
	if _, err := absoluteDirectory(worktreePath); err != nil {
		return nil, fmt.Errorf("recorded head-copy worktree is unavailable: %w", err)
	}
	commit, err := gitOutput(ctx, worktreePath, "rev-parse", "HEAD^{commit}")
	if err != nil || commit != prepared.Commit {
		return nil, errors.New("head-copy worktree no longer matches its recorded HEAD")
	}
	tree, err := gitOutput(ctx, worktreePath, "rev-parse", "HEAD^{tree}")
	if err != nil || tree != prepared.TreeHash {
		return nil, errors.New("head-copy worktree no longer matches its recorded tree")
	}
	sourceRoot, err := sourceRootForWorktree(ctx, worktreePath)
	if err != nil {
		return nil, err
	}
	executionCWD := filepath.Join(worktreePath, filepath.FromSlash(prepared.RelativePath))
	if _, err := absoluteDirectory(executionCWD); err != nil {
		return nil, fmt.Errorf("recorded head-copy subdirectory is unavailable: %w", err)
	}
	materialized := &Materialized{
		Mode:         prepared.Mode,
		ExecutionCWD: executionCWD,
		WorktreePath: worktreePath,
		SourceRoot:   sourceRoot,
		Commit:       prepared.Commit,
		TreeHash:     prepared.TreeHash,
		RelativePath: prepared.RelativePath,
	}
	return materialized, nil
}

// materializeCurrent derives portable Git provenance from the runtime-only
// execution directory when a current-mode repair must append the canonical
// event. The directory is the only persisted local fact; Git facts remain in
// workspace.prepared once that record exists.
func materializeCurrent(ctx context.Context, state persistedState) (*Materialized, error) {
	materialized := state.materialized()
	executionCWD, err := absoluteDirectory(materialized.ExecutionCWD)
	if err != nil {
		return nil, fmt.Errorf("recorded workspace directory is unavailable: %w", err)
	}
	materialized.ExecutionCWD = executionCWD
	root, commit, tree, found, err := gitFacts(ctx, executionCWD)
	if err != nil {
		return nil, err
	}
	if !found {
		return materialized, nil
	}
	relativePath, err := repositoryRelativePath(root, executionCWD)
	if err != nil {
		return nil, err
	}
	materialized.SourceRoot = root
	materialized.Commit = commit
	materialized.TreeHash = tree
	materialized.RelativePath = relativePath
	return materialized, nil
}

// Cleanup removes only the exact detached worktree recorded for this session.
// It deliberately leaves the session directory to its caller.
func Cleanup(ctx context.Context, sess *session.Session) error {
	prepared, found, err := preparedEvent(sess)
	if err != nil {
		return err
	}
	if !found || prepared.Mode != ModeHeadCopy {
		return nil
	}
	materialized, err := rebuildHeadCopy(ctx, sess, prepared)
	if err != nil {
		return err
	}
	return removeWorktree(ctx, materialized.SourceRoot, materialized.WorktreePath)
}

// Projection provides the observable workspace provenance used by reports.
func Projection(sess *session.Session) (map[string]any, error) {
	prepared, found, err := preparedEvent(sess)
	if err != nil {
		return nil, err
	}
	mode := prepared.Mode
	commit := prepared.Commit
	treeHash := prepared.TreeHash
	if !found {
		state, stateErr := load(sess)
		if stateErr != nil {
			return nil, stateErr
		}
		if state.Mode != ModeCurrent {
			return nil, errors.New("head-copy workspace state has no workspace.prepared event")
		}
		mode = state.Mode
	}
	projection := map[string]any{
		"mode":                        mode,
		"commit":                      commit,
		"tree_hash":                   treeHash,
		WorkspaceContentSourceKey:     WorkspaceContentSourceWorkingTree,
		WorkingTreeChangesIncludedKey: true,
	}
	if mode == ModeHeadCopy {
		projection[WorkspaceContentSourceKey] = WorkspaceContentSourceCommittedHead
		projection[WorkingTreeChangesIncludedKey] = false
	}
	return projection, nil
}

func appendPrepared(sess *session.Session, materialized *Materialized) error {
	if materialized == nil {
		return errors.New("workspace materialization is required")
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return err
	}
	writer, err := sess.EventWriter(blobs)
	if err != nil {
		return err
	}
	defer writer.Close()
	_, err = writer.Append(eventlog.NewEvent(
		fmt.Sprintf("workspace-prepared-%d", time.Now().UnixNano()),
		time.Now(),
		eventlog.WorkspacePreparedPayload{
			Mode:         materialized.Mode,
			Commit:       materialized.Commit,
			TreeHash:     materialized.TreeHash,
			RelativePath: materialized.RelativePath,
		},
	))
	return err
}

func preparedEvent(sess *session.Session) (eventlog.WorkspacePreparedPayload, bool, error) {
	if sess == nil || strings.TrimSpace(sess.Root) == "" {
		return eventlog.WorkspacePreparedPayload{}, false, errors.New("session is required")
	}
	body, err := os.ReadFile(filepath.Join(sess.Root, eventlog.EventsFilename))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return eventlog.WorkspacePreparedPayload{}, false, nil
		}
		return eventlog.WorkspacePreparedPayload{}, false, err
	}
	events, err := eventlog.Replay(strings.NewReader(string(body)))
	if err != nil {
		return eventlog.WorkspacePreparedPayload{}, false, err
	}
	var prepared eventlog.WorkspacePreparedPayload
	found := false
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.WorkspacePreparedPayload:
			if found {
				return eventlog.WorkspacePreparedPayload{}, false, errors.New("session has more than one workspace.prepared event")
			}
			prepared, found = payload, true
		case *eventlog.WorkspacePreparedPayload:
			if payload == nil {
				continue
			}
			if found {
				return eventlog.WorkspacePreparedPayload{}, false, errors.New("session has more than one workspace.prepared event")
			}
			prepared, found = *payload, true
		}
	}
	return prepared, found, nil
}

func statePath(sess *session.Session) string {
	return filepath.Join(sess.Root, "runtime", stateFilename)
}

func save(sess *session.Session, materialized Materialized) error {
	if materialized.Mode != ModeCurrent {
		return errors.New("runtime/workspace.json is only used for current workspaces")
	}
	state := persistedState{
		Mode: materialized.Mode, ExecutionCWD: materialized.ExecutionCWD,
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := statePath(sess)
	file, err := os.CreateTemp(filepath.Dir(path), ".workspace-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func load(sess *session.Session) (persistedState, error) {
	if sess == nil || strings.TrimSpace(sess.Root) == "" {
		return persistedState{}, errors.New("session is required")
	}
	body, err := os.ReadFile(statePath(sess))
	if err != nil {
		return persistedState{}, err
	}
	var state persistedState
	if err := json.Unmarshal(body, &state); err != nil {
		return persistedState{}, fmt.Errorf("decode workspace state: %w", err)
	}
	if state.Mode != ModeCurrent && state.Mode != ModeHeadCopy {
		return persistedState{}, errors.New("workspace state has an unsupported mode")
	}
	if strings.TrimSpace(state.ExecutionCWD) == "" {
		return persistedState{}, errors.New("workspace state has no execution directory")
	}
	return state, nil
}

func (s persistedState) materialized() *Materialized {
	return &Materialized{Mode: s.Mode, ExecutionCWD: s.ExecutionCWD}
}

func absoluteDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("directory is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func gitFacts(ctx context.Context, cwd string) (root, commit, tree string, found bool, err error) {
	root, err = gitOutput(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", "", "", false, nil
		}
		return "", "", "", false, err
	}
	root, err = absoluteDirectory(root)
	if err != nil {
		return "", "", "", false, err
	}
	commit, err = gitOutput(ctx, cwd, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return "", "", "", false, err
	}
	tree, err = gitOutput(ctx, cwd, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", "", "", false, err
	}
	return root, commit, tree, true, nil
}

func removeWorktree(ctx context.Context, sourceRoot string, worktreePath string) error {
	if strings.TrimSpace(sourceRoot) == "" || strings.TrimSpace(worktreePath) == "" {
		return errors.New("head-copy cleanup state is incomplete")
	}
	if _, err := os.Lstat(worktreePath); err == nil {
		if _, err := runGit(ctx, sourceRoot, "worktree", "remove", "--force", worktreePath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := runGit(ctx, sourceRoot, "worktree", "prune")
	return err
}

func sourceRootForWorktree(ctx context.Context, worktreePath string) (string, error) {
	commonDir, err := gitOutput(ctx, worktreePath, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("find head-copy source root: %w", err)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreePath, commonDir)
	}
	root, err := absoluteDirectory(filepath.Dir(commonDir))
	if err != nil {
		return "", fmt.Errorf("resolve head-copy source root: %w", err)
	}
	return root, nil
}

func gitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	output, err := runGit(ctx, cwd, args...)
	return strings.TrimSpace(string(output)), err
}

func runGit(ctx context.Context, cwd string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
