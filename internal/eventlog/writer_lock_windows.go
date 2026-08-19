//go:build windows

package eventlog

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const writerLockEntireFile = ^uint32(0)

func tryLockWriterFile(file *os.File) (bool, error) {
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		writerLockEntireFile,
		writerLockEntireFile,
		new(windows.Overlapped),
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

func unlockWriterFile(file *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		writerLockEntireFile,
		writerLockEntireFile,
		new(windows.Overlapped),
	)
}
