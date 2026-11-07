//go:build unix

package hooks

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in a process group of its own and has its
// cancellation kill the whole group, so a shell's children end with it.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
