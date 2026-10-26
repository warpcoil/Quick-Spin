package main

// Cloning a live WordPress site over SSH.
//
// QuickSpin drives the computer's own OpenSSH client (ssh / scp), so SSH keys,
// the SSH agent, ~/.ssh/config host aliases and known_hosts all work as usual.
// Passwords are passed through SSH_ASKPASS (QuickSpin answers the prompt
// itself) and are kept in memory only — never written to disk.
//
//   1. read wp-config.php on the server for the database settings
//   2. stream the site as a tar.gz over ssh (scp fallback), skipping caches,
//      backups and optionally the uploads folder
//   3. run mysqldump on the server and stream the dump back over ssh
//   4. disable cache drop-ins, then import + rewrite URLs like any project

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RemoteSpec describes where a live site lives. Passwords are not part of it.
type RemoteSpec struct {
	Host    string `json:"host"`              // hostname, IP or an alias from ~/.ssh/config
	Port    int    `json:"port,omitempty"`    // default 22 (or what ~/.ssh/config says)
	User    string `json:"user,omitempty"`    // default: ~/.ssh/config / current user
	Path    string `json:"path"`              // WordPress folder on the server
	KeyFile string `json:"keyFile,omitempty"` // private key; empty = agent / default keys
	Uploads string `json:"uploads,omitempty"` // "copy" (default) or "live": load media from the live site
	// Database overrides, for wp-config.php files that read settings from elsewhere.
	DBName string `json:"dbName,omitempty"`
	DBUser string `json:"dbUser,omitempty"`
	DBHost string `json:"dbHost,omitempty"`
}

func (r *RemoteSpec) uploadsFromLive() bool { return r != nil && r.Uploads == "live" }

func (r *RemoteSpec) target() string {
	if r.User != "" {
		return r.User + "@" + r.Host
	}
	return r.Host
}

