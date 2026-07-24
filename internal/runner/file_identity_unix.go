//go:build !windows

package runner

import (
	"fmt"
	"os"
	"syscall"
)

func rootInitializationFileIdentity(_ string, info os.FileInfo) string {
	if info == nil {
		return ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino))
}
