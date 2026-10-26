package main

// Configuration export / import.
//
// A bundle is a zip file:
//
//	manifest.json             settings + project definitions
//	databases/<slug>.sql.gz   optional: each project's current database
//
// Machine-specific things (tool paths, cloned files, browser profile) are not
// included: git projects are re-cloned on import, local-folder projects are
// re-attached when the same folder exists on the new machine.

import (
	"archive/zip"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const bundleFormat = "quickspin-config"

type bundleProject struct {
	Slug      string      `json:"slug"`
	Name      string      `json:"name"`
	Domains   []string    `json:"domains"`
	Source    string      `json:"source"`
	RepoURL   string      `json:"repoUrl,omitempty"`
	Branch    string      `json:"branch,omitempty"`
	LocalPath string      `json:"localPath,omitempty"`
	WebRoot   string      `json:"webRoot"`
	DumpPath  string      `json:"dumpPath"`
	PHP       string      `json:"php"`
	Debug     bool        `json:"debug"`
	OrigURL   string      `json:"originalUrl,omitempty"`
	Remote    *RemoteSpec `json:"remote,omitempty"`   // live site (no passwords)
	Database  string      `json:"database,omitempty"` // path inside the zip
}

type bundleManifest struct {
	Format     string          `json:"format"`
	Version    int             `json:"version"`
	App        string          `json:"app"`
	ExportedAt time.Time       `json:"exportedAt"`
	Settings   json.RawMessage `json:"settings,omitempty"`
	Projects   []bundleProject `json:"projects"`
}

// stripCredentials removes passwords/tokens from https clone URLs.
func stripCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, has := u.User.Password(); has {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// exportableSettings is config.json without machine-specific tool paths.
func (a *App) exportableSettings() json.RawMessage {
	b, _ := json.Marshal(a.cfg)
	var m map[string]any
	json.Unmarshal(b, &m)
	delete(m, "paths")
	out, _ := json.Marshal(m)
	return out
}

// dumpDatabase writes a gzip-compressed mariadb-dump of db to w.
func (a *App) dumpDatabase(db string, w io.Writer) error {
	dumpExe := a.mariadbTool("mariadb-dump", "mysqldump")
	if dumpExe == "" {
		return fmt.Errorf("mariadb-dump not found")
	}
	gz := gzip.NewWriter(w)
	c := exec.Command(dumpExe, "--no-defaults", "-h", "127.0.0.1", "-P", strconv.Itoa(a.cfg.Ports.MariaDB), "-u", dbUser,
		"--single-transaction", "--default-character-set=utf8mb4", "--add-drop-table", "--skip-dump-date", db)
	c.Env = append(os.Environ(), "MYSQL_PWD="+dbPass)
	var stderr strings.Builder
	c.Stdout, c.Stderr = gz, &stderr
	hideCmd(c)
	if err := c.Run(); err != nil {
		return fmt.Errorf("dumping %s: %v %s", db, err, strings.TrimSpace(stderr.String()))
	}
	return gz.Close()
}

// hExport streams a bundle. ?db=1 includes databases, ?projects=a,b limits the projects.
func (a *App) hExport(w http.ResponseWriter, r *http.Request) {
	withDB := r.URL.Query().Get("db") == "1"
	only := map[string]bool{}
	for _, s := range strings.Split(r.URL.Query().Get("projects"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			only[s] = true
		}
	}
	m := bundleManifest{Format: bundleFormat, Version: 1, App: "QuickSpin " + appVersion, ExportedAt: time.Now().UTC()}
	if r.URL.Query().Get("settings") != "0" {
		m.Settings = a.exportableSettings()
	}
	var included []*Project
	for _, p := range a.store.List() {
		if len(only) > 0 && !only[p.Slug] {
			continue
		}
		bp := bundleProject{
			Slug: p.Slug, Name: p.Name, Domains: p.Domains, Source: p.Source,
			RepoURL: stripCredentials(p.RepoURL), Branch: p.Branch,
			WebRoot: p.WebRoot, DumpPath: p.DumpPath, PHP: p.PHP, Debug: p.Debug, OrigURL: p.OldURL,
			Remote: p.Remote,
		}
		if p.Source == "local" {
			bp.LocalPath = p.Path
		}
		if withDB && p.Status == "ready" {
			bp.Database = "databases/" + p.Slug + ".sql.gz"
		}
		m.Projects = append(m.Projects, bp)
		included = append(included, p)
	}

	name := "quickspin-config-" + time.Now().Format("20060102-1504") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	zw := zip.NewWriter(w)
	mw, _ := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate, Modified: time.Now()})
	enc := json.NewEncoder(mw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return
	}
	for i, bp := range m.Projects {
		if bp.Database == "" {
			continue
		}
		// Already gzip-compressed: store, don't deflate twice.
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: bp.Database, Method: zip.Store, Modified: time.Now()})
		if err != nil {
			return
		}
		if err := a.dumpDatabase(included[i].DBName, fw); err != nil {
			a.logf("export: %v", err)
			return // aborting leaves a truncated zip, which the importer rejects
		}
	}
	zw.Close()
	a.logf("Configuration exported (%d projects, databases: %v)", len(m.Projects), withDB)
}

