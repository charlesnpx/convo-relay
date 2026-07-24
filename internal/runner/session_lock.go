package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

type sessionMutationLock struct {
	file        *os.File
	path        string
	sessionRoot string
	identity    os.FileInfo
}

// sessionMutationLockAfterOpen is a test-only synchronization hook.
var sessionMutationLockAfterOpen func(string)

func lockSessionMutation(sessionDir string) (*sessionMutationLock, error) {
	sessionRoot, err := filepath.Abs(sessionDir)
	if err != nil {
		return nil, err
	}
	sessionRoot = filepath.Clean(sessionRoot)
	rootBefore, err := os.Lstat(sessionRoot)
	if err != nil {
		return nil, err
	}
	if rootBefore.Mode()&os.ModeSymlink != 0 || !rootBefore.IsDir() {
		return nil, errors.New("session mutation lock requires a real session directory")
	}
	lockPath := filepath.Join(sessionRoot, ".mutation.lock")
	if info, err := os.Lstat(lockPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("session mutation lock path must not be a symlink")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if sessionMutationLockAfterOpen != nil {
		sessionMutationLockAfterOpen(lockPath)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	fileInfo, fileErr := file.Stat()
	pathInfo, pathErr := os.Lstat(lockPath)
	rootAfter, rootErr := os.Lstat(sessionRoot)
	if fileErr != nil ||
		pathErr != nil ||
		rootErr != nil ||
		pathInfo.Mode()&os.ModeSymlink != 0 ||
		!pathInfo.Mode().IsRegular() ||
		!os.SameFile(fileInfo, pathInfo) ||
		rootAfter.Mode()&os.ModeSymlink != 0 ||
		!rootAfter.IsDir() ||
		!os.SameFile(rootBefore, rootAfter) {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, errors.New("session mutation lock path changed while waiting for the lock")
	}
	return &sessionMutationLock{
		file:        file,
		path:        lockPath,
		sessionRoot: sessionRoot,
		identity:    fileInfo,
	}, nil
}

func (l *sessionMutationLock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func (l *sessionMutationLock) writeMarker(payload map[string]any) error {
	if l == nil || l.file == nil {
		return errors.New("session mutation lock is not held")
	}
	body, err := contracts.CanonicalJSONBytes(payload)
	if err != nil {
		return err
	}
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := l.file.Write(body); err != nil {
		return err
	}
	return l.file.Sync()
}

func (l *sessionMutationLock) removePathAfterUnlock() error {
	if l == nil || l.file != nil || l.path == "" || l.identity == nil {
		return errors.New("session mutation lock must be unlocked before exact path removal")
	}
	current, err := os.Lstat(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(l.identity, current) {
		return errors.New("session mutation lock path changed before removal")
	}
	return os.Remove(l.path)
}

func ensureSessionNotRunning(sessionDir string, action string) error {
	pid, running, err := liveSessionPID(sessionDir)
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("session %s is running with pid %d; stop it before %s", sessionIDFromDir(sessionDir), pid, action)
	}
	return nil
}

func liveSessionPID(sessionDir string) (int, bool, error) {
	pid, err := readPID(sessionDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read relay pid: %w", err)
	}
	return pid, processAlive(pid), nil
}
