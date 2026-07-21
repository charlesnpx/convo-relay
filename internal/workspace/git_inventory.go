package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

var (
	errNotGitRepository = errors.New("not a Git repository")
	errUnbornRepository = errors.New("Git repository has no committed HEAD")
	errInventoryDrift   = errors.New("source changed while its inventory was being captured")
)

type repositorySnapshot struct {
	gitBinary     string
	root          string
	launchCWD     string
	launchSubpath string
	headCommit    string
	headTree      string
	objectFormat  string
	sourceDigest  string
	sourceReport  map[string]any
	exclusions    map[string]any
}

type indexEntry struct {
	path  string
	mode  string
	oid   string
	stage int
}

type headEntry struct {
	path       string
	mode       string
	objectType string
	oid        string
}

type filesystemEntry struct {
	path            string
	present         bool
	mode            string
	permissions     string
	sizeBytes       int64
	rawDigest       string
	obstruction     string
	gitlinkState    string
	submoduleID     string
	submoduleDigest string
}

type gitCommandError struct {
	args   []string
	detail string
	cause  error
}

func (e *gitCommandError) Error() string {
	if e.detail != "" {
		return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), e.detail)
	}
	return fmt.Sprintf("git %s: %v", strings.Join(e.args, " "), e.cause)
}

func (e *gitCommandError) Unwrap() error { return e.cause }

func inspectRepository(ctx context.Context, gitBinary string, launchCWD string) (*repositorySnapshot, error) {
	previous, err := inspectRepositoryPass(ctx, gitBinary, launchCWD)
	if err != nil {
		return nil, err
	}
	// Require two matching complete inventories. One additional pass is a
	// bounded retry for a source that changed between the first two passes.
	for attempt := 0; attempt < 2; attempt++ {
		current, err := inspectRepositoryPass(ctx, gitBinary, launchCWD)
		if err != nil {
			return nil, err
		}
		stable, err := repositorySnapshotsEquivalent(previous, current)
		if err != nil {
			return nil, err
		}
		if stable {
			return current, nil
		}
		previous = current
	}
	return nil, errInventoryDrift
}

func repositorySnapshotsEquivalent(left *repositorySnapshot, right *repositorySnapshot) (bool, error) {
	if left == nil || right == nil {
		return left == right, nil
	}
	leftDigest, err := semanticDigest(map[string]any{
		"root":           left.root,
		"launch_cwd":     left.launchCWD,
		"launch_subpath": left.launchSubpath,
		"source":         left.sourceReport,
		"exclusions":     left.exclusions,
	})
	if err != nil {
		return false, err
	}
	rightDigest, err := semanticDigest(map[string]any{
		"root":           right.root,
		"launch_cwd":     right.launchCWD,
		"launch_subpath": right.launchSubpath,
		"source":         right.sourceReport,
		"exclusions":     right.exclusions,
	})
	if err != nil {
		return false, err
	}
	return leftDigest == rightDigest, nil
}