// ---------- import ----------

type pendingImport struct {
	path     string
	manifest bundleManifest
	created  time.Time
}

var (
	importsMu sync.Mutex
	imports   = map[string]*pendingImport{}
)

func readBundle(path string) (bundleManifest, error) {
	var m bundleManifest
	zr, err := zip.OpenReader(path)
	if err != nil {
		return m, fmt.Errorf("not a QuickSpin export (expected a .zip file): %v", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return m, err
		}
		defer rc.Close()
		if err := json.NewDecoder(io.LimitReader(rc, 32<<20)).Decode(&m); err != nil {
			return m, fmt.Errorf("manifest.json: %v", err)
		}
		if m.Format != bundleFormat {
			return m, fmt.Errorf("this zip is not a QuickSpin configuration export")
		}
		if m.Version > 1 {
			return m, fmt.Errorf("this export was made by a newer QuickSpin — update QuickSpin first")
		}
		return m, nil
	}
	return m, fmt.Errorf("manifest.json missing — this zip is not a QuickSpin configuration export")
}

type importItem struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Domains     []string `json:"domains"`
	Source      string   `json:"source"`
	Location    string   `json:"location"`
	HasDatabase bool     `json:"hasDatabase"`
	Status      string   `json:"status"` // new | exists | conflict | missing
	Note        string   `json:"note,omitempty"`
}

func (a *App) classify(bp bundleProject) importItem {
	it := importItem{Slug: bp.Slug, Name: bp.Name, Domains: bp.Domains, Source: bp.Source, HasDatabase: bp.Database != "", Status: "new"}
	it.Location = bp.RepoURL
	if bp.Source == "local" {
		it.Location = bp.LocalPath
	}
	if bp.Source == "ssh" && bp.Remote != nil {
		it.Location = "ssh " + bp.Remote.label()
	}
	for _, p := range a.store.List() {
		same := (bp.Source == "git" && p.RepoURL != "" && stripCredentials(p.RepoURL) == bp.RepoURL && p.Branch == bp.Branch) ||
			(bp.Source == "local" && p.Path == bp.LocalPath) ||
			(bp.Source == "ssh" && bp.Remote != nil && p.Remote != nil && p.Remote.Host == bp.Remote.Host && p.Remote.Path == bp.Remote.Path)
		if same {
			it.Status, it.Note = "exists", "already set up here as "+p.Name
			return it
		}
	}
	for _, d := range bp.Domains {
		if owner := a.store.DomainOwner(d, ""); owner != "" {
			it.Status, it.Note = "conflict", d+" is used by "+owner
			return it
		}
	}
	if bp.Source == "local" && !isDir(bp.LocalPath) {
		it.Status, it.Note = "missing", "folder not found on this computer"
	}
	return it
}

// hImportUpload receives a bundle (raw request body) and returns what it contains.
func (a *App) hImportUpload(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 8)
	rand.Read(buf)
	id := hex.EncodeToString(buf)
	path := a.data("tmp", "import-"+id+".zip")
	f, err := os.Create(path)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, err = io.Copy(f, r.Body)
	f.Close()
	if err != nil {
		os.Remove(path)
		writeErr(w, fmt.Errorf("upload failed: %v", err))
		return
	}
	m, err := readBundle(path)
	if err != nil {
		os.Remove(path)
		writeErr(w, err)
		return
	}
	importsMu.Lock()
	for k, pi := range imports { // forget abandoned uploads
		if time.Since(pi.created) > time.Hour {
			os.Remove(pi.path)
			delete(imports, k)
		}
	}
	imports[id] = &pendingImport{path: path, manifest: m, created: time.Now()}
	importsMu.Unlock()

	items := []importItem{}
	for _, bp := range m.Projects {
		items = append(items, a.classify(bp))
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "app": m.App, "exportedAt": m.ExportedAt,
		"hasSettings": len(m.Settings) > 0, "projects": items,
	})
}

