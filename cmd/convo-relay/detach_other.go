//go:build !darwin && !linux

package main

import (
	"fmt"
	"runtime"
)

func launchDetachedRun(_ []string, _ string) (detachedLaunchReceipt, error) {
	return detachedLaunchReceipt{}, fmt.Errorf("run --detach is unsupported on %s", runtime.GOOS)
}
