package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogBuf writes a service's output to a file and keeps the last lines in memory
// for the dashboard.
type LogBuf struct {
	mu      sync.Mutex
	f       *os.File
	lines   []string
	partial []byte
	max     int
}

func NewLogBuf(path string, max int) *LogBuf {
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	return &LogBuf{f: f, max: max}
}

func (l *LogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Write(p)
	}
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(l.partial[:i], "\r"))
		l.partial = l.partial[i+1:]
		l.lines = append(l.lines, line)
		if len(l.lines) > l.max {
			l.lines = l.lines[len(l.lines)-l.max:]
		}
	}
	if len(l.partial) > 64<<10 { // pathological line without newline
		l.partial = l.partial[:0]
	}
	return len(p), nil
}

func (l *LogBuf) Tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.lines) {
		n = len(l.lines)
	}
	return append([]string(nil), l.lines[len(l.lines)-n:]...)
}

// Service is a supervised child process.
type Service struct {
	Name        string
	Build       func() (*exec.Cmd, error)
	StopFn      func(cmd *exec.Cmd) // graceful stop; falls back to Kill
	AutoRestart bool
	InJob       bool // tie the process lifetime to QuickSpin (Windows job object)
	Hidden      bool // no console window
	OnExit      func(err error)
	Log         *LogBuf

	mu        sync.Mutex
	cmd       *exec.Cmd
	state     string
	lastErr   string
	restarts  int
	startedAt time.Time
	stopping  bool
	stopCh    chan struct{}
	done      chan struct{}
}

type ServiceStatus struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
	Restarts int    `json:"restarts"`
	PID      int    `json:"pid,omitempty"`
	Uptime   string `json:"uptime,omitempty"`
}

func (s *Service) Status() ServiceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ServiceStatus{Name: s.Name, State: s.state, Error: s.lastErr, Restarts: s.restarts}
	if s.cmd != nil && s.cmd.Process != nil {
		st.PID = s.cmd.Process.Pid
		st.Uptime = time.Since(s.startedAt).Round(time.Second).String()
	}
	return st
}

func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == "running"
}

func (s *Service) set(state, err string) {
	s.mu.Lock()
	s.state, s.lastErr = state, err
	s.mu.Unlock()
}

func (s *Service) Start() {
	s.mu.Lock()
	s.stopCh = make(chan struct{})
	s.done = make(chan struct{})
	s.stopping = false
	s.state = "starting"
	s.mu.Unlock()
	go s.loop()
}

func (s *Service) loop() {
	defer close(s.done)
	backoff := time.Second
	var crashes []time.Time
	for {
		cmd, err := s.Build()
		if err != nil {
			s.set("failed", err.Error())
			fmt.Fprintf(s.Log, "[quickspin] cannot start: %v\n", err)
			return
		}
		cmd.Stdout, cmd.Stderr = s.Log, s.Log
		cmd.WaitDelay = 3 * time.Second // don't hang on grandchildren holding our pipes
		applyProcAttr(cmd, s.Hidden)
		if err := cmd.Start(); err != nil {
			s.set("failed", err.Error())
			fmt.Fprintf(s.Log, "[quickspin] cannot start: %v\n", err)
			return
		}
		if s.InJob {
			addToJob(cmd)
		}
		writePidFile(s.Name, cmd)
		s.mu.Lock()
		s.cmd, s.state, s.lastErr, s.startedAt = cmd, "running", "", time.Now()
		s.mu.Unlock()

		err = cmd.Wait()
		removePidFile(s.Name)
		ran := time.Since(s.startedAt)
		s.mu.Lock()
		stopping := s.stopping
		s.cmd = nil
		s.mu.Unlock()
		if stopping {
			s.set("stopped", "")
			return
		}
		msg := "exited"
		if err != nil {
			msg = err.Error()
		}
		fmt.Fprintf(s.Log, "[quickspin] process %s\n", msg)
		if s.OnExit != nil {
			s.OnExit(err)
		}
		if !s.AutoRestart {
			s.set("stopped", msg)
			return
		}
		if err == nil && ran > 5*time.Second {
			// Planned recycle (e.g. php-cgi after PHP_FCGI_MAX_REQUESTS): restart at once.
			s.mu.Lock()
			s.restarts++
			s.mu.Unlock()
			continue
		}
		now := time.Now()
		recent := crashes[:0]
		for _, t := range crashes {
			if now.Sub(t) < time.Minute {
				recent = append(recent, t)
			}
		}
		crashes = append(recent, now)
		if len(crashes) >= 6 {
			s.set("failed", "crashed repeatedly: "+msg)
			return
		}
		if ran > time.Minute {
			backoff = time.Second
		}
		s.mu.Lock()
		s.state, s.lastErr = "restarting", msg
		s.restarts++
		s.mu.Unlock()
		select {
		case <-time.After(backoff):
		case <-s.stopCh:
			s.set("stopped", "")
			return
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

// Stop stops the process (gracefully when StopFn is set) and waits up to timeout.
func (s *Service) Stop(timeout time.Duration) {
	s.mu.Lock()
	if s.done == nil || s.stopping {
		done := s.done
		s.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-time.After(timeout):
			}
		}
		return
	}
	s.stopping = true
	close(s.stopCh)
	cmd := s.cmd
	done := s.done
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		if s.StopFn != nil {
			s.StopFn(cmd)
		} else {
			cmd.Process.Kill()
		}
	}
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}

// Restart stops and starts the service again.
func (s *Service) Restart() {
	s.Stop(10 * time.Second)
	s.Start()
}

// ---- leftover processes ----
//
// On Windows a job object ends every child with QuickSpin and on Linux the
// parent-death signal does; on macOS children of a killed QuickSpin survive.
// Each service records its PID so the next start can clean up.

var pidDir string

func writePidFile(name string, cmd *exec.Cmd) {
	if pidDir == "" || cmd.Process == nil {
		return
	}
	os.WriteFile(filepath.Join(pidDir, name+".pid"), []byte(fmt.Sprintf("%d\n%s\n", cmd.Process.Pid, filepath.Base(cmd.Path))), 0o644)
}

func removePidFile(name string) {
	if pidDir != "" {
		os.Remove(filepath.Join(pidDir, name+".pid"))
	}
}

// killLeftovers stops processes from a previous run that are still alive.
func killLeftovers(logf func(string, ...any)) {
	if pidDir == "" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(pidDir, "*.pid"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		os.Remove(f)
		lines := strings.Fields(string(b))
		if len(lines) < 2 {
			continue
		}
		pid, err := strconv.Atoi(lines[0])
		if err != nil || pid <= 1 {
			continue
		}
		if processMatches(pid, lines[1]) {
			logf("Stopping a leftover %s process (PID %d) from a previous run", lines[1], pid)
			killProcess(pid)
		}
	}
}
