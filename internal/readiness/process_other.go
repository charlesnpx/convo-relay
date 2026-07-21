//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package readiness

import "os/exec"

// Non-POSIX platforms still receive a finite pipe-drain bound. Platform-native
// process-tree cancellation can be added here when such a target is supported.
func configureProbeCommand(command *exec.Cmd) {
	command.WaitDelay = probeWaitDelay
}
