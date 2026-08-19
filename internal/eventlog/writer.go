package eventlog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
)

const EventsFilename = "events.jsonl"

// readEventLogFile is the sole log-read seam so tests can observe the real
// recovery read and prove that Append does not scan the file again.
var readEventLogFile = os.ReadFile

// BlobVerifier establishes the blob-before-event ordering rule. blobstore.Store
// implements it by opening, draining, and digest-verifying the payload.
type BlobVerifier interface {
	Verify(blobstore.BlobRef) error
}

// BlobNotDurableError means Append refused to publish an event before every
// referenced blob could be verified as durable and intact.
type BlobNotDurableError struct {
	Ref   blobstore.BlobRef
	Cause error
}

func (e *BlobNotDurableError) Error() string {
	return fmt.Sprintf("event references an undurable blob %s: %v", e.Ref.SHA256, e.Cause)
}

func (e *BlobNotDurableError) Unwrap() error { return e.Cause }

// BlobVerifierRequiredError prevents callers from bypassing durable blob
// ordering for an event that contains a BlobRef.
type BlobVerifierRequiredError struct{}

func (BlobVerifierRequiredError) Error() string {
	return "a blob verifier is required before appending an event with blob references"
}

// SequenceError is always fatal during recovery, including on the final line.
type SequenceError struct {
	Line     int
	Expected uint64
	Actual   uint64
}

func (e *SequenceError) Error() string {
	return fmt.Sprintf("event line %d has sequence %d, expected %d", e.Line, e.Actual, e.Expected)
}

// ReplayLineError identifies a malformed complete JSONL record.
type ReplayLineError struct {
	Line  int
	Cause error
}

func (e *ReplayLineError) Error() string {
	return fmt.Sprintf("event line %d is malformed: %v", e.Line, e.Cause)
}

func (e *ReplayLineError) Unwrap() error { return e.Cause }

// WriterLockedError means another Writer owns the runtime lease for this log.
// Callers must close that writer before opening another one for the same log.
type WriterLockedError struct{ Cause error }

func (e *WriterLockedError) Error() string { return "event log already has an active writer" }

func (e *WriterLockedError) Unwrap() error { return e.Cause }

// WriterPoisonedError means an append reached a write or sync failure whose
// durable result is ambiguous. The writer must be closed and reopened before
// another append can safely receive a sequence number.
type WriterPoisonedError struct{ Cause error }

func (e *WriterPoisonedError) Error() string {
	return "event writer is poisoned after an ambiguous append; reopen before appending"
}

func (e *WriterPoisonedError) Unwrap() error { return e.Cause }

type appendFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// Writer keeps the append handle and next sequence in memory. It reads the
// file once on OpenWriter; Append itself never replays or recounts the log.
type Writer struct {
	root    string
	file    appendFile
	lease   *writerLease
	blobs   BlobVerifier
	nextSeq uint64
	closed  bool
	poison  error

	mu sync.Mutex
}

// OpenWriter opens (or initializes) the log inside a session directory. A
// malformed, unterminated crash tail is discarded before future appends so it
// cannot become a malformed middle line. This recovery read happens only at
// open time.
func OpenWriter(sessionDir string, blobs BlobVerifier) (*Writer, error) {
	root := filepath.Clean(sessionDir)
	lease, err := acquireWriterLease(root)
	if err != nil {
		return nil, err
	}
	filename := filepath.Join(root, EventsFilename)
	if info, err := os.Lstat(filename); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			_ = lease.Release()
			return nil, fmt.Errorf("event log must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = lease.Release()
		return nil, err
	}

	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		_ = lease.Release()
		return nil, err
	}
	closeOpenFiles := func() {
		_ = file.Close()
		_ = lease.Release()
	}
	body, err := readEventLogFile(filename)
	if err != nil {
		closeOpenFiles()
		return nil, err
	}
	events, validEnd, ignoredTail, terminalNewline, err := replayBytes(body)
	if err != nil {
		closeOpenFiles()
		return nil, err
	}
	if ignoredTail {
		if err := file.Truncate(validEnd); err != nil {
			closeOpenFiles()
			return nil, err
		}
		if err := file.Sync(); err != nil {
			closeOpenFiles()
			return nil, err
		}
	} else if terminalNewline {
		if err := writeAll(file, []byte("\n")); err != nil {
			closeOpenFiles()
			return nil, err
		}
		if err := file.Sync(); err != nil {
			closeOpenFiles()
			return nil, err
		}
	}
	return &Writer{
		root:    root,
		file:    file,
		lease:   lease,
		blobs:   blobs,
		nextSeq: uint64(len(events) + 1),
	}, nil
}

