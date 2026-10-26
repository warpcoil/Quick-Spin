package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// prependPHP runs before every PHP request and WP-CLI call (auto_prepend_file).
// It injects the database credentials of the site being served — so a
// wp-config.php committed to the repository works untouched — and handles the
// dashboard's one-click admin login.
func prependPHP() string {
	return `<?php
// QuickSpin — generated on every start, do not edit.
if (!defined('QUICKSPIN')) {
    define('QUICKSPIN', '` + appVersion + `');
    (function () {
        $slug = isset($_SERVER['QUICKSPIN_SITE']) && $_SERVER['QUICKSPIN_SITE'] !== '' ? $_SERVER['QUICKSPIN_SITE'] : getenv('QUICKSPIN_SITE');
        if (!$slug) {
            return;
        }
        $sites = @include __DIR__ . '/sites.php';
        if (!is_array($sites) || !isset($sites[$slug])) {
            return;
        }
        $constants = $sites[$slug];
        foreach ($constants as $name => $value) {
            if (!defined($name)) {
                define($name, $value);
            }
        }
        // A committed wp-config.php defines these again; keep ours, quietly.
        $ours = implode('|', array_map(function ($n) { return preg_quote($n, '/'); }, array_keys($constants)));
        set_error_handler(function ($no, $msg) use ($ours) {
            return (bool) preg_match('/^Constant (' . $ours . ') already defined/', $msg);
        }, E_WARNING | E_NOTICE);

        // One-click admin login from the dashboard (single use, short-lived token).
        if (PHP_SAPI !== 'cli' && isset($_GET['quickspin_login'])) {
            $token = (string) $_GET['quickspin_login'];
            $file = __DIR__ . '/run/login-' . $slug . '.json';
            $GLOBALS['wp_filter']['init'][1]['quickspin_login'] = array('accepted_args' => 0, 'function' => function () use ($token, $file) {
                $data = is_file($file) ? json_decode((string) file_get_contents($file), true) : null;
                @unlink($file);
                if (!is_array($data) || !hash_equals((string) $data['token'], $token) || time() > (int) $data['expires']) {
                    wp_die('This QuickSpin login link has expired. Click "WP Admin" on the dashboard again.');
                }
                $users = get_users(array('role' => 'administrator', 'number' => 1, 'orderby' => 'ID', 'order' => 'ASC'));
                if (!$users && is_multisite()) {
                    $supers = get_super_admins();
                    $u = $supers ? get_user_by('login', $supers[0]) : null;
                    $users = $u ? array($u) : array();
                }
                if (!$users) {
                    wp_die('No administrator account was found in this database.');
                }
                wp_set_current_user($users[0]->ID);
                wp_set_auth_cookie($users[0]->ID, true);
                wp_safe_redirect(admin_url());
                exit;
            });
        }
    })();
}
`
}

// writeSupportFiles renders the PHP prepend, the per-site constants and the Adminer wrapper.
func (a *App) writeSupportFiles() error {
	dir := a.data("php")
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "quickspin-prepend.php"), []byte(prependPHP())); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "sites.php"), []byte(a.sitesPHP())); err != nil {
		return err
	}
	if adminer := a.runtimeP("adminer", "adminer.php"); exists(adminer) {
		wrapper := fmt.Sprintf(`<?php
// QuickSpin: Adminer pre-configured for the local MariaDB.
if (!isset($_GET['username']) && $_SERVER['REQUEST_METHOD'] === 'GET') {
    $_POST['auth'] = array('driver' => 'server', 'server' => '127.0.0.1:%d', 'username' => '%s', 'password' => '%s', 'db' => isset($_GET['db']) ? $_GET['db'] : '');
}
function adminer_object() {
    $base = class_exists('Adminer\\Adminer') ? 'Adminer\\Adminer' : 'Adminer';
    eval('class QuickSpinAdminer extends ' . $base . ' {
        function name() { return "QuickSpin DB"; }
        function credentials() { return array("127.0.0.1:%d", "%s", "%s"); }
        function login($login, $password) { return true; }
        function verifyLoginToken() { return false; }
    }');
    return new QuickSpinAdminer;
}
include %s;
`, a.cfg.Ports.MariaDB, dbUser, dbPass, a.cfg.Ports.MariaDB, dbUser, dbPass, phpString(filepath.ToSlash(adminer)))
		if err := os.MkdirAll(filepath.Join(dir, "adminer"), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(dir, "adminer", "index.php"), []byte(wrapper)); err != nil {
			return err
		}
	}
	return nil
}

