// Package bundle creates and verifies the v2 portable session bundle.
package bundle

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/session"
)

const (
	FormatVersion    = 1
	ManifestFilename = "manifest.json"
)

// Manifest is semantic-json and has no local-machine envelope or source-id
// rewrite table. Blob SHA-256 values are already portable identities.
type Manifest struct {
	FormatVersion     int                 `json:"format_version"`
	SessionJSONDigest string              `json:"session_json_digest"`
	EventsJSONLDigest string              `json:"events_jsonl_digest"`
	BlobInventory     []blobstore.BlobRef `json:"blob_inventory"`
}

// MissingFileError distinguishes an absent required portable artifact.
type MissingFileError struct{ Path string }

func (e *MissingFileError) Error() string { return "bundle is missing " + e.Path }

// UnexpectedFileError distinguishes a closed-layout violation.
type UnexpectedFileError struct{ Path string }

func (e *UnexpectedFileError) Error() string { return "bundle contains unexpected file " + e.Path }

// SymlinkError distinguishes forbidden links from ordinary missing files.
type SymlinkError struct{ Path string }

func (e *SymlinkError) Error() string { return "bundle contains symlink " + e.Path }

// DigestMismatchError identifies the tampered bundle member without exposing
// any source-machine path.
type DigestMismatchError struct{ Path string }

func (e *DigestMismatchError) Error() string { return "bundle digest mismatch for " + e.Path }

// UnsupportedVersionError means the verifier deliberately does not accept a
// manifest it cannot interpret exactly.
type UnsupportedVersionError struct{ Version int }

func (e *UnsupportedVersionError) Error() string {
	return fmt.Sprintf("unsupported bundle format version %d", e.Version)
}

// ManifestError reports a malformed but syntactically valid manifest.
type ManifestError struct{ Reason string }

func (e *ManifestError) Error() string { return "invalid bundle manifest: " + e.Reason }

// Create copies a portable v2 session into a new target directory. targetDir
// must not exist; the created directory contains exactly manifest.json,
// session.json, events.jsonl, and blobs/sha256/<hex> payload files.
func Create(sessionDir string, targetDir string) (*Manifest, error) {
	source := filepath.Clean(sessionDir)
	target := strings.TrimSpace(targetDir)
	if target == "" {
		return nil, errors.New("bundle target directory is required")
	}
	target = filepath.Clean(target)
	if _, err := os.Lstat(target); err == nil {
		return nil, fmt.Errorf("bundle target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := validateSessionLayout(source); err != nil {
		return nil, err
	}
	sourceSession, sessionBody, events, eventsBody, inventory, err := inspectSource(source)
	if err != nil {
		return nil, err
	}
	manifest := &Manifest{
		FormatVersion:     FormatVersion,
		SessionJSONDigest: sourceSession.Digest,
		EventsJSONLDigest: eventlog.RawBytesDigest(eventsBody),
		BlobInventory:     inventory,
	}
	if err := validateManifest(*manifest); err != nil {
		return nil, err
	}
	manifestBody, err := eventlog.SemanticJSONBytes(manifest)
	if err != nil {
		return nil, err
	}

	parent := filepath.Dir(target)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return nil, err
	}
	if !parentInfo.IsDir() {
		return nil, errors.New("bundle target parent is not a directory")
	}
	temporary, err := os.MkdirTemp(parent, ".bundle-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	if err := os.MkdirAll(filepath.Join(temporary, "blobs", blobstore.SHA256Directory), 0o700); err != nil {
		return nil, err
	}
	if err := writeNewFile(filepath.Join(temporary, session.SessionFilename), sessionBody); err != nil {
		return nil, err
	}
	if err := writeNewFile(filepath.Join(temporary, eventlog.EventsFilename), eventsBody); err != nil {
		return nil, err
	}
	for _, ref := range inventory {
		if err := copyRegularFile(
			filepath.Join(source, "blobs", blobstore.SHA256Directory, ref.SHA256),
			filepath.Join(temporary, "blobs", blobstore.SHA256Directory, ref.SHA256),
		); err != nil {
			return nil, err
		}
	}
	if err := writeNewFile(filepath.Join(temporary, ManifestFilename), manifestBody); err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Join(temporary, "blobs", blobstore.SHA256Directory)); err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Join(temporary, "blobs")); err != nil {
		return nil, err
	}
	if err := syncDirectory(temporary); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(target); err == nil {
		return nil, fmt.Errorf("bundle target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(temporary, target); err != nil {
		return nil, err
	}
	if err := syncDirectory(parent); err != nil {
		return nil, err
	}
	_ = events // replay is intentionally part of source validation.
	return manifest, nil
}

