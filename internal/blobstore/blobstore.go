// Package blobstore implements the v2 content-addressed payload store.
package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const (
	// SHA256Directory is the only durable payload directory below blobs.
	SHA256Directory  = "sha256"
	defaultMediaType = "application/octet-stream"
)

// BlobRef is the sole payload-reference shape used by the v2 store. SHA256 is
// the lower-case hexadecimal digest of the raw bytes, without a prefix.
type BlobRef struct {
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

// Equal reports whether two references identify the same payload and metadata.
func (r BlobRef) Equal(other BlobRef) bool {
	return r.SHA256 == other.SHA256 && r.Size == other.Size && r.MediaType == other.MediaType
}

// Limits bounds durable payload storage. A zero value means no limit for that
// dimension.
type Limits struct {
	MaxBlobBytes  int64
	MaxTotalBytes int64
}

// LimitError is returned when a configured storage limit would be exceeded.
type LimitError struct {
	Scope  string
	Limit  int64
	Actual int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("blobstore %s limit exceeded: %d > %d", e.Scope, e.Actual, e.Limit)
}

// DigestMismatchError means the bytes stored at a BlobRef's address no longer
// match the reference.
type DigestMismatchError struct {
	Ref    BlobRef
	Actual string
	Size   int64
}

func (e *DigestMismatchError) Error() string {
	return fmt.Sprintf("blob digest mismatch for %s", e.Ref.SHA256)
}

// InvalidRefError is returned for malformed or unsafe blob references.
type InvalidRefError struct {
	Reason string
}

func (e *InvalidRefError) Error() string { return "invalid blob ref: " + e.Reason }

// InvalidBlobError is returned when the physical blob layout is malformed.
type InvalidBlobError struct {
	Path   string
	Reason string
}

func (e *InvalidBlobError) Error() string {
	return fmt.Sprintf("invalid blob %s: %s", e.Path, e.Reason)
}

// Store holds no mutable payload metadata: the raw bytes and their SHA-256
// address are the authority. Media type travels with every BlobRef instead.
type Store struct {
	sessionDir string
	dir        string
	limits     Limits

	mu    sync.Mutex
	total int64
}

// New creates the blobs/sha256 directory below an already-selected session
// directory. Session creation owns selection of that directory; this method
// never creates a session root itself.
func New(sessionDir string, limits Limits) (*Store, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	dir := blobDirectory(sessionDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return openStore(sessionDir, limits)
}

// Open opens an existing blob store without creating any durable layout.
func Open(sessionDir string, limits Limits) (*Store, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	return openStore(sessionDir, limits)
}

func openStore(sessionDir string, limits Limits) (*Store, error) {
	cleaned := filepath.Clean(sessionDir)
	dir := blobDirectory(cleaned)
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, &InvalidBlobError{Path: dir, Reason: "blob directory must be a real directory"}
	}
	store := &Store{sessionDir: cleaned, dir: dir, limits: limits}
	total, err := store.currentTotal()
	if err != nil {
		return nil, err
	}
	store.total = total
	if limits.MaxTotalBytes > 0 && total > limits.MaxTotalBytes {
		return nil, &LimitError{Scope: "total bytes", Limit: limits.MaxTotalBytes, Actual: total}
	}
	return store, nil
}

func validateLimits(limits Limits) error {
	if limits.MaxBlobBytes < 0 || limits.MaxTotalBytes < 0 {
		return &LimitError{Scope: "configuration", Limit: 0, Actual: -1}
	}
	return nil
}

func blobDirectory(sessionDir string) string {
	return filepath.Join(filepath.Clean(sessionDir), "blobs", SHA256Directory)
}

// SessionDir returns the containing session directory. It is deliberately not
// serialized in BlobRef or any other durable record.
func (s *Store) SessionDir() string {
	if s == nil {
		return ""
	}
	return s.sessionDir
}

// Put stores raw bytes using the default binary media type.
func (s *Store) Put(r io.Reader) (BlobRef, error) {
	return s.PutMediaType(r, defaultMediaType)
}

// PutMediaType stores raw bytes atomically and fsyncs them before returning a
// reference. Repeating a put for identical bytes publishes no second file.
func (s *Store) PutMediaType(r io.Reader, mediaType string) (BlobRef, error) {
	if s == nil {
		return BlobRef{}, errors.New("nil blob store")
	}
	if r == nil {
		return BlobRef{}, errors.New("blob reader is required")
	}
	mediaType = strings.TrimSpace(mediaType)
	if mediaType == "" {
		mediaType = defaultMediaType
	}
	if strings.ContainsAny(mediaType, "\r\n\x00") {
		return BlobRef{}, &InvalidRefError{Reason: "media_type contains a control character"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	temporary, err := os.CreateTemp(s.dir, ".put-")
	if err != nil {
		return BlobRef{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)

	hasher := sha256.New()
	var source io.Reader = r
	if s.limits.MaxBlobBytes > 0 {
		source = io.LimitReader(r, s.limits.MaxBlobBytes+1)
	}
	written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), source)
	if copyErr != nil {
		_ = temporary.Close()
		return BlobRef{}, copyErr
	}
	if s.limits.MaxBlobBytes > 0 && written > s.limits.MaxBlobBytes {
		_ = temporary.Close()
		return BlobRef{}, &LimitError{Scope: "per-blob bytes", Limit: s.limits.MaxBlobBytes, Actual: written}
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return BlobRef{}, err
	}
	if err := temporary.Close(); err != nil {
		return BlobRef{}, err
	}

	ref := BlobRef{
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
		Size:      written,
		MediaType: mediaType,
	}
	final, err := s.pathFor(ref.SHA256)
	if err != nil {
		return BlobRef{}, err
	}
	if existing, err := os.Lstat(final); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return BlobRef{}, &InvalidBlobError{Path: final, Reason: "existing digest target is not a regular file"}
		}
		if err := s.Verify(ref); err != nil {
			return BlobRef{}, err
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return BlobRef{}, err
	}
	if s.limits.MaxTotalBytes > 0 && s.total+written > s.limits.MaxTotalBytes {
		return BlobRef{}, &LimitError{Scope: "total bytes", Limit: s.limits.MaxTotalBytes, Actual: s.total + written}
	}

	// link(2) provides no-replace publication: a concurrent writer of the same
	// digest wins harmlessly, while no partially-written inode becomes visible.
	if err := os.Link(temporaryName, final); err != nil {
		if errors.Is(err, os.ErrExist) {
			if err := s.Verify(ref); err != nil {
				return BlobRef{}, err
			}
			return ref, nil
		}
		return BlobRef{}, err
	}
	if err := syncDirectory(s.dir); err != nil {
		return BlobRef{}, err
	}
	s.total += written
	return ref, nil
}

// PutBytes is a convenience for callers that already own the payload bytes.
func (s *Store) PutBytes(body []byte, mediaType string) (BlobRef, error) {
	return s.PutMediaType(bytes.NewReader(body), mediaType)
}

// Open opens a blob and verifies both byte count and SHA-256 as it is consumed.
// Read through EOF (or Close) returns a DigestMismatchError for corruption.
func (s *Store) Open(ref BlobRef) (io.ReadCloser, error) {
	if s == nil {
		return nil, errors.New("nil blob store")
	}
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}
	filename, err := s.pathFor(ref.SHA256)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, &InvalidBlobError{Path: filename, Reason: "blob must be a regular file"}
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	return &verifiedReadCloser{file: file, ref: ref, hasher: sha256.New()}, nil
}

