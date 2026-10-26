package main

import (
	"archive/zip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPHPListingRegex(t *testing.T) {
	listing := `<a href="php-8.3.36-nts-Win32-vs16-x64.zip">x</a> <a href="php-8.3.9-nts-Win32-vs16-x64.zip">
	<a href="php-8.3.36-Win32-vs16-x64.zip"> <a href="php-debug-pack-8.3.36-nts-Win32-vs16-x64.zip">
	<a href="php-8.4.27-nts-Win32-vs17-x64.zip"> <a href="php-7.4.33-nts-Win32-vc15-x64.zip">`
	cases := map[string]string{"8.3": "8.3.36", "8.4": "8.4.27", "7.4": "7.4.33"}
	for minor, want := range cases {
		re := regexp.MustCompile(`php-(` + regexp.QuoteMeta(minor) + `\.\d+)-nts-Win32-(v[cs]\d+)-x64\.zip`)
		best := ""
		for _, m := range re.FindAllStringSubmatch(listing, -1) {
			if best == "" || versionLess(best, m[1]) {
				best = m[1]
			}
		}
		if best != want {
			t.Errorf("PHP %s: got %q want %q", minor, best, want)
		}
	}
}

func TestXdebugRegex(t *testing.T) {
	page := `php_xdebug-3.5.3-8.3-nts-vs16-x86_64.dll php_xdebug-3.5.3-8.3-vs16-x86_64.dll php_xdebug-3.4.0-8.3-nts-vs16-x86_64.dll php_xdebug-3.5.3-8.4-nts-vs17-x86_64.dll`
	re := regexp.MustCompile(`php_xdebug-(\d+\.\d+\.\d+)-8\.3-(?:nts-v[cs]\d+|v[cs]\d+-nts)-x86_64\.dll`)
	ms := re.FindAllStringSubmatch(page, -1)
	if len(ms) != 2 || ms[0][1] != "3.5.3" {
		t.Fatalf("unexpected matches %v", ms)
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess("8.3.9", "8.3.36") || versionLess("8.3.36", "8.3.9") || versionLess("1.0", "1.0") {
		t.Fatal("versionLess wrong")
	}
}

func TestNormalizeDomains(t *testing.T) {
	got, err := normalizeDomains([]string{"Acme.test, http://www.acme.test/ acme.test"})
	if err != nil || len(got) != 2 || got[0] != "acme.test" || got[1] != "www.acme.test" {
		t.Fatalf("got %v %v", got, err)
	}
	for _, bad := range []string{"acme.dev", "acme.app", "nodot", "db.quickspin.test", "bad_domain.test", ""} {
		if _, err := normalizeDomains([]string{bad}); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if _, err := normalizeDomains([]string{"shop.client.co.uk"}); err != nil {
		t.Error(err)
	}
}

func TestSlugify(t *testing.T) {
	if s := slugify("  Client Shop (2024)!! "); s != "client-shop-2024" {
		t.Fatal(s)
	}
}

func TestRedactArgs(t *testing.T) {
	out := redactArgs([]string{"clone", "https://user:ghp_secret@github.com/a/b.git"})
	if out[1] != "https://user:%2A%2A%2A@github.com/a/b.git" && out[1] != "https://user:***@github.com/a/b.git" {
		t.Fatal(out[1])
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	zw.Close()
	f.Close()
}

func TestExtractZipStripAndSlip(t *testing.T) {
	dir := t.TempDir()
	z := filepath.Join(dir, "chrome-win.zip")
	writeZip(t, z, map[string]string{"chrome-win/chrome.exe": "x", "chrome-win/locales/en-US.pak": "y"})
	dest := filepath.Join(dir, "out")
	if err := extractZip(z, dest, true); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dest, "chrome.exe")) || !exists(filepath.Join(dest, "locales", "en-US.pak")) {
		t.Fatal("strip failed")
	}
	evil := filepath.Join(dir, "evil.zip")
	writeZip(t, evil, map[string]string{"../../escape.txt": "x"})
	if err := extractZip(evil, filepath.Join(dir, "out2"), false); err == nil {
		t.Fatal("zip slip not detected")
	}
}

func TestDetectLayout(t *testing.T) {
	repo := t.TempDir()
	mk := func(p, c string) {
		full := filepath.Join(repo, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
	mk("web/wp-settings.php", "<?php")
	mk("web/wp-content/themes/x/style.css", "")
	mk("web/wp-content/uploads/backup.sql", "ignored: uploads are skipped")
	mk("db/small.sql", "x")
	mk("db/full.sql.gz", "xxxxxxxxxxxxxxxx")
	mk("node_modules/pkg/wp-settings.php", "")
	if wr, _ := detectWebRoot(repo); wr != "web" {
		t.Errorf("web root %q", wr)
	}
	if d, _ := detectDump(repo); d != "db/full.sql.gz" {
		t.Errorf("dump %q", d)
	}
	// Bedrock-style: core in web/wp, config one level up
	repo2 := t.TempDir()
	os.MkdirAll(filepath.Join(repo2, "web", "wp"), 0o755)
	os.WriteFile(filepath.Join(repo2, "web", "wp", "wp-settings.php"), nil, 0o644)
	os.WriteFile(filepath.Join(repo2, "web", "wp-config.php"), nil, 0o644)
	if wr, _ := detectWebRoot(repo2); wr != "web" {
		t.Errorf("bedrock web root %q", wr)
	}
	if c := coreDir(filepath.Join(repo2, "web")); c != filepath.Join(repo2, "web", "wp") {
		t.Errorf("core dir %q", c)
	}
	if cfg := findWPConfig(filepath.Join(repo2, "web", "wp")); cfg != filepath.Join(repo2, "web", "wp-config.php") {
		t.Errorf("config %q", cfg)
	}
}

func TestMinGitAssetName(t *testing.T) {
	for tag, want := range map[string]string{"v2.56.0.windows.3": "2.56.0.3", "v2.47.1.windows.1": "2.47.1"} {
		if got := minGitVersion(tag); got != want {
			t.Errorf("%s: %s", tag, minGitVersion(tag))
		}
	}
}

func TestStripCredentials(t *testing.T) {
	cases := map[string]string{
		"https://bob:ghp_tok@github.com/a/b.git": "https://bob@github.com/a/b.git",
		"https://github.com/a/b.git":             "https://github.com/a/b.git",
		"git@bitbucket.org:a/b.git":              "git@bitbucket.org:a/b.git",
	}
	for in, want := range cases {
		if got := stripCredentials(in); got != want {
			t.Errorf("%s -> %s", in, got)
		}
	}
}

func TestExtractZipSymlinks(t *testing.T) {
	dir := t.TempDir()
	z := filepath.Join(dir, "mac.zip")
	f, _ := os.Create(z)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("chrome-mac/Chromium.app/Contents/Frameworks/X.framework/Versions/A/X")
	w.Write([]byte("bin"))
	h := &zip.FileHeader{Name: "chrome-mac/Chromium.app/Contents/Frameworks/X.framework/Versions/Current"}
	h.SetMode(os.ModeSymlink | 0o777)
	w, _ = zw.CreateHeader(h)
	w.Write([]byte("A"))
	bad := &zip.FileHeader{Name: "chrome-mac/evil"}
	bad.SetMode(os.ModeSymlink | 0o777)
	zw.Close()
	f.Close()
	dest := filepath.Join(dir, "out")
	if err := extractZip(z, dest, true); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		target, err := os.Readlink(filepath.Join(dest, "Chromium.app/Contents/Frameworks/X.framework/Versions/Current"))
		if err != nil || target != "A" {
			t.Fatalf("symlink: %q %v", target, err)
		}
	}
	// escaping symlink is rejected
	z2 := filepath.Join(dir, "evil.zip")
	f, _ = os.Create(z2)
	zw = zip.NewWriter(f)
	w, _ = zw.CreateHeader(bad)
	w.Write([]byte("../../../etc"))
	zw.Close()
	f.Close()
	if err := extractZip(z2, filepath.Join(dir, "out2"), false); err == nil {
		t.Fatal("escaping symlink accepted")
	}
}

func TestPlatformID(t *testing.T) {
	if p := platformID(); p == "" || !strings.Contains(p, "-") {
		t.Fatal(p)
	}
}

func TestKillLeftovers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("job objects handle this on Windows")
	}
	pidDir = t.TempDir()
	defer func() { pidDir = "" }()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	writePidFile("svc", cmd)
	// A stale file pointing at an unrelated live process must be ignored.
	os.WriteFile(filepath.Join(pidDir, "other.pid"), []byte(fmt.Sprintf("%d\nfrankenphp\n", os.Getpid())), 0o644)
	killLeftovers(t.Logf)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("leftover process was not stopped")
	}
	if left, _ := filepath.Glob(filepath.Join(pidDir, "*.pid")); len(left) != 0 {
		t.Fatalf("pid files not cleaned: %v", left)
	}
}

