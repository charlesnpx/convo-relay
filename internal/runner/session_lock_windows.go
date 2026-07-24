//go:build windows

package runner

import (
	"os"

	"golang.org/x/sys/windows"
)

const lockEntireFile = ^uint32(0)

func lockFileExclusive(file *os.File) error {
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		lockEntireFile,
		lockEntireFile,
		new(windows.Overlapped),
	)
}

func unlockFile(file *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		lockEntireFile,
		lockEntireFile,
		new(windows.Overlapped),
	)
}