func (r *RemoteSpec) label() string {
	s := r.target()
	if r.Port != 0 && r.Port != 22 {
		s += ":" + strconv.Itoa(r.Port)
	}
	return s + ":" + r.Path
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9._\-]+$`)
var userRe = regexp.MustCompile(`^[A-Za-z0-9._\-]+$`)

func (r *RemoteSpec) normalize() error {
	r.Host = strings.TrimSpace(r.Host)
	r.User = strings.TrimSpace(r.User)
	r.Path = strings.TrimSpace(r.Path)
	r.KeyFile = strings.TrimSpace(r.KeyFile)
	if u, h, ok := strings.Cut(r.Host, "@"); ok && r.User == "" { // "user@host" typed into the host field
		r.User, r.Host = u, h
	}
	if !hostRe.MatchString(r.Host) || strings.HasPrefix(r.Host, "-") {
		return fmt.Errorf("enter the server's host name (e.g. example.com or an alias from your SSH config)")
	}
	if r.User != "" && (!userRe.MatchString(r.User) || strings.HasPrefix(r.User, "-")) {
		return fmt.Errorf("the SSH user name %q is not valid", r.User)
	}
	if r.Port < 0 || r.Port > 65535 {
		return fmt.Errorf("the SSH port must be between 1 and 65535")
	}
	if r.Path == "" {
		return fmt.Errorf("enter the WordPress folder on the server (e.g. /var/www/html)")
	}
	r.Path = strings.TrimRight(r.Path, "/")
	if r.Path == "" {
		r.Path = "/"
	}
	if r.KeyFile != "" {
		if strings.HasPrefix(r.KeyFile, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				r.KeyFile = filepath.Join(home, strings.TrimPrefix(r.KeyFile[1:], "/"))
			}
		}
		if !exists(r.KeyFile) {
			return fmt.Errorf("SSH key file %s not found", r.KeyFile)
		}
	}
	if r.Uploads != "live" {
		r.Uploads = "copy"
	}
	return nil
}

// remoteSecrets holds passwords for the current QuickSpin session only.
type remoteSecrets struct {
	SSHPassword string
	DBPassword  string
}

var (
	secretsMu sync.Mutex
	secrets   = map[string]remoteSecrets{}
)

func rememberSecrets(slug string, s remoteSecrets) {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	old := secrets[slug]
	if s.SSHPassword == "" {
		s.SSHPassword = old.SSHPassword
	}
	if s.DBPassword == "" {
		s.DBPassword = old.DBPassword
	}
	secrets[slug] = s
}

func recallSecrets(slug string) remoteSecrets {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	return secrets[slug]
}

// ---------- ssh plumbing ----------

func (a *App) sshTool(name string) string {
	cands := []string{a.runtimeP("git", "usr", "bin", exeName(name))}
	if isWindows {
		cands = append([]string{filepath.Join(os.Getenv("SystemRoot"), "System32", "OpenSSH", exeName(name))}, cands...)
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return a.lookTool(name, cands, name)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// rpath quotes a server path for sh, keeping a leading ~ (home folder) working.
func rpath(p string) string {
	switch {
	case p == "~":
		return `"$HOME"`
	case strings.HasPrefix(p, "~/"):
		return `"$HOME"/` + shQuote(strings.TrimPrefix(p, "~/"))
	}
	return shQuote(p)
}

// sshOptions are shared by ssh and scp.
func (a *App) sshOptions(r *RemoteSpec, password string) []string {
	opts := []string{
		"-o", "StrictHostKeyChecking=accept-new", // first connection: trust and remember the host key
		"-o", "ConnectTimeout=20",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "NumberOfPasswordPrompts=1",
		"-o", "LogLevel=ERROR",
	}
	if password == "" {
		opts = append(opts, "-o", "BatchMode=yes") // never hang on a prompt
	}
	if r.KeyFile != "" {
		opts = append(opts, "-i", r.KeyFile)
	}
	return opts
}

// sshCommand runs a POSIX shell script on the server.
func (a *App) sshCommand(ctx context.Context, r *RemoteSpec, password, script string, compress bool) (*exec.Cmd, error) {
	ssh := a.sshTool("ssh")
	if ssh == "" {
		return nil, fmt.Errorf("the ssh command was not found — install the OpenSSH client (built into Windows 10/11, macOS and most Linux systems)")
	}
	args := a.sshOptions(r, password)
	if r.Port != 0 {
		args = append(args, "-p", strconv.Itoa(r.Port))
	}
	if compress {
		args = append(args, "-C")
	}
	// The login shell may be bash, zsh, fish or csh: hand the script to sh.
	args = append(args, "-T", r.target(), "sh -c "+shQuote(script))
	c := exec.CommandContext(ctx, ssh, args...)
	a.withPassword(c, password)
	return c, nil
}

// withPassword makes ssh/scp ask QuickSpin for the password (see askpassMain).
func (a *App) withPassword(c *exec.Cmd, password string) {
	c.Env = os.Environ()
	if password == "" {
		return
	}
	self, _ := os.Executable()
	c.Env = append(c.Env, "SSH_ASKPASS="+self, "SSH_ASKPASS_REQUIRE=force", "QUICKSPIN_ASKPASS="+password)
	if os.Getenv("DISPLAY") == "" {
		c.Env = append(c.Env, "DISPLAY=:0") // older OpenSSH only uses SSH_ASKPASS when DISPLAY is set
	}
	newSession(c) // no controlling terminal, so ssh never prompts on the console
}

// askpassMain answers ssh's password prompt when QuickSpin runs as SSH_ASKPASS.
func askpassMain() bool {
	pw, ok := os.LookupEnv("QUICKSPIN_ASKPASS")
	if !ok {
		return false
	}
	fmt.Println(pw)
	return true
}

// sshError turns ssh's stderr into an actionable message.
func sshError(err error, stderr string, r *RemoteSpec) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case strings.Contains(msg, "Permission denied"):
		return fmt.Errorf("the server refused the login for %s — check the user name and password, or that your SSH key is authorised on the server", r.target())
	case strings.Contains(msg, "Host key verification failed") || strings.Contains(msg, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return fmt.Errorf("the server's host key does not match the one remembered in your known_hosts file — if the server was rebuilt, remove its old entry (ssh-keygen -R %s)", r.Host)
	case strings.Contains(msg, "Could not resolve hostname"):
		return fmt.Errorf("host %s not found", r.Host)
	case strings.Contains(msg, "Connection refused"), strings.Contains(msg, "timed out"), strings.Contains(msg, "No route to host"):
		return fmt.Errorf("cannot connect to %s: %s", r.Host, lastLines(msg, 1))
	}
	if msg == "" {
		return err
	}
	return fmt.Errorf("%v: %s", err, lastLines(msg, 3))
}

// sshRun runs a script and returns its stdout.
func (a *App) sshRun(ctx context.Context, r *RemoteSpec, password, script string, stdin []byte) (string, error) {
	c, err := a.sshCommand(ctx, r, password, script, false)
	if err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	if err := c.Run(); err != nil {
		return out.String(), sshError(err, errb.String(), r)
	}
	return out.String(), nil
}

// ---------- inspecting the server ----------

// RemoteInfo is what "Test connection" reports.
type RemoteInfo struct {
	Path        string   `json:"path"`
	WordPress   bool     `json:"wordpress"`
	ConfigFile  string   `json:"configFile,omitempty"`
	DBName      string   `json:"dbName,omitempty"`
	DBUser      string   `json:"dbUser,omitempty"`
	DBHost      string   `json:"dbHost,omitempty"`
	DBPassword  bool     `json:"dbPasswordFound"`
	TablePrefix string   `json:"tablePrefix,omitempty"`
	Tar         bool     `json:"tar"`
	Dump        string   `json:"mysqldump,omitempty"`
	SizeMB      int      `json:"sizeMb"`
	UploadsMB   int      `json:"uploadsMb"`
	Warnings    []string `json:"warnings,omitempty"`

	dbPass string
}

const probeScript = `cd %s 2>/dev/null || { echo "QS_ERR=nopath"; exit 0; }
echo "QS_PWD=$(pwd)"
if [ -f wp-config.php ]; then echo "QS_CFG=wp-config.php"; elif [ -f ../wp-config.php ]; then echo "QS_CFG=../wp-config.php"; fi
[ -f wp-settings.php ] && echo "QS_WP=1"
command -v tar >/dev/null 2>&1 && echo "QS_TAR=1"
D=$(command -v mysqldump 2>/dev/null || command -v mariadb-dump 2>/dev/null); [ -n "$D" ] && echo "QS_DUMP=$D"
echo "QS_SIZE=$(du -sk . 2>/dev/null | cut -f1)"
[ -d wp-content/uploads ] && echo "QS_UPLOADS=$(du -sk wp-content/uploads 2>/dev/null | cut -f1)"
exit 0
`

func (a *App) inspectRemote(ctx context.Context, r *RemoteSpec, sec remoteSecrets) (*RemoteInfo, error) {
	out, err := a.sshRun(ctx, r, sec.SSHPassword, fmt.Sprintf(probeScript, rpath(r.Path)), nil)
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.HasPrefix(k, "QS_") {
			kv[k] = v
		}
	}
	if kv["QS_ERR"] == "nopath" {
		return nil, fmt.Errorf("folder %s does not exist on the server (or %s cannot read it)", r.Path, r.target())
	}
	info := &RemoteInfo{Path: kv["QS_PWD"], WordPress: kv["QS_WP"] == "1", ConfigFile: kv["QS_CFG"], Tar: kv["QS_TAR"] == "1", Dump: kv["QS_DUMP"]}
	if kb, err := strconv.Atoi(kv["QS_SIZE"]); err == nil {
		info.SizeMB = (kb + 1023) / 1024
	}
	if kb, err := strconv.Atoi(kv["QS_UPLOADS"]); err == nil {
		info.UploadsMB = (kb + 1023) / 1024
	}
	if !info.WordPress {
		info.Warnings = append(info.Warnings, "no wp-settings.php in this folder — is it the WordPress folder?")
	}
	if !info.Tar {
		info.Warnings = append(info.Warnings, "tar is not available on the server; files will be copied with scp, which is slower and copies everything")
	}
	if info.Dump == "" {
		info.Warnings = append(info.Warnings, "mysqldump is not available on the server, so the database cannot be copied")
	}
	if info.ConfigFile != "" {
		cfg, err := a.sshRun(ctx, r, sec.SSHPassword, "cat "+rpath(r.Path+"/"+info.ConfigFile), nil)
		if err != nil {
			return nil, err
		}
		c := parseWPConfig(cfg)
		info.DBName, info.DBUser, info.DBHost, info.dbPass, info.TablePrefix = c["DB_NAME"], c["DB_USER"], c["DB_HOST"], c["DB_PASSWORD"], c["table_prefix"]
		_, info.DBPassword = c["DB_PASSWORD"]
		if info.DBName == "" || info.DBUser == "" {
			info.Warnings = append(info.Warnings, "the database settings could not be read from wp-config.php (they may come from environment variables) — enter them under Database")
		}
	} else {
		info.Warnings = append(info.Warnings, "wp-config.php not found — enter the database settings under Database")
	}
	// Overrides typed into the form win.
	if r.DBName != "" {
		info.DBName = r.DBName
	}
	if r.DBUser != "" {
		info.DBUser = r.DBUser
	}
	if r.DBHost != "" {
		info.DBHost = r.DBHost
	}
	if sec.DBPassword != "" {
		info.dbPass, info.DBPassword = sec.DBPassword, true
	}
	if info.DBHost == "" {
		info.DBHost = "localhost"
	}
	return info, nil
}

var (
	defineSingleRe = regexp.MustCompile(`define\s*\(\s*['"](DB_NAME|DB_USER|DB_PASSWORD|DB_HOST)['"]\s*,\s*'((?:\\.|[^'\\])*)'\s*\)`)
	defineDoubleRe = regexp.MustCompile(`define\s*\(\s*['"](DB_NAME|DB_USER|DB_PASSWORD|DB_HOST)['"]\s*,\s*"((?:\\.|[^"\\])*)"\s*\)`)
	phpCommentRe   = regexp.MustCompile(`(?m)^\s*(//|#).*$`)
)

// parseWPConfig extracts the database settings from wp-config.php source.
func parseWPConfig(src string) map[string]string {
	src = phpCommentRe.ReplaceAllString(src, "")
	out := map[string]string{}
	for _, m := range defineSingleRe.FindAllStringSubmatch(src, -1) {
		if _, seen := out[m[1]]; !seen {
			out[m[1]] = strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(m[2])
		}
	}
	for _, m := range defineDoubleRe.FindAllStringSubmatch(src, -1) {
		if _, seen := out[m[1]]; !seen {
			out[m[1]] = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, `\n`, "\n", `\t`, "\t").Replace(m[2])
		}
	}
	if m := tablePrefixRe.FindStringSubmatch(src); m != nil {
		out["table_prefix"] = m[1]
	}
	return out
}