func inspectRepositoryPass(ctx context.Context, gitBinary string, launchCWD string) (*repositorySnapshot, error) {
	rootOutput, err := runGit(ctx, gitBinary, launchCWD, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if gitExecutableUnavailable(err) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", errNotGitRepository, err)
	}
	rootText := strings.TrimSuffix(strings.TrimSuffix(string(rootOutput), "\n"), "\r")
	root, err := canonicalExistingDirectory(rootText)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotGitRepository, err)
	}
	headOutput, err := runGit(ctx, gitBinary, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnbornRepository, err)
	}
	headCommit := strings.TrimSpace(string(headOutput))
	treeOutput, err := runGit(ctx, gitBinary, root, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	headTree := strings.TrimSpace(string(treeOutput))
	objectFormat := "sha1"
	if output, formatErr := runGit(ctx, gitBinary, root, "rev-parse", "--show-object-format"); formatErr == nil {
		if candidate := strings.TrimSpace(string(output)); candidate != "" {
			objectFormat = candidate
		}
	}
	launchSubpath, err := filepath.Rel(root, launchCWD)
	if err != nil || filepath.IsAbs(launchSubpath) || launchSubpath == ".." || strings.HasPrefix(launchSubpath, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("launch CWD is outside Git root")
	}
	if launchSubpath == "" {
		launchSubpath = "."
	}
	launchSubpath = filepath.ToSlash(launchSubpath)

	indexOutput, err := runGit(ctx, gitBinary, root, "ls-files", "--stage", "-z", "--")
	if err != nil {
		return nil, err
	}
	indexEntries, err := parseIndexEntries(indexOutput)
	if err != nil {
		return nil, err
	}
	headTreeOutput, err := runGit(ctx, gitBinary, root, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	if err != nil {
		return nil, err
	}
	headEntries, err := parseHeadEntries(headTreeOutput)
	if err != nil {
		return nil, err
	}
	untrackedOutput, err := runGit(ctx, gitBinary, root, "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return nil, err
	}
	untrackedPaths := uniqueSorted(parseNULPaths(untrackedOutput))

	trackedPaths := trackedPathUnion(indexEntries, headEntries)
	trackedFilesystem := make([]filesystemEntry, 0, len(trackedPaths))
	for _, path := range trackedPaths {
		expectedGitlink := pathIsGitlink(path, indexEntries)
		entry, err := inspectFilesystemEntry(ctx, gitBinary, root, path, expectedGitlink)
		if err != nil {
			return nil, fmt.Errorf("tracked path %q: %w", path, err)
		}
		trackedFilesystem = append(trackedFilesystem, entry)
	}
	untrackedFilesystem := make([]filesystemEntry, 0, len(untrackedPaths))
	for _, path := range untrackedPaths {
		entry, err := inspectFilesystemEntry(ctx, gitBinary, root, path, false)
		if err != nil {
			return nil, fmt.Errorf("untracked path %q: %w", path, err)
		}
		untrackedFilesystem = append(untrackedFilesystem, entry)
	}

	stagedOutput, err := runGit(ctx, gitBinary, root, "diff-index", "--cached", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return nil, err
	}
	unstagedOutput, err := runGit(ctx, gitBinary, root, "diff-files", "--name-only", "-z", "--")
	if err != nil {
		return nil, err
	}
	stagedPaths := uniqueSorted(parseNULPaths(stagedOutput))
	unstagedPaths := uniqueSorted(parseNULPaths(unstagedOutput))

	indexReport := indexEntriesReport(indexEntries)
	trackedReport := filesystemEntriesReport(trackedFilesystem)
	untrackedReport := filesystemEntriesReport(untrackedFilesystem)
	digestMaterial := map[string]any{
		"head_commit":        headCommit,
		"head_tree":          headTree,
		"object_format":      objectFormat,
		"index_entries":      indexReport,
		"tracked_worktree":   trackedReport,
		"untracked_worktree": untrackedReport,
	}
	sourceDigest, err := semanticDigest(digestMaterial)
	if err != nil {
		return nil, err
	}
	exclusions := map[string]any{
		"staged":    pathRecords(stagedPaths),
		"unstaged":  pathRecords(unstagedPaths),
		"untracked": pathRecords(untrackedPaths),
	}
	sourceReport := map[string]any{
		"repository_state":     "committed",
		"git_root":             root,
		"launch_cwd":           launchCWD,
		"launch_subpath":       launchSubpath,
		"head_commit":          headCommit,
		"head_tree":            headTree,
		"object_format":        objectFormat,
		"source_before_digest": sourceDigest,
		"inventory": map[string]any{
			"index_entries":      indexReport,
			"tracked_worktree":   trackedReport,
			"untracked_worktree": untrackedReport,
		},
	}
	return &repositorySnapshot{
		gitBinary:     gitBinary,
		root:          root,
		launchCWD:     launchCWD,
		launchSubpath: launchSubpath,
		headCommit:    headCommit,
		headTree:      headTree,
		objectFormat:  objectFormat,
		sourceDigest:  sourceDigest,
		sourceReport:  sourceReport,
		exclusions:    exclusions,
	}, nil
}

func verifyCommittedLaunchSubpath(ctx context.Context, repository *repositorySnapshot) error {
	if repository == nil || repository.launchSubpath == "." {
		return nil
	}
	spec := "HEAD:" + repository.launchSubpath
	output, err := runGit(ctx, repository.gitBinary, repository.root, "cat-file", "-t", spec)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != "tree" {
		return fmt.Errorf("%s is not a committed directory", repository.launchSubpath)
	}
	return nil
}

func runGit(ctx context.Context, gitBinary string, cwd string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	commandArgs := make([]string, 0, len(args)+2)
	if strings.TrimSpace(cwd) != "" {
		commandArgs = append(commandArgs, "-C", cwd)
	}
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, gitBinary, commandArgs...)
	command.Env = controlledGitEnvironment(os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, &gitCommandError{args: args, detail: strings.TrimSpace(string(output)), cause: err}
	}
	return output, nil
}

