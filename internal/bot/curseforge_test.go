package bot

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCurseProxyPassesOnlyAllowedCallsWithTheKey(t *testing.T) {
	var gotKey, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("x-api-key"), r.URL.Path+"?"+r.URL.RawQuery
		io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()
	p := newCurseProxy("secret")
	p.base = upstream.URL

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "1.2.3.4:5"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("POST", "/api/curseforge/v1/fingerprints/432", `{"fingerprints":[1]}`); rec.Code != 200 || gotKey != "secret" || gotPath != "/v1/fingerprints/432?" {
		t.Fatalf("fingerprints: %d %q %q", rec.Code, gotKey, gotPath)
	}
	if rec := do("GET", "/api/curseforge/v1/mods/238222/files", ""); rec.Code != 200 {
		t.Fatalf("files: %d", rec.Code)
	}
	if rec := do("GET", "/api/curseforge/v1/games", ""); rec.Code != 404 {
		t.Fatalf("games must not pass: %d", rec.Code)
	}
	if rec := do("GET", "/api/curseforge/v1/mods/search?gameId=1", ""); rec.Code != 400 {
		t.Fatalf("other games must not pass: %d", rec.Code)
	}
	if rec := do("DELETE", "/api/curseforge/v1/mods/1", ""); rec.Code != 404 {
		t.Fatalf("delete must not pass: %d", rec.Code)
	}
	gotPath = ""
	if rec := do("GET", "/api/curseforge/v1/mods/238222/files", ""); rec.Header().Get("X-Cache") != "hit" || gotPath != "" {
		t.Fatalf("second read should come from the cache")
	}
}

func TestCurseProxyBudget(t *testing.T) {
	p := newCurseProxy("k")
	now := time.Now()
	for i := 0; i < curseBudget; i++ {
		if !p.allow("9.9.9.9", now) {
			t.Fatalf("refused at %d", i)
		}
	}
	if p.allow("9.9.9.9", now) {
		t.Fatal("over budget should be refused")
	}
	if !p.allow("9.9.9.9", now.Add(curseWindow+time.Second)) {
		t.Fatal("budget should come back")
	}
	if !p.allow("8.8.8.8", now) {
		t.Fatal("other addresses have their own budget")
	}
}

func TestCurseProxyRoutesNextToTheSite(t *testing.T) {
	// the patterns must live next to "GET /" without a conflict (that panicked in 0.5.0)
	mux := http.NewServeMux()
	cp := newCurseProxy("k")
	mux.Handle("GET /api/curseforge/", cp)
	mux.Handle("POST /api/curseforge/", cp)
	mux.Handle("GET /", http.NotFoundHandler())
	req := httptest.NewRequest("GET", "/api/curseforge/v1/games", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "proxy") {
		t.Fatalf("the proxy should answer: %d %s", rec.Code, rec.Body.String())
	}
}
