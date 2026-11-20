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
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", rec.Code)
	}
}
