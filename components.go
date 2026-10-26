package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Pinned fallbacks used when "latest" cannot be discovered.
const (
	fallbackMinGit     = "v2.56.0.windows.3"
	fallbackAdminer    = "6.2.0"
	fallbackMariaDB    = "11.8.9"
	fallbackXdebug     = "3.5.3"
	fallbackFrankenPHP = "1.13.3"
	fallbackMySQL      = "8.4.13"

	// Where the FrankenPHP + Xdebug builds are published (see the FrankenPHP-XDebug repository).
	defaultBuildsURL = "https://github.com/warpcoil/FrankenPHP-XDebug/releases/latest/download/"
)

// PHP versions QuickSpin offers. Append newer versions here (and to the
// builds workflow) when they are needed; each runs in its own FrankenPHP process.
var phpCatalog = []string{"8.5"}

func phpIndex(v string) int {
	for i, c := range phpCatalog {
		if c == v {
			return i
		}
	}
	return -1
}

var (
	isWindows = runtime.GOOS == "windows"
	isMac     = runtime.GOOS == "darwin"
	isLinux   = runtime.GOOS == "linux"
)

func exeName(n string) string {
	if isWindows {
		return n + ".exe"
	}
	return n
}

// platformID names this OS/CPU the way the release assets do.
func platformID() string {
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]
	switch runtime.GOOS {
	case "windows":
		return "windows-" + arch
	case "darwin":
		return "mac-" + arch
	default:
		if arch == "arm64" {
			arch = "aarch64"
		}
		return "linux-" + arch
	}
}

// component is something QuickSpin downloads into runtime/<dir>.
type component struct {
	name     string
	dir      string                                                                     // under runtime/
	optional bool                                                                       // failure is logged, not fatal
	when     func(a *App) bool                                                          // nil = always
	install  func(ctx context.Context, a *App, dest string) (version string, err error) // dest is a fresh temp dir
}

func (a *App) components() []component {
	return []component{
		{name: "database", dir: "mariadb", optional: !isWindows, when: func(a *App) bool { return a.needDatabaseDownload() }, install: installDatabase},
		{name: "chromium", dir: "chromium", optional: platformID() == "linux-aarch64", when: func(a *App) bool { return !a.cfg.Browser.Disabled && a.cfg.Paths["chromium"] == "" }, install: installChromium},
		{name: "vscode", dir: "vscode", optional: true, when: func(a *App) bool { return !a.cfg.Editor.Disabled && a.cfg.Paths["code"] == "" }, install: installVSCode},
		{name: "git", dir: "git", optional: true, when: func(a *App) bool { return isWindows && a.cfg.Paths["git"] == "" && a.systemGit() == "" }, install: installMinGit},
		{name: "wp-cli", dir: "wp-cli", install: installWPCLI},
		{name: "adminer", dir: "adminer", optional: true, install: installAdminer},
		{name: "cacert", dir: "cacert", optional: true, install: installCACert},
	}
}

func markerPath(dir string) string { return filepath.Join(dir, ".quickspin-version") }