type importApply struct {
	Settings  bool     `json:"settings"`
	Databases bool     `json:"databases"`
	Projects  []string `json:"projects"`
}

var safeSlugRe = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

func (a *App) hImportApply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	importsMu.Lock()
	pi := imports[id]
	delete(imports, id)
	importsMu.Unlock()
	if pi == nil {
		writeErr(w, fmt.Errorf("this import has expired — choose the file again"))
		return
	}
	defer os.Remove(pi.path)
	var req importApply
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}

	restart := false
	if req.Settings && len(pi.manifest.Settings) > 0 {
		before, _ := json.Marshal(a.cfg)
		paths := a.cfg.Paths
		if err := json.Unmarshal(pi.manifest.Settings, a.cfg); err != nil {
			writeErr(w, fmt.Errorf("settings: %v", err))
			return
		}
		a.cfg.Paths = paths // machine-specific, never imported
		if phpIndex(a.cfg.DefaultPHP) < 0 {
			a.cfg.DefaultPHP = defaultConfig().DefaultPHP
		}
		if err := a.cfg.Save(); err != nil {
			writeErr(w, err)
			return
		}
		after, _ := json.Marshal(a.cfg)
		restart = string(before) != string(after)
	}

	want := map[string]bool{}
	for _, s := range req.Projects {
		want[s] = true
	}
	zr, err := zip.OpenReader(pi.path)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer zr.Close()
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}

	type result struct {
		Slug    string `json:"slug"`
		Name    string `json:"name"`
		NewSlug string `json:"newSlug,omitempty"`
		Job     string `json:"job,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	results := []result{}
	for _, bp := range pi.manifest.Projects {
		if !want[bp.Slug] || !safeSlugRe.MatchString(bp.Slug) {
			continue
		}
		res := result{Slug: bp.Slug, Name: bp.Name}
		cr := CreateRequest{
			Name: bp.Name, Domains: strings.Join(bp.Domains, ", "), Source: bp.Source,
			RepoURL: bp.RepoURL, Branch: bp.Branch, LocalPath: bp.LocalPath,
			WebRoot: bp.WebRoot, DumpPath: bp.DumpPath, PHP: bp.PHP, Debug: bp.Debug, OrigURL: bp.OrigURL,
			Remote: bp.Remote,
		}
		if phpIndex(cr.PHP) < 0 {
			cr.PHP = "" // exported with a PHP version QuickSpin no longer offers: use the default
		}
		if cr.WebRoot == "" {
			cr.WebRoot = "." // "" means auto-detect when creating; the export meant the repo root
		}
		if cr.DumpPath == "" {
			cr.DumpPath = "-"
		}
		if cr.Source == "git" && a.gitExe() == "" {
			res.Error = "Git is not available"
			results = append(results, res)
			continue
		}
		if req.Databases && bp.Database != "" {
			path, err := extractDatabase(files[bp.Database], a.data("tmp"))
			if err != nil {
				res.Error = err.Error()
				results = append(results, res)
				continue
			}
			cr.DumpFile = path
		}
		p, j, err := a.createProject(cr)
		if err != nil {
			if cr.DumpFile != "" {
				os.Remove(cr.DumpFile)
			}
			res.Error = err.Error()
		} else {
			res.NewSlug, res.Job = p.Slug, j.ID
		}
		results = append(results, res)
	}
	a.logf("Configuration imported: %d project(s), settings: %v", len(results), req.Settings)
	writeJSON(w, 200, map[string]any{"restartRequired": restart, "results": results})
}

// extractDatabase copies a database dump out of the bundle into dir.
func extractDatabase(f *zip.File, dir string) (string, error) {
	if f == nil {
		return "", fmt.Errorf("database missing from the export file")
	}
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	out, err := os.CreateTemp(dir, "import-db-*.sql.gz")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, rc)
	out.Close()
	if err != nil {
		os.Remove(out.Name())
		return "", fmt.Errorf("reading %s from the export: %v", filepath.Base(f.Name), err)
	}
	return out.Name(), nil
}