// mysqlClientOptions renders a [client] option file (sent over stdin, never stored).
func mysqlClientOptions(user, password, host string) []byte {
	esc := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
	var b strings.Builder
	b.WriteString("[client]\n")
	fmt.Fprintf(&b, "user=%s\npassword=%s\n", esc(user), esc(password))
	h, rest, hasRest := strings.Cut(host, ":")
	switch {
	case strings.HasPrefix(host, "/"): // a bare socket path
		fmt.Fprintf(&b, "socket=%s\n", esc(host))
	case hasRest && strings.HasPrefix(rest, "/"):
		fmt.Fprintf(&b, "host=%s\nsocket=%s\n", esc(h), esc(rest))
	case hasRest:
		if _, err := strconv.Atoi(rest); err == nil {
			fmt.Fprintf(&b, "host=%s\nport=%s\nprotocol=tcp\n", esc(h), rest)
		} else {
			fmt.Fprintf(&b, "host=%s\n", esc(host))
		}
	default:
		fmt.Fprintf(&b, "host=%s\n", esc(host))
	}
	return []byte(b.String())
}

// ---------- copying ----------

var remoteExcludes = []string{
	"./.git", "./wp-content/cache", "./wp-content/et-cache", "./wp-content/litespeed",
	"./wp-content/uploads/cache", "./wp-content/updraft", "./wp-content/ai1wm-backups",
	"./wp-content/backups-dup-lite", "./wp-content/backup-db", "./wp-content/upgrade",
	"./error_log", "./wp-content/debug.log",
}

