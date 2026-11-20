package site

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// asset is what the handler knows about one file of the site before it opens it.
type asset struct {
	etag string // from the content, so it survives a release that left the file alone
	gz   []byte // the file compressed, nil when that saves nothing
}

// catalog lists every file of one unpacked version by its address ("/css/site.css").
type catalog struct {
	files map[string]asset
	dirs  map[string]bool
}

// types are the files that shrink when compressed, with the type they are sent as.
var types = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".json": "application/json",
	".svg":  "image/svg+xml",
	".xml":  "text/xml; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
}

// pictures stay in the browser for a day and are then shown from the cache while the
// browser asks in the background whether they changed.
var pictures = map[string]bool{".webp": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".ico": true, ".avif": true}

// survey reads every file under root once: its fingerprint, and its compressed form when
// it is text. The quest book makes this about 10 000 files and a few megabytes of text.
func survey(root string) (*catalog, error) {
	c := &catalog{files: map[string]asset{}, dirs: map[string]bool{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := "/" + filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." {
				c.dirs[name] = true
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		a := asset{etag: hex.EncodeToString(sum[:10])}
		if _, ok := types[path.Ext(name)]; ok && len(b) >= 512 {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(b)
			zw.Close()
			if buf.Len() < len(b)*9/10 {
				a.gz = buf.Bytes()
			}
		}
		c.files[name] = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// find gives the file an address is answered with, the way noListing resolves it: the
// file itself, a folder's index.html, an alias, or the page without its ".html". Folders
// without a trailing slash and /index.html are left to the file server, which redirects.
func (c *catalog) find(p string) (string, asset, bool) {
	if strings.HasSuffix(p, "/") {
		name := path.Join(path.Clean(p), "index.html")
		a, ok := c.files[name]
		return name, a, ok
	}
	name := path.Clean(p)
	if strings.HasSuffix(name, "/index.html") || c.dirs[name] {
		return "", asset{}, false
	}
	if a, ok := c.files[name]; ok {
		return name, a, true
	}
	if alias, ok := aliases[name]; ok {
		a, ok := c.files[alias]
		return alias, a, ok
	}
	if path.Ext(name) == "" {
		a, ok := c.files[name+".html"]
		return name + ".html", a, ok
	}
	return "", asset{}, false
}

// lifetime is the Cache-Control of an address. An address with ?v= names one exact
// content (the build writes the file's fingerprint there), so it never has to be asked
// for again.
func lifetime(r *http.Request, name string) string {
	switch ext := path.Ext(name); {
	case r.URL.Query().Has("v"):
		return "public, max-age=31536000, immutable"
	case strings.HasPrefix(name, "/fonts/"):
		return "public, max-age=2592000"
	case pictures[ext]:
		return "public, max-age=86400, stale-while-revalidate=604800"
	case ext == ".css" || ext == ".js":
		return "public, max-age=3600"
	default:
		return "public, max-age=300"
	}
}

// Policy is what a page of the site may load and where it may be shown: its own files,
// pictures from anywhere over https (stream previews, avatars), and data from the three
// services its scripts ask (GitHub only as long as an old page is in a browser's cache). No script inside a page's markup runs, so text that ends up
// in a page by mistake cannot act for the visitor. Styles on tags stay allowed: the scenes
// set their sizes there.
const Policy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; font-src 'self'; " +
	"connect-src 'self' https://ttv.kronwerke.com https://discord.com https://api.github.com; " +
	"frame-src 'self'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'; object-src 'none'"

// guard sets the headers that protect a page; pictures and scripts do not need them.
func guard(h http.Header) {
	h.Set("Content-Security-Policy", Policy)
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
}

// serve answers from the catalog: the fingerprint as ETag, so a browser that still has
// the file gets a short 304, and text compressed for every browser that takes gzip. It
// reports false when the file server should answer instead.
func (c *catalog) serve(w http.ResponseWriter, r *http.Request) bool {
	name, a, ok := c.find(r.URL.Path)
	if !ok {
		w.Header().Set("Cache-Control", "public, max-age=300")
		guard(w.Header())
		return false
	}
	h := w.Header()
	h.Set("Cache-Control", lifetime(r, name))
	if strings.HasSuffix(name, ".html") {
		guard(h)
	}
	if a.gz == nil {
		h.Set("ETag", `"`+a.etag+`"`)
		return false
	}
	h.Add("Vary", "Accept-Encoding")
	// a part of a file is cut from the file as it is, not from its compressed form
	if r.Header.Get("Range") != "" || !takesGzip(r) {
		h.Set("ETag", `"`+a.etag+`"`)
		return false
	}
	h.Set("ETag", `"`+a.etag+`-gz"`)
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Type", types[path.Ext(name)])
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.gz))
	return true
}

func takesGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			q = strings.TrimSpace(q)
			return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
		}
	}
	return false
}
