//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package readiness

import (
	"errors"
	"os"
	"os/exec"
)

// Non-POSIX platforms still receive a finite pipe-drain bound. Platform-native
// process-tree cancellation can be added here when such a target is supported.
func configureProbeCommand(command *exec.Cmd, markInterrupted func()) {
	cancel := command.Cancel
	command.Cancel = func() error {
		if cancel == nil {
			return nil
		}
		err := cancel()
		if !errors.Is(err, os.ErrProcessDone) {
			markInterrupted()
		}
		return err
	}
	command.WaitDelay = probeWaitDelay
}
