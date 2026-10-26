package main

// Publishing local work back to where the project came from.
//
//   Git projects:  (optionally) export the database with the production URLs
//                  into the repository's dump, commit, push to the current branch.
//   SSH projects:  upload changed files only (preview first) and/or replace the
//                  live database. Both are backed up on the server first, in
//                  ~/quickspin-backups (outside the web root).
//
// Safety rules for SSH file publishing:
//   - wp-config.php is never uploaded or deleted (the local copy has no password).
//   - wp-content/uploads: new files are added, existing ones are never changed or deleted.
//   - caches, backups and .git are ignored on both sides.
//   - files deleted locally are only deleted on the server when asked.
//   - files changed on the server since the last pull are "conflicts" and are
//     skipped unless overwriting them is confirmed.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ===================== shared =====================

type PublishRequest struct {
	// Git
	Message         string `json:"message"`
	IncludeDatabase bool   `json:"includeDatabase"`
	AuthorName      string `json:"authorName"`  // saved in the repository's own git config
	AuthorEmail     string `json:"authorEmail"` // when Git does not know the user yet
	// SSH
	Files              bool   `json:"files"`
	Database           bool   `json:"database"`
	DeleteRemoved      bool   `json:"deleteRemoved"`
	OverwriteConflicts bool   `json:"overwriteConflicts"`
	ConfirmDatabase    bool   `json:"confirmDatabase"`    // "replace the live database" was confirmed
	ConfirmLiveChanged bool   `json:"confirmLiveChanged"` // ... even though it changed since the pull
	SSHPassword        string `json:"sshPassword"`
}

func (a *App) publishKind(p *Project) string {
	switch {
	case p.Source == "ssh" && p.Remote != nil:
		return "ssh"
	case isDir(filepath.Join(p.Path, ".git")):
		return "git"
	}
	return ""
}

// publishExportRegex matches local URLs (absolute, protocol-relative and
// JSON-escaped) so they can be turned back into production URLs.
func publishExportRegex(localHost string) string {
	return `(?:https?:)?(\\?/\\?/)` + regexp.QuoteMeta(localHost) + `(?![\w-]|\.[\w-])`
}

// exportForProduction writes the local database with production URLs to out (plain SQL).
func (a *App) exportForProduction(j *Job, p *Project, out string) error {
	if p.OldURL == "" {
		return fmt.Errorf("the production URL of %s is unknown, so the database cannot be prepared for publishing", p.Name)
	}
	if p.Multisite {
		return fmt.Errorf("publishing the database of a multisite network is not supported yet")
	}
	_, oldHost, _ := originOf(p.OldURL)
	scheme := strings.SplitN(p.OldURL, ":", 2)[0]
	j.SetStep("Preparing the database with production URLs ("+p.OldURL+")", -1)
	_, err := a.wp(p, j, false, "search-replace", publishExportRegex(p.Domains[0]), scheme+`:${1}`+oldHost,
		"--regex", "--regex-delimiter=~", "--all-tables-with-prefix", "--skip-plugins", "--skip-themes",
		"--report-changed-only", "--export="+out)
	if err != nil {
		return err
	}
	if st, err := os.Stat(out); err != nil || st.Size() < 200 {
		return fmt.Errorf("the database export is empty")
	}
	return nil
}

// ===================== Git =====================

type GitPreview struct {
	Branch   string   `json:"branch"`
	Upstream string   `json:"upstream,omitempty"`
	Ahead    int      `json:"ahead"`
	Behind   int      `json:"behind"`
	Changes  []string `json:"changes"` // "M path", "?? path" …
	DumpPath string   `json:"dumpPath,omitempty"`
	OldURL   string   `json:"oldUrl,omitempty"`
	Warning  string   `json:"warning,omitempty"`
	// NeedsIdentity: Git has no user.name / user.email yet; the dialog asks for them.
	NeedsIdentity bool `json:"needsIdentity"`
}

