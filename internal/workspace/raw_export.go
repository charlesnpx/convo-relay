package workspace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/gitexec"
)

type rawTreeEntry struct {
	path       string
	mode       string
	objectType string
	oid        string
	sizeBytes  int64
	rawDigest  string
}

type rawExportDescriptor struct {
	entries         []rawTreeEntry
	fileCount       int64
	byteCount       int64
	caseInsensitive bool
}

func exportCapturedTree(
	ctx context.Context,
	repository *repositorySnapshot,
	worktreePath string,
	limits repositoryInventoryLimits,
) (*rawExportDescriptor, error) {
	if repository == nil {
		return nil, errors.New("repository snapshot is required for raw export")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	treeOutput, err := runGitNULRecords(
		ctx,
		repository.gitBinary,
		repository.root,
		limits.maxFiles,
		limits.maxFiles,
		"ls-tree", "-r", "-z", "--full-tree", repository.headTree,
	)
	if err != nil {
		return nil, fmt.Errorf("read captured Git tree: %w", err)
	}
	headEntries, err := parseHeadEntries(treeOutput)
	if err != nil {
		return nil, err
	}
	caseInsensitive, err := filesystemCaseInsensitive(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("probe export filesystem path semantics: %w", err)
	}
	entries, err := validateRawTreeEntries(headEntries, caseInsensitive)
	if err != nil {
		return nil, err
	}

	batch, err := startCatFileBatch(ctx, repository.gitBinary, repository.root)
	if err != nil {
		return nil, err
	}
	descriptor := &rawExportDescriptor{
		entries:         make([]rawTreeEntry, 0, len(entries)),
		caseInsensitive: caseInsensitive,
	}
	accounting := newRepositoryInventoryState(limits)
	var exportErr error
	for _, entry := range entries {
		if err := contextError(ctx); err != nil {
			exportErr = err
			break
		}
		switch entry.mode {
		case "100644", "100755":
			entry, err = exportRawRegular(ctx, batch, worktreePath, entry, accounting)
		case "120000":
			entry, err = exportRawSymlink(ctx, batch, worktreePath, entry, accounting)
		case "160000":
			err = accounting.addEntry(0)
			if err == nil {
				err = exportRawGitlink(worktreePath, entry)
			}
		default:
			err = fmt.Errorf("unsupported captured-tree mode %s", entry.mode)
		}
		if err != nil {
			exportErr = fmt.Errorf("export captured path %q: %w", entry.path, err)
			break
		}
		descriptor.entries = append(descriptor.entries, entry)
	}
	closeErr := batch.Close()
	if exportErr != nil || closeErr != nil {
		return nil, errors.Join(exportErr, closeErr)
	}
	descriptor.fileCount = accounting.files
	descriptor.byteCount = accounting.bytes
	if err := verifyRawExport(ctx, worktreePath, descriptor); err != nil {
		return nil, err
	}
	return descriptor, nil
}

func validateRawTreeEntries(entries []headEntry, caseInsensitive ...bool) ([]rawTreeEntry, error) {
	foldCase := len(caseInsensitive) > 0 && caseInsensitive[0]
	result := make([]rawTreeEntry, 0, len(entries))
	normalizedTargets := map[string]string{}
	symlinks := map[string]bool{}
	for _, entry := range entries {
		if err := validateRawTreePath(entry.path); err != nil {
			return nil, fmt.Errorf("captured tree path %q: %w", entry.path, err)
		}
		if err := validateRawModeType(entry.mode, entry.objectType); err != nil {
			return nil, fmt.Errorf("captured tree path %q: %w", entry.path, err)
		}
		key := normalizedRawTarget(entry.path, foldCase)
		if prior, exists := normalizedTargets[key]; exists {
			return nil, fmt.Errorf("captured tree paths %q and %q normalize to the same target", prior, entry.path)
		}
		normalizedTargets[key] = entry.path
		for ancestor := parentGitPath(entry.path); ancestor != ""; ancestor = parentGitPath(ancestor) {
			if symlinks[normalizedRawTarget(ancestor, foldCase)] {
				return nil, fmt.Errorf("captured tree path %q descends through symlink %q", entry.path, ancestor)
			}
		}
		if entry.mode == "120000" {
			symlinks[key] = true
		}
		result = append(result, rawTreeEntry{
			path:       entry.path,
			mode:       entry.mode,
			objectType: entry.objectType,
			oid:        entry.oid,
		})
	}
	return result, nil
}

func validateRawTreePath(gitPath string) error {
	if gitPath == "" || strings.IndexByte(gitPath, 0) >= 0 || strings.HasPrefix(gitPath, "/") {
		return errors.New("path must be non-empty and relative")
	}
	components := strings.Split(gitPath, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return errors.New("path contains an empty or traversal component")
		}
		if strings.EqualFold(component, ".git") {
			return errors.New("path collides with Git administrative data")
		}
	}
	native := filepath.FromSlash(gitPath)
	if filepath.IsAbs(native) {
		return errors.New("path is absolute after platform conversion")
	}
	cleaned := filepath.Clean(native)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return errors.New("path escapes the export root")
	}
	return nil
}

