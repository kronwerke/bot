// Package site serves the Kronwerke website from its latest GitHub release.
//
// The website repository publishes site.tar.gz and SHA256SUMS for every tag. Sync asks
// GitHub for the latest release, and when its tag differs from the one being served it
// downloads the archive, checks it against the sum, unpacks it next to the old version
// and switches over. The old version is removed afterwards. Handler serves whatever is
// current and answers 404 until the first release is in.
package site

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Archive is the release file with the built site.
const Archive = "site.tar.gz"

const (
	maxArchive = 64 << 20  // compressed
	maxFiles   = 2000      // files in the archive
	maxTotal   = 256 << 20 // unpacked
)

// Site keeps one website in Dir.
type Site struct {
	Repo      string // owner/name
	API       string // https://api.github.com
	Dir       string // where versions are unpacked
	UserAgent string
	HTTP      *http.Client

	syncMu sync.Mutex // one Sync at a time
	mu     sync.RWMutex
	tag    string
	root   string // directory of the current version
	etag   string
	cached Release // the answer that goes with etag
}

// Release is the part of a GitHub release Sync reads.
type Release struct {
	Tag    string `json:"tag_name"`
	Draft  bool   `json:"draft"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r Release) asset(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

func (s *Site) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

// Load picks up the version unpacked by an earlier run, so the site is there right
// after a restart without asking GitHub.
func (s *Site) Load() {
	b, err := os.ReadFile(filepath.Join(s.Dir, "current"))
	if err != nil {
		return
	}
	tag := strings.TrimSpace(string(b))
	root := filepath.Join(s.Dir, "v-"+safe(tag))
	if st, err := os.Stat(root); err == nil && st.IsDir() {
		s.mu.Lock()
		s.tag, s.root = tag, root
		s.mu.Unlock()
	}
}

// Tag is the version being served, empty before the first release.
func (s *Site) Tag() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tag
}

// Sync installs the latest release when it is not the one being served. It returns the
// tag served afterwards and whether it changed.
func (s *Site) Sync(ctx context.Context) (string, bool, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	r, err := s.latest(ctx)
	if err != nil {
		return s.Tag(), false, err
	}
	if r.Draft || r.Tag == "" || r.Tag == s.Tag() {
		return s.Tag(), false, nil
	}
	archURL, sumURL := r.asset(Archive), r.asset("SHA256SUMS")
	if archURL == "" || sumURL == "" {
		return s.Tag(), false, fmt.Errorf("site: release %s lacks %s or SHA256SUMS", r.Tag, Archive)
	}
	sums, err := s.get(ctx, sumURL, 1<<16)
	if err != nil {
		return s.Tag(), false, err
	}
	want := sumFor(sums, Archive)
	if want == "" {
		return s.Tag(), false, fmt.Errorf("site: SHA256SUMS of %s has no line for %s", r.Tag, Archive)
	}
	arch, err := s.get(ctx, archURL, maxArchive)
	if err != nil {
		return s.Tag(), false, err
	}
	got := sha256.Sum256(arch)
	if hex.EncodeToString(got[:]) != want {
		return s.Tag(), false, fmt.Errorf("site: %s of %s does not match its checksum", Archive, r.Tag)
	}

	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return s.Tag(), false, err
	}
	root := filepath.Join(s.Dir, "v-"+safe(r.Tag))
	tmp := root + ".tmp"
	os.RemoveAll(tmp)
	if err := unpack(arch, tmp); err != nil {
		os.RemoveAll(tmp)
		return s.Tag(), false, err
	}
	if _, err := os.Stat(filepath.Join(tmp, "index.html")); err != nil {
		os.RemoveAll(tmp)
		return s.Tag(), false, fmt.Errorf("site: %s of %s has no index.html", Archive, r.Tag)
	}
	os.RemoveAll(root)
	if err := os.Rename(tmp, root); err != nil {
		return s.Tag(), false, err
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "current"), []byte(r.Tag+"\n"), 0o644); err != nil {
		return s.Tag(), false, err
	}

	s.mu.Lock()
	old := s.root
	s.tag, s.root = r.Tag, root
	s.mu.Unlock()
	if old != "" && old != root {
		os.RemoveAll(old)
	}
	return r.Tag, true, nil
}

func (s *Site) latest(ctx context.Context) (Release, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.API+"/repos/"+s.Repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", s.UserAgent)
	if s.etag != "" {
		req.Header.Set("If-None-Match", s.etag)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("site: ask for the latest release: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return s.cached, nil
	case http.StatusNotFound:
		return Release{}, errors.New("site: no release published yet")
	case http.StatusOK:
	default:
		return Release{}, fmt.Errorf("site: GitHub answered %s", resp.Status)
	}
	var r Release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("site: decode release: %w", err)
	}
	s.etag = resp.Header.Get("ETag")
	s.cached = r
	return r, nil
}

func (s *Site) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", s.UserAgent)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("site: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("site: download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("site: %s is larger than expected", url)
	}
	return b, nil
}

func sumFor(sums []byte, name string) string {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// unpack writes the regular files and directories of a gzipped tar into dir. Links,
// devices and anything that would land outside dir are refused.
func unpack(arch []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(arch))
	if err != nil {
		return fmt.Errorf("site: open archive: %w", err)
	}
	tr := tar.NewReader(zr)
	var files int
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("site: read archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("site: archive entry %q leaves the site folder", h.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			files++
			total += h.Size
			if files > maxFiles || total > maxTotal {
				return errors.New("site: archive is larger than a website should be")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			f.Close()
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("site: archive entry %q is not a file or folder", h.Name)
		}
	}
}

// safe turns a tag into a folder name.
func safe(tag string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, tag)
}

// Handler serves the current version. Pages are revalidated after five minutes, fonts
// and scripts after an hour.
func (s *Site) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		root := s.root
		s.mu.RUnlock()
		if root == "" {
			http.Error(w, "The site is not published yet.", http.StatusNotFound)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/fonts/"):
			w.Header().Set("Cache-Control", "public, max-age=86400")
		case strings.HasPrefix(r.URL.Path, "/css/"), strings.HasPrefix(r.URL.Path, "/js/"):
			w.Header().Set("Cache-Control", "public, max-age=3600")
		default:
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		http.FileServer(noListing{http.Dir(root)}).ServeHTTP(w, r)
	})
}

// noListing hides directory listings: a folder without index.html is a 404.
type noListing struct{ fs http.FileSystem }

func (n noListing) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		idx, err := n.fs.Open(strings.TrimSuffix(name, "/") + "/index.html")
		if err != nil {
			f.Close()
			return nil, os.ErrNotExist
		}
		idx.Close()
	}
	return f, nil
}
