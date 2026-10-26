// QuickSpin — a portable, self-contained WordPress development environment.
//
// One executable that downloads its own runtime (Chromium, PHP, MariaDB, Caddy,
// VS Code server, WP-CLI, Adminer), keeps every project and database inside its
// own folder, and serves a dashboard for cloning repositories into running
// WordPress sites that open inside the bundled Chromium.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// appVersion is replaced at build time: go build -ldflags "-X main.appVersion=2.3.0"
// (scripts/build.sh and the release workflow take it from the git tag).
var appVersion = "2.2.0-dev"

type App struct {
	Root    string // portable root (folder containing quickspin.exe)
	Data    string // <root>/data — everything that must persist
	Runtime string // <root>/runtime — downloaded tools (re-creatable)

	cfg   *Config
	store *Store
	jobs  *JobManager

	mu       sync.Mutex
	services map[string]*Service

	caMu              sync.Mutex
	ca                *localCA // local HTTPS CA, see tls.go
	browserMu         sync.Mutex
	browserRules      string // resolver rules the running browser was launched with
	browserRestarting bool

	defaultPHP string // PHP version actually used as default (see pickDefaultPHP)
	flavorOnce sync.Once
	flavor     string

	shutdownOnce sync.Once
	shutdownCh   chan string

	log *log.Logger
}

func main() {
	if askpassMain() { // QuickSpin is answering an ssh password prompt
		return
	}
	root, err := portableRoot()
	if err != nil {
		fatal(err)
	}
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = strings.TrimLeft(os.Args[1], "-/")
	}
	if cmd == "help" || cmd == "h" || cmd == "?" {
		usage()
		return
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		fatal(fmt.Errorf("QuickSpin supports Apple Silicon Macs only (this Mac has an Intel processor)"))
	}
	if cmd == "version" {
		fmt.Println("QuickSpin", appVersion, runtime.GOOS+"/"+runtime.GOARCH)
		return
	}

	app, err := NewApp(root)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	switch cmd {
	case "run", "start":
		err = app.Run(ctx)
	case "setup", "update":
		ln, lerr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.cfg.Ports.Dashboard))
		if lerr != nil {
			fatal(fmt.Errorf("QuickSpin is running — stop it first (⋯ menu → Stop QuickSpin)"))
		}
		ln.Close()
		err = app.EnsureRuntime(ctx, cmd == "update")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func usage() {
	fmt.Print(`QuickSpin ` + appVersion + ` — portable WordPress development environment

Usage:
  quickspin            Download anything missing, start all services and open the browser
  quickspin setup      Only download/prepare the runtime (Chromium, PHP, MariaDB, ...)
  quickspin update     Re-download the runtime at the latest versions (projects are kept)
  quickspin version    Print the version

Everything lives next to the executable:
  runtime\   downloaded tools (safe to delete; re-downloaded on next start)
  data\      projects, databases, browser profile, editor settings, logs, config.json
`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "\nERROR:", err)
	if runtime.GOOS == "windows" && os.Getenv("QUICKSPIN_NO_PAUSE") == "" {
		fmt.Fprintln(os.Stderr, "\nPress Enter to close this window.")
		_, _ = fmt.Fscanln(os.Stdin)
	}
	os.Exit(1)
}