func inspectSource(root string) (*session.Session, []byte, []eventlog.Event, []byte, []blobstore.BlobRef, error) {
	opened, err := session.Open(root)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	sessionBody, err := readRegularFile(root, session.SessionFilename)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	eventsBody, err := readRegularFile(root, eventlog.EventsFilename)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	events, err := eventlog.Replay(strings.NewReader(string(eventsBody)))
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if err := session.ValidateEventBindings(opened.Plan, events); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	store, err := blobstore.Open(root, blobstore.Limits{})
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	physical, err := store.List()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	references, err := collectReferences(opened.Plan, events)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	for _, ref := range references {
		if err := store.Verify(ref); err != nil {
			return nil, nil, nil, nil, nil, err
		}
	}
	referenced := make(map[string]blobstore.BlobRef, len(references))
	for _, ref := range references {
		if previous, exists := referenced[ref.SHA256]; exists && !previous.Equal(ref) {
			return nil, nil, nil, nil, nil, &ManifestError{Reason: "one blob digest is referenced with conflicting metadata"}
		}
		referenced[ref.SHA256] = ref
	}
	inventory := make([]blobstore.BlobRef, 0, len(physical))
	for _, ref := range physical {
		if sourceRef, exists := referenced[ref.SHA256]; exists {
			ref.MediaType = sourceRef.MediaType
		} else {
			ref.MediaType = "application/octet-stream"
		}
		if err := store.Verify(ref); err != nil {
			return nil, nil, nil, nil, nil, err
		}
		inventory = append(inventory, ref)
	}
	for digest := range referenced {
		found := false
		for _, physicalRef := range physical {
			if physicalRef.SHA256 == digest {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, nil, nil, nil, &MissingFileError{Path: filepath.ToSlash(filepath.Join("blobs", blobstore.SHA256Directory, digest))}
		}
	}
	sort.Slice(inventory, func(left, right int) bool { return inventory[left].SHA256 < inventory[right].SHA256 })
	return opened, sessionBody, events, eventsBody, inventory, nil
}

// Verify validates both closed layout and every declared digest. It does not
// inspect runtime state because portable bundles contain none.
func Verify(directory string) (*Manifest, error) {
	root := filepath.Clean(directory)
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, &SymlinkError{Path: "."}
	}
	if !info.IsDir() {
		return nil, errors.New("bundle root is not a directory")
	}
	manifestBody, err := readRegularFile(root, ManifestFilename)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := eventlog.DecodeCanonicalJSON(manifestBody, &manifest); err != nil {
		return nil, err
	}
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	if err := validateBundleLayout(root, manifest); err != nil {
		return nil, err
	}
	sessionBody, err := readRegularFile(root, session.SessionFilename)
	if err != nil {
		return nil, err
	}
	sessionDigest, err := eventlog.SemanticJSONDigestBytes(sessionBody)
	if err != nil {
		return nil, err
	}
	if sessionDigest != manifest.SessionJSONDigest {
		return nil, &DigestMismatchError{Path: session.SessionFilename}
	}
	opened, err := session.Open(root)
	if err != nil {
		return nil, err
	}
	eventsBody, err := readRegularFile(root, eventlog.EventsFilename)
	if err != nil {
		return nil, err
	}
	if eventlog.RawBytesDigest(eventsBody) != manifest.EventsJSONLDigest {
		return nil, &DigestMismatchError{Path: eventlog.EventsFilename}
	}
	events, err := eventlog.Replay(strings.NewReader(string(eventsBody)))
	if err != nil {
		return nil, err
	}
	if err := session.ValidateEventBindings(opened.Plan, events); err != nil {
		return nil, err
	}
	store, err := blobstore.Open(root, blobstore.Limits{})
	if err != nil {
		return nil, err
	}
	byDigest := make(map[string]blobstore.BlobRef, len(manifest.BlobInventory))
	for _, ref := range manifest.BlobInventory {
		if err := store.Verify(ref); err != nil {
			return nil, &DigestMismatchError{Path: filepath.ToSlash(filepath.Join("blobs", blobstore.SHA256Directory, ref.SHA256))}
		}
		byDigest[ref.SHA256] = ref
	}
	for _, ref := range append(session.BlobRefs(opened.Plan), eventlog.BlobRefs(events)...) {
		declared, found := byDigest[ref.SHA256]
		if !found || !declared.Equal(ref) {
			return nil, &ManifestError{Reason: "blob reference is absent from or differs from inventory"}
		}
	}
	return &manifest, nil
}

