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

// Materialized is the durable execution boundary reconstructed from runtime
// state. Commit and TreeHash are empty only when a current workspace is not a
// Git repository.
type Materialized struct {
	Mode         string
	ExecutionCWD string
	WorktreePath string
	SourceRoot   string
	Commit       string
	TreeHash     string
}

type persistedState struct {
	Mode         string `json:"mode"`
	ExecutionCWD string `json:"execution_cwd"`
	WorktreePath string `json:"worktree_path,omitempty"`
	SourceRoot   string `json:"source_root,omitempty"`
	Commit       string `json:"commit,omitempty"`
	TreeHash     string `json:"tree_hash,omitempty"`
}

// Prepare records one workspace decision. Current intentionally sees the
// supplied working tree. Head-copy intentionally sees exactly the HEAD tree.
func Prepare(ctx context.Context, sess *session.Session, options Options) (*Materialized, error) {
	if sess == nil || strings.TrimSpace(sess.Root) == "" {
		return nil, errors.New("session is required")
	}
	if _, err := os.Stat(statePath(sess)); err == nil {
		return nil, errors.New("workspace is already prepared")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect workspace state: %w", err)
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
		if err := prepareHeadCopy(ctx, sess, launchCWD, materialized); err != nil {
			return nil, err
		}
	} else if root, commit, tree, found, gitErr := gitFacts(ctx, launchCWD); gitErr != nil {
		return nil, gitErr
	} else if found {
		materialized.SourceRoot = root
		materialized.Commit = commit
		materialized.TreeHash = tree
	}
	if err := save(sess, *materialized); err != nil {
		if materialized.WorktreePath != "" {
			_ = removeWorktree(ctx, materialized.SourceRoot, materialized.WorktreePath)
		}
		return nil, err
	}
	if err := appendPrepared(sess, materialized); err != nil {
		return nil, err
	}
	return materialized, nil
}

func prepareHeadCopy(ctx context.Context, sess *session.Session, launchCWD string, materialized *Materialized) error {
	sourceRoot, commit, tree, found, err := gitFacts(ctx, launchCWD)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("head-copy workspace requires a Git repository")
	}
	relative, err := filepath.Rel(sourceRoot, launchCWD)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("workspace directory is outside its Git root")
	}
	worktreePath := filepath.Join(sess.Root, "runtime", "workspace")
	if _, err := os.Lstat(worktreePath); err == nil {
		return fmt.Errorf("head-copy worktree path already exists: %s", worktreePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := runGit(ctx, sourceRoot, "worktree", "add", "--detach", worktreePath, commit); err != nil {
		return fmt.Errorf("create detached head-copy worktree: %w", err)
	}
	executionCWD := filepath.Join(worktreePath, relative)
	if _, err := absoluteDirectory(executionCWD); err != nil {
		_ = removeWorktree(ctx, sourceRoot, worktreePath)
		return fmt.Errorf("recorded head-copy subdirectory is unavailable: %w", err)
	}
	materialized.ExecutionCWD = executionCWD
	materialized.WorktreePath = worktreePath
	materialized.SourceRoot = sourceRoot
	materialized.Commit = commit
	materialized.TreeHash = tree
	return nil
}

// Recover returns the recorded execution boundary without source inventories
// or re-digests. A head-copy is checked only through Git's own commit/tree
// view, which is the reproducibility boundary this package owns.
func Recover(ctx context.Context, sess *session.Session) (*Materialized, error) {
	state, err := load(sess)
	if err != nil {
		return nil, err
	}
	materialized := state.materialized()
	if _, err := absoluteDirectory(materialized.ExecutionCWD); err != nil {
		return nil, fmt.Errorf("recorded workspace directory is unavailable: %w", err)
	}
	if materialized.Mode != ModeHeadCopy {
		return materialized, nil
	}
	if strings.TrimSpace(materialized.WorktreePath) == "" || strings.TrimSpace(materialized.Commit) == "" || strings.TrimSpace(materialized.TreeHash) == "" {
		return nil, errors.New("head-copy workspace state is incomplete")
	}
	commit, err := gitOutput(ctx, materialized.WorktreePath, "rev-parse", "HEAD^{commit}")
	if err != nil || commit != materialized.Commit {
		return nil, errors.New("head-copy worktree no longer matches its recorded HEAD")
	}
	tree, err := gitOutput(ctx, materialized.WorktreePath, "rev-parse", "HEAD^{tree}")
	if err != nil || tree != materialized.TreeHash {
		return nil, errors.New("head-copy worktree no longer matches its recorded tree")
	}
	return materialized, nil
}

// Cleanup removes only the exact detached worktree recorded for this session.
// It deliberately leaves the session directory to its caller.
func Cleanup(ctx context.Context, sess *session.Session) error {
	state, err := load(sess)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Mode != ModeHeadCopy || state.WorktreePath == "" {
		return nil
	}
	return removeWorktree(ctx, state.SourceRoot, state.WorktreePath)
}

// Projection provides the observable workspace provenance used by reports.
func Projection(sess *session.Session) (map[string]any, error) {
	state, err := load(sess)
	if err != nil {
		return nil, err
	}
	projection := map[string]any{
		"mode":                        state.Mode,
		"commit":                      state.Commit,
		"tree_hash":                   state.TreeHash,
		WorkspaceContentSourceKey:     WorkspaceContentSourceWorkingTree,
		WorkingTreeChangesIncludedKey: true,
	}
	if state.Mode == ModeHeadCopy {
		projection[WorkspaceContentSourceKey] = WorkspaceContentSourceCommittedHead
		projection[WorkingTreeChangesIncludedKey] = false
	}
	return projection, nil
}

func appendPrepared(sess *session.Session, materialized *Materialized) error {
	if materialized == nil || materialized.Commit == "" || materialized.TreeHash == "" {
		return nil
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
		eventlog.WorkspacePreparedPayload{Mode: materialized.Mode, Commit: materialized.Commit, TreeHash: materialized.TreeHash},
	))
	return err
}

func statePath(sess *session.Session) string {
	return filepath.Join(sess.Root, "runtime", stateFilename)
}

func save(sess *session.Session, materialized Materialized) error {
	state := persistedState{
		Mode: materialized.Mode, ExecutionCWD: materialized.ExecutionCWD, WorktreePath: materialized.WorktreePath,
		SourceRoot: materialized.SourceRoot, Commit: materialized.Commit, TreeHash: materialized.TreeHash,
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := statePath(sess)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
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
	return file.Close()
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
	return &Materialized{Mode: s.Mode, ExecutionCWD: s.ExecutionCWD, WorktreePath: s.WorktreePath, SourceRoot: s.SourceRoot, Commit: s.Commit, TreeHash: s.TreeHash}
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
	if _, err := os.Lstat(worktreePath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	_, err := runGit(ctx, sourceRoot, "worktree", "remove", "--force", worktreePath)
	return err
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
