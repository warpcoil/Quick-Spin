package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Project is one WordPress site. Projects persist in data/projects.json; their
// files live in data/projects/<slug> (or a folder you pointed at) and their
// database in QuickSpin's MariaDB.
type Project struct {
	Slug      string      `json:"slug"`
	Name      string      `json:"name"`
	Domains   []string    `json:"domains"` // first one is the primary domain
	Source    string      `json:"source"`  // "git", "local" or "ssh" (copied from a live site)
	RepoURL   string      `json:"repoUrl,omitempty"`
	Branch    string      `json:"branch,omitempty"`
	Path      string      `json:"path"`               // repository / project folder
	Managed   bool        `json:"managed"`            // folder was created by QuickSpin (can be deleted with the project)
	WebRoot   string      `json:"webRoot"`            // relative to Path: the folder served by the web server
	DumpPath  string      `json:"dumpPath"`           // relative to Path: SQL dump (.sql / .sql.gz)
	DBName    string      `json:"dbName"`             //
	Prefix    string      `json:"tablePrefix"`        //
	OldURL    string      `json:"oldUrl,omitempty"`   // site URL found in the dump
	HomePath  string      `json:"homePath,omitempty"` // sub-path of home (e.g. "/blog"), usually empty
	SitePath  string      `json:"sitePath,omitempty"` // sub-path of siteurl (e.g. "/wp")
	PHP       string      `json:"php"`                //
	Debug     bool        `json:"debug"`              // WP_DEBUG + display
	Multisite bool        `json:"multisite,omitempty"`
	Remote    *RemoteSpec `json:"remote,omitempty"` // live site this project was copied from (Source "ssh")
	Status    string      `json:"status"`           // new | provisioning | ready | error
	Error     string      `json:"error,omitempty"`  //
	Created   time.Time   `json:"created"`
	Notes     []string    `json:"notes,omitempty"` // things worth knowing (detected layout, warnings)
}

func (p *Project) DocRoot() string { return filepath.Join(p.Path, filepath.FromSlash(p.WebRoot)) }
func (p *Project) URL() string     { return "http://" + p.Domains[0] }

