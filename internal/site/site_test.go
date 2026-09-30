package site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type file struct {
	name, body string
	dir        bool
	link       bool
}

func archive(t *testing.T, files []file) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		h := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if f.dir {
			h = &tar.Header{Name: f.name, Mode: 0o755, Typeflag: tar.TypeDir}
		}
		if f.link {
			h = &tar.Header{Name: f.name, Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if !f.dir && !f.link {
			io.WriteString(tw, f.body)
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// github fakes the releases API and the downloads for one release at a time.
type github struct {
	tag  string
	arch []byte
	sum  string
	hits int
}

func (g *github) server(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kronwerke/website/releases/latest":
			g.hits++
			etag := `"` + g.tag + `"`
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":"site.tar.gz","browser_download_url":"%s/dl/site.tar.gz"},{"name":"SHA256SUMS","browser_download_url":"%s/dl/SHA256SUMS"}]}`, g.tag, srv.URL, srv.URL)
		case "/dl/site.tar.gz":
			w.Write(g.arch)
		case "/dl/SHA256SUMS":
			sum := g.sum
			if sum == "" {
				h := sha256.Sum256(g.arch)
				sum = hex.EncodeToString(h[:])
			}
			fmt.Fprintf(w, "%s  site.tar.gz\n", sum)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestSyncServesTheLatestReleaseAndSwitchesToTheNextOne(t *testing.T) {
	g := &github{tag: "v1.0.0", arch: archive(t, []file{
		{name: "./", dir: true},
		{name: "./index.html", body: "<h1>one</h1>"},
		{name: "./css/site.css", body: "body{}"},
		{name: "./js/", dir: true},
	})}
	srv := g.server(t)
	dir := t.TempDir()
	s := &Site{Repo: "kronwerke/website", API: srv.URL, Dir: dir, UserAgent: "test"}
	h := s.Handler()

	if code, _ := get(t, h, "/"); code != http.StatusNotFound {
		t.Fatalf("before the first sync: %d, want 404", code)
	}
	tag, changed, err := s.Sync(context.Background())
	if err != nil || !changed || tag != "v1.0.0" {
		t.Fatalf("first sync: %q %v %v", tag, changed, err)
	}
	if code, body := get(t, h, "/"); code != 200 || !strings.Contains(body, "one") {
		t.Fatalf("index: %d %q", code, body)
	}
	if code, _ := get(t, h, "/css/site.css"); code != 200 {
		t.Fatalf("css: %d", code)
	}
	if code, _ := get(t, h, "/js/"); code != http.StatusNotFound {
		t.Fatalf("a folder without index.html must not be listed, got %d", code)
	}

	// nothing new: no download, same tag
	if tag, changed, err := s.Sync(context.Background()); err != nil || changed || tag != "v1.0.0" {
		t.Fatalf("second sync: %q %v %v", tag, changed, err)
	}

	g.tag, g.arch = "v1.1.0", archive(t, []file{{name: "index.html", body: "<h1>two</h1>"}})
	if tag, changed, err := s.Sync(context.Background()); err != nil || !changed || tag != "v1.1.0" {
		t.Fatalf("third sync: %q %v %v", tag, changed, err)
	}
	if _, body := get(t, h, "/"); !strings.Contains(body, "two") {
		t.Fatalf("after the switch: %q", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "v-v1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("the old version should be gone: %v", err)
	}

	// a restart finds the version on disk without asking GitHub
	again := &Site{Repo: "kronwerke/website", API: srv.URL, Dir: dir, UserAgent: "test"}
	again.Load()
	if again.Tag() != "v1.1.0" {
		t.Fatalf("after load: %q", again.Tag())
	}
	if _, body := get(t, again.Handler(), "/"); !strings.Contains(body, "two") {
		t.Fatalf("after load: %q", body)
	}
}

func TestSyncRefusesBadArchives(t *testing.T) {
	cases := map[string]*github{
		"wrong checksum": {tag: "v1.0.0", arch: archive(t, []file{{name: "index.html", body: "x"}}), sum: strings.Repeat("0", 64)},
		"path outside":   {tag: "v1.0.0", arch: archive(t, []file{{name: "index.html", body: "x"}, {name: "../evil", body: "x"}})},
		"symlink":        {tag: "v1.0.0", arch: archive(t, []file{{name: "index.html", body: "x"}, {name: "link", link: true}})},
		"no index":       {tag: "v1.0.0", arch: archive(t, []file{{name: "other.html", body: "x"}})},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			srv := g.server(t)
			dir := t.TempDir()
			s := &Site{Repo: "kronwerke/website", API: srv.URL, Dir: dir, UserAgent: "test"}
			if _, changed, err := s.Sync(context.Background()); err == nil || changed {
				t.Fatalf("want an error, got changed=%v err=%v", changed, err)
			}
			if s.Tag() != "" {
				t.Fatalf("nothing should be served, got %q", s.Tag())
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
				t.Fatal("a file was written outside the site folder")
			}
		})
	}
}

func TestSyncRetriesAfterAFailedDownload(t *testing.T) {
	g := &github{tag: "v1.0.0", arch: archive(t, []file{{name: "index.html", body: "ok"}}), sum: strings.Repeat("0", 64)}
	srv := g.server(t)
	s := &Site{Repo: "kronwerke/website", API: srv.URL, Dir: t.TempDir(), UserAgent: "test"}
	if _, _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("the bad checksum should fail")
	}
	g.sum = "" // fixed; GitHub still answers 304 for the same release
	if tag, changed, err := s.Sync(context.Background()); err != nil || !changed || tag != "v1.0.0" {
		t.Fatalf("retry: %q %v %v", tag, changed, err)
	}
}
