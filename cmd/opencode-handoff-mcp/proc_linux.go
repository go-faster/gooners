package main

import (
	"os/exec"
	"syscall"
)

func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Send SIGTERM to the child if this process dies (even on SIGKILL).
		Pdeathsig: syscall.SIGTERM,
	}
}
