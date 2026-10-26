package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type CreateRequest struct {
	Name      string `json:"name"`
	Domains   string `json:"domains"` // comma/space separated, first is primary
	Source    string `json:"source"`  // git | local
	RepoURL   string `json:"repoUrl"`
	Branch    string `json:"branch"`
	Shallow   bool   `json:"shallow"`
	LocalPath string `json:"localPath"`
	WebRoot   string `json:"webRoot"`  // "" = auto-detect, "." = repository root
	DumpPath  string `json:"dumpPath"` // "" = auto-detect, "-" = no dump (fresh install)
	PHP       string `json:"php"`
	Debug     bool   `json:"debug"`
	HTTPS     *bool  `json:"https"` // nil = on

	// Copy a live site over SSH (Source "ssh"). Passwords are kept in memory only.
	Remote      *RemoteSpec `json:"remote"`
	SSHPassword string      `json:"sshPassword"`
	DBPassword  string      `json:"dbPassword"`

	// Set by configuration import only.
	DumpFile string `json:"-"` // database to import instead of the repository's dump
	OrigURL  string `json:"-"` // production URL recorded on the exporting machine
}

type provisionOpts struct {
	clone    bool
	webRoot  string
	dumpPath string
	php      string
	dumpFile string // absolute path, overrides the repository dump once (import)
	origURL  string // original production URL to remember (import)
}

var repoNameRe = regexp.MustCompile(`([^/:\\]+?)(\.git)?/?$`)

