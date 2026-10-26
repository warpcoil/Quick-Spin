package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var httpClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}

func newRequest(ctx context.Context, method, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "QuickSpin/"+appVersion+" (+portable WordPress dev environment)")
	return req, nil
}

// httpGetString fetches a small text resource (version listings, JSON).
func httpGetString(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := newRequest(ctx, "GET", url)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return string(b), err
}

// httpOK reports whether url answers a HEAD request successfully.
func httpOK(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := newRequest(ctx, "HEAD", url)
	if err != nil {
		return false
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// latestGitHubTag reads the tag behind github.com/<repo>/releases/latest from
// its redirect, which avoids the rate-limited REST API.
func latestGitHubTag(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := &http.Client{
		Transport:     httpClient.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := newRequest(ctx, "HEAD", "https://github.com/"+repo+"/releases/latest")
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("no release redirect for %s (%s)", repo, resp.Status)
	}
	return loc[i+5:], nil
}

// downloadFile streams url into dest, logging progress.
func (a *App) downloadFile(ctx context.Context, url, dest, label string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	req, err := newRequest(ctx, "GET", url)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	pw := &progressWriter{a: a, label: label, total: total, last: time.Now()}
	_, err = io.Copy(f, io.TeeReader(resp.Body, pw))
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("download %s: %w", url, err)
	}
	if total > 0 && pw.n != total {
		os.Remove(tmp)
		return fmt.Errorf("download %s: incomplete (%d of %d bytes)", url, pw.n, total)
	}
	a.logf("  [%s] downloaded %s", label, humanBytes(pw.n))
	return os.Rename(tmp, dest)
}

type progressWriter struct {
	a        *App
	label    string
	total, n int64
	last     time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if time.Since(p.last) > 3*time.Second {
		p.last = time.Now()
		if p.total > 0 {
			p.a.logf("  [%s] %d%% of %s", p.label, p.n*100/p.total, humanBytes(p.total))
		} else {
			p.a.logf("  [%s] %s", p.label, humanBytes(p.n))
		}
	}
	return len(b), nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// safeJoin protects against archive entries escaping the destination ("zip slip").
func safeJoin(dest, name string) (string, error) {
	p := filepath.Join(dest, filepath.FromSlash(name))
	if p != dest && !strings.HasPrefix(p, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry escapes destination: %s", name)
	}
	return p, nil
}

// commonTopDir returns the single top-level directory shared by all names, if any.
func commonTopDir(names []string) string {
	top := ""
	for _, n := range names {
		n = strings.TrimPrefix(strings.ReplaceAll(n, "\\", "/"), "./")
		if n == "" {
			continue
		}
		first, rest, hasSlash := strings.Cut(n, "/")
		if !hasSlash && rest == "" {
			return "" // a file at the top level
		}
		if top == "" {
			top = first
		} else if top != first {
			return ""
		}
	}
	return top
}

func stripTop(name, top string) string {
	name = strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./")
	if top != "" {
		name = strings.TrimPrefix(strings.TrimPrefix(name, top), "/")
	}
	return name
}

// extractZip unpacks src into dest; with strip, a single top-level folder is removed.
func extractZip(src, dest string, strip bool) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	top := ""
	if strip {
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		top = commonTopDir(names)
	}
	for _, f := range zr.File {
		name := stripTop(f.Name, top)
		if name == "" {
			continue
		}
		p, err := safeJoin(dest, name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if f.Mode()&os.ModeSymlink != 0 { // macOS .app bundles (Chromium) rely on these
			rc, err := f.Open()
			if err != nil {
				return err
			}
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			t := string(target)
			if filepath.IsAbs(t) {
				return fmt.Errorf("archive symlink points outside the archive: %s -> %s", f.Name, t)
			}
			if _, err := safeJoin(dest, filepath.Join(filepath.Dir(name), t)); err != nil {
				return err
			}
			if isWindows {
				continue // not needed on Windows
			}
			os.Remove(p)
			if err := os.Symlink(t, p); err != nil {
				return err
			}
			continue
		}
		mode := f.Mode().Perm() | 0o600
		if mode&0o111 == 0 && (strings.HasSuffix(name, ".sh") || !strings.Contains(filepath.Base(name), ".")) {
			mode |= 0o100
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// extractTarGz unpacks a .tar.gz (used for Linux/macOS builds of tools).
func extractTarGz(src, dest string, strip bool) error {
	open := func() (*tar.Reader, func(), error) {
		f, err := os.Open(src)
		if err != nil {
			return nil, nil, err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		return tar.NewReader(gz), func() { gz.Close(); f.Close() }, nil
	}
	top := ""
	if strip {
		tr, done, err := open()
		if err != nil {
			return err
		}
		var names []string
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				done()
				return err
			}
			names = append(names, h.Name)
		}
		done()
		top = commonTopDir(names)
	}
	tr, done, err := open()
	if err != nil {
		return err
	}
	defer done()
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := stripTop(h.Name, top)
		if name == "" {
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
		case tar.TypeSymlink:
			_ = os.Symlink(h.Linkname, p)
		}
	}
}