func phpString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`) + "'"
}

func phpValue(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	default:
		return phpString(fmt.Sprint(x))
	}
}

func (a *App) siteConstants(p *Project) map[string]any {
	c := map[string]any{
		"DB_NAME":                    p.DBName,
		"DB_USER":                    dbUser,
		"DB_PASSWORD":                dbPass,
		"DB_HOST":                    fmt.Sprintf("127.0.0.1:%d", a.cfg.Ports.MariaDB),
		"WP_ENVIRONMENT_TYPE":        "local",
		"WP_DEBUG":                   p.Debug,
		"WP_DEBUG_DISPLAY":           p.Debug,
		"FS_METHOD":                  "direct",
		"AUTOMATIC_UPDATER_DISABLED": true,
		"WP_AUTO_UPDATE_CORE":        false,
		// Production settings that break a plain-HTTP local copy.
		"FORCE_SSL_ADMIN": false,
		"WP_CACHE":        false,
		"COOKIE_DOMAIN":   false,
	}
	if p.Debug {
		c["WP_DEBUG_LOG"] = filepath.ToSlash(a.data("logs", "wp-"+p.Slug+"-debug.log"))
	} else {
		c["WP_DEBUG_LOG"] = false
	}
	if p.Multisite && len(p.Domains) > 0 {
		c["DOMAIN_CURRENT_SITE"] = p.Domains[0]
	} else if home, site := a.siteURLs(p); home != "" {
		c["WP_HOME"], c["WP_SITEURL"] = home, site
	}
	return c
}

// siteURLs gives home/siteurl on the primary domain, keeping any sub-path.
func (a *App) siteURLs(p *Project) (string, string) {
	if p.Status != "ready" || len(p.Domains) == 0 {
		return "", ""
	}
	return p.URL() + p.HomePath, p.URL() + p.SitePath
}

func (a *App) sitesPHP() string {
	var b strings.Builder
	b.WriteString("<?php\n// QuickSpin — per-site constants, generated.\nreturn array(\n")
	for _, p := range a.store.List() {
		if p.DBName == "" {
			continue
		}
		consts := a.siteConstants(p)
		keys := make([]string, 0, len(consts))
		for k := range consts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "  %s => array(\n", phpString(p.Slug))
		for _, k := range keys {
			fmt.Fprintf(&b, "    %s => %s,\n", phpString(k), phpValue(consts[k]))
		}
		b.WriteString("  ),\n")
	}
	b.WriteString(");\n")
	return b.String()
}

// ===================== detection =====================

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "uploads": true, ".idea": true, ".vscode": true, "cache": true}

// walkLimited visits files/dirs under root up to maxDepth, skipping heavy folders.
func walkLimited(root string, maxDepth int, fn func(rel string, d fs.DirEntry)) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		depth := strings.Count(filepath.ToSlash(rel), "/") + 1
		if d.IsDir() && (skipDirs[d.Name()] || depth > maxDepth) {
			return filepath.SkipDir
		}
		fn(filepath.ToSlash(rel), d)
		if d.IsDir() && (d.Name() == "wp-content" || d.Name() == "wp-includes" || d.Name() == "wp-admin") {
			return filepath.SkipDir
		}
		return nil
	})
}

// detectWebRoot finds the folder WordPress should be served from.
func detectWebRoot(repo string) (string, string) {
	type cand struct {
		rel   string
		score int
		depth int
	}
	var cands []cand
	add := func(rel string, score int) {
		d := 0
		if rel != "" {
			d = strings.Count(rel, "/") + 1
		}
		cands = append(cands, cand{rel, score, d})
	}
	check := func(rel string) {
		dir := filepath.Join(repo, filepath.FromSlash(rel))
		switch {
		case exists(filepath.Join(dir, "wp-config.php")) && exists(filepath.Join(dir, "wp-settings.php")):
			add(rel, 4)
		case exists(filepath.Join(dir, "wp-settings.php")):
			add(rel, 3)
		case exists(filepath.Join(dir, "wp-config.php")):
			add(rel, 2)
		case isDir(filepath.Join(dir, "wp-content")):
			add(rel, 1)
		}
	}
	check("")
	walkLimited(repo, 3, func(rel string, d fs.DirEntry) {
		if d.IsDir() {
			check(rel)
		}
	})
	if len(cands) == 0 {
		return "", "no WordPress files found in the repository — serving its root and downloading WordPress core"
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].depth != cands[j].depth {
			return cands[i].depth < cands[j].depth
		}
		return cands[i].score > cands[j].score
	})
	best := cands[0]
	where := best.rel
	if where == "" {
		where = "(repository root)"
	}
	return best.rel, "web root detected: " + where
}

func isDumpName(n string) bool {
	n = strings.ToLower(n)
	return strings.HasSuffix(n, ".sql") || strings.HasSuffix(n, ".sql.gz") || strings.HasSuffix(n, ".sql.zip")
}

// detectDump picks the largest SQL dump in the repository.
func detectDump(repo string) (string, string) {
	best, bestSize := "", int64(-1)
	walkLimited(repo, 5, func(rel string, d fs.DirEntry) {
		if d.IsDir() || !isDumpName(d.Name()) {
			return
		}
		if info, err := d.Info(); err == nil && info.Size() > bestSize {
			best, bestSize = rel, info.Size()
		}
	})
	if best == "" {
		return "", "no SQL dump found — a fresh WordPress install will be created"
	}
	return best, fmt.Sprintf("database dump detected: %s (%s)", best, humanBytes(bestSize))
}

// repoDefaults reads optional .quickspin.json from the repository root so a
// team can commit the layout once: {"webRoot": "public", "dumpPath": "db/site.sql.gz", "domains": ["acme.test"], "php": "8.2"}
type repoDefaults struct {
	Name     string   `json:"name"`
	Domains  []string `json:"domains"`
	WebRoot  *string  `json:"webRoot"`
	DumpPath *string  `json:"dumpPath"`
	PHP      string   `json:"php"`
}

func readRepoDefaults(repo string) *repoDefaults {
	b, err := os.ReadFile(filepath.Join(repo, ".quickspin.json"))
	if err != nil {
		return nil
	}
	var d repoDefaults
	if json.Unmarshal(b, &d) != nil {
		return nil
	}
	return &d
}

// coreDir is where wp-settings.php lives (the web root or one level below it).
func coreDir(docroot string) string {
	if exists(filepath.Join(docroot, "wp-settings.php")) {
		return docroot
	}
	entries, _ := os.ReadDir(docroot)
	for _, e := range entries {
		if e.IsDir() && exists(filepath.Join(docroot, e.Name(), "wp-settings.php")) {
			return filepath.Join(docroot, e.Name())
		}
	}
	return ""
}

// findWPConfig follows WordPress' own lookup: ABSPATH or its parent.
func findWPConfig(core string) string {
	if c := filepath.Join(core, "wp-config.php"); exists(c) {
		return c
	}
	parent := filepath.Dir(core)
	if c := filepath.Join(parent, "wp-config.php"); exists(c) && !exists(filepath.Join(parent, "wp-settings.php")) {
		return c
	}
	return ""
}

var tablePrefixRe = regexp.MustCompile(`\$table_prefix\s*=\s*['"]([A-Za-z0-9_]+)['"]`)

// ===================== git =====================

func (a *App) git(j *Job, dir string, args ...string) error {
	g := a.gitExe()
	if g == "" {
		return fmt.Errorf("Git is not available")
	}
	c := exec.Command(g, append([]string{"-c", "core.longpaths=true"}, args...)...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	c.Stdout, c.Stderr = j, j
	hideCmd(c)
	j.Logf("$ git %s", strings.Join(redactArgs(args), " "))
	return c.Run()
}

// redactArgs hides tokens embedded in clone URLs from the logs.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, s := range args {
		if u, err := url.Parse(s); err == nil && u.User != nil {
			if _, has := u.User.Password(); has {
				u.User = url.UserPassword(u.User.Username(), "***")
				s = u.String()
			}
		}
		out[i] = s
	}
	return out
}