// Verify drains Open so a caller can establish blob durability before an event
// is appended that refers to it.
func (s *Store) Verify(ref BlobRef) error {
	reader, err := s.Open(ref)
	if err != nil {
		return err
	}
	_, readErr := io.Copy(io.Discard, reader)
	closeErr := reader.Close()
	if readErr != nil {
		return readErr
	}
	return closeErr
}

type verifiedReadCloser struct {
	file      *os.File
	ref       BlobRef
	hasher    hash.Hash
	written   int64
	finished  bool
	finishErr error
	closed    bool
}

func (r *verifiedReadCloser) Read(buffer []byte) (int, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	count, err := r.file.Read(buffer)
	if count > 0 {
		_, _ = r.hasher.Write(buffer[:count])
		r.written += int64(count)
	}
	if err == io.EOF {
		if verifyErr := r.finish(); verifyErr != nil {
			return count, verifyErr
		}
	}
	return count, err
}

func (r *verifiedReadCloser) finish() error {
	if r.finished {
		return r.finishErr
	}
	r.finished = true
	actual := hex.EncodeToString(r.hasher.Sum(nil))
	if actual != r.ref.SHA256 || r.written != r.ref.Size {
		r.finishErr = &DigestMismatchError{Ref: r.ref, Actual: actual, Size: r.written}
	}
	return r.finishErr
}

