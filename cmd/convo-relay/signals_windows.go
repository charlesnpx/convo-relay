//go:build windows

package main

import "os"

func commandInterruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