// excludeFromGit adds paths (relative to the repo root) to .git/info/exclude so
// files QuickSpin generates never show up as changes.
func excludeFromGit(repo string, paths ...string) {
	gitDir := filepath.Join(repo, ".git")
	if !isDir(gitDir) {
		return
	}
	f := filepath.Join(gitDir, "info", "exclude")
	os.MkdirAll(filepath.Dir(f), 0o755)
	cur, _ := os.ReadFile(f)
	var add []string
	for _, p := range paths {
		line := "/" + strings.TrimPrefix(filepath.ToSlash(p), "/")
		if !bytes.Contains(cur, []byte("\n"+line+"\n")) && !bytes.HasPrefix(cur, []byte(line+"\n")) {
			add = append(add, line)
		}
	}
	if len(add) == 0 {
		return
	}
	if len(cur) > 0 && !bytes.HasSuffix(cur, []byte("\n")) {
		cur = append(cur, '\n')
	}
	cur = append(cur, []byte("# added by QuickSpin\n"+strings.Join(add, "\n")+"\n")...)
	os.WriteFile(f, cur, 0o644)
}

// ===================== WP-CLI =====================

// wp runs WP-CLI for a project and returns stdout.
func (a *App) wp(p *Project, j *Job, quiet bool, args ...string) (string, error) {
	core := coreDir(p.DocRoot())
	if core == "" {
		core = p.DocRoot()
	}
	full := []string{a.wpCLI(), "--path=" + core, "--no-color"}
	if os.Geteuid() == 0 { // containers / CI on Linux
		full = append(full, "--allow-root")
	}
	full = append(full, args...)
	c, err := a.phpCLI(a.projectPHP(p), full...)
	if err != nil {
		return "", err
	}
	c.Env = append(c.Env, "QUICKSPIN_SITE="+p.Slug)
	c.Dir = core
	var out bytes.Buffer
	if quiet {
		c.Stdout = &out
	} else {
		c.Stdout = io.MultiWriter(&out, j)
	}
	var errBuf bytes.Buffer
	c.Stderr = io.MultiWriter(&errBuf, j)
	j.Logf("$ wp %s", strings.Join(args, " "))
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if i := strings.LastIndex(msg, "Error:"); i >= 0 {
			msg = msg[i:]
		}
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return out.String(), fmt.Errorf("wp %s failed: %v %s", args[0], err, msg)
	}
	return out.String(), nil
}