type Store struct {
	mu       sync.Mutex
	path     string
	projects map[string]*Project
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, projects: map[string]*Project{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Project
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, p := range list {
		if p.Status == "provisioning" { // interrupted by a shutdown
			p.Status, p.Error = "error", "provisioning was interrupted — use Rebuild"
		}
		s.projects[p.Slug] = p
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	var list []*Project
	for _, p := range s.projects {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	b, _ := json.MarshalIndent(list, "", "  ")
	return writeFileAtomic(s.path, b)
}

// List returns copies, so callers can read without locking.
func (s *Store) List() []*Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Project
	for _, p := range s.projects {
		c := *p
		c.Domains = append([]string(nil), p.Domains...)
		c.Notes = append([]string(nil), p.Notes...)
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (s *Store) Get(slug string) *Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[slug]
	if !ok {
		return nil
	}
	c := *p
	c.Domains = append([]string(nil), p.Domains...)
	c.Notes = append([]string(nil), p.Notes...)
	return &c
}

// Update applies fn to the stored project and saves.
func (s *Store) Update(slug string, fn func(p *Project)) (*Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[slug]
	if !ok {
		return nil, errNotFound
	}
	fn(p)
	c := *p
	return &c, s.saveLocked()
}

func (s *Store) Add(p *Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.projects[p.Slug]; ok {
		return fmt.Errorf("a project called %q already exists", p.Slug)
	}
	for _, o := range s.projects {
		for _, d := range o.Domains {
			for _, nd := range p.Domains {
				if d == nd {
					return fmt.Errorf("domain %s is already used by %s", d, o.Name)
				}
			}
		}
	}
	s.projects[p.Slug] = p
	return s.saveLocked()
}

func (s *Store) Delete(slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.projects, slug)
	return s.saveLocked()
}

func (s *Store) DomainOwner(domain, except string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.projects {
		if p.Slug == except {
			continue
		}
		for _, d := range p.Domains {
			if d == domain {
				return p.Name
			}
		}
	}
	return ""
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{1,62}$`)

// TLDs on the HSTS preload list force https in Chromium, which our plain-http
// dev sites can't serve.
var hstsTLDs = map[string]bool{"dev": true, "app": true, "page": true, "new": true, "day": true, "foo": true, "zip": true, "mov": true, "bank": true, "insurance": true, "meme": true, "nexus": true, "phd": true, "prof": true, "esq": true, "ing": true, "boo": true, "dad": true, "channel": true, "rsvp": true, "how": true, "soy": true, "gle": true, "google": true}

func normalizeDomains(raw []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, d := range raw {
		for _, part := range strings.FieldsFunc(d, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == ';' }) {
			part = strings.ToLower(strings.TrimSpace(part))
			part = strings.TrimPrefix(strings.TrimPrefix(part, "http://"), "https://")
			part = strings.TrimSuffix(part, "/")
			if part == "" || seen[part] {
				continue
			}
			if !domainRe.MatchString(part) {
				return nil, fmt.Errorf("%q is not a valid domain name (example: mysite.test)", part)
			}
			tld := part[strings.LastIndex(part, ".")+1:]
			if hstsTLDs[tld] {
				return nil, fmt.Errorf("%q uses .%s, which browsers force onto HTTPS — use something like %s.test instead", part, tld, strings.TrimSuffix(part, "."+tld))
			}
			if strings.HasSuffix(part, "quickspin.test") {
				return nil, fmt.Errorf("%s is reserved for QuickSpin itself", part)
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one domain is required (example: mysite.test)")
	}
	return out, nil
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// ===================== jobs =====================

type Job struct {
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Kind     string    `json:"kind"`
	Status   string    `json:"status"` // running | done | failed
	Step     string    `json:"step"`
	Progress int       `json:"progress"` // 0-100, -1 unknown
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitempty"`
	Result   string    `json:"result,omitempty"`

	mu  sync.Mutex
	log bytes.Buffer
}

func (j *Job) Write(b []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.log.Len() > 4<<20 { // keep memory bounded
		tail := append([]byte(nil), j.log.Bytes()[j.log.Len()-(2<<20):]...)
		j.log.Reset()
		j.log.WriteString("…(earlier output trimmed)…\n")
		j.log.Write(tail)
	}
	return j.log.Write(b)
}

func (j *Job) Logf(format string, args ...any) {
	fmt.Fprintf(j, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func (j *Job) SetStep(step string, progress int) {
	j.mu.Lock()
	j.Step, j.Progress = step, progress
	j.mu.Unlock()
	j.Logf("▶ %s", step)
}

func (j *Job) Snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{
		"id": j.ID, "project": j.Project, "kind": j.Kind, "status": j.Status, "step": j.Step,
		"progress": j.Progress, "error": j.Error, "started": j.Started, "ended": j.Ended,
		"result": j.Result, "log": j.log.String(),
	}
}

type JobManager struct {
	a      *App
	mu     sync.Mutex
	jobs   map[string]*Job
	latest map[string]string // project -> job id
	busy   map[string]bool   // project -> a job is running
	seq    int
}

func NewJobManager(a *App) *JobManager {
	return &JobManager{a: a, jobs: map[string]*Job{}, latest: map[string]string{}, busy: map[string]bool{}}
}

// Run starts fn in the background unless the project already has a job running.
func (m *JobManager) Run(project, kind string, fn func(j *Job) error) (*Job, error) {
	m.mu.Lock()
	if m.busy[project] {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s is busy with another task — wait for it to finish", project)
	}
	m.seq++
	j := &Job{ID: strconv.Itoa(m.seq), Project: project, Kind: kind, Status: "running", Started: time.Now(), Progress: -1}
	m.jobs[j.ID] = j
	m.latest[project] = j.ID
	m.busy[project] = true
	m.mu.Unlock()

	logPath := m.a.data("logs", "project-"+project+".log")
	go func() {
		defer func() {
			if r := recover(); r != nil {
				j.mu.Lock()
				j.Status, j.Error = "failed", fmt.Sprint("internal error: ", r)
				j.mu.Unlock()
			}
			m.mu.Lock()
			m.busy[project] = false
			m.mu.Unlock()
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				j.mu.Lock()
				fmt.Fprintf(f, "\n===== %s %s (%s) =====\n%s", j.Started.Format(time.RFC3339), kind, j.Status, j.log.String())
				j.mu.Unlock()
				f.Close()
			}
		}()
		j.Logf("%s: %s", kind, project)
		err := fn(j)
		j.mu.Lock()
		j.Ended = time.Now()
		if err != nil {
			j.Status, j.Error = "failed", err.Error()
		} else {
			j.Status, j.Progress = "done", 100
		}
		j.mu.Unlock()
		if err != nil {
			j.Logf("✖ %v", err)
			m.a.logf("[%s] %s failed: %v", project, kind, err)
		} else {
			j.Logf("✔ finished in %s", time.Since(j.Started).Round(time.Second))
			m.a.logf("[%s] %s finished", project, kind)
		}
	}()
	return j, nil
}

func (m *JobManager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

func (m *JobManager) Latest(project string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[m.latest[project]]
}

func (m *JobManager) Busy(project string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy[project]
}
