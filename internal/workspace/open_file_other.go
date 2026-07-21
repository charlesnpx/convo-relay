//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package workspace

import "os"

func openWorkspaceFileNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