func (a *App) createProject(req CreateRequest) (*Project, *Job, error) {
	req.Source = strings.ToLower(strings.TrimSpace(req.Source))
	if req.Source == "" {
		req.Source = "git"
	}
	req.RepoURL = strings.TrimSpace(req.RepoURL)
	req.LocalPath = strings.TrimSpace(req.LocalPath)
	if req.Source == "ssh" && req.Remote != nil {
		if err := req.Remote.normalize(); err != nil {
			return nil, nil, err
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		src := req.RepoURL
		if req.Source == "local" {
			src = filepath.ToSlash(req.LocalPath)
		}
		if req.Source == "ssh" && req.Remote != nil {
			src = siteNameFromHost(req.Remote.Host)
		}
		if m := repoNameRe.FindStringSubmatch(src); m != nil {
			name = m[1]
		}
	}
	slug := slugify(name)
	if slug == "" {
		return nil, nil, fmt.Errorf("please give the project a name")
	}
	base := slug
	for i := 2; a.store.Get(slug) != nil; i++ {
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	if strings.TrimSpace(req.Domains) == "" {
		req.Domains = slug + ".test"
	}
	domains, err := normalizeDomains([]string{req.Domains})
	if err != nil {
		return nil, nil, err
	}
	https := req.HTTPS == nil || *req.HTTPS
	if err := checkDomainsScheme(domains, https); err != nil {
		return nil, nil, err
	}
	php := req.PHP
	if php == "" {
		php = a.defaultPHP
	}
	if phpIndex(php) < 0 {
		return nil, nil, fmt.Errorf("unsupported PHP version %q", php)
	}
	p := &Project{
		Slug: slug, Name: name, Domains: domains, Source: req.Source, PHP: php, Debug: req.Debug, HTTPS: https,
		DBName: dbNameFor(slug), Status: "provisioning", Created: time.Now(),
	}
	switch req.Source {
	case "git":
		if req.RepoURL == "" {
			return nil, nil, fmt.Errorf("enter the repository URL (HTTPS or SSH)")
		}
		p.RepoURL, p.Branch = req.RepoURL, strings.TrimSpace(req.Branch)
		p.Path, p.Managed = a.data("projects", slug), true
	case "ssh":
		if req.Remote == nil {
			return nil, nil, fmt.Errorf("enter the live server's details")
		}
		r := *req.Remote
		if err := r.normalize(); err != nil {
			return nil, nil, err
		}
		p.Remote = &r
		p.Path, p.Managed = a.data("projects", slug), true
		p.WebRoot, p.DumpPath = liveWebRoot, liveDump
	case "local":
		abs, err := filepath.Abs(req.LocalPath)
		if err != nil || !isDir(abs) {
			return nil, nil, fmt.Errorf("folder %q does not exist", req.LocalPath)
		}
		p.Path = abs
	default:
		return nil, nil, fmt.Errorf("unknown source %q", req.Source)
	}
	if err := a.store.Add(p); err != nil {
		return nil, nil, err
	}
	opts := provisionOpts{clone: req.Source == "git", webRoot: req.WebRoot, dumpPath: req.DumpPath, dumpFile: req.DumpFile, origURL: req.OrigURL}
	if req.PHP != "" {
		opts.php = req.PHP
	}
	shallow := req.Shallow
	if req.Source == "ssh" {
		rememberSecrets(slug, remoteSecrets{SSHPassword: req.SSHPassword, DBPassword: req.DBPassword})
		j, err := a.jobs.Run(slug, "Copy live site", func(j *Job) error {
			os.MkdirAll(p.Path, 0o755)
			return a.pullLive(j, slug, true)
		})
		return p, j, err
	}
	j, err := a.jobs.Run(slug, "Create project", func(j *Job) error { return a.provision(j, slug, opts, shallow) })
	return p, j, err
}

// provision brings a project from "files somewhere" to "running WordPress site".
// It is idempotent and also used by Rebuild.
func (a *App) provision(j *Job, slug string, o provisionOpts, shallow bool) (err error) {
	a.store.Update(slug, func(p *Project) { p.Status, p.Error = "provisioning", "" })
	if o.dumpFile != "" {
		defer os.Remove(o.dumpFile)
	}
	defer func() {
		a.store.Update(slug, func(p *Project) {
			if err != nil {
				p.Status, p.Error = "error", err.Error()
			} else {
				p.Status, p.Error = "ready", ""
			}
		})
		if rerr := a.reloadWeb(); rerr != nil {
			j.Logf("warning: %v", rerr)
		}
	}()
	p := a.store.Get(slug)
	notes := []string{}

	// 1. Source code.
	if p.Source == "git" && (o.clone || !isDir(filepath.Join(p.Path, ".git"))) {
		j.SetStep("Cloning "+redactArgs([]string{p.RepoURL})[0], 5)
		if p.Managed {
			os.RemoveAll(p.Path)
		}
		args := []string{"clone"}
		if p.Branch != "" {
			args = append(args, "--branch", p.Branch)
		}
		if shallow {
			args = append(args, "--depth", "1", "--no-single-branch")
		}
		args = append(args, p.RepoURL, p.Path)
		if err := a.git(j, a.data("projects"), args...); err != nil {
			return fmt.Errorf("git clone failed (%v). For private repositories use an SSH URL with your key loaded, or sign in when Git asks", err)
		}
	}
	if !isDir(p.Path) {
		return fmt.Errorf("project folder %s is missing", p.Path)
	}

	// 2. Layout: explicit > .quickspin.json > detection.
	if p.Source == "ssh" {
		j.SetStep("Inspecting the copied site", 12)
	} else {
		j.SetStep("Inspecting the repository", 12)
	}
	defs := readRepoDefaults(p.Path)
	if defs != nil {
		notes = append(notes, "settings read from .quickspin.json")
	}
	webRoot, dump := strings.TrimSpace(o.webRoot), strings.TrimSpace(o.dumpPath)
	if webRoot == "" && !o.clone && p.WebRoot != "" {
		webRoot = p.WebRoot
	}
	if dump == "" && !o.clone && p.DumpPath != "" {
		dump = p.DumpPath
	}
	if webRoot == "" && defs != nil && defs.WebRoot != nil {
		webRoot = *defs.WebRoot
		if webRoot == "" {
			webRoot = "."
		}
	}
	if dump == "" && defs != nil && defs.DumpPath != nil {
		dump = *defs.DumpPath
		if dump == "" {
			dump = "-"
		}
	}
	if webRoot == "" {
		var note string
		webRoot, note = detectWebRoot(p.Path)
		notes = append(notes, note)
	}
	if dump == "" {
		var note string
		dump, note = detectDump(p.Path)
		notes = append(notes, note)
	}
	webRoot = strings.Trim(filepath.ToSlash(webRoot), "/")
	if webRoot == "." {
		webRoot = ""
	}
	if dump == "-" {
		dump = ""
	}
	dump = strings.TrimPrefix(filepath.ToSlash(dump), "/")
	for _, rel := range []string{webRoot, dump} {
		if strings.Contains("/"+rel+"/", "/../") {
			return fmt.Errorf("paths must stay inside the project folder: %s", rel)
		}
	}
	if dump != "" && !exists(filepath.Join(p.Path, filepath.FromSlash(dump))) {
		return fmt.Errorf("SQL dump %q not found in the repository", dump)
	}
	php := p.PHP
	if o.php == "" && defs != nil && defs.PHP != "" && phpIndex(defs.PHP) >= 0 && o.clone {
		php = defs.PHP
	}
	p, _ = a.store.Update(slug, func(p *Project) { p.WebRoot, p.DumpPath, p.PHP, p.Notes = webRoot, dump, php, notes })
	for _, n := range notes {
		j.Logf("• %s", n)
	}

	// 3. PHP version.
	if !a.phpInstalled(p.PHP) {
		j.SetStep("Installing PHP "+p.PHP, 15)
		if err := a.installPHP(context.Background(), p.PHP); err != nil {
			j.Logf("warning: %v — the site runs on PHP %s until it is available", err, a.defaultPHP)
		}
	}
	if err := a.ensurePHP(a.projectPHP(p)); err != nil {
		return err
	}

	// 4. WordPress core.
	docroot := p.DocRoot()
	if err := os.MkdirAll(docroot, 0o755); err != nil {
		return err
	}
	core := coreDir(docroot)
	downloadedCore := false
	if core == "" {
		j.SetStep("Downloading WordPress core (not in the repository)", 18)
		if _, err := a.wp(p, j, false, "core", "download", "--skip-content"); err != nil {
			return err
		}
		core, downloadedCore = docroot, true
		rel, _ := filepath.Rel(p.Path, docroot)
		rel = strings.TrimPrefix(filepath.ToSlash(rel)+"/", "./")
		if rel == "/" {
			rel = ""
		}
		var ex []string
		for _, f := range []string{"wp-admin/", "wp-includes/", "wp-*.php", "index.php", "xmlrpc.php", "license.txt", "readme.html", "wp-content/index.php", "wp-content/plugins/index.php", "wp-content/themes/index.php"} {
			ex = append(ex, rel+f)
		}
		excludeFromGit(p.Path, ex...)
	}

	// 5. Database.
	j.SetStep("Creating database "+p.DBName, 20)
	if err := a.recreateDB(p.DBName); err != nil {
		return err
	}
	hint := ""
	if cfgFile := findWPConfig(core); cfgFile != "" {
		if b, err := os.ReadFile(cfgFile); err == nil {
			if m := tablePrefixRe.FindSubmatch(b); m != nil {
				hint = string(m[1])
			}
		}
	}
	prefix := hint
	hasDump := dump != "" || o.dumpFile != ""
	if hasDump {
		src, label := filepath.Join(p.Path, filepath.FromSlash(dump)), dump
		if o.dumpFile != "" {
			src, label = o.dumpFile, "the database from the imported configuration"
		}
		j.SetStep("Importing "+label, 22)
		if err := a.importDump(p, j, src); err != nil {
			return err
		}
		if prefix, err = a.detectPrefix(p, hint); err != nil {
			return err
		}
		if hint != "" && hint != prefix {
			j.Logf("warning: wp-config.php uses table prefix %q but the dump contains %q", hint, prefix)
		}
	} else if prefix == "" {
		prefix = "wp_"
	}
	multisite := false
	if rows, _ := a.sqlQuery("", fmt.Sprintf("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='%s' AND table_name='%sblogs'", p.DBName, prefix)); len(rows) > 0 && rows[0][0] != "0" {
		multisite = true
	}
	p, _ = a.store.Update(slug, func(p *Project) { p.Prefix, p.Multisite = prefix, multisite })

	// 6. wp-config.php
	if cfgFile := findWPConfig(core); cfgFile == "" {
		j.SetStep("Writing wp-config.php", 78)
		if err := a.generateWPConfig(p, core); err != nil {
			return err
		}
		rel, _ := filepath.Rel(p.Path, filepath.Join(core, "wp-config.php"))
		excludeFromGit(p.Path, rel)
	} else {
		if p.Source == "ssh" {
			j.Logf("• using the site's %s — QuickSpin injects the local database settings and URLs", filepath.Base(cfgFile))
		} else {
			j.Logf("• using the repository's %s — QuickSpin injects the database settings, nothing is modified", filepath.Base(cfgFile))
		}
	}
	if err := a.writeSupportFiles(); err != nil {
		return err
	}

	// 7. URLs.
	newOrigin := p.URL()
	if !hasDump {
		j.SetStep("Installing a fresh WordPress site", 80)
		if _, err := a.wp(p, j, false, "core", "install", "--url="+newOrigin, "--title="+p.Name, "--admin_user=admin", "--admin_password=admin", "--admin_email=admin@"+p.Domains[0], "--skip-email"); err != nil {
			return err
		}
		a.store.Update(slug, func(p *Project) { p.OldURL, p.HomePath, p.SitePath = "", "", "" })
		j.Logf("• log in with admin / admin (or use the WP Admin button)")
		a.ensureTheme(p, j)
	} else {
		home, site := a.optionValue(p, "home"), a.optionValue(p, "siteurl")
		oldOrigin, oldHost, homePath := originOf(home)
		_, siteHost, sitePath := originOf(site)
		if oldOrigin == "" {
			j.Logf("warning: could not read the site URL from the database; URLs were not rewritten")
		} else if strings.EqualFold(oldHost, p.Domains[0]) && (siteHost == "" || strings.EqualFold(siteHost, oldHost)) {
			j.Logf("• URLs already point at %s", p.Domains[0])
		} else {
			j.SetStep(fmt.Sprintf("Rewriting URLs %s → %s", oldOrigin, newOrigin), 82)
			if err := a.replaceHost(p, j, oldHost, newOrigin); err != nil {
				return err
			}
			if siteHost != "" && !strings.EqualFold(siteHost, oldHost) {
				if err := a.replaceHost(p, j, siteHost, newOrigin); err != nil {
					return err
				}
			}
			if multisite {
				a.multisiteDomains(p, j, oldHost)
			}
		}
		if o.dumpFile != "" {
			oldOrigin = o.origURL // production URL from the other machine, not its dev domain
		}
		a.store.Update(slug, func(p *Project) { p.OldURL, p.HomePath, p.SitePath = oldOrigin, homePath, sitePath })
		if downloadedCore {
			a.wp(p, j, false, "core", "update-db", "--skip-plugins", "--skip-themes")
		}
	}
	p = a.store.Get(slug)
	if multisite {
		j.Logf("• multisite detected: *.%s is routed to this project", p.Domains[0])
	}

	// 8. Editor workspace.
	j.SetStep("Finishing up", 95)
	if err := a.writeWorkspace(p); err != nil {
		return err
	}
	a.wp(p, j, true, "cache", "flush", "--skip-plugins", "--skip-themes")
	j.mu.Lock()
	j.Result = newOrigin
	j.mu.Unlock()
	return nil
}

// multisiteDomains updates the bare domains stored in wp_blogs / wp_site.
func (a *App) multisiteDomains(p *Project, j *Job, oldHost string) {
	bare := strings.TrimPrefix(strings.ToLower(oldHost), "www.")
	newHost := p.Domains[0]
	for _, t := range []string{"blogs", "site"} {
		q := fmt.Sprintf("UPDATE `%s%s` SET domain = REPLACE(domain, '%s', '%s')", p.Prefix, t, sqlEscape(bare), sqlEscape(newHost))
		if _, err := a.sqlQuery(p.DBName, q); err != nil {
			j.Logf("warning: %v", err)
		}
	}
}

func sqlEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// ===================== other actions =====================

func (a *App) actionPull(j *Job, slug string) error {
	p := a.store.Get(slug)
	if !isDir(filepath.Join(p.Path, ".git")) {
		return fmt.Errorf("%s is not a git repository", p.Path)
	}
	j.SetStep("git pull", 10)
	return a.git(j, p.Path, "pull", "--ff-only")
}

func (a *App) actionRebuild(j *Job, slug string) error {
	return a.provision(j, slug, provisionOpts{}, false)
}

// actionExport dumps the database. With originalURLs the site's original
// domain is restored in the export, ready to commit or deploy.
func (a *App) actionExport(j *Job, slug string, toRepo, originalURLs bool) error {
	p := a.store.Get(slug)
	out := a.data("exports", fmt.Sprintf("%s-%s.sql", p.Slug, time.Now().Format("20060102-150405")))
	if originalURLs && p.OldURL != "" {
		oldO, oldHost, _ := originOf(p.OldURL)
		_ = oldO
		scheme := strings.SplitN(p.OldURL, ":", 2)[0]
		j.SetStep("Exporting with original URLs ("+p.OldURL+")", 20)
		pat := publishExportRegex(p.Domains[0]) // absolute, protocol-relative and JSON-escaped URLs
		if _, err := a.wp(p, j, false, "search-replace", pat, scheme+`:${1}`+oldHost, "--regex", "--regex-delimiter=~", "--all-tables-with-prefix", "--skip-plugins", "--skip-themes", "--report-changed-only", "--export="+out); err != nil {
			return err
		}
	} else {
		j.SetStep("Exporting database "+p.DBName, 20)
		dumpExe := a.mariadbTool("mariadb-dump", "mysqldump")
		if dumpExe == "" {
			return fmt.Errorf("mariadb-dump not found")
		}
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		c := exec.Command(dumpExe, "--no-defaults", "-h", "127.0.0.1", "-P", strconv.Itoa(a.cfg.Ports.MariaDB), "-u", dbUser,
			"--single-transaction", "--default-character-set=utf8mb4", "--add-drop-table", "--skip-dump-date", p.DBName)
		c.Env = append(os.Environ(), "MYSQL_PWD="+dbPass)
		c.Stdout, c.Stderr = f, j
		hideCmd(c)
		err = c.Run()
		f.Close()
		if err != nil {
			return fmt.Errorf("mariadb-dump failed: %v", err)
		}
	}
	result := out
	if toRepo {
		rel := p.DumpPath
		if rel == "" {
			rel = "database.sql"
		}
		dest := filepath.Join(p.Path, filepath.FromSlash(rel))
		if strings.HasSuffix(strings.ToLower(dest), ".zip") {
			return fmt.Errorf("writing back to a .zip dump is not supported — the export is in %s", out)
		}
		j.SetStep("Writing "+rel, 80)
		if err := copyMaybeGzip(out, dest); err != nil {
			return err
		}
		if p.DumpPath == "" {
			a.store.Update(slug, func(p *Project) { p.DumpPath = rel })
		}
		result = dest
	}
	j.mu.Lock()
	j.Result = result
	j.mu.Unlock()
	j.Logf("• saved %s", result)
	return nil
}

func copyMaybeGzip(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	os.MkdirAll(filepath.Dir(dest), 0o755)
	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if strings.HasSuffix(strings.ToLower(dest), ".gz") {
		gz := gzip.NewWriter(out)
		_, err = io.Copy(gz, in)
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
	} else {
		_, err = io.Copy(out, in)
	}
	out.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

type UpdateRequest struct {
	Name    *string `json:"name"`
	Domains *string `json:"domains"`
	PHP     *string `json:"php"`
	Debug   *bool   `json:"debug"`
	WebRoot *string `json:"webRoot"`
	HTTPS   *bool   `json:"https"`
}

// updateProject changes settings; a new primary domain rewrites the database URLs.
func (a *App) updateProject(slug string, req UpdateRequest) (*Job, error) {
	cur := a.store.Get(slug)
	if cur == nil {
		return nil, errNotFound
	}
	if a.jobs.Busy(slug) {
		return nil, fmt.Errorf("%s is busy — wait for the running task to finish", cur.Name)
	}
	var domains []string
	if req.Domains != nil {
		d, err := normalizeDomains([]string{*req.Domains})
		if err != nil {
			return nil, err
		}
		for _, x := range d {
			if owner := a.store.DomainOwner(x, slug); owner != "" {
				return nil, fmt.Errorf("domain %s is already used by %s", x, owner)
			}
		}
		domains = d
	}
	if req.PHP != nil && phpIndex(*req.PHP) < 0 {
		return nil, fmt.Errorf("unsupported PHP version %q", *req.PHP)
	}
	https := cur.HTTPS
	if req.HTTPS != nil {
		https = *req.HTTPS
	}
	checkDomains := domains
	if checkDomains == nil {
		checkDomains = cur.Domains
	}
	if err := checkDomainsScheme(checkDomains, https); err != nil {
		return nil, err
	}
	oldPrimary := cur.Domains[0]
	p, err := a.store.Update(slug, func(p *Project) {
		if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
			p.Name = strings.TrimSpace(*req.Name)
		}
		if domains != nil {
			p.Domains = domains
		}
		if req.PHP != nil {
			p.PHP = *req.PHP
		}
		if req.Debug != nil {
			p.Debug = *req.Debug
		}
		if req.WebRoot != nil {
			p.WebRoot = strings.Trim(filepath.ToSlash(*req.WebRoot), "/.")
		}
		p.HTTPS = https
	})
	if err != nil {
		return nil, err
	}
	needDomainRewrite := domains != nil && domains[0] != oldPrimary && cur.Status == "ready"
	needSchemeRewrite := https != cur.HTTPS && cur.Status == "ready"
	needPHP := p.PHP != "" && !a.phpInstalled(p.PHP) // "" = the default version
	if !needDomainRewrite && !needSchemeRewrite && !needPHP {
		a.writeWorkspace(p)
		return nil, a.reloadWeb()
	}
	return a.jobs.Run(slug, "Update settings", func(j *Job) (err error) {
		defer func() {
			if rerr := a.reloadWeb(); err == nil {
				err = rerr
			}
		}()
		if needPHP {
			j.SetStep("Installing PHP "+p.PHP, 10)
			if err := a.installPHP(context.Background(), p.PHP); err != nil {
				return err
			}
			if err := a.ensurePHP(p.PHP); err != nil {
				return err
			}
		}
		if needDomainRewrite {
			j.SetStep(fmt.Sprintf("Rewriting URLs %s → %s", oldPrimary, p.Domains[0]), 40)
			// Serve the new domain from the old constants while rewriting.
			if err := a.replaceHost(p, j, oldPrimary, p.URL()); err != nil {
				return err
			}
			if p.Multisite {
				a.multisiteDomains(p, j, oldPrimary)
			}
		}
		if needSchemeRewrite {
			j.SetStep("Switching the site's URLs to "+p.Scheme()+"://", 70)
			if err := a.rewriteScheme(p, j); err != nil {
				return err
			}
		}
		a.writeWorkspace(p)
		return nil
	})
}

func (a *App) deleteProject(slug string, deleteFiles bool) error {
	p := a.store.Get(slug)
	if p == nil {
		return errNotFound
	}
	if a.jobs.Busy(slug) {
		return fmt.Errorf("%s is busy — wait for the running task to finish", p.Name)
	}
	if _, err := a.sqlQuery("", fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", p.DBName)); err != nil {
		return err
	}
	if err := a.store.Delete(slug); err != nil {
		return err
	}
	os.Remove(a.workspaceFile(p))
	os.Remove(a.data("php", "run", "login-"+slug+".json"))
	var ferr error
	if deleteFiles && p.Managed && strings.HasPrefix(p.Path, a.data("projects")+string(os.PathSeparator)) {
		ferr = os.RemoveAll(p.Path)
	}
	if err := a.reloadWeb(); err != nil {
		return err
	}
	if ferr != nil {
		return fmt.Errorf("project removed, but some files could not be deleted (close the editor and try deleting %s yourself): %v", p.Path, ferr)
	}
	return nil
}

// ensureTheme activates a usable theme when the default one is not present
// (fresh installs from repositories that carry no themes).
func (a *App) ensureTheme(p *Project, j *Job) {
	out, err := a.wp(p, j, true, "option", "get", "stylesheet", "--skip-plugins", "--skip-themes")
	core := coreDir(p.DocRoot())
	if err == nil && core != "" && isDir(filepath.Join(core, "wp-content", "themes", strings.TrimSpace(out))) {
		return
	}
	list, _ := a.wp(p, j, true, "theme", "list", "--field=name", "--skip-plugins", "--skip-themes")
	if names := strings.Fields(list); len(names) > 0 {
		a.wp(p, j, false, "theme", "activate", names[0], "--skip-plugins", "--skip-themes")
		return
	}
	if _, err := a.wp(p, j, false, "theme", "install", "twentytwentyfive", "--activate", "--skip-plugins", "--skip-themes"); err != nil {
		j.Logf("warning: no theme available — add one to wp-content/themes")
	}
}

// siteNameFromHost turns "www.acme-prod.co.uk" into "acme-prod".
func siteNameFromHost(host string) string {
	if net.ParseIP(host) != nil {
		return "live-site"
	}
	h := strings.ToLower(strings.TrimPrefix(strings.ToLower(host), "www."))
	parts := strings.Split(h, ".")
	if len(parts) >= 3 && len(parts[len(parts)-2]) <= 3 { // co.uk, com.au …
		parts = parts[:len(parts)-2]
	} else if len(parts) >= 2 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return h
	}
	return parts[len(parts)-1]
}
