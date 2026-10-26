//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setDeathSignal makes children die with QuickSpin (Linux only).
func setDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
