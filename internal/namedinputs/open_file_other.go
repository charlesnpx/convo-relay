//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package namedinputs

import "os"

func openNamedInputFileNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