func validateRawModeType(mode string, objectType string) error {
	switch mode {
	case "100644", "100755", "120000":
		if objectType != "blob" {
			return fmt.Errorf("mode %s requires a blob, got %s", mode, objectType)
		}
	case "160000":
		if objectType != "commit" {
			return fmt.Errorf("gitlink mode requires a commit, got %s", objectType)
		}
	default:
		return fmt.Errorf("mode %s is unsupported", mode)
	}
	return nil
}

func normalizedRawTarget(gitPath string, caseInsensitive bool) string {
	value := filepath.Clean(filepath.FromSlash(gitPath))
	if caseInsensitive {
		value = strings.ToLower(value)
	}
	return value
}

func filesystemCaseInsensitive(root string) (result bool, err error) {
	probe, err := os.CreateTemp(root, "convo-relay-case-probe-a-")
	if err != nil {
		return false, err
	}
	probePath := probe.Name()
	defer func() {
		err = errors.Join(err, probe.Close(), os.Remove(probePath))
	}()
	base := filepath.Base(probePath)
	variantBase := strings.Replace(base, "a", "A", 1)
	if variantBase == base {
		return false, errors.New("case-sensitivity probe could not construct a case variant")
	}
	variantPath := filepath.Join(filepath.Dir(probePath), variantBase)
	originalInfo, err := os.Lstat(probePath)
	if err != nil {
		return false, err
	}
	variantInfo, err := os.Lstat(variantPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(originalInfo, variantInfo), nil
}

func parentGitPath(gitPath string) string {
	index := strings.LastIndexByte(gitPath, '/')
	if index < 0 {
		return ""
	}
	return gitPath[:index]
}

func exportRawRegular(
	ctx context.Context,
	batch *catFileBatch,
	root string,
	entry rawTreeEntry,
	accounting *repositoryInventoryState,
) (rawTreeEntry, error) {
	target, err := prepareRawTarget(root, entry.path)
	if err != nil {
		return entry, err
	}
	header, err := batch.begin(entry.oid)
	if err != nil {
		return entry, err
	}
	if header.objectType != "blob" {
		return entry, fmt.Errorf("object %s is %s, want blob", entry.oid, header.objectType)
	}
	if err := accounting.addEntry(header.size); err != nil {
		return entry, err
	}
	mode := os.FileMode(0o644)
	if entry.mode == "100755" {
		mode = 0o755
	}
	handle, err := createWorkspaceFileNoFollow(target, mode)
	if err != nil {
		return entry, err
	}
	hasher := sha256.New()
	writer := io.MultiWriter(handle, hasher)
	_, copyErr := io.CopyN(writer, &contextReader{ctx: ctx, reader: batch.reader}, header.size)
	closeErr := handle.Close()
	finishErr := batch.finish()
	if copyErr != nil || closeErr != nil || finishErr != nil {
		return entry, errors.Join(copyErr, closeErr, finishErr)
	}
	if err := os.Chmod(target, mode); err != nil {
		return entry, err
	}
	entry.sizeBytes = header.size
	entry.rawDigest = "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	return entry, nil
}

func exportRawSymlink(
	ctx context.Context,
	batch *catFileBatch,
	root string,
	entry rawTreeEntry,
	accounting *repositoryInventoryState,
) (rawTreeEntry, error) {
	target, err := prepareRawTarget(root, entry.path)
	if err != nil {
		return entry, err
	}
	header, err := batch.begin(entry.oid)
	if err != nil {
		return entry, err
	}
	if header.objectType != "blob" {
		return entry, fmt.Errorf("object %s is %s, want blob", entry.oid, header.objectType)
	}
	if err := accounting.addEntry(header.size); err != nil {
		return entry, err
	}
	if header.size > int64(int(^uint(0)>>1)) {
		return entry, errors.New("symlink target is too large for this platform")
	}
	data := make([]byte, int(header.size))
	_, readErr := io.ReadFull(&contextReader{ctx: ctx, reader: batch.reader}, data)
	finishErr := batch.finish()
	if readErr != nil || finishErr != nil {
		return entry, errors.Join(readErr, finishErr)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return entry, errors.New("symlink target contains NUL")
	}
	if err := os.Symlink(string(data), target); err != nil {
		return entry, err
	}
	entry.sizeBytes = header.size
	entry.rawDigest = digestBytes(data)
	return entry, nil
}

func exportRawGitlink(root string, entry rawTreeEntry) error {
	target, err := prepareRawTarget(root, entry.path)
	if err != nil {
		return err
	}
	return os.Mkdir(target, 0o755)
}

func prepareRawTarget(root string, gitPath string) (string, error) {
	components := strings.Split(gitPath, "/")
	current := root
	for _, component := range components[:len(components)-1] {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		switch {
		case os.IsNotExist(err):
			if err := os.Mkdir(current, 0o755); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case info.Mode()&os.ModeSymlink != 0:
			return "", fmt.Errorf("target ancestor %s is a symlink", current)
		case !info.IsDir():
			return "", fmt.Errorf("target ancestor %s is not a directory", current)
		}
	}
	target := filepath.Join(root, filepath.FromSlash(gitPath))
	if info, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("target already exists with mode %s", info.Mode())
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if !pathContains(root, target) {
		return "", errors.New("target escapes export root")
	}
	return target, nil
}

func verifyRawExport(ctx context.Context, root string, descriptor *rawExportDescriptor) error {
	if descriptor == nil {
		return errors.New("raw export descriptor is required")
	}
	expected := make(map[string]rawTreeEntry, len(descriptor.entries))
	allowedDirectories := map[string]bool{".": true}
	for _, entry := range descriptor.entries {
		expected[filepath.Clean(filepath.FromSlash(entry.path))] = entry
		for parent := filepath.Dir(filepath.FromSlash(entry.path)); parent != "."; parent = filepath.Dir(parent) {
			allowedDirectories[filepath.Clean(parent)] = true
		}
		if entry.mode == "160000" {
			allowedDirectories[filepath.Clean(filepath.FromSlash(entry.path))] = true
		}
	}

	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.Clean(relative)
		if relative == "." {
			return nil
		}
		if relative == ".git" {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entry, isExpected := expected[relative]
		if item.IsDir() {
			if !allowedDirectories[relative] {
				return fmt.Errorf("raw export contains unexpected directory %s", relative)
			}
			if isExpected && entry.mode == "160000" {
				children, err := os.ReadDir(path)
				if err != nil {
					return err
				}
				if len(children) != 0 {
					return fmt.Errorf("gitlink directory %s is not empty", relative)
				}
				seen[relative] = true
				return filepath.SkipDir
			}
			return nil
		}
		if !isExpected {
			return fmt.Errorf("raw export contains unexpected entry %s", relative)
		}
		if err := verifyRawEntry(ctx, path, entry); err != nil {
			return fmt.Errorf("verify raw export path %q: %w", entry.path, err)
		}
		seen[relative] = true
		return nil
	})
	if err != nil {
		return err
	}
	for relative, entry := range expected {
		if !seen[relative] {
			return fmt.Errorf("raw export is missing path %q", entry.path)
		}
	}
	return nil
}

