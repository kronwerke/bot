package bot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// CurseForge proxy. Launchers ask here instead of CurseForge, and the bot adds its key, so
// the key never leaves this machine. Only the calls a server console needs pass, every
// address gets a budget, and answers to reads are kept for ten minutes.
//
//	POST /api/curseforge/v1/fingerprints/432    match jars by fingerprint
//	POST /api/curseforge/v1/mods                 several mods by id
//	GET  /api/curseforge/v1/mods/search          Minecraft only (gameId 432)
//	GET  /api/curseforge/v1/mods/{id}            one mod
//	GET  /api/curseforge/v1/mods/{id}/files      its files
//	GET  /api/curseforge/v1/mods/{id}/files/{f}  one file (downloadUrl is empty when the author forbids other apps)
const (
	curseAPI     = "https://api.curseforge.com"
	curseBudget  = 300 // requests per address per ten minutes
	curseWindow  = 10 * time.Minute
	curseMaxBody = 256 << 10
	// the cache holds at most this many bytes of answers; the oldest go first
	curseCacheBytes = 64 << 20
	// a single answer larger than this is passed on but not kept
	curseCacheItem = 2 << 20
)

var curseAllowed = []struct {
	method string
	re     *regexp.Regexp
}{
	{"POST", regexp.MustCompile(`^/v1/fingerprints/432$`)},
	{"POST", regexp.MustCompile(`^/v1/mods$`)},
	{"GET", regexp.MustCompile(`^/v1/mods/search$`)},
	{"GET", regexp.MustCompile(`^/v1/mods/[0-9]{1,10}$`)},
	{"GET", regexp.MustCompile(`^/v1/mods/[0-9]{1,10}/files$`)},
	{"GET", regexp.MustCompile(`^/v1/mods/[0-9]{1,10}/files/[0-9]{1,12}$`)},
}

type curseProxy struct {
	key    string
	base   string
	client *http.Client

	mu     sync.Mutex
	counts map[string][]time.Time
	cache  map[string]curseCached
	order  []string // cache keys, oldest first
	bytes  int
}

type curseCached struct {
	at     time.Time
	status int
	body   []byte
}

func newCurseProxy(key string) *curseProxy {
	return &curseProxy{key: key, base: curseAPI, client: &http.Client{Timeout: 30 * time.Second},
		counts: map[string][]time.Time{}, cache: map[string]curseCached{}}
}

// clientIP is the visitor as the reverse proxy in front of the bot saw it: the last entry
// of X-Forwarded-For, the one that proxy appended. Earlier entries come from the client and
// would let anyone pick a new address for every request.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Cloudflare, then Caddy on this machine: Caddy appends Cloudflare's edge to
	// X-Forwarded-For, so everyone behind one edge would share a budget. Cloudflare's own
	// header names the visitor and is overwritten by Cloudflare, and only Cloudflare reaches
	// Caddy (client certificate), so it is trusted when the request comes from loopback.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); net.ParseIP(cf) != nil {
			return cf
		}
	}
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		parts := strings.Split(f, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	return host
}

func (p *curseProxy) allow(ip string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.counts[ip]
	cut := now.Add(-curseWindow)
	i := 0
	for i < len(list) && list[i].Before(cut) {
		i++
	}
	list = list[i:]
	if len(list) >= curseBudget {
		p.counts[ip] = list
		return false
	}
	p.counts[ip] = append(list, now)
	if len(p.counts) > 10000 {
		// forget quiet addresses so the map cannot grow without end
		for k, v := range p.counts {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(p.counts, k)
			}
		}
	}
	return true
}

func (p *curseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/curseforge")
	ok := false
	for _, a := range curseAllowed {
		if a.method == r.Method && a.re.MatchString(path) {
			ok = true
			break
		}
	}
	if !ok {
		http.Error(w, "not a call this proxy passes", http.StatusNotFound)
		return
	}
	if path == "/v1/mods/search" && r.URL.Query().Get("gameId") != "432" {
		http.Error(w, "Minecraft only (gameId=432)", http.StatusBadRequest)
		return
	}
	if !p.allow(clientIP(r), time.Now()) {
		w.Header().Set("Retry-After", "600")
		http.Error(w, "too many requests, try again in ten minutes", http.StatusTooManyRequests)
		return
	}
	var body []byte
	if r.Method == "POST" {
		b, err := io.ReadAll(io.LimitReader(r.Body, curseMaxBody+1))
		if err != nil || len(b) > curseMaxBody {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		body = b
	}
	sum := sha256.Sum256(body)
	key := r.Method + " " + path + "?" + r.URL.RawQuery + " " + hex.EncodeToString(sum[:])
	p.mu.Lock()
	c, hit := p.cache[key]
	p.mu.Unlock()
	if hit && time.Since(c.at) < curseWindow {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "hit")
		w.WriteHeader(c.status)
		w.Write(c.body)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.base+path+queryOf(r), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Header.Set("x-api-key", p.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "CurseForge does not answer", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	out, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		http.Error(w, "CurseForge answer broke off", http.StatusBadGateway)
		return
	}
	if res.StatusCode == http.StatusOK && len(out) <= curseCacheItem {
		p.keep(key, curseCached{at: time.Now(), status: res.StatusCode, body: out})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.StatusCode)
	w.Write(out)
}

// keep stores an answer and drops the oldest ones while the cache is over its byte budget.
func (p *curseProxy) keep(key string, c curseCached) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.cache[key]; ok {
		p.bytes -= len(old.body)
	} else {
		p.order = append(p.order, key)
	}
	p.cache[key] = c
	p.bytes += len(c.body)
	for p.bytes > curseCacheBytes && len(p.order) > 0 {
		k := p.order[0]
		p.order = p.order[1:]
		if old, ok := p.cache[k]; ok {
			p.bytes -= len(old.body)
			delete(p.cache, k)
		}
	}
}

func queryOf(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}
