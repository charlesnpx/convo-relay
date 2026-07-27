//go:build !windows

package main

import (
	"os"
	"syscall"
)

func commandInterruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