// Append assigns the next contiguous sequence, verifies all direct BlobRefs,
// then performs one O(1) write plus fsync. It returns the durable event value.
func (w *Writer) Append(event Event) (Event, error) {
	if w == nil {
		return Event{}, errors.New("nil event writer")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return Event{}, os.ErrClosed
	}
	if w.poison != nil {
		return Event{}, w.poison
	}
	if event.Seq != 0 && event.Seq != w.nextSeq {
		return Event{}, &SequenceError{Expected: w.nextSeq, Actual: event.Seq}
	}
	event.Seq = w.nextSeq
	normalized, err := normalizeEvent(event, true, w.root)
	if err != nil {
		return Event{}, err
	}
	for _, ref := range BlobRefs([]Event{normalized}) {
		if w.blobs == nil {
			return Event{}, BlobVerifierRequiredError{}
		}
		if err := w.blobs.Verify(ref); err != nil {
			return Event{}, &BlobNotDurableError{Ref: ref, Cause: err}
		}
	}
	line, err := CanonicalEventBytes(normalized)
	if err != nil {
		return Event{}, err
	}
	line = append(line, '\n')
	if err := writeAll(w.file, line); err != nil {
		return Event{}, w.poisonAfterAppendFailure(err)
	}
	if err := w.file.Sync(); err != nil {
		return Event{}, w.poisonAfterAppendFailure(err)
	}
	w.nextSeq++
	return normalized, nil
}

func (w *Writer) poisonAfterAppendFailure(cause error) error {
	if w.poison == nil {
		w.poison = &WriterPoisonedError{Cause: cause}
	}
	return w.poison
}

// NextSeq reports the sequence that a successful next Append will receive.
func (w *Writer) NextSeq() uint64 {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nextSeq
}

func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var closeErr error
	if w.file != nil {
		closeErr = w.file.Close()
	}
	leaseErr := w.lease.Release()
	w.lease = nil
	return errors.Join(closeErr, leaseErr)
}

// Replay reads canonical JSONL using the crash recovery rules. It never
// modifies the source reader or tries to repair a malformed middle line.
func Replay(reader io.Reader) ([]Event, error) {
	if reader == nil {
		return nil, errors.New("event reader is required")
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	events, _, _, _, err := replayBytes(body)
	return events, err
}

func replayBytes(body []byte) ([]Event, int64, bool, bool, error) {
	lines := splitLines(body)
	events := make([]Event, 0, len(lines))
	validEnd := int64(0)
	for index, line := range lines {
		event, err := decodeEvent(line.body)
		if err != nil {
			if index == len(lines)-1 && !line.terminated && recoverableFinalLine(err) {
				return events, validEnd, true, false, nil
			}
			return nil, 0, false, false, &ReplayLineError{Line: index + 1, Cause: err}
		}
		expected := uint64(len(events) + 1)
		if event.Seq != expected {
			return nil, 0, false, false, &SequenceError{Line: index + 1, Expected: expected, Actual: event.Seq}
		}
		events = append(events, event)
		validEnd = int64(line.end)
	}
	terminalNeedsNewline := len(lines) > 0 && !lines[len(lines)-1].terminated
	return events, validEnd, false, terminalNeedsNewline, nil
}

type eventLine struct {
	body       []byte
	end        int
	terminated bool
}

func splitLines(body []byte) []eventLine {
	if len(body) == 0 {
		return []eventLine{}
	}
	lines := []eventLine{}
	start := 0
	for index, character := range body {
		if character != '\n' {
			continue
		}
		lines = append(lines, eventLine{body: bytes.TrimSuffix(body[start:index], []byte("\r")), end: index + 1, terminated: true})
		start = index + 1
	}
	if start < len(body) {
		lines = append(lines, eventLine{body: bytes.TrimSuffix(body[start:], []byte("\r")), end: len(body)})
	}
	return lines
}

func recoverableFinalLine(err error) bool {
	var sequence *SequenceError
	if errors.As(err, &sequence) {
		return false
	}
	var unknown *UnknownTypeError
	if errors.As(err, &unknown) {
		return false
	}
	return true
}

func writeAll(file appendFile, body []byte) error {
	for len(body) > 0 {
		count, err := file.Write(body)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		body = body[count:]
	}
	return nil
}

// Initialize creates a newline-free empty append log. It is used only by
// session.Create; OpenWriter can also safely open that file later.
func Initialize(sessionDir string) error {
	filename := filepath.Join(filepath.Clean(sessionDir), EventsFilename)
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncEventDirectory(filepath.Dir(filename))
}

func syncEventDirectory(directory string) error {
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