func TestRemoteHelpers(t *testing.T) {
	for in, want := range map[string]string{"www.acme-prod.co.uk": "acme-prod", "acme.com": "acme", "shop.client.io": "client", "prod": "prod", "10.0.0.5": "live-site"} {
		if got := siteNameFromHost(in); got != want {
			t.Errorf("siteNameFromHost(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"/var/www/html": `'/var/www/html'`, "~/sites/a b": `"$HOME"/'sites/a b'`, "/it's": `'/it'\''s'`} {
		if got := rpath(in); got != want {
			t.Errorf("rpath(%q) = %s, want %s", in, got, want)
		}
	}
	cfg := parseWPConfig(`<?php
// define('DB_NAME', 'commented_out');
define( 'DB_NAME', 'acme_live' );
define("DB_USER", "acme_wp");
define( 'DB_PASSWORD', 'S3cr"et\'pw$x' );
define( 'DB_HOST', 'localhost:/var/run/mysqld/mysqld.sock' );
$table_prefix = 'acm_';`)
	if cfg["DB_NAME"] != "acme_live" || cfg["DB_USER"] != "acme_wp" || cfg["DB_PASSWORD"] != `S3cr"et'pw$x` || cfg["table_prefix"] != "acm_" {
		t.Fatalf("parseWPConfig: %#v", cfg)
	}
	opts := string(mysqlClientOptions("u", `p"w\x`, "localhost:/run/m.sock"))
	if !strings.Contains(opts, `password="p\"w\\x"`) || !strings.Contains(opts, `socket="/run/m.sock"`) {
		t.Fatalf("options: %s", opts)
	}
	if o := string(mysqlClientOptions("u", "p", "db.internal:3307")); !strings.Contains(o, "port=3307") {
		t.Fatalf("port: %s", o)
	}
	r := RemoteSpec{Host: "deploy@example.com", Path: "/var/www/html/"}
	if err := r.normalize(); err != nil || r.User != "deploy" || r.Host != "example.com" || r.Path != "/var/www/html" || r.Uploads != "copy" {
		t.Fatalf("normalize: %+v %v", r, err)
	}
	for _, bad := range []RemoteSpec{{Host: "-oProxyCommand=x", Path: "/x"}, {Host: "a b", Path: "/x"}, {Host: "ok.com"}} {
		if err := bad.normalize(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestPublishRules(t *testing.T) {
	never := []string{"wp-config.php", "wp-content/object-cache.php.quickspin-off", "wp-content/object-cache.php", "wp-content/cache/page.html", ".git/HEAD", "wp-content/updraft/backup.zip"}
	for _, p := range never {
		if !publishSkip(p) {
			t.Errorf("%s should never be published", p)
		}
	}
	for _, p := range []string{"index.php", "wp-content/themes/x/style.css", "wp-content/uploads/2026/10/a.jpg", "wp-content/cache-local.txt", "sub/wp-config.php"} {
		if publishSkip(p) {
			t.Errorf("%s should be publishable", p)
		}
	}
	if !isUpload("wp-content/uploads/a.jpg") || isUpload("wp-content/themes/a.jpg") {
		t.Error("isUpload")
	}
	re := regexp.MustCompile(strings.ReplaceAll(publishExportRegex("acme.test"), `(?![\w-]|\.[\w-])`, `\b`))
	for in, match := range map[string]bool{"http://acme.test/x": true, "//acme.test/x": true, `https:\/\/acme.test\/x`: true, "acme.test": false} {
		if re.MatchString(in) != match {
			t.Errorf("export regex on %q: want %v", in, match)
		}
	}
}
