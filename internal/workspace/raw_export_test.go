package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestRawExportBypassesHooksFiltersReplacementSparseAndCheckoutConversion(t *testing.T) {
	root := newCommittedRepo(t)
	rawFiltered := []byte("raw-filtered-bytes\n")
	lfsPointer := []byte("version https://git-lfs.github.com/spec/v1\noid sha256:0123456789abcdef\nsize 42\n")
	utf16WorkingTree := []byte{0xff, 0xfe, 'e', 0, 'n', 0, 'c', 0, 'o', 0, 'd', 0, 'e', 0, 'd', 0, '\n', 0}
	writeTestFile(t, filepath.Join(root, ".gitattributes"), []byte(strings.Join([]string{
		"filtered.bin filter=danger",
		"pointer.txt filter=lfs",
		"eol.txt text eol=crlf",
		"encoded.txt working-tree-encoding=UTF-16",
		"exported.txt export-ignore",
		"",
	}, "\n")), 0o644)
	writeTestFile(t, filepath.Join(root, "filtered.bin"), rawFiltered, 0o644)
	writeTestFile(t, filepath.Join(root, "pointer.txt"), lfsPointer, 0o644)
	writeTestFile(t, filepath.Join(root, "eol.txt"), []byte("line-one\nline-two\n"), 0o644)
	writeTestFile(t, filepath.Join(root, "encoded.txt"), utf16WorkingTree, 0o644)
	writeTestFile(t, filepath.Join(root, "exported.txt"), []byte("must remain\n"), 0o644)
	writeTestFile(t, filepath.Join(root, "executable.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	if runtime.GOOS != "windows" {
		if err := os.Symlink("../committed.txt", filepath.Join(root, "raw-link")); err != nil {
			t.Fatalf("create committed symlink: %v", err)
		}
		writeTestFile(t, filepath.Join(root, "line\nbreak\tname.txt"), []byte("hostile filename\n"), 0o644)
	}
	testSanitizedGit(t, root, "add", "--all")
	testSanitizedGit(t, root, "commit", "-q", "-m", "raw export fixtures")
	testSanitizedGit(t, root, "reset", "--hard", "HEAD")
	if status := testSanitizedGit(t, root, "status", "--short", "--untracked-files=all"); status != "" {
		t.Fatalf("raw export fixture is dirty after checkout refresh:\n%s", status)
	}
	encodedBlob := gitBlobBytes(t, root, "HEAD:encoded.txt")
	if !bytes.Equal(encodedBlob, []byte("encoded\n")) || bytes.Equal(encodedBlob, utf16WorkingTree) {
		t.Fatalf("working-tree encoding fixture committed unexpected blob bytes: %x", encodedBlob)
	}

	markerRoot := t.TempDir()
	hookMarker := filepath.Join(markerRoot, "hook-ran")
	filterMarker := filepath.Join(markerRoot, "filter-ran")
	fsmonitorMarker := filepath.Join(markerRoot, "fsmonitor-ran")
	hooksDir := filepath.Join(markerRoot, "hooks")
	writeExecutableTestScript(t, filepath.Join(hooksDir, "post-checkout"), hookMarker)
	filterScript := filepath.Join(markerRoot, "filter")
	writeExecutableTestScript(t, filterScript, filterMarker)
	fsmonitorScript := filepath.Join(markerRoot, "fsmonitor")
	writeExecutableTestScript(t, fsmonitorScript, fsmonitorMarker)
	testGit(t, root, "config", "core.hooksPath", hooksDir)
	testGit(t, root, "config", "filter.danger.smudge", filterScript)
	testGit(t, root, "config", "filter.danger.process", filterScript)
	testGit(t, root, "config", "filter.lfs.smudge", filterScript)
	testGit(t, root, "config", "core.fsmonitor", fsmonitorScript)
	testGit(t, root, "config", "core.sparseCheckout", "true")
	testGit(t, root, "config", "core.sparseCheckoutCone", "true")
	testGit(t, root, "config", "maintenance.auto", "true")
	testGit(t, root, "config", "gc.auto", "1")
	sparsePath := filepath.Join(root, ".git", "info", "sparse-checkout")
	writeTestFile(t, sparsePath, []byte("/committed.txt\n"), 0o644)

	originalOID := testGit(t, root, "rev-parse", "HEAD:filtered.bin")
	replacementOID := gitHashObject(t, root, []byte("replacement-object-bytes\n"))
	testGit(t, root, "replace", originalOID, replacementOID)

	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{
		LaunchCWD:       root,
		SessionDir:      sessionDir,
		MinimumPolicy:   PolicyEphemeral,
		RequestedPolicy: PolicyEphemeral,
	})
	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if err != nil {
		t.Fatalf("raw materialize: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)

	assertRawExportBytes(t, materialized.WorktreePath, "filtered.bin", rawFiltered)
	assertRawExportBytes(t, materialized.WorktreePath, "pointer.txt", lfsPointer)
	assertRawExportBytes(t, materialized.WorktreePath, "eol.txt", []byte("line-one\nline-two\n"))
	assertRawExportBytes(t, materialized.WorktreePath, "encoded.txt", encodedBlob)
	assertRawExportBytes(t, materialized.WorktreePath, "exported.txt", []byte("must remain\n"))
	assertRawExportBytes(t, materialized.WorktreePath, "executable.sh", []byte("#!/bin/sh\nexit 0\n"))
	if runtime.GOOS != "windows" {
		target, err := os.Readlink(filepath.Join(materialized.WorktreePath, "raw-link"))
		if err != nil || target != "../committed.txt" {
			t.Fatalf("raw symlink target = %q, %v", target, err)
		}
		assertRawExportBytes(t, materialized.WorktreePath, "line\nbreak\tname.txt", []byte("hostile filename\n"))
	}
	for _, marker := range []string{hookMarker, filterMarker, fsmonitorMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("sanitized Git helper executed %s: %v", marker, err)
		}
	}
	export := materialized.Artifact["committed_export"].(map[string]any)
	if intFromWorkspaceAny(export["file_count"]) < 8 || intFromWorkspaceAny(export["byte_count"]) < int64(len(rawFiltered)) {
		t.Fatalf("committed export accounting = %#v", export)
	}
}

func TestRawExportMaterializesGitlinksAsCountedEmptyDirectories(t *testing.T) {
	root := newCommittedRepo(t)
	head := testGit(t, root, "rev-parse", "HEAD")
	testGit(t, root, "update-index", "--add", "--cacheinfo", "160000", head, "vendor/submodule")
	testGit(t, root, "commit", "-q", "-m", "add gitlink")

	sessionDir := filepath.Join(t.TempDir(), "session")
	snapshot := mustPreflight(t, Options{
		LaunchCWD:        root,
		SessionDir:       sessionDir,
		MinimumPolicy:    PolicyEphemeral,
		AllowDirtySource: true,
	})
	materialized, err := Materialize(context.Background(), store.New(sessionDir), snapshot)
	if err != nil {
		t.Fatalf("materialize gitlink: %v", err)
	}
	registerWorktreeCleanup(t, root, materialized.WorktreePath)
	gitlinkPath := filepath.Join(materialized.WorktreePath, "vendor", "submodule")
	entries, err := os.ReadDir(gitlinkPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("gitlink materialization = %#v, %v", entries, err)
	}
	export := materialized.Artifact["committed_export"].(map[string]any)
	found := false
	for _, raw := range export["entries"].([]any) {
		entry := raw.(map[string]any)
		if entry["path"] == "vendor/submodule" {
			found = entry["mode"] == "160000" &&
				entry["materialization"] == "empty_directory" &&
				intFromWorkspaceAny(entry["size_bytes"]) == 0
		}
	}
	if !found {
		t.Fatalf("gitlink export descriptor = %#v", export)
	}
}

func TestValidateRawTreeEntriesRejectsUnsafeTargetsAndModes(t *testing.T) {
	caseInsensitive, err := filesystemCaseInsensitive(t.TempDir())
	if err != nil {
		t.Fatalf("probe test filesystem case semantics: %v", err)
	}
	tests := []struct {
		name    string
		entries []headEntry
	}{
		{name: "absolute", entries: []headEntry{{path: "/absolute", mode: "100644", objectType: "blob", oid: "a"}}},
		{name: "traversal", entries: []headEntry{{path: "../outside", mode: "100644", objectType: "blob", oid: "a"}}},
		{name: "git admin", entries: []headEntry{{path: "nested/.GiT/config", mode: "100644", objectType: "blob", oid: "a"}}},
		{name: "invalid pair", entries: []headEntry{{path: "value", mode: "100644", objectType: "commit", oid: "a"}}},
		{name: "unsupported mode", entries: []headEntry{{path: "value", mode: "100664", objectType: "blob", oid: "a"}}},
		{name: "symlink descendant", entries: []headEntry{
			{path: "link", mode: "120000", objectType: "blob", oid: "a"},
			{path: "link/child", mode: "100644", objectType: "blob", oid: "b"},
		}},
		{name: "duplicate", entries: []headEntry{
			{path: "same", mode: "100644", objectType: "blob", oid: "a"},
			{path: "same", mode: "100644", objectType: "blob", oid: "b"},
		}},
	}
	if caseInsensitive {
		tests = append(tests, struct {
			name    string
			entries []headEntry
		}{
			name: "platform normalized duplicate",
			entries: []headEntry{
				{path: "Case", mode: "100644", objectType: "blob", oid: "a"},
				{path: "case", mode: "100644", objectType: "blob", oid: "b"},
			},
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateRawTreeEntries(test.entries, caseInsensitive); err == nil {
				t.Fatalf("unsafe entries accepted: %#v", test.entries)
			}
		})
	}
}

func writeExecutableTestScript(t *testing.T, path string, marker string) {
	t.Helper()
	body := "#!/bin/sh\n: > " + shellSingleQuote(marker) + "\nexit 1\n"
	writeTestFile(t, path, []byte(body), 0o755)
}

func testSanitizedGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	output, err := runGit(context.Background(), "git", root, args...)
	if err != nil {
		t.Fatalf("sanitized git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func gitHashObject(t *testing.T, root string, data []byte) string {
	t.Helper()
	command := exec.Command("git", "-C", root, "hash-object", "-w", "--stdin")
	command.Stdin = strings.NewReader(string(data))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("hash replacement object: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitBlobBytes(t *testing.T, root string, object string) []byte {
	t.Helper()
	command := exec.Command("git", "-C", root, "cat-file", "blob", object)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("read Git blob %s: %v", object, err)
	}
	return output
}

func assertRawExportBytes(t *testing.T, root string, relative string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil || string(got) != string(want) {
		t.Fatalf("raw export %q = %q, %v; want %q", relative, got, err, want)
	}
}

func intFromWorkspaceAny(value any) int64 {
	parsed, _ := nonnegativeInt64(value)
	return parsed
}
