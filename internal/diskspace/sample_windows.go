//go:build windows

package diskspace

import (
	"fmt"
	"strconv"

	"golang.org/x/sys/windows"
)

type sysSampler struct{}

func (sysSampler) Sample(path string) (Usage, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Usage{}, fmt.Errorf("reading free space for %s: %w", path, err)
	}
	// freeForCaller honours per-user quotas; total and free describe the volume.
	// The caller-visible figure is the one a recording can actually use.
	var freeForCaller, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeForCaller, &total, &free); err != nil {
		return Usage{}, fmt.Errorf("reading free space for %s: %w", path, err)
	}
	dev, err := deviceID(path)
	if err != nil {
		return Usage{}, err
	}
	return Usage{TotalBytes: total, FreeBytes: freeForCaller, Device: dev}, nil
}

// deviceID returns the volume serial number the path lives on. Reading it from
// an open handle works for drive letters and UNC shares alike, which matters
// because an archive folder is usually a network share.
func deviceID(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", fmt.Errorf("identifying volume for %s: %w", path, err)
	}
	// FILE_FLAG_BACKUP_SEMANTICS is required to open a directory handle.
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", fmt.Errorf("identifying volume for %s: %w", path, err)
	}
	defer windows.CloseHandle(h)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return "", fmt.Errorf("identifying volume for %s: %w", path, err)
	}
	return strconv.FormatUint(uint64(info.VolumeSerialNumber), 10), nil
}