func verifyRawEntry(ctx context.Context, target string, entry rawTreeEntry) error {
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	switch entry.mode {
	case "100644", "100755":
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("mode is %s, want regular file", info.Mode())
		}
		if info.Size() != entry.sizeBytes {
			return fmt.Errorf("raw size mismatch")
		}
		wantExecutable := entry.mode == "100755"
		if gotExecutable := info.Mode().Perm()&0o111 != 0; gotExecutable != wantExecutable {
			return fmt.Errorf("executable mode mismatch")
		}
		handle, err := openWorkspaceFileNoFollow(target)
		if err != nil {
			return err
		}
		opened, err := handle.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() != entry.sizeBytes {
			_ = handle.Close()
			return errors.Join(err, errors.New("raw export file changed before verification"))
		}
		hasher := sha256.New()
		reader := &contextReader{ctx: ctx, reader: handle}
		size, readErr := io.CopyN(hasher, reader, entry.sizeBytes)
		var extra [1]byte
		extraCount, extraErr := reader.Read(extra[:])
		if extraErr != nil && !errors.Is(extraErr, io.EOF) {
			readErr = errors.Join(readErr, extraErr)
		}
		after, statErr := handle.Stat()
		closeErr := handle.Close()
		pathAfter, pathErr := os.Lstat(target)
		if readErr != nil ||
			statErr != nil ||
			closeErr != nil ||
			pathErr != nil ||
			size != entry.sizeBytes ||
			extraCount != 0 ||
			!os.SameFile(opened, after) ||
			!os.SameFile(opened, pathAfter) ||
			after.Size() != entry.sizeBytes ||
			pathAfter.Size() != entry.sizeBytes ||
			after.Mode() != opened.Mode() ||
			pathAfter.Mode() != opened.Mode() {
			return errors.Join(readErr, statErr, closeErr, pathErr, errors.New("raw export file changed during bounded verification"))
		}
		digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if size != entry.sizeBytes || digest != entry.rawDigest {
			return fmt.Errorf("raw size or digest mismatch")
		}
	case "120000":
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("mode is %s, want symlink", info.Mode())
		}
		targetValue, err := os.Readlink(target)
		if err != nil {
			return err
		}
		data := []byte(targetValue)
		if int64(len(data)) != entry.sizeBytes || digestBytes(data) != entry.rawDigest {
			return fmt.Errorf("symlink target size or digest mismatch")
		}
	default:
		return fmt.Errorf("unexpected leaf mode %s", entry.mode)
	}
	return nil
}

