//go:build !windows

package main

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func initPlatform() {}

func addToJob(cmd *exec.Cmd) {}

func applyProcAttr(cmd *exec.Cmd, hidden bool) { setDeathSignal(cmd) }

func gracefulStop(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }

func hideCmd(cmd *exec.Cmd) {}

// processMatches reports whether pid is alive and runs the named executable
// (guards against PID reuse).
func processMatches(pid int, exe string) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	comm := strings.TrimSpace(string(out))
	return comm != "" && (filepath.Base(comm) == exe || strings.HasPrefix(exe, filepath.Base(comm)))
}

func killProcess(pid int) {
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
}

// newSession detaches a child from the terminal, so ssh asks SSH_ASKPASS
// for passwords instead of prompting on the console.
func newSession(cmd *exec.Cmd) {
	setDeathSignal(cmd)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