// downloadFiles copies the site into dest (which must not exist yet).
func (a *App) downloadFiles(ctx context.Context, j *Job, r *RemoteSpec, sec remoteSecrets, info *RemoteInfo, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if !info.Tar {
		return a.scpFiles(ctx, j, r, sec, dest)
	}
	var ex []string
	for _, e := range remoteExcludes {
		ex = append(ex, "--exclude="+shQuote(e))
	}
	if r.uploadsFromLive() {
		ex = append(ex, "--exclude="+shQuote("./wp-content/uploads"))
	}
	script := fmt.Sprintf("cd %s && tar -czf - %s .", rpath(r.Path), strings.Join(ex, " "))
	c, err := a.sshCommand(ctx, r, sec.SSHPassword, script, false)
	if err != nil {
		return err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Start(); err != nil {
		return err
	}
	cr := &countingReader{r: stdout}
	stop := a.reportTransfer(j, "Copying files", cr, info.SizeMB)
	exErr := extractTarStream(cr, dest)
	stop()
	if exErr != nil {
		io.Copy(io.Discard, stdout)
	}
	werr := c.Wait()
	if exErr != nil {
		if werr != nil {
			return sshError(werr, errb.String(), r)
		}
		return fmt.Errorf("unpacking the files: %w", exErr)
	}
	if werr != nil {
		// GNU tar exits 1 when files changed while being read (busy live site): keep going.
		if ee, ok := werr.(*exec.ExitError); ok && ee.ExitCode() == 1 && strings.Contains(errb.String(), "tar") {
			j.Logf("warning: some files changed on the server while they were copied")
		} else {
			return sshError(werr, errb.String(), r)
		}
	}
	j.Logf("• copied %s of files", humanBytes(cr.n.Load()))
	return nil
}

// scpFiles is the fallback for servers without tar.
func (a *App) scpFiles(ctx context.Context, j *Job, r *RemoteSpec, sec remoteSecrets, dest string) error {
	scp := a.sshTool("scp")
	if scp == "" {
		return fmt.Errorf("the server has no tar and scp was not found on this computer")
	}
	j.SetStep("Copying files with scp (the server has no tar)", -1)
	args := append(a.sshOptions(r, sec.SSHPassword), "-r", "-q")
	if r.Port != 0 {
		args = append(args, "-P", strconv.Itoa(r.Port))
	}
	src := strings.TrimPrefix(strings.TrimPrefix(r.Path, "~"), "/")
	if !strings.HasPrefix(r.Path, "~") {
		src = r.Path
	}
	args = append(args, r.target()+":"+src+"/.", dest)
	c := exec.CommandContext(ctx, scp, args...)
	a.withPassword(c, sec.SSHPassword)
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return sshError(err, errb.String(), r)
	}
	for _, e := range remoteExcludes {
		os.RemoveAll(filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(e, "./"))))
	}
	if r.uploadsFromLive() {
		os.RemoveAll(filepath.Join(dest, "wp-content", "uploads"))
	}
	return nil
}