func rawExportArtifactMap(descriptor *rawExportDescriptor) map[string]any {
	if descriptor == nil {
		return nil
	}
	entries := make([]any, 0, len(descriptor.entries))
	for _, entry := range descriptor.entries {
		item := pathMap(entry.path)
		item["mode"] = entry.mode
		item["object_type"] = entry.objectType
		item["object_id"] = entry.oid
		item["size_bytes"] = entry.sizeBytes
		if entry.rawDigest != "" {
			item["raw_digest"] = entry.rawDigest
		}
		if entry.mode == "160000" {
			item["materialization"] = "empty_directory"
		} else {
			item["materialization"] = "raw_blob"
		}
		entries = append(entries, item)
	}
	return map[string]any{
		"file_count":          descriptor.fileCount,
		"byte_count":          descriptor.byteCount,
		"path_case_sensitive": !descriptor.caseInsensitive,
		"entries":             entries,
	}
}

func rawExportDescriptorFromArtifact(artifact map[string]any) (*rawExportDescriptor, bool, error) {
	raw, exists := artifact["committed_export"]
	if !exists {
		return nil, false, nil
	}
	payload, ok := raw.(map[string]any)
	if !ok {
		return nil, true, errors.New("execution_workspace committed_export must be an object")
	}
	rawEntries, ok := payload["entries"].([]any)
	if !ok {
		return nil, true, errors.New("execution_workspace committed_export entries must be an array")
	}
	fileCount, filesOK := nonnegativeInt64(payload["file_count"])
	byteCount, bytesOK := nonnegativeInt64(payload["byte_count"])
	if !filesOK || !bytesOK || fileCount != int64(len(rawEntries)) {
		return nil, true, errors.New("execution_workspace committed_export accounting is invalid")
	}
	descriptor := &rawExportDescriptor{
		entries:   make([]rawTreeEntry, 0, len(rawEntries)),
		fileCount: fileCount,
		byteCount: byteCount,
	}
	if caseSensitive, ok := payload["path_case_sensitive"].(bool); ok {
		descriptor.caseInsensitive = !caseSensitive
	} else {
		// Legacy artifacts predate the recorded filesystem probe.
		descriptor.caseInsensitive = runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	}
	var summedBytes int64
	for index, rawEntry := range rawEntries {
		entryMap, ok := rawEntry.(map[string]any)
		if !ok {
			return nil, true, fmt.Errorf("execution_workspace committed_export entry %d must be an object", index+1)
		}
		pathBytes, err := base64.StdEncoding.Strict().DecodeString(stringValue(entryMap["path_bytes_base64"]))
		if err != nil || len(pathBytes) == 0 {
			return nil, true, fmt.Errorf("execution_workspace committed_export entry %d has invalid path bytes", index+1)
		}
		gitPath := string(pathBytes)
		if display, exists := entryMap["path"]; exists && stringValue(display) != gitPath {
			return nil, true, fmt.Errorf("execution_workspace committed_export entry %d path projections disagree", index+1)
		}
		entry := rawTreeEntry{
			path:       gitPath,
			mode:       strings.TrimSpace(stringValue(entryMap["mode"])),
			objectType: strings.TrimSpace(stringValue(entryMap["object_type"])),
			oid:        strings.TrimSpace(stringValue(entryMap["object_id"])),
			rawDigest:  strings.TrimSpace(stringValue(entryMap["raw_digest"])),
		}
		entry.sizeBytes, ok = nonnegativeInt64(entryMap["size_bytes"])
		if !ok || entry.oid == "" {
			return nil, true, fmt.Errorf("execution_workspace committed_export entry %d has invalid identity or size", index+1)
		}
		if err := validateRawTreePath(entry.path); err != nil {
			return nil, true, err
		}
		if err := validateRawModeType(entry.mode, entry.objectType); err != nil {
			return nil, true, err
		}
		if entry.mode != "160000" && entry.rawDigest == "" {
			return nil, true, fmt.Errorf("execution_workspace committed_export entry %d is missing its raw digest", index+1)
		}
		if entry.mode == "160000" && entry.sizeBytes != 0 {
			return nil, true, fmt.Errorf("execution_workspace committed_export gitlink %d has nonzero size", index+1)
		}
		summedBytes, err = checkedDescriptorBytes(summedBytes, entry.sizeBytes)
		if err != nil {
			return nil, true, err
		}
		descriptor.entries = append(descriptor.entries, entry)
	}
	if summedBytes != byteCount {
		return nil, true, errors.New("execution_workspace committed_export byte count disagrees with its entries")
	}
	validated, err := validateRawTreeEntries(rawEntriesAsHeadEntries(descriptor.entries), descriptor.caseInsensitive)
	if err != nil {
		return nil, true, err
	}
	if len(validated) != len(descriptor.entries) {
		return nil, true, errors.New("execution_workspace committed_export descriptor changed during validation")
	}
	return descriptor, true, nil
}

