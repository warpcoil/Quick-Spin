package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

//go:embed ui/index.html
var uiFS embed.FS

func (a *App) serveAPI(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := uiFS.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/status", a.hStatus)
	mux.HandleFunc("GET /api/projects", a.hProjects)
	mux.HandleFunc("POST /api/projects", a.hCreate)
	mux.HandleFunc("PUT /api/projects/{slug}", a.hUpdate)
	mux.HandleFunc("DELETE /api/projects/{slug}", a.hDelete)
	mux.HandleFunc("POST /api/projects/{slug}/{action}", a.hAction)
	mux.HandleFunc("GET /api/projects/{slug}/login", a.hLogin)
	mux.HandleFunc("GET /api/jobs/{id}", a.hJob)
	mux.HandleFunc("GET /api/services/{name}/log", a.hServiceLog)
	mux.HandleFunc("POST /api/services/{name}/restart", a.hServiceRestart)
	mux.HandleFunc("POST /api/browser/restart", func(w http.ResponseWriter, r *http.Request) {
		go a.restartBrowser()
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/open-folder", a.hOpenFolder)
	mux.HandleFunc("GET /api/export", a.hExport)
	mux.HandleFunc("POST /api/import", a.hImportUpload)
	mux.HandleFunc("POST /api/import/{id}/apply", a.hImportApply)
	mux.HandleFunc("POST /api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
		go func() { time.Sleep(300 * time.Millisecond); a.requestShutdown("stopped from the dashboard") }()
	})
	srv := &http.Server{Handler: a.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.logf("dashboard server: %v", err)
	}
}

// guard blocks cross-site requests: any page open in the browser could
// otherwise drive this API. Mutations need our header (which forces a CORS
// preflight we never approve) and a same-origin Origin.
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden host", http.StatusForbidden) // DNS rebinding
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("X-QuickSpin") != "1" {
				http.Error(w, "missing X-QuickSpin header", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := 400
	if errors.Is(err, errNotFound) {
		code = 404
	}
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func (a *App) hStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	var svcs []ServiceStatus
	for _, s := range a.services {
		svcs = append(svcs, s.Status())
	}
	a.mu.Unlock()
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].Name < svcs[j].Name })
	versions := map[string]string{}
	for _, c := range []string{"mariadb", "chromium", "vscode", "git", "wp-cli", "adminer"} {
		label := c
		if c == "mariadb" {
			label = "database"
		}
		if v := installedVersion(a.runtimeP(c)); v != "" {
			versions[label] = v
		} else if c == "mariadb" {
			versions[label] = a.dbName() + " (installed on this computer)"
		}
	}
	for _, v := range a.installedPHPVersions() {
		versions["php "+v] = installedVersion(a.runtimeP("php", v))
		if versions["php "+v] == "" {
			versions["php "+v] = a.frankenphpExe(v)
		}
		if a.xdebugModule(v) == "" && !strings.Contains(versions["php "+v], "Xdebug") {
			versions["php "+v] += " — no Xdebug"
		}
	}
	editor := ""
	if s := a.service("editor"); s != nil {
		editor = a.editorURL(nil)
	}
	adminer := ""
	if exists(a.runtimeP("adminer", "adminer.php")) {
		adminer = "http://" + a.dbAdminHost() + "/"
	}
	writeJSON(w, 200, map[string]any{
		"version":        appVersion,
		"os":             runtime.GOOS,
		"platform":       platformID(),
		"dbName":         a.dbName(),
		"xdebug":         a.xdebugModule(a.defaultPHP) != "",
		"root":           a.Root,
		"services":       svcs,
		"versions":       versions,
		"phpCatalog":     phpCatalog,
		"phpInstalled":   a.installedPHPVersions(),
		"defaultPhp":     a.defaultPHP,
		"browserRestart": a.browserNeedsRestart(),
		"editorUrl":      editor,
		"adminerUrl":     adminer,
		"ports":          a.cfg.Ports,
		"db":             map[string]any{"host": "127.0.0.1", "port": a.cfg.Ports.MariaDB, "user": dbUser, "password": dbPass},
		"gitAvailable":   a.gitExe() != "",
		"projectsDir":    a.data("projects"),
		"configFile":     a.data("config.json"),
	})
}

type projectView struct {
	*Project
	URL        string         `json:"url"`
	DocRoot    string         `json:"docRoot"`
	EditorURL  string         `json:"editorUrl,omitempty"`
	AdminerURL string         `json:"adminerUrl,omitempty"`
	Job        map[string]any `json:"job,omitempty"`
	Busy       bool           `json:"busy"`
	DebugURL   string         `json:"debugUrl"`
	Xdebug     bool           `json:"xdebug"`
	RunsPHP    string         `json:"runsPhp"`
}