const dumpScript = `umask 077
f=$(mktemp 2>/dev/null || echo "/tmp/qs-my-$$.cnf")
trap 'rm -f "$f"' EXIT
cat > "$f"
D=$(command -v mysqldump 2>/dev/null || command -v mariadb-dump 2>/dev/null)
[ -n "$D" ] || { echo "mysqldump was not found on the server" >&2; exit 127; }
o=""
"$D" --help 2>/dev/null | grep -q column-statistics && o="--column-statistics=0"
"$D" --defaults-extra-file="$f" --single-transaction --quick --no-tablespaces --default-character-set=utf8mb4 --skip-dump-date $o %s
`

// downloadDatabase runs mysqldump on the server and writes a .sql.gz to dest.
func (a *App) downloadDatabase(ctx context.Context, j *Job, r *RemoteSpec, sec remoteSecrets, info *RemoteInfo, dest string) error {
	if info.DBName == "" || info.DBUser == "" {
		return fmt.Errorf("the database name and user are unknown — enter them under Database")
	}
	if info.Dump == "" {
		return fmt.Errorf("mysqldump is not installed on the server")
	}
	c, err := a.sshCommand(ctx, r, sec.SSHPassword, fmt.Sprintf(dumpScript, shQuote(info.DBName)), true)
	if err != nil {
		return err
	}
	c.Stdin = bytes.NewReader(mysqlClientOptions(info.DBUser, info.dbPass, info.DBHost))
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		f.Close()
		return err
	}
	cr := &countingReader{r: stdout}
	stop := a.reportTransfer(j, "Copying the database", cr, 0)
	gz := gzip.NewWriter(f)
	_, cerr := io.Copy(gz, bufio.NewReaderSize(cr, 1<<20))
	stop()
	gz.Close()
	f.Close()
	werr := c.Wait()
	if werr != nil || cerr != nil {
		os.Remove(tmp)
		msg := strings.TrimSpace(errb.String())
		if strings.Contains(msg, "Access denied") && strings.Contains(msg, "to database") {
			return fmt.Errorf("database user %s cannot open the database %s — check the database name", info.DBUser, info.DBName)
		}
		if strings.Contains(msg, "Unknown database") {
			return fmt.Errorf("the database %s does not exist on the server — check the database name", info.DBName)
		}
		if strings.Contains(msg, "Access denied") {
			return fmt.Errorf("the database refused the login for %s — check the database user and password", info.DBUser)
		}
		if werr != nil {
			return sshError(werr, msg, r)
		}
		return cerr
	}
	if cr.n.Load() < 200 {
		os.Remove(tmp)
		return fmt.Errorf("the database dump is empty: %s", lastLines(errb.String(), 2))
	}
	j.Logf("• copied the database %s (%s)", info.DBName, humanBytes(cr.n.Load()))
	return os.Rename(tmp, dest)
}