func (r *verifiedReadCloser) Close() error {
	if r.closed {
		return nil
	}
	if !r.finished {
		_, _ = io.Copy(io.Discard, r)
	}
	verifyErr := r.finish()
	r.closed = true
	closeErr := r.file.Close()
	if verifyErr != nil {
		return verifyErr
	}
	return closeErr
}

// List returns every physical blob, in deterministic digest order. Since an
// unreferenced blob has no durable media-type sidecar, its MediaType is empty.
func (s *Store) List() ([]BlobRef, error) {
	if s == nil {
		return nil, errors.New("nil blob store")
	}
	return s.list()
}

func (s *Store) list() ([]BlobRef, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	refs := make([]BlobRef, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, &InvalidBlobError{Path: filepath.Join(s.dir, entry.Name()), Reason: "symlinks are not allowed"}
		}
		if !validSHA256(entry.Name()) {
			return nil, &InvalidBlobError{Path: filepath.Join(s.dir, entry.Name()), Reason: "filename is not a SHA-256 hex digest"}
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, &InvalidBlobError{Path: filepath.Join(s.dir, entry.Name()), Reason: "blob is not a regular file"}
		}
		refs = append(refs, BlobRef{SHA256: entry.Name(), Size: info.Size()})
	}
	sort.Slice(refs, func(left, right int) bool { return refs[left].SHA256 < refs[right].SHA256 })
	return refs, nil
}

// SweepReport describes harmless, unreferenced payload files. Sweep never
// deletes them; a crash after Put but before event append is therefore readable.
type SweepReport struct {
	Unreferenced []BlobRef
	TotalBytes   int64
}

// Sweep compares the supplied authority references with physical blobs.
func (s *Store) Sweep(references []BlobRef) (SweepReport, error) {
	if s == nil {
		return SweepReport{}, errors.New("nil blob store")
	}
	referenced := make(map[string]struct{}, len(references))
	for _, ref := range references {
		if err := ValidateRef(ref); err != nil {
			return SweepReport{}, err
		}
		referenced[ref.SHA256] = struct{}{}
	}
	all, err := s.list()
	if err != nil {
		return SweepReport{}, err
	}
	report := SweepReport{}
	for _, ref := range all {
		report.TotalBytes += ref.Size
		if _, ok := referenced[ref.SHA256]; !ok {
			report.Unreferenced = append(report.Unreferenced, ref)
		}
	}
	return report, nil
}

// ValidateRef validates the path-independent BlobRef shape.
func ValidateRef(ref BlobRef) error {
	if !validSHA256(ref.SHA256) {
		return &InvalidRefError{Reason: "sha256 must be 64 lower-case hexadecimal characters"}
	}
	if ref.Size < 0 {
		return &InvalidRefError{Reason: "size must not be negative"}
	}
	if strings.TrimSpace(ref.MediaType) == "" {
		return &InvalidRefError{Reason: "media_type is required"}
	}
	if strings.ContainsAny(ref.MediaType, "\r\n\x00") {
		return &InvalidRefError{Reason: "media_type contains a control character"}
	}
	return nil
}

func (s *Store) pathFor(digest string) (string, error) {
	if !validSHA256(digest) {
		return "", &InvalidRefError{Reason: "sha256 must be 64 lower-case hexadecimal characters"}
	}
	return filepath.Join(s.dir, digest), nil
}

func (s *Store) currentTotal() (int64, error) {
	refs, err := s.list()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, ref := range refs {
		total += ref.Size
	}
	return total, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
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
