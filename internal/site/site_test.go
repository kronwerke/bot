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
	"time"
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
	tag       string
	arch      []byte
	sum       string
	hits      int
	bare      bool      // the release has no files yet
	published time.Time // zero: not sent
}

func (g *github) server(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kronwerke/website/releases/latest":
			g.hits++
			etag := fmt.Sprintf(`"%s-%v"`, g.tag, g.bare)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			if g.bare {
				fmt.Fprintf(w, `{"tag_name":%q,"published_at":%q,"assets":[]}`, g.tag, g.published.UTC().Format(time.RFC3339))
				return
			}
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
		{name: "./installieren.html", body: "<!doctype html><h1>install</h1>"},
		{name: "./faq.html", body: "<!doctype html><h1>faq</h1>"},
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
	if code, body := get(t, h, "/installieren"); code != 200 || !strings.Contains(body, "install") {
		t.Fatalf("a page without .html: %d %q", code, body)
	}
	if code, body := get(t, h, "/fragen"); code != 200 || !strings.Contains(body, "faq") {
		t.Fatalf("an alias: %d %q", code, body)
	}
	if code, _ := get(t, h, "/installieren.html"); code != http.StatusMovedPermanently {
		t.Fatalf("an old .html link should move to the short address: %d", code)
	}
	if code, _ := get(t, h, "/nothing"); code != http.StatusNotFound {
		t.Fatalf("an unknown page: %d", code)
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

func TestSyncWaitsForTheFilesOfAFreshRelease(t *testing.T) {
	g := &github{tag: "v1.0.0", arch: archive(t, []file{{name: "index.html", body: "ok"}}), bare: true, published: time.Now().Add(-time.Minute)}
	srv := g.server(t)
	s := &Site{Repo: "kronwerke/website", API: srv.URL, Dir: t.TempDir(), UserAgent: "test"}
	if tag, changed, err := s.Sync(context.Background()); err != nil || changed || tag != "" {
		t.Fatalf("a fresh release without files should wait quietly: %q %v %v", tag, changed, err)
	}
	g.bare = false // CI attached the files
	if tag, changed, err := s.Sync(context.Background()); err != nil || !changed || tag != "v1.0.0" {
		t.Fatalf("after the files came: %q %v %v", tag, changed, err)
	}
}

func TestSyncComplainsWhenTheFilesNeverCome(t *testing.T) {
	g := &github{tag: "v1.0.0", bare: true, published: time.Now().Add(-time.Hour)}
	srv := g.server(t)
	s := &Site{Repo: "kronwerke/website", API: srv.URL, Dir: t.TempDir(), UserAgent: "test"}
	if _, _, err := s.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "lacks") {
		t.Fatalf("want the lacks error, got %v", err)
	}
}

func ask(t *testing.T, h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func unzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestTextIsCompressedAndUnchangedFilesStayCached(t *testing.T) {
	css := "body{color:red}\n" + strings.Repeat(".scene img{height:calc(var(--h)*var(--b))}\n", 200)
	page := "<!doctype html><h1>install</h1>" + strings.Repeat("<p>Schritt für Schritt.</p>", 100)
	png := "\x89PNG" + strings.Repeat("\x00\x17\x2a\xff", 300)
	files := []file{
		{name: "index.html", body: "<h1>one</h1>"},
		{name: "installieren.html", body: page},
		{name: "faq.html", body: page + "faq"},
		{name: "css/site.css", body: css},
		{name: "img/mobs/fox.png", body: png},
		{name: "fonts/a.woff2", body: "font"},
	}
	g := &github{tag: "v1.0.0", arch: archive(t, files)}
	srv := g.server(t)
	s := &Site{Repo: "kronwerke/website", API: srv.URL, Dir: t.TempDir(), UserAgent: "test"}
	h := s.Handler()
	if _, _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	// a browser gets the stylesheet compressed, and it is the same stylesheet
	rec := ask(t, h, "/css/site.css", "Accept-Encoding", "gzip, deflate, br")
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("css for a browser: %d %v", rec.Code, rec.Header())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("css type: %q", ct)
	}
	if rec.Body.Len() >= len(css)/2 || unzip(t, rec.Body.Bytes()) != css {
		t.Fatalf("css was not compressed well: %d of %d bytes", rec.Body.Len(), len(css))
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the stylesheet")
	}
	// asking again with the fingerprint costs no body
	if rec := ask(t, h, "/css/site.css", "Accept-Encoding", "gzip", "If-None-Match", etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("revalidation: %d with %d bytes", rec.Code, rec.Body.Len())
	}
	// a client without gzip gets the plain file, under another fingerprint
	plain := ask(t, h, "/css/site.css")
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != css || plain.Header().Get("ETag") == etag || plain.Header().Get("ETag") == "" {
		t.Fatalf("css without gzip: %v", plain.Header())
	}
	if rec := ask(t, h, "/css/site.css", "Accept-Encoding", "gzip;q=0"); rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("gzip;q=0 means no gzip")
	}
	// a part of a file comes from the file as it is
	if rec := ask(t, h, "/css/site.css", "Accept-Encoding", "gzip", "Range", "bytes=0-3"); rec.Code != http.StatusPartialContent || rec.Body.String() != "body" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}

	// pages reached by their short address and by an alias are compressed too
	for _, p := range []string{"/installieren", "/fragen"} {
		rec := ask(t, h, p, "Accept-Encoding", "gzip")
		if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(unzip(t, rec.Body.Bytes()), "install") {
			t.Fatalf("%s: %d %v", p, rec.Code, rec.Header())
		}
	}
	// small files and pictures go out as they are, with a fingerprint
	if rec := ask(t, h, "/", "Accept-Encoding", "gzip"); rec.Code != 200 || rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("ETag") == "" || !strings.Contains(rec.Body.String(), "one") {
		t.Fatalf("index: %d %v", rec.Code, rec.Header())
	}
	pic := ask(t, h, "/img/mobs/fox.png", "Accept-Encoding", "gzip")
	if pic.Header().Get("Content-Encoding") != "" || pic.Body.String() != png || pic.Header().Get("ETag") == "" {
		t.Fatalf("picture: %v", pic.Header())
	}

	// pages say what they may load; pictures and styles do not carry that
	if rec := ask(t, h, "/installieren"); rec.Header().Get("Content-Security-Policy") != Policy || rec.Header().Get("X-Frame-Options") != "SAMEORIGIN" || rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatalf("a page without its policy: %v", rec.Header())
	}
	if rec := ask(t, h, "/img/mobs/fox.png"); rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("a picture needs no policy")
	}

	// how long a browser keeps what
	for p, want := range map[string]string{
		"/":                      "public, max-age=300",
		"/installieren":          "public, max-age=300",
		"/css/site.css":          "public, max-age=3600",
		"/css/site.css?v=abc123": "public, max-age=31536000, immutable",
		"/img/mobs/fox.png":      "public, max-age=86400, stale-while-revalidate=604800",
		"/img/mobs/fox.png?v=1":  "public, max-age=31536000, immutable",
		"/fonts/a.woff2":         "public, max-age=2592000",
	} {
		if got := ask(t, h, p).Header().Get("Cache-Control"); got != want {
			t.Errorf("Cache-Control of %s: %q, want %q", p, got, want)
		}
	}

	// the next release changes one page: everything else keeps its fingerprint, so
	// browsers keep what they have
	files[2].body = page + "faq, new"
	g.tag, g.arch = "v1.1.0", archive(t, files)
	if _, changed, err := s.Sync(context.Background()); err != nil || !changed {
		t.Fatalf("second release: %v %v", changed, err)
	}
	if rec := ask(t, h, "/css/site.css", "Accept-Encoding", "gzip", "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Fatalf("an unchanged stylesheet after a release: %d", rec.Code)
	}
	if rec := ask(t, h, "/img/mobs/fox.png", "If-None-Match", pic.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Fatalf("an unchanged picture after a release: %d", rec.Code)
	}
	old := ask(t, h, "/fragen", "Accept-Encoding", "gzip")
	if !strings.Contains(unzip(t, old.Body.Bytes()), "faq, new") {
		t.Fatal("the changed page is not the new one")
	}

	// after a restart the catalog is rebuilt from disk
	again := &Site{Repo: "kronwerke/website", API: srv.URL, Dir: s.Dir, UserAgent: "test"}
	again.Load()
	if rec := ask(t, again.Handler(), "/css/site.css", "Accept-Encoding", "gzip", "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Fatalf("after a restart: %d", rec.Code)
	}
}