func checkedDescriptorBytes(current int64, increment int64) (int64, error) {
	if current < 0 || increment < 0 || current > int64(^uint64(0)>>1)-increment {
		return 0, errors.New("execution_workspace committed_export byte accounting overflowed")
	}
	return current + increment, nil
}

func rawEntriesAsHeadEntries(entries []rawTreeEntry) []headEntry {
	result := make([]headEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, headEntry{
			path:       entry.path,
			mode:       entry.mode,
			objectType: entry.objectType,
			oid:        entry.oid,
		})
	}
	return result
}

type catFileHeader struct {
	objectType string
	size       int64
}

type catFileBatch struct {
	prepared *gitexec.PreparedCommand
	stdin    io.WriteCloser
	reader   *bufio.Reader
	stderr   bytes.Buffer
	pending  bool
	closed   bool
}

func startCatFileBatch(ctx context.Context, binary string, cwd string) (*catFileBatch, error) {
	prepared, err := gitexec.Prepare(ctx, binary, cwd, nil, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	stdin, err := prepared.Command.StdinPipe()
	if err != nil {
		_ = prepared.Close()
		return nil, err
	}
	stdout, err := prepared.Command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = prepared.Close()
		return nil, err
	}
	batch := &catFileBatch{
		prepared: prepared,
		stdin:    stdin,
		reader:   bufio.NewReader(stdout),
	}
	prepared.Command.Stderr = &batch.stderr
	if err := prepared.Command.Start(); err != nil {
		_ = stdin.Close()
		_ = prepared.Close()
		return nil, err
	}
	return batch, nil
}

