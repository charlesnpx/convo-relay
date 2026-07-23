//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package workspace

import (
	"os"
	"syscall"
)

func openWorkspaceFileNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func createWorkspaceFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
}
