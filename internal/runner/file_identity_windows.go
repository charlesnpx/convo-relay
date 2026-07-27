//go:build windows

package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func rootInitializationFileIdentity(path string, info os.FileInfo) string {
	if info == nil || path == "" {
		return ""
	}
	pointer, err := syscall.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return ""
	}
	handle, err := syscall.CreateFile(
		pointer,
		0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(handle)
	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &data); err != nil {
		return ""
	}
	return fmt.Sprintf(
		"%d:%d:%d",
		data.VolumeSerialNumber,
		data.FileIndexHigh,
		data.FileIndexLow,
	)
}