func controlledGitEnvironment(environ []string) []string {
	result := make([]string, 0, len(environ)+3)
	for _, entry := range environ {
		key := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			key = entry[:separator]
		}
		if strings.EqualFold(key, "LC_ALL") || strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
}

func parseIndexEntries(data []byte) ([]indexEntry, error) {
	entries := make([]indexEntry, 0)
	for _, record := range splitNUL(data) {
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("invalid git index record")
		}
		fields := strings.Fields(string(record[:tab]))
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid git index metadata")
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil || stage < 0 || stage > 3 {
			return nil, fmt.Errorf("invalid git index stage")
		}
		entries = append(entries, indexEntry{path: string(record[tab+1:]), mode: fields[0], oid: fields[1], stage: stage})
	}
	sort.Slice(entries, func(left int, right int) bool {
		if entries[left].path == entries[right].path {
			return entries[left].stage < entries[right].stage
		}
		return entries[left].path < entries[right].path
	})
	return entries, nil
}

func parseHeadEntries(data []byte) ([]headEntry, error) {
	entries := make([]headEntry, 0)
	for _, record := range splitNUL(data) {
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("invalid git tree record")
		}
		fields := strings.Fields(string(record[:tab]))
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid git tree metadata")
		}
		entries = append(entries, headEntry{path: string(record[tab+1:]), mode: fields[0], objectType: fields[1], oid: fields[2]})
	}
	sort.Slice(entries, func(left int, right int) bool { return entries[left].path < entries[right].path })
	return entries, nil
}

func splitNUL(data []byte) [][]byte {
	parts := bytes.Split(data, []byte{0})
	result := make([][]byte, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			result = append(result, part)
		}
	}
	return result
}

func parseNULPaths(data []byte) []string {
	parts := splitNUL(data)
	result := make([]string, len(parts))
	for index, part := range parts {
		result[index] = string(part)
	}
	return result
}

func trackedPathUnion(indexEntries []indexEntry, headEntries []headEntry) []string {
	paths := map[string]bool{}
	for _, entry := range indexEntries {
		paths[entry.path] = true
	}
	for _, entry := range headEntries {
		paths[entry.path] = true
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Slice(result, func(left int, right int) bool { return result[left] < result[right] })
	return result
}

func pathIsGitlink(path string, indexEntries []indexEntry) bool {
	for _, entry := range indexEntries {
		if entry.path == path && entry.mode == "160000" {
			return true
		}
	}
	return false
}

func inspectFilesystemEntry(ctx context.Context, gitBinary string, root string, gitPath string, expectedGitlink bool) (filesystemEntry, error) {
	if err := validateGitPath(gitPath); err != nil {
		return filesystemEntry{}, err
	}
	fullPath := filepath.Join(root, filepath.FromSlash(gitPath))
	before, err := os.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			entry := filesystemEntry{path: gitPath, present: false}
			if errors.Is(err, syscall.ENOTDIR) {
				entry.obstruction = "ancestor_not_directory"
			}
			if expectedGitlink {
				entry.gitlinkState = "uninitialized"
			}
			return entry, nil
		}
		return filesystemEntry{}, err
	}
	entry := filesystemEntry{path: gitPath, present: true, permissions: fmt.Sprintf("%04o", before.Mode().Perm())}
	if before.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		if err != nil {
			return filesystemEntry{}, err
		}
		after, err := os.Lstat(fullPath)
		if err != nil || !os.SameFile(before, after) {
			return filesystemEntry{}, fmt.Errorf("symlink changed during inventory")
		}
		entry.mode = "120000"
		entry.sizeBytes = int64(len([]byte(target)))
		entry.rawDigest = digestBytes([]byte(target))
		return entry, nil
	}
	if before.IsDir() {
		if expectedGitlink {
			return inspectGitlinkEntry(ctx, gitBinary, fullPath, entry)
		}
		return filesystemEntry{path: gitPath, present: false, obstruction: "directory"}, nil
	}
	if !before.Mode().IsRegular() {
		return filesystemEntry{}, fmt.Errorf("source inventory supports regular files, symlinks, and gitlinks only")
	}
	handle, err := openWorkspaceFileNoFollow(fullPath)
	if err != nil {
		return filesystemEntry{}, err
	}
	defer handle.Close()
	opened, err := handle.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || before.Size() != opened.Size() || before.Mode() != opened.Mode() || !before.ModTime().Equal(opened.ModTime()) {
		return filesystemEntry{}, fmt.Errorf("file changed before inventory read")
	}
	hasher := sha256.New()
	size, err := io.Copy(hasher, handle)
	if err != nil {
		return filesystemEntry{}, err
	}
	after, err := handle.Stat()
	if err != nil || after.Size() != opened.Size() || after.Mode() != opened.Mode() || !after.ModTime().Equal(opened.ModTime()) {
		return filesystemEntry{}, fmt.Errorf("file changed during inventory read")
	}
	pathAfter, err := os.Lstat(fullPath)
	if err != nil || !os.SameFile(opened, pathAfter) || pathAfter.Size() != opened.Size() || pathAfter.Mode() != opened.Mode() || !pathAfter.ModTime().Equal(opened.ModTime()) {
		return filesystemEntry{}, fmt.Errorf("file path changed during inventory read")
	}
	entry.permissions = fmt.Sprintf("%04o", opened.Mode().Perm())
	entry.mode = "100644"
	if opened.Mode().Perm()&0o111 != 0 {
		entry.mode = "100755"
	}
	entry.sizeBytes = size
	entry.rawDigest = "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	return entry, nil
}