// reportTransfer updates the job step with the amount copied so far.
func (a *App) reportTransfer(j *Job, label string, cr *countingReader, expectMB int) (stop func()) {
	done := make(chan struct{})
	j.SetStep(label+"…", -1)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				n := cr.n.Load()
				j.mu.Lock()
				j.Step = fmt.Sprintf("%s… %s", label, humanBytes(n))
				if expectMB > 0 {
					j.Step += fmt.Sprintf(" (site is %d MB uncompressed)", expectMB)
				}
				j.mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// extractTarStream unpacks a gzip-compressed tar stream into dest.
func extractTarStream(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" || name == "." {
			continue
		}
		p, err := safeJoin(dest, name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode).Perm()|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			if err != nil {
				return err
			}
			os.Chtimes(p, h.ModTime, h.ModTime)
		case tar.TypeSymlink:
			// Keep relative links that stay inside the site; skip the rest (and all on Windows).
			if runtime.GOOS == "windows" || filepath.IsAbs(h.Linkname) {
				continue
			}
			if _, err := safeJoin(dest, filepath.Join(filepath.Dir(name), h.Linkname)); err != nil {
				continue
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.Remove(p)
			os.Symlink(h.Linkname, p)
		}
	}
}

// disableDropIns renames cache drop-ins that only work on the live server.
func disableDropIns(j *Job, webroot string) {
	for _, f := range []string{"object-cache.php", "advanced-cache.php"} {
		p := filepath.Join(webroot, "wp-content", f)
		if exists(p) {
			if os.Rename(p, p+".quickspin-off") == nil {
				j.Logf("• disabled wp-content/%s (a live-server cache); it is kept as %s.quickspin-off", f, f)
			}
		}
	}
}

