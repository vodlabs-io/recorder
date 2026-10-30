//go:build !windows

package preview

import (
	"os/exec"
	"syscall"
)

func lowerPriority(*exec.Cmd) {}

// afterStart renices the process. A failure only leaves it at normal
// priority.
func afterStart(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, 10)
	}
}
