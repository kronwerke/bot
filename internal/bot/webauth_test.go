package bot

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/mcauth"
	"github.com/kronwerke/bot/internal/store"
)

func TestCodeSignsTheBrowserInAndShowsTheWhitelistPlace(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{store: st, web: newWebAuth(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	b.link = b.newHub()
	st.PutGrant(store.Grant{Player: "Carla", DiscordID: "1", Kind: store.KindStreamer, Created: time.Now()})
	st.PutInvite(store.Invite{Player: "Ben", DiscordID: "3", StreamerID: "1", UUID: "bf8ccf0d-901d-4cdd-88b6-6af144428974", Created: time.Now()})
	mux := http.NewServeMux()
	b.routesWebAuth(mux)

	code, err := b.web.newCode(mcauth.Account{UUID: "bf8ccf0d901d4cdd88b66af144428974", Name: "Ben"})
	if err != nil || len(code) != 6 {
		t.Fatalf("code %q %v", code, err)
	}
	// a wrong code does nothing
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/auth/code", strings.NewReader(`{"code":"AAAAAA"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/auth/code", strings.NewReader(`{"code":"`+strings.ToLower(code)+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d %s", rec.Code, rec.Body)
	}
	cookie := rec.Result().Cookies()[0]
	if cookie.Name != webCookie || !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("cookie %+v", cookie)
	}
	// the code is used up
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/auth/code", strings.NewReader(`{"code":"`+code+`"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second use: %d", rec.Code)
	}

	req := httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var me struct {
		Name      string         `json:"name"`
		Whitelist map[string]any `json:"whitelist"`
	}
	json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Name != "Ben" || me.Whitelist["on"] != true || me.Whitelist["by"] != "Carla" {
		t.Fatalf("me: %s", rec.Body)
	}

	req = httptest.NewRequest("POST", "/api/auth/logout", nil)
	req.AddCookie(cookie)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	req = httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	// signed out is an answer, not an error: 200 with an empty name
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"name":""}` {
		t.Fatalf("after logout: %d %s", rec.Code, rec.Body)
	}
}

func TestWebsiteLinksDiscordLikeTheSlashCommand(t *testing.T) {
	b, d, _, h := newAppBot(t)
	mux := http.NewServeMux()
	mux.Handle("/whitelist/", h)
	b.routesWebAuth(mux)

	code, _ := b.web.newCode(mcauth.Account{UUID: "bf8ccf0d901d4cdd88b66af144428974", Name: "BobMC"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/auth/code", strings.NewReader(`{"code":"`+code+`"}`)))
	mc := rec.Result().Cookies()[0]

	// not signed in: no trip to Discord
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/auth/discord", nil))
	if rec.Header().Get("Location") != "/anmelden" {
		t.Fatalf("without a session: %s", rec.Header().Get("Location"))
	}

	req := httptest.NewRequest("GET", "/api/auth/discord", nil)
	req.AddCookie(mc)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	loc := rec.Header().Get("Location")
	if rec.Code != 302 || !strings.Contains(loc, "discord.com/oauth2/authorize") || !strings.Contains(loc, "state=mclink.") {
		t.Fatalf("start: %d %s", rec.Code, loc)
	}
	state := strings.SplitN(strings.SplitN(loc, "state=", 2)[1], "&", 2)[0]
	stateCookie := rec.Result().Cookies()[0]

	d.mu.Lock()
	d.who = wlBob
	d.mu.Unlock()
	req = httptest.NewRequest("GET", "/whitelist/callback?code=good&state="+state, nil)
	req.AddCookie(mc)
	req.AddCookie(stateCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if loc = rec.Header().Get("Location"); !strings.HasPrefix(loc, "/anmelden#discord=Verkn") {
		t.Fatalf("callback: %s", loc)
	}
	if got := b.store.Link(wlBob); got != "BobMC" {
		t.Fatalf("link: %q", got)
	}

	req = httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(mc)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var me struct {
		Discord struct{ ID string } `json:"discord"`
	}
	json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Discord.ID != wlBob {
		t.Fatalf("me: %s", rec.Body)
	}

	// someone else's Discord account cannot take the name
	d.mu.Lock()
	d.who = wlCarl
	d.mu.Unlock()
	if msg, ok := b.linkWeb(req.Context(), discord.User{ID: wlCarl}, &discord.Member{User: &discord.User{ID: wlCarl}}, "BobMC"); ok {
		t.Fatalf("second account linked: %s", msg)
	}
}

func TestSessionLastsSixMonthsAndIsRenewedInUse(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	b := &Bot{store: st, web: newWebAuth(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	b.link = b.newHub()
	mux := http.NewServeMux()
	b.routesWebAuth(mux)
	// a session made five months ago, one month left
	old := time.Now().Add(30 * 24 * time.Hour)
	st.PutWebSession(tokenHash("tok-of-more-than-twenty-chars"), store.WebSession{UUID: "u", Name: "Ben", Created: time.Now().Add(-150 * 24 * time.Hour), Expires: old}, 5)
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: webCookie, Value: "tok-of-more-than-twenty-chars"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || len(rec.Result().Cookies()) != 1 || rec.Result().Cookies()[0].MaxAge < 180*24*3600 {
		t.Fatalf("renewal: %d %v", rec.Code, rec.Result().Cookies())
	}
	s, _ := st.WebSession(tokenHash("tok-of-more-than-twenty-chars"))
	if time.Until(s.Expires) < 180*24*time.Hour {
		t.Fatalf("session ends %v", s.Expires)
	}
}