func inspectGitlinkEntry(ctx context.Context, gitBinary string, fullPath string, entry filesystemEntry) (filesystemEntry, error) {
	rootOutput, err := runGit(ctx, gitBinary, fullPath, "rev-parse", "--show-toplevel")
	if err != nil {
		entry.present = false
		entry.gitlinkState = "uninitialized"
		entry.permissions = ""
		return entry, nil
	}
	discoveredRoot, err := canonicalExistingDirectory(strings.TrimSpace(string(rootOutput)))
	if err != nil {
		return filesystemEntry{}, err
	}
	if !pathsEquivalent(discoveredRoot, fullPath) {
		entry.present = false
		entry.gitlinkState = "uninitialized"
		entry.permissions = ""
		return entry, nil
	}
	submodule, err := inspectRepository(ctx, gitBinary, fullPath)
	if err != nil {
		return filesystemEntry{}, fmt.Errorf("gitlink inventory: %w", err)
	}
	entry.mode = "160000"
	entry.gitlinkState = "initialized"
	entry.submoduleID = submodule.headCommit
	entry.submoduleDigest = submodule.sourceDigest
	entry.rawDigest = submodule.sourceDigest
	return entry, nil
}

func validateGitPath(path string) error {
	if path == "" || strings.IndexByte(path, 0) >= 0 || filepath.IsAbs(filepath.FromSlash(path)) {
		return fmt.Errorf("invalid Git path")
	}
	cleaned := filepath.Clean(filepath.FromSlash(path))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("Git path escapes repository")
	}
	return nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func indexEntriesReport(entries []indexEntry) []any {
	result := make([]any, 0, len(entries))
	for _, entry := range entries {
		item := pathMap(entry.path)
		item["mode"] = entry.mode
		item["oid"] = entry.oid
		item["stage"] = entry.stage
		result = append(result, item)
	}
	return result
}

func filesystemEntriesReport(entries []filesystemEntry) []any {
	result := make([]any, 0, len(entries))
	for _, entry := range entries {
		item := pathMap(entry.path)
		item["present"] = entry.present
		if entry.obstruction != "" {
			item["obstruction"] = entry.obstruction
		}
		if entry.gitlinkState != "" {
			item["gitlink_state"] = entry.gitlinkState
		}
		if entry.present {
			item["mode"] = entry.mode
			item["permissions"] = entry.permissions
			item["size_bytes"] = entry.sizeBytes
			item["raw_digest"] = entry.rawDigest
			if entry.submoduleID != "" {
				item["submodule_head"] = entry.submoduleID
			}
			if entry.submoduleDigest != "" {
				item["submodule_source_digest"] = entry.submoduleDigest
			}
		}
		result = append(result, item)
	}
	return result
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Slice(result, func(left int, right int) bool { return result[left] < result[right] })
	return result
}

func repositoryStateForError(err error) string {
	if errors.Is(err, errUnbornRepository) {
		return "unborn"
	}
	return "non_git"
}

func gitExecutableUnavailable(err error) bool {
	var commandError *gitCommandError
	if !errors.As(err, &commandError) {
		return false
	}
	if errors.Is(commandError.cause, exec.ErrNotFound) {
		return true
	}
	var executableError *exec.Error
	if errors.As(commandError.cause, &executableError) {
		return true
	}
	var pathError *os.PathError
	return errors.As(commandError.cause, &pathError)
}