func collectReferences(plan session.Plan, events []eventlog.Event) ([]blobstore.BlobRef, error) {
	refs := append(session.BlobRefs(plan), eventlog.BlobRefs(events)...)
	for _, ref := range refs {
		if err := blobstore.ValidateRef(ref); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

func validateManifest(manifest Manifest) error {
	if manifest.FormatVersion != FormatVersion {
		return &UnsupportedVersionError{Version: manifest.FormatVersion}
	}
	if !validDigest(manifest.SessionJSONDigest) {
		return &ManifestError{Reason: "session_json_digest is invalid"}
	}
	if !validDigest(manifest.EventsJSONLDigest) {
		return &ManifestError{Reason: "events_jsonl_digest is invalid"}
	}
	seen := make(map[string]struct{}, len(manifest.BlobInventory))
	for _, ref := range manifest.BlobInventory {
		if err := blobstore.ValidateRef(ref); err != nil {
			return &ManifestError{Reason: err.Error()}
		}
		if _, exists := seen[ref.SHA256]; exists {
			return &ManifestError{Reason: "blob inventory contains duplicate sha256"}
		}
		seen[ref.SHA256] = struct{}{}
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

func validateSessionLayout(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &SymlinkError{Path: "."}
	}
	if !info.IsDir() {
		return errors.New("session root is not a directory")
	}
	expected := map[string]entryShape{
		session.SessionFilename: regularFile,
		eventlog.EventsFilename: regularFile,
		"blobs":                 directory,
		"runtime":               directory,
	}
	return validateEntries(root, "", expected)
}

func validateBundleLayout(root string, manifest Manifest) error {
	if err := validateEntries(root, "", map[string]entryShape{
		ManifestFilename:        regularFile,
		session.SessionFilename: regularFile,
		eventlog.EventsFilename: regularFile,
		"blobs":                 directory,
	}); err != nil {
		return err
	}
	if err := validateEntries(filepath.Join(root, "blobs"), "blobs", map[string]entryShape{
		blobstore.SHA256Directory: directory,
	}); err != nil {
		return err
	}
	expected := make(map[string]entryShape, len(manifest.BlobInventory))
	for _, ref := range manifest.BlobInventory {
		expected[ref.SHA256] = regularFile
	}
	return validateEntries(filepath.Join(root, "blobs", blobstore.SHA256Directory), filepath.ToSlash(filepath.Join("blobs", blobstore.SHA256Directory)), expected)
}

type entryShape uint8

const (
	regularFile entryShape = iota
	directory
)

func validateEntries(directoryPath string, displayPrefix string, expected map[string]entryShape) error {
	entries, err := os.ReadDir(directoryPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &MissingFileError{Path: displayPrefix}
		}
		return err
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		relative := name
		if displayPrefix != "" {
			relative = filepath.ToSlash(filepath.Join(displayPrefix, name))
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return &SymlinkError{Path: relative}
		}
		shape, allowed := expected[name]
		if !allowed {
			return &UnexpectedFileError{Path: relative}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if shape == regularFile && !info.Mode().IsRegular() {
			return &UnexpectedFileError{Path: relative}
		}
		if shape == directory && !info.IsDir() {
			return &UnexpectedFileError{Path: relative}
		}
		seen[name] = struct{}{}
	}
	for name := range expected {
		if _, found := seen[name]; !found {
			relative := name
			if displayPrefix != "" {
				relative = filepath.ToSlash(filepath.Join(displayPrefix, name))
			}
			return &MissingFileError{Path: relative}
		}
	}
	return nil
}

func readRegularFile(root string, relative string) ([]byte, error) {
	filename := filepath.Join(root, relative)
	info, err := os.Lstat(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &MissingFileError{Path: filepath.ToSlash(relative)}
		}
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, &SymlinkError{Path: filepath.ToSlash(relative)}
	}
	if !info.Mode().IsRegular() {
		return nil, &UnexpectedFileError{Path: filepath.ToSlash(relative)}
	}
	return os.ReadFile(filename)
}

func copyRegularFile(source string, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("bundle source member is not a regular file")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func writeNewFile(filename string, body []byte) error {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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

func syncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
