//go:build windows

package preview

import (
	"os/exec"
	"syscall"
)

// belowNormalPriorityClass is BELOW_NORMAL_PRIORITY_CLASS.
const belowNormalPriorityClass = 0x00004000

func lowerPriority(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: belowNormalPriorityClass}
}

func afterStart(*exec.Cmd) {}
