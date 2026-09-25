//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup gives the shell its own process group and a Cancel that
// SIGKILLs the whole group. Without it, backgrounded children (`cmd &`) outlive
// the shell on cancel or timeout, the leak we prevent. Unix only: Setpgid and
// negative PID Kill aren't portable (Windows has its own build).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the whole group (shell is the leader).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
