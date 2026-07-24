package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/gitexec"
)

var rootInitializationGitHookSamples = map[string]bool{
	"applypatch-msg.sample":     true,
	"commit-msg.sample":         true,
	"fsmonitor-watchman.sample": true,
	"post-update.sample":        true,
	"pre-applypatch.sample":     true,
	"pre-commit.sample":         true,
	"pre-merge-commit.sample":   true,
	"pre-push.sample":           true,
	"pre-rebase.sample":         true,
	"pre-receive.sample":        true,
	"prepare-commit-msg.sample": true,
	"push-to-checkout.sample":   true,
	"sendemail-validate.sample": true,
	"update.sample":             true,
}

// validateInitializationSessionRepository proves that a completed git init
// created only the deterministic administrative namespace needed by the empty
// initialization commit. This lets crash recovery finish a persisted
// session-repository operation intent without accepting an arbitrary .git
// subtree observed after failure.
func validateInitializationSessionRepository(sessionRoot string) error {
	ctx := context.Background()
	topLevel, err := rootInitializationGitOutput(ctx, sessionRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	canonicalTopLevel, err := initializationLexicalPath(topLevel)
	if err != nil || canonicalTopLevel != sessionRoot {
		return errors.New("initialization session repository root does not match the journal")
	}
	head, err := rootInitializationGitOutput(ctx, sessionRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || head == "" {
		return errors.New("initialization session repository is missing its empty commit")
	}
	treeListing, err := rootInitializationGitOutput(ctx, sessionRoot, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		return err
	}
	if treeListing != "" {
		return errors.New("initialization session repository commit is not empty")
	}
	if _, err := rootInitializationGitOutput(ctx, sessionRoot, "fsck", "--full", "--strict", "--no-reflogs"); err != nil {
		return err
	}
	headRef, err := rootInitializationGitOutput(ctx, sessionRoot, "symbolic-ref", "-q", "HEAD")
	if err != nil || !strings.HasPrefix(headRef, "refs/heads/") {
		return errors.New("initialization session repository HEAD is not a local branch")
	}
	branchPath := strings.TrimPrefix(headRef, "refs/heads/")
	if branchPath == "" ||
		filepath.IsAbs(branchPath) ||
		branchPath == ".." ||
		strings.HasPrefix(branchPath, "../") {
		return errors.New("initialization session repository branch identity is invalid")
	}
	objectListing, err := rootInitializationGitOutput(ctx, sessionRoot, "rev-list", "--objects", "HEAD")
	if err != nil {
		return err
	}
	allowedFiles := map[string]bool{
		"HEAD":           true,
		"COMMIT_EDITMSG": true,
		"config":         true,
		"description":    true,
		"index":          true,
		filepath.ToSlash(filepath.Join("refs", "heads", branchPath)):         true,
		filepath.ToSlash(filepath.Join("logs", "HEAD")):                      true,
		filepath.ToSlash(filepath.Join("logs", "refs", "heads", branchPath)): true,
	}
	for _, line := range strings.Split(objectListing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		oid := fields[0]
		if len(oid) != 40 && len(oid) != 64 {
			return errors.New("initialization session repository object identity is invalid")
		}
		allowedFiles[filepath.ToSlash(filepath.Join("objects", oid[:2], oid[2:]))] = true
	}
	allowedDirectories := map[string]bool{
		".":               true,
		"branches":        true,
		"hooks":           true,
		"info":            true,
		"logs":            true,
		"logs/refs":       true,
		"logs/refs/heads": true,
		"objects":         true,
		"objects/info":    true,
		"objects/pack":    true,
		"refs":            true,
		"refs/heads":      true,
		"refs/tags":       true,
	}
	for relative := range allowedFiles {
		for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative))); parent != "."; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
			allowedDirectories[parent] = true
		}
	}
	gitRoot := filepath.Join(sessionRoot, ".git")
	return filepath.WalkDir(gitRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(gitRoot, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(filepath.Clean(relative))
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("initialization session repository path %s is a symlink", relative)
		}
		if info.IsDir() {
			if !allowedDirectories[relative] {
				return fmt.Errorf("initialization session repository contains foreign directory %s", relative)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("initialization session repository path %s has unsupported type", relative)
		}
		if strings.HasPrefix(relative, "hooks/") {
			if !rootInitializationGitHookSamples[strings.TrimPrefix(relative, "hooks/")] {
				return fmt.Errorf("initialization session repository contains foreign hook %s", relative)
			}
			return nil
		}
		if relative == "info/exclude" || allowedFiles[relative] {
			return nil
		}
		return fmt.Errorf("initialization session repository contains foreign file %s", relative)
	})
}

func rootInitializationGitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	output, err := gitexec.Run(ctx, "git", cwd, nil, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
