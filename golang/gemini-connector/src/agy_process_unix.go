//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// configureAgyProcess puts agy into its own process group so that the whole
// subtree can be signalled at once.
func configureAgyProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killAgyProcess terminates the agy process group (negative PID targets the
// group), covering descendants that ignore the parent dying.
func killAgyProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}