// ---------- the job ----------

const (
	liveWebRoot = "public"
	liveDump    = "db/live.sql.gz"
)

// pullLive copies files and/or the database from the live site, then
// (re)provisions the project from them.
func (a *App) pullLive(j *Job, slug string, withFiles bool) (err error) {
	p := a.store.Get(slug)
	if p == nil || p.Remote == nil {
		return fmt.Errorf("this project was not copied from a live site")
	}
	r := *p.Remote
	sec := recallSecrets(slug)
	ctx := context.Background()

	// A failed refresh leaves a working local copy alone; a failed first copy is an error.
	firstCopy := p.Status != "ready"
	defer func() {
		if err != nil && firstCopy {
			a.store.Update(slug, func(p *Project) { p.Status, p.Error = "error", err.Error() })
		}
	}()

	j.SetStep("Connecting to "+r.label(), 2)
	info, err := a.inspectRemote(ctx, &r, sec)
	if err != nil {
		return err
	}
	for _, w := range info.Warnings {
		j.Logf("warning: %s", w)
	}
	j.Logf("• connected to %s (WordPress %v, database %s on %s)", r.label(), info.WordPress, info.DBName, info.DBHost)

	webroot := filepath.Join(p.Path, liveWebRoot)
	if withFiles || !isDir(webroot) {
		fresh := webroot + ".download"
		os.RemoveAll(fresh)
		if err := a.downloadFiles(ctx, j, &r, sec, info, fresh); err != nil {
			os.RemoveAll(fresh)
			return err
		}
		disableDropIns(j, fresh)
		redactWPConfig(j, fresh)
		if isDir(webroot) {
			old := webroot + ".old"
			os.RemoveAll(old)
			if err := os.Rename(webroot, old); err != nil {
				os.RemoveAll(fresh)
				return fmt.Errorf("cannot replace the site files (close any editor using them): %w", err)
			}
			defer os.RemoveAll(old)
		}
		if err := os.Rename(fresh, webroot); err != nil {
			return err
		}
		// What is on the server right now — publishing compares against it.
		if err := a.saveFileBaseline(p); err != nil {
			j.Logf("warning: could not record the file list: %v", err)
		}
	}
	if err := a.downloadDatabase(ctx, j, &r, sec, info, filepath.Join(p.Path, filepath.FromSlash(liveDump))); err != nil {
		return err
	}
	a.saveDBBaseline(ctx, j, p, &r, sec, info)
	return a.provision(j, slug, provisionOpts{webRoot: liveWebRoot, dumpPath: liveDump}, false)
}

// liveOrigin is where missing uploads are fetched from ("Load media from the live site").
func liveOrigin(p *Project) string {
	if !p.Remote.uploadsFromLive() || p.OldURL == "" {
		return ""
	}
	o, _, _ := originOf(p.OldURL)
	return o
}

// redactWPConfig blanks the production database password in the local copy of
// wp-config.php: QuickSpin injects its own database settings, so the copy never
// needs it, and it should not sit on a laptop. The server is not touched.
func redactWPConfig(j *Job, webroot string) {
	for _, f := range []string{filepath.Join(webroot, "wp-config.php"), filepath.Join(filepath.Dir(webroot), "wp-config.php")} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		src := string(b)
		changed := false
		for _, re := range []*regexp.Regexp{defineSingleRe, defineDoubleRe} {
			src = re.ReplaceAllStringFunc(src, func(m string) string {
				if sub := re.FindStringSubmatch(m); sub != nil && sub[1] == "DB_PASSWORD" && sub[2] != "" {
					changed = true
					return "define( 'DB_PASSWORD', '' ) /* production password removed by QuickSpin */"
				}
				return m
			})
		}
		if changed && os.WriteFile(f, []byte(src), 0o644) == nil {
			j.Logf("• removed the production database password from the local copy of wp-config.php")
		}
		return
	}
}
