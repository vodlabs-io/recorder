//go:build windows

package vlupdate

import "syscall"

// CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS — daemon survives CLI exit.
const detachFlags = 0x00000200 | 0x00000008

func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachFlags}
}
