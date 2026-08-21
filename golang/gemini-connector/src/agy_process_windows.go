//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
)

// configureAgyProcess applies platform-specific process setup. Windows needs
// no process group; the tree is killed via taskkill.
func configureAgyProcess(cmd *exec.Cmd) {}

// killAgyProcess terminates the agy process and all of its descendants
// (node helpers, browsers spawned by tools, ...). taskkill /T walks the
// child tree; /F forces termination.
func killAgyProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	return exec.Command("taskkill", "/PID", strconv.Itoa(p.Pid), "/T", "/F").Run()
}