// ===================== database =====================

func dbNameFor(slug string) string {
	return "qs_" + strings.ReplaceAll(slug, "-", "_")
}

func (a *App) recreateDB(name string) error {
	_, err := a.sqlQuery("", fmt.Sprintf("DROP DATABASE IF EXISTS `%s`; CREATE DATABASE `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;", name, name))
	return err
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n.Add(int64(n))
	return n, err
}

var mysql8Collation = regexp.MustCompile(`utf8mb4_0900_[a-z_]+`)

// importDump streams a .sql / .sql.gz / .sql.zip file into the project database.
func (a *App) importDump(p *Project, j *Job, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()
	cr := &countingReader{r: f}
	var src io.Reader = cr
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".gz"):
		gz, err := gzip.NewReader(cr)
		if err != nil {
			return fmt.Errorf("%s is not a valid gzip file: %w", filepath.Base(path), err)
		}
		defer gz.Close()
		src = gz
	case strings.HasSuffix(lower, ".zip"):
		zr, err := zip.OpenReader(path)
		if err != nil {
			return err
		}
		defer zr.Close()
		var entry *zip.File
		for _, zf := range zr.File {
			if strings.HasSuffix(strings.ToLower(zf.Name), ".sql") {
				entry = zf
				break
			}
		}
		if entry == nil {
			return fmt.Errorf("no .sql file inside %s", filepath.Base(path))
		}
		rc, err := entry.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		cr = &countingReader{r: rc}
		src = cr
		st = entry.FileInfo()
	}

	c, err := a.mysqlCmd(p.DBName, "--max-allowed-packet=1G", "--binary-mode")
	if err != nil {
		return err
	}
	stdin, err := c.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	c.Stderr = io.MultiWriter(&stderr, j)
	if err := c.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { // progress
		for {
			select {
			case <-done:
				return
			case <-time.After(2 * time.Second):
				if st != nil && st.Size() > 0 {
					pct := int(cr.n.Load() * 100 / st.Size())
					j.mu.Lock()
					j.Progress = 20 + pct*55/100
					j.mu.Unlock()
				}
			}
		}
	}()
	// Stream, translating MySQL 8 collations MariaDB doesn't know.
	br := bufio.NewReaderSize(src, 1<<20)
	bw := bufio.NewWriterSize(stdin, 1<<20)
	var copyErr error
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// very long line: pass through in pieces (collation names never span 1 MB)
			if _, werr := bw.Write(mysql8Collation.ReplaceAll(line, []byte("utf8mb4_unicode_ci"))); werr != nil {
				copyErr = werr
				break
			}
			continue
		}
		if len(line) > 0 {
			if bytes.Contains(line, []byte("_0900_")) {
				line = mysql8Collation.ReplaceAll(line, []byte("utf8mb4_unicode_ci"))
			}
			if _, werr := bw.Write(line); werr != nil {
				copyErr = werr
				break
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			copyErr = err
			break
		}
	}
	bw.Flush()
	stdin.Close()
	werr := c.Wait()
	close(done)
	if werr != nil {
		return fmt.Errorf("database import failed: %s", strings.TrimSpace(lastLines(stderr.String(), 3)))
	}
	if copyErr != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(path), copyErr)
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// detectPrefix finds the WordPress table prefix in the imported database.
func (a *App) detectPrefix(p *Project, hint string) (string, error) {
	rows, err := a.sqlQuery("", fmt.Sprintf("SELECT table_name FROM information_schema.tables WHERE table_schema='%s'", p.DBName))
	if err != nil {
		return "", err
	}
	tables := map[string]bool{}
	for _, r := range rows {
		tables[r[0]] = true
	}
	if hint != "" && tables[hint+"options"] {
		return hint, nil
	}
	var found []string
	for t := range tables {
		if strings.HasSuffix(t, "options") {
			pre := strings.TrimSuffix(t, "options")
			if tables[pre+"users"] && tables[pre+"posts"] {
				found = append(found, pre)
			}
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("no WordPress tables (…options, …users, …posts) found in the imported database")
	}
	sort.Slice(found, func(i, j int) bool { return len(found[i]) < len(found[j]) }) // multisite: wp_ before wp_2_
	return found[0], nil
}

func (a *App) optionValue(p *Project, name string) string {
	rows, err := a.sqlQuery(p.DBName, fmt.Sprintf("SELECT option_value FROM `%soptions` WHERE option_name='%s' LIMIT 1", p.Prefix, name))
	if err != nil || len(rows) == 0 || len(rows[0]) == 0 {
		return ""
	}
	return rows[0][0]
}

func originOf(raw string) (origin, host, path string) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", ""
	}
	return u.Scheme + "://" + u.Host, u.Host, strings.TrimRight(u.Path, "/")
}

