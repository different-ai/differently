//go:build unix

package guest

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup runs the command in its own process group so a
// timeout or cancellation kills every child, not just the shell.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
