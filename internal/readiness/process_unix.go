//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package readiness

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProbeCommand(command *exec.Cmd, markInterrupted func()) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		markInterrupted()
		return err
	}
	command.WaitDelay = probeWaitDelay
}