// portableRoot is the folder holding the executable, unless QUICKSPIN_HOME is
// set or we are running under `go run` (then the current directory is used).
func portableRoot() (string, error) {
	if h := os.Getenv("QUICKSPIN_HOME"); h != "" {
		return filepath.Abs(h)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	dir := filepath.Dir(exe)
	if strings.Contains(dir, "go-build") {
		return os.Getwd()
	}
	return dir, nil
}

func NewApp(root string) (*App, error) {
	a := &App{
		Root:       root,
		Data:       filepath.Join(root, "data"),
		Runtime:    filepath.Join(root, "runtime"),
		services:   map[string]*Service{},
		shutdownCh: make(chan string, 1),
	}
	for _, d := range []string{a.Data, a.Runtime, a.data("logs"), a.data("tmp"), a.data("projects"), a.data("php", "run"), a.data("workspaces"), a.data("exports")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	lf, err := os.OpenFile(a.data("logs", "quickspin.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	a.log = log.New(io.MultiWriter(os.Stdout, lf), "", log.LstdFlags)

	if a.cfg, err = LoadConfig(a.data("config.json")); err != nil {
		return nil, err
	}
	if a.store, err = OpenStore(a.data("projects.json")); err != nil {
		return nil, err
	}
	a.jobs = NewJobManager(a)
	// Projects pinned to a PHP version QuickSpin no longer offers use the default.
	for _, p := range a.store.List() {
		if p.PHP != "" && phpIndex(p.PHP) < 0 {
			a.store.Update(p.Slug, func(p *Project) { p.PHP = "" })
		}
	}
	pidDir = a.data("run")
	os.MkdirAll(pidDir, 0o755)
	if old, _ := filepath.Glob(a.data("tmp", "import-*")); len(old) > 0 { // abandoned imports
		for _, f := range old {
			os.Remove(f)
		}
	}
	return a, nil
}

func (a *App) data(p ...string) string { return filepath.Join(append([]string{a.Data}, p...)...) }
func (a *App) runtimeP(p ...string) string {
	return filepath.Join(append([]string{a.Runtime}, p...)...)
}

func (a *App) logf(format string, args ...any) { a.log.Printf(format, args...) }

// Run starts the whole environment and blocks until shutdown.
func (a *App) Run(ctx context.Context) error {
	a.logf("QuickSpin %s starting in %s", appVersion, a.Root)

	// Single instance: the dashboard port doubles as the lock.
	apiLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", a.cfg.Ports.Dashboard))
	if err != nil {
		return fmt.Errorf("port %d is in use — is QuickSpin already running? (change ports.dashboard in %s)", a.cfg.Ports.Dashboard, a.data("config.json"))
	}
	killLeftovers(a.logf)
	if err := a.checkPorts(); err != nil {
		apiLn.Close()
		return err
	}
	initPlatform()

	if err := a.EnsureRuntime(ctx, false); err != nil {
		apiLn.Close()
		return err
	}

	// Dashboard first, so problems with other services are visible in it.
	go a.serveAPI(apiLn)

	if err := a.startCore(); err != nil {
		a.stopAll()
		return err
	}
	a.startEditor()
	a.startBrowser(false)

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	a.logf("Ready. Dashboard: %s  (press Ctrl+C here to stop everything)", a.dashboardURL())
	var reason string
	select {
	case s := <-sig:
		reason = "signal " + s.String()
	case reason = <-a.shutdownCh:
	case <-ctx.Done():
		reason = "context cancelled"
	}
	a.logf("Shutting down (%s)…", reason)
	a.stopAll()
	a.logf("Bye.")
	return nil
}

func (a *App) requestShutdown(reason string) {
	a.shutdownOnce.Do(func() { a.shutdownCh <- reason })
}

func (a *App) dashboardURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/", a.cfg.Ports.Dashboard)
}

// checkPorts fails early with a readable message instead of cryptic service crashes.
func (a *App) checkPorts() error {
	p := a.cfg.Ports
	ports := map[string]int{"http": p.HTTP, "mariadb": p.MariaDB, "webAdmin": p.CaddyAdmin}
	if !a.cfg.Editor.Disabled {
		ports["editor"] = p.Editor
	}
	var busy []string
	for name, port := range ports {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			busy = append(busy, fmt.Sprintf("%s (%d)", name, port))
			continue
		}
		ln.Close()
	}
	if len(busy) > 0 {
		return fmt.Errorf("these ports are already in use: %s — stop the other program or change \"ports\" in %s", strings.Join(busy, ", "), a.data("config.json"))
	}
	return nil
}

// startCore starts the database and the FrankenPHP processes.
func (a *App) startCore() error {
	if err := a.writeSupportFiles(); err != nil {
		return err
	}
	if err := a.startMariaDB(); err != nil {
		return err
	}
	return a.startWeb()
}

func (a *App) stopAll() {
	a.browserMu.Lock()
	a.browserRestarting = true // closing the browser now must not trigger another shutdown
	a.browserMu.Unlock()

	for _, n := range []string{"browser", "editor"} {
		if s := a.service(n); s != nil {
			s.Stop(10 * time.Second)
		}
	}
	// FrankenPHP processes stop in parallel.
	a.mu.Lock()
	var php []*Service
	for n, s := range a.services {
		if strings.HasPrefix(n, "php-") || n == "web" {
			php = append(php, s)
		}
	}
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range php {
		wg.Add(1)
		go func(s *Service) { defer wg.Done(); s.Stop(5 * time.Second) }(s)
	}
	wg.Wait()
	if s := a.service("mariadb"); s != nil {
		s.Stop(30 * time.Second)
	}
}

func (a *App) service(name string) *Service {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services[name]
}

func (a *App) addService(s *Service) {
	a.mu.Lock()
	a.services[s.Name] = s
	a.mu.Unlock()
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func isDir(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

var errNotFound = errors.New("not found")
