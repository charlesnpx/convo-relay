//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package workspace

import "os"

func openWorkspaceFileNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

func createWorkspaceFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
}
