package eventlog

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const writerLockFilename = "events.lock"

var writerLeaseRegistry = struct {
	sync.Mutex
	held map[string]struct{}
}{held: make(map[string]struct{})}

// WriterLease combines an OS lock, which excludes other processes, with a
// small process-local registry. The registry gives same-process callers the
// same non-blocking failure semantics on platforms where advisory locks are
// process scoped.
type WriterLease struct {
	file *os.File
	key  string
}

// AcquireWriterLease acquires the same non-blocking OS lock used by Writer.
// A WriterLockedError means an event writer currently owns the session.
func AcquireWriterLease(sessionDir string) (*WriterLease, error) {
	return acquireWriterLease(sessionDir, true)
}

// ProbeWriterLease reports whether an event writer owns the session lock.
// It only opens an existing lock file and releases any lock it acquires before
// returning, so a read does not create or leave behind runtime lock state.
func ProbeWriterLease(sessionDir string) (bool, error) {
	lease, err := acquireWriterLease(sessionDir, false)
	if err != nil {
		var locked *WriterLockedError
		if errors.As(err, &locked) {
			return true, nil
		}
		return false, err
	}
	if lease == nil {
		return false, nil
	}
	if err := lease.Release(); err != nil {
		return false, err
	}
	return false, nil
}

func acquireWriterLease(sessionDir string, create bool) (*WriterLease, error) {
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
	if create {
		if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
			return nil, err
		}
	} else if _, err := os.Lstat(runtimeDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
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
	} else if !create && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(lockPath, flags, 0o600)
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
	return &WriterLease{file: file, key: lockPath}, nil
}

func (l *WriterLease) Release() error {
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
