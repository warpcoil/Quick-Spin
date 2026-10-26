package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ===================== VS Code (serve-web) =====================

func (a *App) editorToken() string {
	p := a.data("vscode", "token")
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) >= 16 {
		return string(bytes.TrimSpace(b))
	}
	buf := make([]byte, 16)
	rand.Read(buf)
	t := hex.EncodeToString(buf)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(t), 0o600)
	return t
}

func (a *App) startEditor() {
	if a.cfg.Editor.Disabled {
		return
	}
	exe := a.codeExe()
	if exe == "" {
		a.logf("VS Code CLI not found — the in-browser editor is unavailable.")
		return
	}
	a.editorToken()
	a.writeEditorSettings()
	s := &Service{
		Name: "editor", AutoRestart: true, InJob: true, Hidden: true,
		Log: NewLogBuf(a.data("logs", "editor.log"), 300),
		Build: func() (*exec.Cmd, error) {
			c := exec.Command(exe, "serve-web",
				"--host", "127.0.0.1",
				"--port", strconv.Itoa(a.cfg.Ports.Editor),
				"--connection-token-file", a.data("vscode", "token"),
				"--accept-server-license-terms",
				"--server-data-dir", a.data("vscode", "server"),
				"--cli-data-dir", a.data("vscode", "cli"),
			)
			php := a.phpForEditor(a.defaultPHP)
			path := os.Getenv("PATH")
			if php != "" {
				path = filepath.Dir(php) + string(os.PathListSeparator) + path
			}
			if g := a.gitExe(); g != "" {
				path = filepath.Dir(g) + string(os.PathListSeparator) + path
			}
			c.Env = append(os.Environ(), "PATH="+path, "QUICKSPIN_SITE=")
			return c, nil
		},
	}
	a.addService(s)
	s.Start()
	a.logf("Starting VS Code (serve-web) on 127.0.0.1:%d…", a.cfg.Ports.Editor)
}

func (a *App) writeEditorSettings() {
	p := a.data("vscode", "server", "data", "Machine", "settings.json")
	settings := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &settings)
	}
	php := a.phpForEditor(a.defaultPHP)
	defaults := map[string]any{
		"php.validate.executablePath":         php,
		"php.debug.executablePath":            php,
		"security.workspace.trust.enabled":    false,
		"workbench.startupEditor":             "none",
		"extensions.ignoreRecommendations":    false,
		"intelephense.environment.phpVersion": a.defaultPHP + ".0",
	}
	if g := a.gitExe(); g != "" {
		defaults["git.path"] = g
	}
	changed := false
	for k, v := range defaults {
		if _, ok := settings[k]; !ok {
			settings[k] = v
			changed = true
		}
	}
	if changed {
		b, _ := json.MarshalIndent(settings, "", "  ")
		writeFileAtomic(p, b)
	}
}

// vscodePath turns a local path into the form VS Code web expects in ?folder= / ?workspace=.
func vscodePath(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func (a *App) editorURL(p *Project) string {
	u := fmt.Sprintf("http://127.0.0.1:%d/?tkn=%s", a.cfg.Ports.Editor, a.editorToken())
	if p != nil {
		u += "&workspace=" + urlQueryEscape(vscodePath(a.workspaceFile(p)))
	}
	return u
}

// ===================== Chromium =====================

// resolverRules maps every dev domain to Caddy — only inside QuickSpin's
// browser, so no hosts-file edits or admin rights are needed.
func (a *App) resolverRules() string {
	target := fmt.Sprintf("127.0.0.1:%d", a.cfg.Ports.HTTP)
	rules := []string{"MAP *.test " + target}
	seen := map[string]bool{}
	for _, p := range a.store.List() {
		for _, d := range p.Domains {
			if strings.HasSuffix(d, ".test") || seen[d] {
				continue
			}
			seen[d] = true
			rules = append(rules, "MAP "+d+" "+target)
		}
	}
	sort.Strings(rules[1:])
	return strings.Join(rules, ", ")
}

func (a *App) browserNeedsRestart() bool {
	a.browserMu.Lock()
	defer a.browserMu.Unlock()
	s := a.service("browser")
	return s != nil && s.Running() && a.browserRules != a.resolverRules()
}

func (a *App) startBrowser(restore bool) {
	if a.cfg.Browser.Disabled {
		a.logf("Browser disabled in config — open %s yourself.", a.dashboardURL())
		return
	}
	exe := a.chromiumExe()
	if exe == "" {
		a.logf("Chromium not found — open %s in a browser.", a.dashboardURL())
		return
	}
	rules := a.resolverRules()
	a.browserMu.Lock()
	a.browserRules = rules
	a.browserRestarting = false
	a.browserMu.Unlock()

	var bypass []string
	for _, r := range strings.Split(rules, ", ") {
		bypass = append(bypass, strings.Fields(r)[1])
	}
	args := []string{
		"--user-data-dir=" + a.data("browser"),
		"--no-first-run",
		"--no-default-browser-check",
		"--host-resolver-rules=" + rules,
		// Dev sites are plain http; don't let Chromium try https first.
		"--disable-features=HttpsUpgrades,HttpsFirstBalancedModeAutoEnable,HttpsFirstModeV2ForTypicallySecureUsers",
	}
	if a.cfg.Browser.ProxyServer != "" {
		args = append(args, "--proxy-server="+a.cfg.Browser.ProxyServer, "--proxy-bypass-list="+strings.Join(bypass, ";")+";127.0.0.1;localhost")
	}
	args = append(args, a.cfg.Browser.ExtraArgs...)
	if restore {
		args = append(args, "--restore-last-session")
	} else {
		args = append(args, a.dashboardURL())
	}

	s := &Service{
		Name: "browser", AutoRestart: false, InJob: true, Hidden: false,
		Log: NewLogBuf(a.data("logs", "browser.log"), 200),
		Build: func() (*exec.Cmd, error) {
			c := exec.Command(exe, args...)
			// Silences the "Google API keys are missing" bar of Chromium builds.
			c.Env = append(os.Environ(), "GOOGLE_API_KEY=no", "GOOGLE_DEFAULT_CLIENT_ID=no", "GOOGLE_DEFAULT_CLIENT_SECRET=no")
			return c, nil
		},
		StopFn: func(cmd *exec.Cmd) { gracefulStop(cmd.Process.Pid) },
		OnExit: func(err error) {
			a.browserMu.Lock()
			restarting := a.browserRestarting
			a.browserMu.Unlock()
			if !restarting && a.cfg.Browser.ExitWithBrowser {
				a.requestShutdown("browser closed")
			}
		},
	}
	a.addService(s)
	s.Start()
	a.logf("Browser started (%s).", filepath.Base(exe))
}

func (a *App) restartBrowser() {
	a.browserMu.Lock()
	a.browserRestarting = true
	a.browserMu.Unlock()
	if s := a.service("browser"); s != nil {
		s.Stop(15 * time.Second)
	}
	time.Sleep(500 * time.Millisecond)
	a.startBrowser(true)
}
