package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type sessionMutationLock struct {
	file *os.File
}

func lockSessionMutation(sessionDir string) (*sessionMutationLock, error) {
	file, err := os.OpenFile(filepath.Join(sessionDir, ".mutation.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &sessionMutationLock{file: file}, nil
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