// replaceHost rewrites every URL pointing at fromHost (http/https, protocol-
// relative and JSON-escaped forms, www and bare) to newOrigin — serialized
// data is handled by WP-CLI.
func (a *App) replaceHost(p *Project, j *Job, fromHost, newOrigin string) error {
	newHost := strings.TrimPrefix(strings.TrimPrefix(newOrigin, "https://"), "http://")
	if fromHost == "" || strings.EqualFold(fromHost, newHost) {
		return nil
	}
	bare := strings.TrimPrefix(strings.ToLower(fromHost), "www.")
	hostRe := `(?:www\.)?` + regexp.QuoteMeta(bare)
	end := `(?![\w-]|\.[\w-])`
	scheme := strings.SplitN(newOrigin, ":", 2)[0]
	common := []string{"--regex", "--regex-delimiter=~", "--regex-flags=i", "--all-tables-with-prefix", "--skip-columns=guid", "--skip-plugins", "--skip-themes", "--report-changed-only"}
	// 1) absolute URLs, plain or JSON-escaped (https:\/\/host)
	if _, err := a.wp(p, j, false, append([]string{"search-replace", `https?:(\\?/\\?/)` + hostRe + end, scheme + `:${1}` + newHost}, common...)...); err != nil {
		return err
	}
	// 2) protocol-relative URLs (//host)
	_, err := a.wp(p, j, false, append([]string{"search-replace", `(?<![\w:\\/])(\\?/\\?/)` + hostRe + end, `${1}` + newHost}, common...)...)
	return err
}