func (a *App) view(p *Project) projectView {
	v := projectView{Project: p, URL: p.URL(), DocRoot: p.DocRoot(), Busy: a.jobs.Busy(p.Slug)}
	if a.service("editor") != nil {
		v.EditorURL = a.editorURL(p)
	}
	if exists(a.runtimeP("adminer", "adminer.php")) {
		v.AdminerURL = fmt.Sprintf("http://%s/?db=%s", a.dbAdminHost(), p.DBName)
	}
	v.DebugURL = p.URL() + "/?XDEBUG_SESSION_START=quickspin"
	v.RunsPHP = a.projectPHP(p)
	v.Xdebug = a.xdebugModule(v.RunsPHP) != ""
	if j := a.jobs.Latest(p.Slug); j != nil {
		snap := j.Snapshot()
		delete(snap, "log")
		v.Job = snap
	}
	return v
}

func (a *App) hProjects(w http.ResponseWriter, r *http.Request) {
	out := []projectView{}
	for _, p := range a.store.List() {
		out = append(out, a.view(p))
	}
	writeJSON(w, 200, out)
}

func (a *App) hCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Source != "local" && a.gitExe() == "" {
		writeErr(w, fmt.Errorf("Git is not available — install Git for Windows or let QuickSpin download MinGit (restart QuickSpin with internet access)"))
		return
	}
	p, j, err := a.createProject(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"project": a.view(p), "job": j.ID})
}

func (a *App) hUpdate(w http.ResponseWriter, r *http.Request) {
	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	j, err := a.updateProject(r.PathValue("slug"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	resp := map[string]any{"ok": true}
	if j != nil {
		resp["job"] = j.ID
	}
	writeJSON(w, 200, resp)
}

func (a *App) hDelete(w http.ResponseWriter, r *http.Request) {
	err := a.deleteProject(r.PathValue("slug"), r.URL.Query().Get("files") == "1")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) hAction(w http.ResponseWriter, r *http.Request) {
	slug, action := r.PathValue("slug"), r.PathValue("action")
	p := a.store.Get(slug)
	if p == nil {
		writeErr(w, errNotFound)
		return
	}
	var body struct {
		ToRepo       bool `json:"toRepo"`
		OriginalURLs bool `json:"originalUrls"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	var fn func(j *Job) error
	label := ""
	switch action {
	case "pull":
		label, fn = "Git pull", func(j *Job) error { return a.actionPull(j, slug) }
	case "rebuild":
		label, fn = "Rebuild (re-import database)", func(j *Job) error { return a.actionRebuild(j, slug) }
	case "export":
		label, fn = "Export database", func(j *Job) error { return a.actionExport(j, slug, body.ToRepo, body.OriginalURLs) }
	default:
		writeErr(w, fmt.Errorf("unknown action %q", action))
		return
	}
	j, err := a.jobs.Run(slug, label, fn)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"job": j.ID})
}

func (a *App) hLogin(w http.ResponseWriter, r *http.Request) {
	p := a.store.Get(r.PathValue("slug"))
	if p == nil {
		writeErr(w, errNotFound)
		return
	}
	u, err := a.loginURL(p)
	if err != nil {
		writeErr(w, err)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

func (a *App) hJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j := a.jobs.Get(id)
	if j == nil && strings.HasPrefix(id, "latest:") {
		j = a.jobs.Latest(strings.TrimPrefix(id, "latest:"))
	}
	if j == nil {
		writeErr(w, errNotFound)
		return
	}
	writeJSON(w, 200, j.Snapshot())
}

func (a *App) hServiceLog(w http.ResponseWriter, r *http.Request) {
	s := a.service(r.PathValue("name"))
	if s == nil {
		writeErr(w, errNotFound)
		return
	}
	writeJSON(w, 200, map[string]any{"lines": s.Log.Tail(300), "status": s.Status()})
}

func (a *App) hServiceRestart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "browser" {
		go a.restartBrowser()
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	s := a.service(name)
	if s == nil {
		writeErr(w, errNotFound)
		return
	}
	go s.Restart()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// hOpenFolder reveals a project folder (or QuickSpin's own) in the file manager.
func (a *App) hOpenFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug string `json:"slug"`
		What string `json:"what"` // project | exports | logs | root
	}
	json.NewDecoder(r.Body).Decode(&body)
	dir := a.Root
	switch body.What {
	case "exports":
		dir = a.data("exports")
	case "logs":
		dir = a.data("logs")
	case "project":
		p := a.store.Get(body.Slug)
		if p == nil {
			writeErr(w, errNotFound)
			return
		}
		dir = p.Path
	}
	if !isDir(dir) {
		writeErr(w, fmt.Errorf("%s does not exist", dir))
		return
	}
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("explorer.exe", filepath.Clean(dir))
	case "darwin":
		c = exec.Command("open", dir)
	default:
		c = exec.Command("xdg-open", dir)
	}
	c.Env = os.Environ()
	if err := c.Start(); err != nil {
		writeErr(w, err)
		return
	}
	go c.Wait()
	writeJSON(w, 200, map[string]any{"ok": true, "path": dir})
}