func (a *App) gitOut(dir string, args ...string) (string, error) {
	g := a.gitExe()
	if g == "" {
		return "", fmt.Errorf("Git is not available")
	}
	c := exec.Command(g, append([]string{"-c", "core.longpaths=true", "-c", "core.quotepath=off"}, args...)...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	hideCmd(c)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %s", args[0], lastLines(strings.TrimSpace(errb.String()), 2))
	}
	return out.String(), nil
}

func (a *App) gitPreview(p *Project) (*GitPreview, error) {
	pv := &GitPreview{DumpPath: p.DumpPath, OldURL: p.OldURL}
	branch, err := a.gitOut(p.Path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	pv.Branch = strings.TrimSpace(branch)
	if pv.Branch == "HEAD" {
		pv.Warning = "the repository is not on a branch (detached HEAD) — check out a branch in the editor first"
	}
	if up, err := a.gitOut(p.Path, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		pv.Upstream = strings.TrimSpace(up)
		a.gitOut(p.Path, "fetch", "--quiet") // best effort, for an accurate behind count
		if lr, err := a.gitOut(p.Path, "rev-list", "--left-right", "--count", "HEAD...@{u}"); err == nil {
			f := strings.Fields(lr)
			if len(f) == 2 {
				pv.Ahead, _ = strconv.Atoi(f[0])
				pv.Behind, _ = strconv.Atoi(f[1])
			}
		}
	}
	name, _ := a.gitOut(p.Path, "config", "user.name")
	email, _ := a.gitOut(p.Path, "config", "user.email")
	pv.NeedsIdentity = strings.TrimSpace(name) == "" || strings.TrimSpace(email) == ""
	st, err := a.gitOut(p.Path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(strings.TrimRight(st, "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			pv.Changes = append(pv.Changes, strings.TrimSpace(l))
		}
	}
	return pv, nil
}

func (a *App) publishGit(j *Job, slug string, req PublishRequest) error {
	p := a.store.Get(slug)
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = "Update from QuickSpin"
	}
	if n, e := strings.TrimSpace(req.AuthorName), strings.TrimSpace(req.AuthorEmail); n != "" && e != "" {
		if !strings.Contains(e, "@") || strings.ContainsAny(n+e, "\n<>") {
			return fmt.Errorf("enter a valid name and email for your commits")
		}
		if _, err := a.gitOut(p.Path, "config", "user.name", n); err != nil {
			return err
		}
		if _, err := a.gitOut(p.Path, "config", "user.email", e); err != nil {
			return err
		}
	}
	if req.IncludeDatabase {
		rel := p.DumpPath
		if rel == "" {
			rel = "database.sql.gz"
		}
		if strings.HasSuffix(strings.ToLower(rel), ".zip") {
			return fmt.Errorf("the repository's dump is a .zip, which QuickSpin cannot write — use .sql or .sql.gz")
		}
		tmp := a.data("tmp", "publish-"+slug+".sql")
		defer os.Remove(tmp)
		if err := a.exportForProduction(j, p, tmp); err != nil {
			return err
		}
		j.SetStep("Writing "+rel, 30)
		if err := copyMaybeGzip(tmp, filepath.Join(p.Path, filepath.FromSlash(rel))); err != nil {
			return err
		}
		if p.DumpPath == "" {
			a.store.Update(slug, func(p *Project) { p.DumpPath = rel })
		}
	}
	j.SetStep("Committing", 50)
	if err := a.git(j, p.Path, "add", "-A"); err != nil {
		return err
	}
	if out, _ := a.gitOut(p.Path, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		if err := a.git(j, p.Path, "commit", "-m", msg); err != nil {
			if who, _ := a.gitOut(p.Path, "config", "user.email"); strings.TrimSpace(who) == "" {
				return fmt.Errorf("Git does not know who you are yet — run: git config --global user.name \"Your Name\" and git config --global user.email you@example.com")
			}
			return fmt.Errorf("git commit failed: %v", err)
		}
	} else {
		j.Logf("• nothing new to commit")
	}
	j.SetStep("Pushing", 70)
	_, upErr := a.gitOut(p.Path, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	pushArgs := []string{"push"}
	if upErr != nil {
		pushArgs = []string{"push", "-u", "origin", "HEAD"}
	}
	if err := a.git(j, p.Path, pushArgs...); err != nil {
		// Most often the branch moved on the server: replay our commits on top and retry once.
		j.Logf("• push was rejected — fetching the latest changes and retrying")
		if err := a.git(j, p.Path, "pull", "--rebase", "--autostash"); err != nil {
			a.git(j, p.Path, "rebase", "--abort")
			return fmt.Errorf("the branch changed on the server and the changes conflict with yours — open the editor, pull and resolve the conflicts, then publish again")
		}
		if err := a.git(j, p.Path, pushArgs...); err != nil {
			return fmt.Errorf("git push failed — check your access to the repository: %v", err)
		}
	}
	j.mu.Lock()
	j.Result = "pushed " + msg
	j.mu.Unlock()
	return nil
}

// ===================== SSH: baselines recorded at pull time =====================

type fileState struct {
	Size  int64 `json:"s"`
	MTime int64 `json:"m"`
}

func (a *App) metaDir(p *Project) string { return filepath.Join(p.Path, ".quickspin") }

func loadJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(b, v) == nil
}

func saveJSON(path string, v any) error {
	b, _ := json.Marshal(v)
	return writeFileAtomic(path, b)
}

// publishSkip: paths that are never uploaded, changed or deleted on the server.
func publishSkip(rel string) bool {
	switch {
	case rel == "wp-config.php",
		strings.HasSuffix(rel, ".quickspin-off"),
		rel == "wp-content/object-cache.php", rel == "wp-content/advanced-cache.php":
		return true
	}
	for _, e := range remoteExcludes {
		e = strings.TrimPrefix(e, "./")
		if rel == e || strings.HasPrefix(rel, e+"/") {
			return true
		}
	}
	return false
}

func isUpload(rel string) bool { return strings.HasPrefix(rel, "wp-content/uploads/") }

// localFiles lists the site files (relative, slash-separated) with size and mtime.
func localFiles(root string) (map[string]fileState, error) {
	out := map[string]fileState{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if publishSkip(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || publishSkip(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out[rel] = fileState{Size: info.Size(), MTime: info.ModTime().Unix()}
		return nil
	})
	return out, err
}

func (a *App) saveFileBaseline(p *Project) error {
	files, err := localFiles(filepath.Join(p.Path, liveWebRoot))
	if err != nil {
		return err
	}
	return saveJSON(filepath.Join(a.metaDir(p), "live-files.json"), files)
}

// ===================== SSH: server-side helpers =====================

// remoteListScript lists files as "size<TAB>mtime<TAB>path" (GNU find, or BSD stat).
func remoteListScript(path string) string {
	var prune []string
	for _, e := range remoteExcludes {
		prune = append(prune, "-path "+shQuote(e))
	}
	expr := `\( ` + strings.Join(prune, " -o ") + ` \) -prune -o -type f`
	return fmt.Sprintf(`cd %s || exit 3
if find . -maxdepth 0 -printf '' >/dev/null 2>&1; then
  find . %s -printf '%%s\t%%T@\t%%P\n'
else
  find . %s -exec stat -f '%%z%%t%%m%%t%%N' {} +
fi
`, rpath(path), expr, expr)
}

func (a *App) remoteFiles(ctx context.Context, r *RemoteSpec, sec remoteSecrets) (map[string]fileState, error) {
	out, err := a.sshRun(ctx, r, sec.SSHPassword, remoteListScript(r.Path), nil)
	if err != nil {
		return nil, err
	}
	files := map[string]fileState{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		size, err1 := strconv.ParseInt(parts[0], 10, 64)
		mt, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rel := strings.TrimPrefix(parts[2], "./")
		if publishSkip(rel) {
			continue
		}
		files[rel] = fileState{Size: size, MTime: int64(mt)}
	}
	return files, nil
}

// dbClientScript wraps a command that needs the database login: the [client]
// options arrive on stdin first, terminated by a line "--QS-END--", and are kept
// in a private temporary file that is removed afterwards.
const dbClientScript = `umask 077
f=$(mktemp 2>/dev/null || echo "/tmp/qs-my-$$.cnf")
trap 'rm -f "$f"' EXIT
while IFS= read -r l; do [ "$l" = "--QS-END--" ] && break; printf '%%s\n' "$l" >> "$f"; done
M=$(command -v mysql 2>/dev/null || command -v mariadb 2>/dev/null)
D=$(command -v mysqldump 2>/dev/null || command -v mariadb-dump 2>/dev/null)
%s
`

func dbStdin(info *RemoteInfo, rest io.Reader) io.Reader {
	head := append(mysqlClientOptions(info.DBUser, info.dbPass, info.DBHost), []byte("--QS-END--\n")...)
	if rest == nil {
		return bytes.NewReader(head)
	}
	return io.MultiReader(bytes.NewReader(head), rest)
}

// liveDBFingerprint checksums every table of the live database.
func (a *App) liveDBFingerprint(ctx context.Context, r *RemoteSpec, sec remoteSecrets, info *RemoteInfo) (string, error) {
	script := fmt.Sprintf(dbClientScript, `[ -n "$M" ] || { echo "the mysql client is not installed on the server" >&2; exit 127; }
T=$("$M" --defaults-extra-file="$f" -N -B -e "SET SESSION group_concat_max_len=10000000; SELECT GROUP_CONCAT(CONCAT(CHAR(96),table_name,CHAR(96)) ORDER BY table_name) FROM information_schema.tables WHERE table_schema=DATABASE()" `+shQuote(info.DBName)+`) || exit 1
[ -n "$T" ] && [ "$T" != "NULL" ] || exit 0
"$M" --defaults-extra-file="$f" -N -B -e "CHECKSUM TABLE $T" `+shQuote(info.DBName))
	c, err := a.sshCommand(ctx, r, sec.SSHPassword, script, false)
	if err != nil {
		return "", err
	}
	c.Stdin = dbStdin(info, nil)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", sshError(err, errb.String(), r)
	}
	h := sha256.Sum256(out.Bytes())
	return hex.EncodeToString(h[:]), nil
}

func (a *App) saveDBBaseline(ctx context.Context, j *Job, p *Project, r *RemoteSpec, sec remoteSecrets, info *RemoteInfo) {
	fp, err := a.liveDBFingerprint(ctx, r, sec, info)
	if err != nil {
		j.Logf("warning: cannot fingerprint the live database (%v) — publishing will not be able to tell whether it changed", err)
		os.Remove(filepath.Join(a.metaDir(p), "live-db.json"))
		return
	}
	saveJSON(filepath.Join(a.metaDir(p), "live-db.json"), map[string]any{"fingerprint": fp, "at": time.Now().UTC()})
}

// ===================== SSH: preview =====================

type SSHPreview struct {
	New       []string `json:"new"`
	Changed   []string `json:"changed"`
	Deleted   []string `json:"deleted"`   // deleted locally, still on the server
	Conflicts []string `json:"conflicts"` // changed locally and on the server since the pull
	LiveOnly  int      `json:"liveChanged"`
	Skipped   []string `json:"skipped"` // existing uploads changed locally (never overwritten)

	DBName        string `json:"dbName"`
	DBChanged     *bool  `json:"dbChangedSinceLastPull"` // nil = unknown
	OldURL        string `json:"oldUrl"`
	Multisite     bool   `json:"multisite"`
	NoBaseline    bool   `json:"noBaseline"`
	LastPull      string `json:"lastPull,omitempty"`
	RemoteLabel   string `json:"remote"`
	BackupsFolder string `json:"backupsFolder"`

	upload []string
	info   *RemoteInfo
}

func (a *App) sshPreview(ctx context.Context, p *Project, sec remoteSecrets, wantFiles bool) (*SSHPreview, error) {
	r := *p.Remote
	info, err := a.inspectRemote(ctx, &r, sec)
	if err != nil {
		return nil, err
	}
	pv := &SSHPreview{DBName: info.DBName, OldURL: p.OldURL, Multisite: p.Multisite, RemoteLabel: r.label(), BackupsFolder: "~/quickspin-backups", info: info}
	var base map[string]fileState
	if !loadJSON(filepath.Join(a.metaDir(p), "live-files.json"), &base) {
		pv.NoBaseline = true
	}
	var dbBase struct {
		Fingerprint string    `json:"fingerprint"`
		At          time.Time `json:"at"`
	}
	if loadJSON(filepath.Join(a.metaDir(p), "live-db.json"), &dbBase) && dbBase.Fingerprint != "" {
		pv.LastPull = dbBase.At.Local().Format("2 Jan 2006 15:04")
		if fp, err := a.liveDBFingerprint(ctx, &r, sec, info); err == nil {
			changed := fp != dbBase.Fingerprint
			pv.DBChanged = &changed
		}
	}
	if !wantFiles {
		return pv, nil
	}
	local, err := localFiles(filepath.Join(p.Path, liveWebRoot))
	if err != nil {
		return nil, err
	}
	remote, err := a.remoteFiles(ctx, &r, sec)
	if err != nil {
		return nil, err
	}
	for rel, l := range local {
		rm, onServer := remote[rel]
		b, inBase := base[rel]
		switch {
		case !onServer:
			pv.New = append(pv.New, rel)
			pv.upload = append(pv.upload, rel)
		case rm == l:
			// identical
		case isUpload(rel):
			if !inBase || b != l { // only report uploads that were edited locally
				pv.Skipped = append(pv.Skipped, rel)
			}
		case inBase && b == l:
			pv.LiveOnly++ // changed on the server only: leave it
		case inBase && b != rm:
			pv.Conflicts = append(pv.Conflicts, rel) // changed on both sides
		default:
			pv.Changed = append(pv.Changed, rel)
			pv.upload = append(pv.upload, rel)
		}
	}
	for rel, rm := range remote {
		if _, ok := local[rel]; ok || isUpload(rel) {
			continue
		}
		if b, inBase := base[rel]; !pv.NoBaseline && !inBase {
			continue // created on the server after the pull — not ours to delete
		} else if inBase && b != rm {
			continue // changed on the server since the pull — keep
		}
		pv.Deleted = append(pv.Deleted, rel)
	}
	for _, l := range [][]string{pv.New, pv.Changed, pv.Deleted, pv.Conflicts, pv.Skipped, pv.upload} {
		sort.Strings(l)
	}
	return pv, nil
}

// ===================== SSH: publish =====================

func backupStamp(slug string) string { return slug + "-" + time.Now().Format("20060102-150405") }

func (a *App) publishSSH(j *Job, slug string, req PublishRequest) error {
	p := a.store.Get(slug)
	if p == nil || p.Remote == nil {
		return fmt.Errorf("this project was not copied from a live site")
	}
	if !req.Files && !req.Database {
		return fmt.Errorf("choose files, database or both")
	}
	r := *p.Remote
	sec := recallSecrets(slug)
	ctx := context.Background()

	j.SetStep("Comparing with "+r.label(), 5)
	pv, err := a.sshPreview(ctx, p, sec, req.Files)
	if err != nil {
		return err
	}
	if req.Database {
		if !req.ConfirmDatabase {
			return fmt.Errorf("replacing the live database must be confirmed")
		}
		if pv.DBChanged != nil && *pv.DBChanged && !req.ConfirmLiveChanged {
			return fmt.Errorf("the live database changed since your last pull — confirm that it may be replaced, or refresh from live first")
		}
		if pv.info.DBName == "" || pv.info.DBUser == "" {
			return fmt.Errorf("the live database settings are unknown")
		}
	}
	stamp := backupStamp(slug)

	if req.Files {
		if err := a.publishFiles(ctx, j, p, &r, sec, pv, req, stamp); err != nil {
			return err
		}
	}
	if req.Database {
		if err := a.publishDatabase(ctx, j, p, &r, sec, pv.info, stamp); err != nil {
			return err
		}
	}
	j.mu.Lock()
	j.Result = "published to " + r.label()
	j.mu.Unlock()
	return nil
}

func (a *App) publishFiles(ctx context.Context, j *Job, p *Project, r *RemoteSpec, sec remoteSecrets, pv *SSHPreview, req PublishRequest, stamp string) error {
	upload := append([]string(nil), pv.upload...)
	if req.OverwriteConflicts {
		upload = append(upload, pv.Conflicts...)
	} else if len(pv.Conflicts) > 0 {
		j.Logf("• skipped %d file(s) that also changed on the server: %s", len(pv.Conflicts), strings.Join(firstN(pv.Conflicts, 5), ", "))
	}
	var del []string
	if req.DeleteRemoved {
		del = pv.Deleted
	}
	if len(upload) == 0 && len(del) == 0 {
		j.Logf("• no file changes to publish")
		return nil
	}
	// 1. Back up what will be overwritten or deleted.
	existing := append(append([]string(nil), del...), pv.Changed...)
	if req.OverwriteConflicts {
		existing = append(existing, pv.Conflicts...)
	}
	if len(existing) > 0 {
		j.SetStep(fmt.Sprintf("Backing up %d file(s) on the server", len(existing)), 20)
		backup := `~/quickspin-backups/` + stamp + `-files.tar.gz`
		script := fmt.Sprintf(`umask 077; mkdir -p "$HOME/quickspin-backups" && cd %s && tar -czf "$HOME/quickspin-backups/%s-files.tar.gz" -T -`, rpath(r.Path), stamp)
		if _, err := a.sshRun(ctx, r, sec.SSHPassword, script, []byte(strings.Join(existing, "\n")+"\n")); err != nil {
			return fmt.Errorf("backing up the server files failed, nothing was changed: %w", err)
		}
		j.Logf("• backup of the replaced files: %s", backup)
	}
	// 2. Upload.
	if len(upload) > 0 {
		j.SetStep(fmt.Sprintf("Uploading %d file(s)", len(upload)), 40)
		if err := a.uploadFiles(ctx, r, sec, filepath.Join(p.Path, liveWebRoot), upload); err != nil {
			return err
		}
		j.Logf("• uploaded %d file(s): %s", len(upload), strings.Join(firstN(upload, 8), ", "))
	}
	// 3. Delete.
	if len(del) > 0 {
		j.SetStep(fmt.Sprintf("Deleting %d file(s) on the server", len(del)), 60)
		script := fmt.Sprintf(`cd %s || exit 3
while IFS= read -r f; do [ -n "$f" ] && rm -f -- "./$f"; done`, rpath(r.Path))
		if _, err := a.sshRun(ctx, r, sec.SSHPassword, script, []byte(strings.Join(del, "\n")+"\n")); err != nil {
			return err
		}
		j.Logf("• deleted %d file(s) on the server: %s", len(del), strings.Join(firstN(del, 8), ", "))
	}
	// The server now matches what was published: that is the new baseline.
	var base map[string]fileState
	loadJSON(filepath.Join(a.metaDir(p), "live-files.json"), &base)
	if base == nil {
		base = map[string]fileState{}
	}
	local, _ := localFiles(filepath.Join(p.Path, liveWebRoot))
	for _, rel := range upload {
		base[rel] = local[rel]
	}
	for _, rel := range del {
		delete(base, rel)
	}
	return saveJSON(filepath.Join(a.metaDir(p), "live-files.json"), base)
}

func firstN(l []string, n int) []string {
	if len(l) <= n {
		return l
	}
	return append(append([]string(nil), l[:n]...), fmt.Sprintf("… (%d more)", len(l)-n))
}

// uploadFiles streams the given files as a tar.gz into the server's site folder.
func (a *App) uploadFiles(ctx context.Context, r *RemoteSpec, sec remoteSecrets, root string, files []string) error {
	script := fmt.Sprintf(`cd %s || exit 3
o=""; tar --help 2>&1 | grep -q -- --no-same-owner && o="--no-same-owner"
tar -xzf - $o`, rpath(r.Path))
	c, err := a.sshCommand(ctx, r, sec.SSHPassword, script, false)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	c.Stdin = pr
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Start(); err != nil {
		return err
	}
	werr := writeTarGz(pw, root, files)
	pw.CloseWithError(werr)
	rerr := c.Wait()
	if werr != nil {
		return fmt.Errorf("packing files: %w", werr)
	}
	if rerr != nil {
		return sshError(rerr, errb.String(), r)
	}
	return nil
}

func writeTarGz(w io.Writer, root string, files []string) error {
	// Files only: tar creates missing folders itself, and existing folders on the
	// server keep their permissions.
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, rel := range files {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		mode := int64(0o644)
		if !isWindows && st.Mode()&0o111 != 0 {
			mode = 0o755
		}
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "./" + rel, Size: st.Size(), Mode: mode, ModTime: st.ModTime(), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			f.Close()
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func (a *App) publishDatabase(ctx context.Context, j *Job, p *Project, r *RemoteSpec, sec remoteSecrets, info *RemoteInfo, stamp string) error {
	// 1. Prepare the local database with production URLs.
	tmp := a.data("tmp", "publish-"+p.Slug+".sql")
	defer os.Remove(tmp)
	if err := a.exportForProduction(j, p, tmp); err != nil {
		return err
	}
	// 2. Back up the live database on the server.
	j.SetStep("Backing up the live database on the server", 70)
	backup := fmt.Sprintf("%s-db-%s.sql.gz", stamp, info.DBName)
	script := fmt.Sprintf(dbClientScript, fmt.Sprintf(`[ -n "$D" ] || { echo "mysqldump is not installed on the server" >&2; exit 127; }
mkdir -p "$HOME/quickspin-backups" || exit 1
o=""; "$D" --help 2>/dev/null | grep -q column-statistics && o="--column-statistics=0"
"$D" --defaults-extra-file="$f" --single-transaction --quick --no-tablespaces --default-character-set=utf8mb4 $o %s | gzip -c > "$HOME/quickspin-backups/%s" || exit 1
[ -s "$HOME/quickspin-backups/%s" ] || { echo "the backup is empty" >&2; exit 1; }`, shQuote(info.DBName), backup, backup))
	c, err := a.sshCommand(ctx, r, sec.SSHPassword, script, false)
	if err != nil {
		return err
	}
	c.Stdin = dbStdin(info, nil)
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("backing up the live database failed, it was not changed: %w", sshError(err, errb.String(), r))
	}
	j.Logf("• backup of the live database: ~/quickspin-backups/%s", backup)

	// 3. Import.
	j.SetStep("Replacing the live database", 85)
	sqlFile, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer sqlFile.Close()
	script = fmt.Sprintf(dbClientScript, fmt.Sprintf(`[ -n "$M" ] || { echo "the mysql client is not installed on the server" >&2; exit 127; }
"$M" --defaults-extra-file="$f" --default-character-set=utf8mb4 %s`, shQuote(info.DBName)))
	c, err = a.sshCommand(ctx, r, sec.SSHPassword, script, true)
	if err != nil {
		return err
	}
	c.Stdin = dbStdin(info, sqlFile)
	errb.Reset()
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("importing into the live database failed (restore it from ~/quickspin-backups/%s if the site is broken): %w", backup, sshError(err, errb.String(), r))
	}
	j.Logf("• replaced the live database %s", info.DBName)

	// 4. Clear the live object cache when WP-CLI is available there; best effort.
	a.sshRun(ctx, r, sec.SSHPassword, fmt.Sprintf(`command -v wp >/dev/null 2>&1 && cd %s && wp cache flush --skip-plugins --skip-themes >/dev/null 2>&1; exit 0`, rpath(r.Path)), nil)

	// The live database now equals what was published: new baseline.
	a.saveDBBaseline(ctx, j, p, r, sec, info)
	return nil
}