func (b *catFileBatch) begin(oid string) (catFileHeader, error) {
	if b == nil || b.closed || b.pending {
		return catFileHeader{}, errors.New("cat-file batch is not ready")
	}
	if strings.ContainsAny(oid, "\r\n\x00") || strings.TrimSpace(oid) == "" {
		return catFileHeader{}, errors.New("invalid object id")
	}
	if _, err := io.WriteString(b.stdin, oid+"\n"); err != nil {
		return catFileHeader{}, err
	}
	line, err := b.reader.ReadString('\n')
	if err != nil {
		return catFileHeader{}, err
	}
	fields := strings.Fields(strings.TrimSuffix(line, "\n"))
	if len(fields) == 2 && fields[1] == "missing" {
		return catFileHeader{}, fmt.Errorf("object %s is missing", oid)
	}
	if len(fields) != 3 {
		return catFileHeader{}, fmt.Errorf("invalid cat-file batch header %q", strings.TrimSpace(line))
	}
	if fields[0] != oid {
		return catFileHeader{}, fmt.Errorf("cat-file returned object %s, want %s", fields[0], oid)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return catFileHeader{}, fmt.Errorf("invalid cat-file object size %q", fields[2])
	}
	b.pending = true
	return catFileHeader{objectType: fields[1], size: size}, nil
}

func (b *catFileBatch) finish() error {
	if b == nil || !b.pending {
		return errors.New("cat-file batch has no pending object")
	}
	terminator, err := b.reader.ReadByte()
	b.pending = false
	if err != nil {
		return err
	}
	if terminator != '\n' {
		return fmt.Errorf("invalid cat-file object terminator %q", terminator)
	}
	return nil
}

func (b *catFileBatch) Close() error {
	if b == nil || b.closed {
		return nil
	}
	b.closed = true
	var failures []error
	aborted := b.pending
	if aborted {
		failures = append(failures, errors.New("cat-file batch closed with an unread object"))
		if b.prepared != nil && b.prepared.Command.Process != nil {
			if err := b.prepared.Command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				failures = append(failures, fmt.Errorf("abort cat-file batch: %w", err))
			}
		}
	}
	if b.stdin != nil {
		failures = append(failures, b.stdin.Close())
	}
	if b.prepared != nil && b.prepared.Command.Process != nil {
		if err := b.prepared.Command.Wait(); err != nil {
			if !aborted {
				detail := strings.TrimSpace(b.stderr.String())
				if detail != "" {
					err = fmt.Errorf("git cat-file --batch: %s: %w", detail, err)
				}
				failures = append(failures, err)
			}
		}
		failures = append(failures, b.prepared.Close())
	}
	return errors.Join(failures...)
}