// ===================== wp-config / workspace =====================

func randomKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) generateWPConfig(p *Project, core string) error {
	var b strings.Builder
	b.WriteString("<?php\n/**\n * Generated by QuickSpin for local development (git-ignored via .git/info/exclude).\n")
	b.WriteString(" * Database credentials and URLs are injected by QuickSpin at runtime.\n */\n")
	fmt.Fprintf(&b, "defined('DB_NAME') || define('DB_NAME', %s);\n", phpString(p.DBName))
	fmt.Fprintf(&b, "defined('DB_USER') || define('DB_USER', %s);\n", phpString(dbUser))
	fmt.Fprintf(&b, "defined('DB_PASSWORD') || define('DB_PASSWORD', %s);\n", phpString(dbPass))
	fmt.Fprintf(&b, "defined('DB_HOST') || define('DB_HOST', %s);\n", phpString(fmt.Sprintf("127.0.0.1:%d", a.cfg.Ports.MariaDB)))
	b.WriteString("define('DB_CHARSET', 'utf8mb4');\ndefine('DB_COLLATE', '');\n\n")
	for _, k := range []string{"AUTH_KEY", "SECURE_AUTH_KEY", "LOGGED_IN_KEY", "NONCE_KEY", "AUTH_SALT", "SECURE_AUTH_SALT", "LOGGED_IN_SALT", "NONCE_SALT"} {
		fmt.Fprintf(&b, "define('%s', '%s');\n", k, randomKey())
	}
	fmt.Fprintf(&b, "\n$table_prefix = %s;\n\n", phpString(p.Prefix))
	b.WriteString("defined('WP_DEBUG') || define('WP_DEBUG', false);\n\n")
	b.WriteString("if (!defined('ABSPATH')) {\n\tdefine('ABSPATH', __DIR__ . '/');\n}\nrequire_once ABSPATH . 'wp-settings.php';\n")
	return os.WriteFile(filepath.Join(core, "wp-config.php"), []byte(b.String()), 0o644)
}

func (a *App) workspaceFile(p *Project) string { return a.data("workspaces", p.Slug+".code-workspace") }

func (a *App) writeWorkspace(p *Project) error {
	php := a.phpForEditor(a.projectPHP(p))
	ws := map[string]any{
		"folders": []map[string]string{{"path": p.Path, "name": p.Name}},
		"settings": map[string]any{
			"php.validate.executablePath":         php,
			"php.debug.executablePath":            php,
			"intelephense.environment.phpVersion": a.projectPHP(p) + ".0",
			"files.exclude":                       map[string]bool{"**/.git": true},
		},
		"launch": map[string]any{
			"version": "0.2.0",
			"configurations": []map[string]any{{
				"name": "Listen for Xdebug (QuickSpin)", "type": "php", "request": "launch", "port": 9003,
			}},
		},
		"extensions": map[string]any{
			"recommendations": []string{"xdebug.php-debug", "bmewburn.vscode-intelephense-client"},
		},
	}
	b, _ := json.MarshalIndent(ws, "", "  ")
	return writeFileAtomic(a.workspaceFile(p), b)
}

// loginURL creates a single-use admin login link.
func (a *App) loginURL(p *Project) (string, error) {
	tok := randomKey()
	data, _ := json.Marshal(map[string]any{"token": tok, "expires": time.Now().Add(2 * time.Minute).Unix()})
	if err := os.WriteFile(a.data("php", "run", "login-"+p.Slug+".json"), data, 0o600); err != nil {
		return "", err
	}
	return p.URL() + "/?quickspin_login=" + tok, nil
}
