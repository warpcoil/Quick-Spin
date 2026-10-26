//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	jobHandle                    uintptr
)

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct{ a, b, c, d, e, f uint64 }

type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IO                    ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	createNoWindow                    = 0x08000000
)

// initPlatform creates a job object: when QuickSpin exits (even if killed or
// its console is closed) Windows terminates every service started in it.
func initPlatform() {
	h, _, _ := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return
	}
	info := jobExtendedLimit{}
	info.Basic.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, _ := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return
	}
	jobHandle = h
}

func addToJob(cmd *exec.Cmd) {
	if jobHandle == 0 || cmd.Process == nil {
		return
	}
	const processSetQuota, processTerminate = 0x0100, 0x0001
	ph, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	procAssignProcessToJobObject.Call(jobHandle, uintptr(ph))
	syscall.CloseHandle(ph)
}

func applyProcAttr(cmd *exec.Cmd, hidden bool) {
	if hidden {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	}
}

// gracefulStop asks a process tree to close (WM_CLOSE), like closing its window.
func gracefulStop(pid int) {
	c := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T")
	applyProcAttr(c, true)
	c.WaitDelay = time.Second
	_ = c.Run()
}

func hideCmd(cmd *exec.Cmd) { applyProcAttr(cmd, true) }

// Windows: the job object already ends every child together with QuickSpin.
func processMatches(pid int, exe string) bool { return false }

func killProcess(pid int) {}

// newSession: Windows consoles are not inherited the same way; ssh uses SSH_ASKPASS
// when SSH_ASKPASS_REQUIRE=force is set.
func newSession(cmd *exec.Cmd) { applyProcAttr(cmd, true) }
