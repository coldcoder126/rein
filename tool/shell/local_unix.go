//go:build unix

package shell

import (
	"os/exec"
	"syscall"
)

// killProcessGroup runs the command in its own process group and kills the whole group on cancel, leaving no orphans.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