func installedVersion(dir string) string {
	b, err := os.ReadFile(markerPath(dir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// EnsureRuntime downloads every missing component in parallel, plus FrankenPHP
// for the default PHP version. With force, everything is re-downloaded.
func (a *App) EnsureRuntime(ctx context.Context, force bool) error {
	var todo []component
	for _, c := range a.components() {
		if c.when != nil && !c.when(a) {
			continue
		}
		if !force && installedVersion(a.runtimeP(c.dir)) != "" {
			continue
		}
		todo = append(todo, c)
	}
	a.removeRetiredPHP()
	phpTodo := []string{}
	if force {
		phpTodo = a.installedPHPVersions()
	}
	if !a.phpInstalled(a.cfg.DefaultPHP) && !contains(phpTodo, a.cfg.DefaultPHP) {
		phpTodo = append(phpTodo, a.cfg.DefaultPHP)
	}
	if len(todo) == 0 && len(phpTodo) == 0 {
		a.upgradeOfficialBuilds(ctx)
		a.pickDefaultPHP()
		return nil
	}
	if len(a.installedPHPVersions()) == 0 {
		a.logf("Preparing the runtime for %s — the first start downloads roughly 400 MB…", platformID())
	}
	var wg sync.WaitGroup
	errs := make([]error, len(todo))
	for i, c := range todo {
		wg.Add(1)
		go func(i int, c component) {
			defer wg.Done()
			errs[i] = a.installComponent(ctx, c)
		}(i, c)
	}
	phpErrs := make([]error, len(phpTodo))
	for i, v := range phpTodo {
		wg.Add(1)
		go func(i int, v string) {
			defer wg.Done()
			phpErrs[i] = a.installPHPForce(ctx, v, force)
		}(i, v)
	}
	wg.Wait()
	var fatalErrs []string
	for i, err := range errs {
		if err == nil {
			continue
		}
		if todo[i].optional {
			a.logf("  [%s] skipped: %v", todo[i].name, err)
			continue
		}
		fatalErrs = append(fatalErrs, fmt.Sprintf("%s: %v", todo[i].name, err))
	}
	for i, err := range phpErrs {
		if err != nil {
			a.logf("  [php-%s] %v", phpTodo[i], err)
		}
	}
	a.upgradeOfficialBuilds(ctx)
	if !a.pickDefaultPHP() {
		fatalErrs = append(fatalErrs, fmt.Sprintf("PHP: no FrankenPHP build could be installed (wanted PHP %s)", a.cfg.DefaultPHP))
	}
	if len(fatalErrs) > 0 {
		return fmt.Errorf("could not prepare the runtime:\n  %s", strings.Join(fatalErrs, "\n  "))
	}
	a.logf("Runtime ready.")
	return nil
}

// pickDefaultPHP sets the PHP version actually used as default: the configured
// one, or the closest installed one when it is unavailable.
func (a *App) pickDefaultPHP() bool {
	if a.phpInstalled(a.cfg.DefaultPHP) {
		a.defaultPHP = a.cfg.DefaultPHP
		return true
	}
	inst := a.installedPHPVersions()
	if len(inst) == 0 {
		return false
	}
	a.defaultPHP = inst[len(inst)-1]
	a.logf("PHP %s is not available yet; using PHP %s as the default.", a.cfg.DefaultPHP, a.defaultPHP)
	return true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (a *App) installComponent(ctx context.Context, c component) error {
	final := a.runtimeP(c.dir)
	tmp := final + ".new"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	a.logf("  [%s] installing…", c.name)
	ver, err := c.install(ctx, a, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.WriteFile(markerPath(tmp), []byte(ver+"\n"), 0o644); err != nil {
		return err
	}
	// Keep the previous copy until the new one is in place.
	old := final + ".old"
	os.RemoveAll(old)
	if exists(final) {
		if err := os.Rename(final, old); err != nil {
			return fmt.Errorf("cannot replace %s (is it in use?): %w", final, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	os.RemoveAll(old)
	a.logf("  [%s] %s ready", c.name, ver)
	return nil
}

// fetchExtract downloads url and unpacks it (zip or tar.gz, detected from the content) into dest.
func (a *App) fetchExtract(ctx context.Context, url, dest, label string, strip bool) error {
	file := a.runtimeP(".downloads", label+".download")
	if err := a.downloadFile(ctx, url, file, label); err != nil {
		return err
	}
	defer os.Remove(file)
	return a.extractAny(file, dest, label, strip)
}

func (a *App) extractAny(file, dest, label string, strip bool) error {
	a.logf("  [%s] extracting…", label)
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	magic := make([]byte, 4)
	io.ReadFull(f, magic)
	f.Close()
	switch {
	case bytes.HasPrefix(magic, []byte("PK")):
		return extractZip(file, dest, strip)
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		return extractTarGz(file, dest, strip)
	}
	return fmt.Errorf("%s: unknown archive format", label)
}

// ===================== PHP (FrankenPHP) =====================

type buildManifest struct {
	Format     string `json:"format"`
	FrankenPHP string `json:"frankenphp"`
	Assets     []struct {
		File       string `json:"file"`
		PHP        string `json:"php"`
		Platform   string `json:"platform"`
		PHPVersion string `json:"phpVersion"`
		Xdebug     string `json:"xdebug"`
		SHA256     string `json:"sha256"`
	} `json:"assets"`
}

func (a *App) buildsURL() string {
	u := a.cfg.FrankenPHPBuilds
	if u == "" {
		u = defaultBuildsURL
	}
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}

// installPHPFromBuilds installs FrankenPHP + Xdebug for a PHP minor from the builds repository.
func (a *App) installPHPFromBuilds(ctx context.Context, minor, dest string) (string, error) {
	body, err := httpGetString(ctx, a.buildsURL()+"manifest.json")
	if err != nil {
		return "", fmt.Errorf("builds manifest: %v", err)
	}
	var m buildManifest
	if err := json.Unmarshal([]byte(body), &m); err != nil || m.Format != "quickspin-frankenphp" {
		return "", fmt.Errorf("builds manifest is not valid")
	}
	for _, as := range m.Assets {
		if as.PHP != minor || as.Platform != platformID() {
			continue
		}
		file := a.runtimeP(".downloads", "php-"+minor+".download")
		if err := a.downloadFile(ctx, a.buildsURL()+as.File, file, "php-"+minor); err != nil {
			return "", err
		}
		defer os.Remove(file)
		if as.SHA256 != "" {
			if sum, err := sha256File(file); err != nil || !strings.EqualFold(sum, as.SHA256) {
				return "", fmt.Errorf("%s: checksum mismatch", as.File)
			}
		}
		if err := a.extractAny(file, dest, "php-"+minor, false); err != nil {
			return "", err
		}
		return fmt.Sprintf("FrankenPHP %s · PHP %s · Xdebug %s", m.FrankenPHP, as.PHPVersion, as.Xdebug), nil
	}
	return "", fmt.Errorf("no build of PHP %s for %s in %s", minor, platformID(), a.buildsURL())
}

// installPHPOfficial installs the official FrankenPHP release (one PHP version, no
// Xdebug; on Windows Xdebug is added from xdebug.org). It returns the PHP minor it contains.
func (a *App) installPHPOfficial(ctx context.Context, dest string) (minor, ver string, err error) {
	tag := "v" + fallbackFrankenPHP
	if t, err := latestGitHubTag(ctx, "php/frankenphp"); err == nil {
		tag = t
	}
	base := "https://github.com/php/frankenphp/releases/download/" + tag + "/"
	switch p := platformID(); {
	case isWindows:
		if err := a.fetchExtract(ctx, base+"frankenphp-windows-x86_64.zip", dest, "frankenphp", false); err != nil {
			return "", "", err
		}
	default:
		asset := "frankenphp-" + p
		if isLinux {
			asset += "-gnu" // the musl build cannot load Xdebug or other modules
		}
		if err := a.downloadFile(ctx, base+asset, filepath.Join(dest, "frankenphp"), "frankenphp"); err != nil {
			return "", "", err
		}
		os.Chmod(filepath.Join(dest, "frankenphp"), 0o755)
	}
	out, err := exec.Command(filepath.Join(dest, exeName("frankenphp")), "php-cli", "-r", `echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;`).Output()
	if err != nil {
		return "", "", fmt.Errorf("downloaded FrankenPHP does not run: %v", err)
	}
	minor = strings.TrimSpace(string(out))
	ver = fmt.Sprintf("FrankenPHP %s · PHP %s (official build", strings.TrimPrefix(tag, "v"), minor)
	if isWindows {
		if xurl, xver, err := resolveXdebugTS(ctx, minor); err == nil && a.downloadFile(ctx, xurl, filepath.Join(dest, "ext", "php_xdebug.dll"), "xdebug") == nil {
			return minor, ver + ", Xdebug " + xver + ")", nil
		}
	}
	return minor, ver + ", no Xdebug)", nil
}

var phpInstallMu sync.Mutex

// installPHP makes a PHP version available, downloading it if needed.
func (a *App) installPHP(ctx context.Context, v string) error {
	return a.installPHPForce(ctx, v, false)
}

func (a *App) installPHPForce(ctx context.Context, v string, force bool) error {
	phpInstallMu.Lock()
	defer phpInstallMu.Unlock()
	if phpIndex(v) < 0 {
		return fmt.Errorf("unsupported PHP version %q (available: %s)", v, strings.Join(phpCatalog, ", "))
	}
	if !force && a.phpInstalled(v) {
		return nil
	}
	if a.cfg.Paths["php-"+v] != "" {
		return fmt.Errorf("paths.php-%s in config.json does not contain a frankenphp binary", v)
	}
	err := a.installComponent(ctx, component{name: "php-" + v, dir: filepath.Join("php", v), install: func(ctx context.Context, a *App, dest string) (string, error) {
		return a.installPHPFromBuilds(ctx, v, dest)
	}})
	if err == nil {
		return nil
	}
	// Known from an earlier start: the official build is another PHP version.
	if b, rerr := os.ReadFile(a.runtimeP("php", ".official-minor")); rerr == nil && !force {
		if m := strings.TrimSpace(string(b)); m != v && a.phpInstalled(m) {
			return fmt.Errorf("PHP %s is not published for %s yet (%v)", v, platformID(), err)
		}
	}
	a.logf("  [php-%s] %v — trying the official FrankenPHP build", v, err)
	// The official build carries a single PHP version; install it under that version.
	tmp := a.runtimeP("php", ".official")
	os.RemoveAll(tmp)
	os.MkdirAll(tmp, 0o755)
	minor, ver, oerr := a.installPHPOfficial(ctx, tmp)
	if oerr != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("PHP %s: %v; official build: %v", v, err, oerr)
	}
	os.WriteFile(a.runtimeP("php", ".official-minor"), []byte(minor+"\n"), 0o644)
	if phpIndex(minor) >= 0 && (force || !a.phpInstalled(minor)) {
		os.WriteFile(markerPath(tmp), []byte(ver+"\n"), 0o644)
		final := a.runtimeP("php", minor)
		os.RemoveAll(final)
		if rerr := os.Rename(tmp, final); rerr == nil {
			a.logf("  [php-%s] %s ready", minor, ver)
		}
	}
	os.RemoveAll(tmp)
	if minor != v {
		return fmt.Errorf("PHP %s is not published for %s yet (the official FrankenPHP build is PHP %s)", v, platformID(), minor)
	}
	return nil
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resolveXdebugTS finds xdebug.org's thread-safe Windows DLL for a PHP minor.
func resolveXdebugTS(ctx context.Context, minor string) (url, version string, err error) {
	re := regexp.MustCompile(`php_xdebug-(\d+\.\d+\.\d+)-` + regexp.QuoteMeta(minor) + `-(?:ts-)?vs1[67]-x86_64\.dll`)
	for _, page := range []string{"https://xdebug.org/download", "https://xdebug.org/download/historical"} {
		body, err := httpGetString(ctx, page)
		if err != nil {
			continue
		}
		best, bestFile := "", ""
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if best == "" || versionLess(best, m[1]) {
				best, bestFile = m[1], m[0]
			}
		}
		if best != "" {
			return "https://xdebug.org/files/" + bestFile, best, nil
		}
	}
	return "", "", fmt.Errorf("no thread-safe Xdebug DLL found for PHP %s", minor)
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func (a *App) phpDir(v string) string {
	if p := a.cfg.Paths["php-"+v]; p != "" {
		if !isDir(p) {
			return filepath.Dir(p)
		}
		return p
	}
	return a.runtimeP("php", v)
}

// frankenphpExe is the FrankenPHP binary serving a PHP version.
func (a *App) frankenphpExe(v string) string {
	if p := a.cfg.Paths["php-"+v]; p != "" && !isDir(p) && exists(p) {
		return p
	}
	if c := filepath.Join(a.phpDir(v), exeName("frankenphp")); exists(c) {
		return c
	}
	return ""
}

// xdebugModule is the Xdebug shared module that matches a PHP version, if any.
func (a *App) xdebugModule(v string) string {
	dir := a.phpDir(v)
	for _, c := range []string{filepath.Join(dir, "xdebug.so"), filepath.Join(dir, "ext", "php_xdebug.dll")} {
		if exists(c) {
			return c
		}
	}
	return ""
}

func (a *App) phpInstalled(v string) bool { return phpIndex(v) >= 0 && a.frankenphpExe(v) != "" }

func (a *App) installedPHPVersions() []string {
	var out []string
	for _, v := range phpCatalog {
		if a.phpInstalled(v) {
			out = append(out, v)
		}
	}
	return out
}

func (a *App) neededPHPVersions() []string {
	set := map[string]bool{a.defaultPHP: true}
	for _, p := range a.store.List() {
		if v := a.projectPHP(p); v != "" {
			set[v] = true
		}
	}
	var out []string
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ===================== database =====================

// The database server: MariaDB on Windows and Linux, MySQL 8.4 LTS on macOS
// (MariaDB publishes no portable macOS build). On Linux an installed
// MariaDB/MySQL is preferred because the generic tarballs need system libraries.
func (a *App) needDatabaseDownload() bool {
	if a.cfg.Paths["mariadb"] != "" || a.cfg.Paths["mysql"] != "" {
		return false
	}
	if isLinux && a.systemDBServer() != "" {
		return false
	}
	return true
}

func (a *App) systemDBServer() string {
	for _, n := range []string{"mariadbd", "mysqld"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
		for _, d := range []string{"/usr/sbin", "/usr/local/sbin", "/usr/local/bin", "/opt/homebrew/bin", "/opt/homebrew/opt/mariadb/bin", "/opt/homebrew/opt/mysql/bin"} {
			if c := filepath.Join(d, n); exists(c) {
				return c
			}
		}
	}
	return ""
}

func installDatabase(ctx context.Context, a *App, dest string) (string, error) {
	if isMac {
		return installMySQLMac(ctx, a, dest)
	}
	return installMariaDB(ctx, a, dest)
}

func installMariaDB(ctx context.Context, a *App, dest string) (string, error) {
	series := a.cfg.MariaDBSeries
	want := map[string]string{
		"windows-x86_64": `^mariadb-[\d.]+-winx64\.zip$`,
		"linux-x86_64":   `^mariadb-[\d.]+-linux-systemd-x86_64\.tar\.gz$`,
		"linux-aarch64":  `^mariadb-[\d.]+-linux-systemd-aarch64\.tar\.gz$`,
	}[platformID()]
	if want == "" {
		return "", fmt.Errorf("no MariaDB download for %s", platformID())
	}
	re := regexp.MustCompile(want)
	url, ver := "", ""
	if body, err := httpGetString(ctx, "https://downloads.mariadb.org/rest-api/mariadb/"+series+"/latest/"); err == nil {
		var r struct {
			Releases map[string]struct {
				Files []struct {
					FileName string `json:"file_name"`
					URL      string `json:"file_download_url"`
				} `json:"files"`
			} `json:"releases"`
		}
		if json.Unmarshal([]byte(body), &r) == nil {
			for v, rel := range r.Releases {
				for _, f := range rel.Files {
					if re.MatchString(f.FileName) {
						url, ver = strings.Replace(f.URL, "http://", "https://", 1), v
					}
				}
			}
		}
	}
	if url == "" {
		if !isWindows {
			return "", fmt.Errorf("MariaDB %s download not found — install MariaDB with your package manager", series)
		}
		ver = fallbackMariaDB
		url = fmt.Sprintf("https://archive.mariadb.org/mariadb-%s/winx64-packages/mariadb-%s-winx64.zip", ver, ver)
	}
	return "MariaDB " + ver, a.fetchExtract(ctx, url, dest, "mariadb", true)
}

var mysqlMacRe = regexp.MustCompile(`mysql-(8\.4\.\d+)-macos(\d+)-(arm64)\.tar\.gz`)

func installMySQLMac(ctx context.Context, a *App, dest string) (string, error) {
	arch := strings.TrimPrefix(platformID(), "mac-")
	// Find the newest 8.4 LTS tarball. File names carry the macOS release they target.
	var url, ver string
	if body, err := httpGetString(ctx, "https://dev.mysql.com/downloads/mysql/8.4.html"); err == nil {
		best := ""
		for _, m := range mysqlMacRe.FindAllStringSubmatch(body, -1) {
			if m[3] == arch && (best == "" || versionLess(best, m[1])) {
				best, url = m[1], "https://cdn.mysql.com/Downloads/MySQL-8.4/"+m[0]
			}
		}
		ver = best
	}
	if url == "" {
		// Probe the CDN: newest patch first, newest macOS target first.
		base, _ := strconv.Atoi(strings.TrimPrefix(fallbackMySQL, "8.4."))
		for p := base + 4; p >= base-2 && url == ""; p-- {
			for osv := 16; osv >= 13; osv-- {
				u := fmt.Sprintf("https://cdn.mysql.com/Downloads/MySQL-8.4/mysql-8.4.%d-macos%d-%s.tar.gz", p, osv, arch)
				if httpOK(ctx, u) {
					url, ver = u, fmt.Sprintf("8.4.%d", p)
					break
				}
			}
		}
	}
	if url == "" {
		return "", fmt.Errorf("MySQL 8.4 for macOS not found — install MariaDB or MySQL with Homebrew and restart QuickSpin")
	}
	return "MySQL " + ver, a.fetchExtract(ctx, url, dest, "mysql", true)
}

// dbTool returns a database binary by its MariaDB name, falling back to the MySQL name.
func (a *App) dbTool(mariadbName, mysqlName string) string {
	var cands []string
	for _, n := range []string{mariadbName, mysqlName} {
		cands = append(cands, a.runtimeP("mariadb", "bin", exeName(n)), a.runtimeP("mariadb", "scripts", exeName(n)))
	}
	key := "mariadb"
	if a.cfg.Paths["mysql"] != "" {
		key = "mysql"
	}
	return a.lookTool(key, cands, mariadbName, mysqlName)
}

// mariadbTool keeps the older call sites working: names are tried in order.
func (a *App) mariadbTool(names ...string) string {
	if len(names) == 1 {
		return a.dbTool(names[0], names[0])
	}
	return a.dbTool(names[0], names[1])
}

// ===================== browser, editor, git, small tools =====================

type chromiumBuild struct{ snapDir, snapZip, cftPlatform string }

func chromiumForPlatform() (chromiumBuild, bool) {
	b, ok := map[string]chromiumBuild{
		"windows-x86_64": {"Win_x64", "chrome-win.zip", "win64"},
		"linux-x86_64":   {"Linux_x64", "chrome-linux.zip", "linux64"},
		"mac-arm64":      {"Mac_Arm", "chrome-mac.zip", "mac-arm64"},
	}[platformID()]
	return b, ok
}

func installChromium(ctx context.Context, a *App, dest string) (string, error) {
	b, ok := chromiumForPlatform()
	if !ok {
		return "", fmt.Errorf("no Chromium download for %s — install Chromium with your package manager", platformID())
	}
	if a.cfg.Browser.Source != "chrome-for-testing" {
		snap := "https://storage.googleapis.com/chromium-browser-snapshots/" + b.snapDir + "/"
		if rev, err := httpGetString(ctx, snap+"LAST_CHANGE"); err == nil {
			rev = strings.TrimSpace(rev)
			url := snap + rev + "/" + b.snapZip
			if httpOK(ctx, url) {
				if err := a.fetchExtract(ctx, url, dest, "chromium", true); err == nil {
					return "Chromium r" + rev, nil
				} else {
					a.logf("  [chromium] snapshot failed (%v), using Chrome for Testing", err)
					os.RemoveAll(dest)
					os.MkdirAll(dest, 0o755)
				}
			}
		}
	}
	body, err := httpGetString(ctx, "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json")
	if err != nil {
		return "", err
	}
	var r struct {
		Channels map[string]struct {
			Version   string `json:"version"`
			Downloads map[string][]struct {
				Platform string `json:"platform"`
				URL      string `json:"url"`
			} `json:"downloads"`
		} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return "", err
	}
	st := r.Channels["Stable"]
	for _, d := range st.Downloads["chrome"] {
		if d.Platform == b.cftPlatform {
			return "Chrome for Testing " + st.Version, a.fetchExtract(ctx, d.URL, dest, "chromium", true)
		}
	}
	return "", fmt.Errorf("no %s Chromium download found", b.cftPlatform)
}

func installVSCode(ctx context.Context, a *App, dest string) (string, error) {
	target := map[string]string{
		"windows-x86_64": "cli-win32-x64", "linux-x86_64": "cli-alpine-x64", "linux-aarch64": "cli-alpine-arm64",
		"mac-arm64": "cli-darwin-arm64",
	}[platformID()]
	if target == "" {
		return "", fmt.Errorf("no VS Code CLI for %s", platformID())
	}
	if err := a.fetchExtract(ctx, "https://update.code.visualstudio.com/latest/"+target+"/stable", dest, "vscode", false); err != nil {
		return "", err
	}
	if !exists(filepath.Join(dest, exeName("code"))) {
		return "", fmt.Errorf("code binary missing from VS Code CLI download")
	}
	return "VS Code CLI (stable)", nil
}

func installMinGit(ctx context.Context, a *App, dest string) (string, error) {
	tag := fallbackMinGit
	if t, err := latestGitHubTag(ctx, "git-for-windows/git"); err == nil {
		tag = t
	}
	asset := minGitVersion(tag)
	url := fmt.Sprintf("https://github.com/git-for-windows/git/releases/download/%s/MinGit-%s-64-bit.zip", tag, asset)
	return "MinGit " + asset, a.fetchExtract(ctx, url, dest, "git", false)
}

func installWPCLI(ctx context.Context, a *App, dest string) (string, error) {
	err := a.downloadFile(ctx, "https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar", filepath.Join(dest, "wp-cli.phar"), "wp-cli")
	return "WP-CLI (latest)", err
}

func installAdminer(ctx context.Context, a *App, dest string) (string, error) {
	v := fallbackAdminer
	if tag, err := latestGitHubTag(ctx, "vrana/adminer"); err == nil {
		v = strings.TrimPrefix(tag, "v")
	}
	url := fmt.Sprintf("https://github.com/vrana/adminer/releases/download/v%s/adminer-%s-mysql.php", v, v)
	return "Adminer " + v, a.downloadFile(ctx, url, filepath.Join(dest, "adminer.php"), "adminer")
}

func installCACert(ctx context.Context, a *App, dest string) (string, error) {
	err := a.downloadFile(ctx, "https://curl.se/ca/cacert.pem", filepath.Join(dest, "cacert.pem"), "cacert")
	return "Mozilla CA bundle", err
}

// ---- tool lookup ----

// lookTool finds an executable: config override, then runtime dir, then PATH.
func (a *App) lookTool(key string, runtimeCandidates []string, pathNames ...string) string {
	if p := a.cfg.Paths[key]; p != "" {
		if isDir(p) {
			for _, n := range pathNames {
				for _, c := range []string{filepath.Join(p, exeName(n)), filepath.Join(p, "bin", exeName(n))} {
					if exists(c) {
						return c
					}
				}
			}
		} else if exists(p) {
			return p
		}
	}
	for _, c := range runtimeCandidates {
		if exists(c) {
			return c
		}
	}
	for _, n := range pathNames {
		if n == "" {
			continue
		}
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
		for _, d := range []string{"/usr/sbin", "/usr/local/sbin", "/opt/homebrew/bin", "/usr/local/bin", "/opt/homebrew/opt/mariadb/bin", "/opt/homebrew/opt/mysql/bin"} {
			if c := filepath.Join(d, n); !isWindows && exists(c) {
				return c
			}
		}
	}
	return ""
}

func (a *App) systemGit() string {
	if !a.cfg.Git.PreferSystem {
		return ""
	}
	if p, err := exec.LookPath("git"); err == nil {
		return p
	}
	return ""
}

func (a *App) gitExe() string {
	if p := a.cfg.Paths["git"]; p != "" && exists(p) {
		return p
	}
	if p := a.systemGit(); p != "" {
		return p
	}
	if p := a.runtimeP("git", "cmd", "git.exe"); exists(p) {
		return p
	}
	if p, err := exec.LookPath("git"); err == nil {
		return p
	}
	return ""
}

func (a *App) codeExe() string {
	return a.lookTool("code", []string{a.runtimeP("vscode", exeName("code"))})
}

func (a *App) chromiumExe() string {
	return a.lookTool("chromium", []string{
		a.runtimeP("chromium", "chrome.exe"),
		a.runtimeP("chromium", "chrome"),
		a.runtimeP("chromium", "Chromium.app", "Contents", "MacOS", "Chromium"),
		a.runtimeP("chromium", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
	}, "chromium", "chromium-browser", "google-chrome")
}

func (a *App) wpCLI() string { return a.runtimeP("wp-cli", "wp-cli.phar") }

// minGitVersion: v2.56.0.windows.3 -> 2.56.0.3 ; v2.47.1.windows.1 -> 2.47.1
func minGitVersion(tag string) string {
	base, n, _ := strings.Cut(strings.TrimPrefix(tag, "v"), ".windows.")
	if n != "" && n != "1" {
		return base + "." + n
	}
	return base
}

// upgradeOfficialBuilds replaces PHP versions installed from the official
// FrankenPHP release (no Xdebug) once the builds repository publishes them.
func (a *App) upgradeOfficialBuilds(ctx context.Context) {
	var official []string
	for _, v := range a.installedPHPVersions() {
		if a.cfg.Paths["php-"+v] == "" && strings.Contains(installedVersion(a.runtimeP("php", v)), "(official build") {
			official = append(official, v)
		}
	}
	if len(official) == 0 {
		return
	}
	if _, err := httpGetString(ctx, a.buildsURL()+"manifest.json"); err != nil {
		return // not published (yet) or offline: keep what works
	}
	for _, v := range official {
		err := a.installComponent(ctx, component{name: "php-" + v, dir: filepath.Join("php", v), install: func(ctx context.Context, a *App, dest string) (string, error) {
			return a.installPHPFromBuilds(ctx, v, dest)
		}})
		if err != nil {
			a.logf("  [php-%s] keeping the official build: %v", v, err)
		}
	}
}

// removeRetiredPHP deletes downloaded PHP versions QuickSpin no longer offers.
func (a *App) removeRetiredPHP() {
	entries, _ := os.ReadDir(a.runtimeP("php"))
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && phpIndex(e.Name()) < 0 {
			if os.RemoveAll(a.runtimeP("php", e.Name())) == nil {
				a.logf("Removed PHP %s (no longer offered)", e.Name())
			}
		}
	}
}
