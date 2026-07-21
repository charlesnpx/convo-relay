//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package namedinputs

import (
	"errors"
	"os"
	"syscall"
)

func openNamedInputFileNoFollow(path string) (*os.File, error) {
	handle, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
		return nil, errNotRegular
	}
	return handle, err
}
