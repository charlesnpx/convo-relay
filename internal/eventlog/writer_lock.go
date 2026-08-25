package eventlog

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const writerLockFilename = "events.lock"

// WriterLease holds the runtime lock used by event writers. Callers must
// release it when they are done with a liveness check or an exclusive session
// operation.
type WriterLease interface {
	Release() error
}

var writerLeaseRegistry = struct {
	sync.Mutex
	held map[string]struct{}
}{held: make(map[string]struct{})}

// writerLease combines an OS lock, which excludes other processes, with a
// small process-local registry. The registry gives same-process callers the
// same non-blocking failure semantics on platforms where advisory locks are
// process scoped.
type writerLease struct {
	file *os.File
	key  string
}

// AcquireWriterLease acquires the same non-blocking OS lock used by Writer.
// A WriterLockedError means an event writer currently owns the session.
func AcquireWriterLease(sessionDir string) (WriterLease, error) {
	return acquireWriterLease(sessionDir)
}

func acquireWriterLease(sessionDir string) (*writerLease, error) {
	root, err := filepath.Abs(sessionDir)
	if err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("event writer requires a real session directory")
	}

	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, err
	}
	runtimeInfo, err := os.Lstat(runtimeDir)
	if err != nil {
		return nil, err
	}
	if runtimeInfo.Mode()&os.ModeSymlink != 0 || !runtimeInfo.IsDir() {
		return nil, errors.New("event writer runtime directory must be a real directory")
	}

	lockPath := filepath.Join(runtimeDir, writerLockFilename)
	writerLeaseRegistry.Lock()
	defer writerLeaseRegistry.Unlock()
	if _, held := writerLeaseRegistry.held[lockPath]; held {
		return nil, &WriterLockedError{}
	}
	if pathInfo, err := os.Lstat(lockPath); err == nil {
		if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
			return nil, errors.New("event writer lock must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	locked, err := tryLockWriterFile(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !locked {
		_ = file.Close()
		return nil, &WriterLockedError{}
	}
	fileInfo, fileErr := file.Stat()
	pathInfo, pathErr := os.Lstat(lockPath)
	if fileErr != nil ||
		pathErr != nil ||
		pathInfo.Mode()&os.ModeSymlink != 0 ||
		!pathInfo.Mode().IsRegular() ||
		!os.SameFile(fileInfo, pathInfo) {
		_ = unlockWriterFile(file)
		_ = file.Close()
		return nil, errors.New("event writer lock path changed while waiting for the lock")
	}
	writerLeaseRegistry.held[lockPath] = struct{}{}
	return &writerLease{file: file, key: lockPath}, nil
}

func (l *writerLease) Release() error {
	if l == nil {
		return nil
	}
	writerLeaseRegistry.Lock()
	defer writerLeaseRegistry.Unlock()
	if l.file == nil {
		delete(writerLeaseRegistry.held, l.key)
		return nil
	}
	unlockErr := unlockWriterFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	delete(writerLeaseRegistry.held, l.key)
	return errors.Join(unlockErr, closeErr)
}
