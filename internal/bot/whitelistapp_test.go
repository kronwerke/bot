package bot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/store"
)

const (
	wlStreamer = "810000000000000001"
	wlAlice    = "810000000000000002" // verified, linked Alice
	wlBob      = "810000000000000003" // verified, no name linked
	wlCarl     = "810000000000000004" // not verified
	wlStranger = "810000000000000009" // not on the server
)

// wlDiscord is the part of Discord the whitelist app talks to: OAuth, members, messages.
type wlDiscord struct {
	srv   *httptest.Server
	mu    sync.Mutex
	posts []string // method path body
	who   string   // the account the next sign in belongs to
}

func newWLDiscord(t *testing.T) *wlDiscord {
	d := &wlDiscord{who: wlStreamer}
	member := func(id, name string, roles ...string) map[string]any {
		return map[string]any{"user": map[string]any{"id": id, "username": strings.ToLower(name), "global_name": name}, "roles": roles}
	}
	members := map[string]map[string]any{
		wlStreamer: member(wlStreamer, "Streamerin", defaults["role.member"], defaults["role.streamer"]),
		wlAlice:    member(wlAlice, "Alice", defaults["role.member"]),
		wlBob:      member(wlBob, "Bobby", defaults["role.member"]),
		wlCarl:     member(wlCarl, "Carl"),
	}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.posts = append(d.posts, r.Method+" "+r.URL.Path+" "+string(body))
		who := d.who
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		g := "/guilds/" + defaults["guild"] + "/members"
		switch {
		case p == "/oauth2/token" && strings.Contains(string(body), "client_credentials"):
			if u, pw, _ := r.BasicAuth(); u != appID || pw != "right" {
				w.WriteHeader(401)
				w.Write([]byte(`{"error":"invalid_client"}`))
				return
			}
			w.Write([]byte(`{"access_token":"app","scope":"identify"}`))
		case p == "/oauth2/token":
			if !strings.Contains(string(body), "client_secret=shh") || !strings.Contains(string(body), "code=good") {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Write([]byte(`{"access_token":"at-` + who + `","scope":"identify"}`))
		case p == "/users/@me":
			fmt.Fprintf(w, `{"id":%q,"username":"x"}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-"))
		case p == g:
			var all []any
			for _, m := range members {
				all = append(all, m)
			}
			json.NewEncoder(w).Encode(all)
		case strings.HasPrefix(p, g+"/") && r.Method == "GET":
			m, ok := members[strings.TrimPrefix(p, g+"/")]
			if !ok {
				w.WriteHeader(404)
				w.Write([]byte(`{"message":"Unknown Member","code":10007}`))
				return
			}
			json.NewEncoder(w).Encode(m)
		case strings.HasSuffix(p, "/messages") && r.Method == "POST", p == "/users/@me/channels":
			w.Write([]byte(`{"id":"990000000000000001"}`))
		default:
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *wlDiscord) sent(part string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.posts {
		if strings.Contains(p, part) {
			return true
		}
	}
	return false
}

// fakeMojang knows Alice and Bobby, with a red face and a blue hat pixel.
func fakeMojang(t *testing.T) *httptest.Server {
	skin := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 8; y < 16; y++ {
		for x := 8; x < 16; x++ {
			skin.SetNRGBA(x, y, color.NRGBA{200, 30, 30, 255})
		}
	}
	skin.SetNRGBA(40, 8, color.NRGBA{20, 40, 220, 255}) // hat over the top left face pixel
	var buf bytes.Buffer
	png.Encode(&buf, skin)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/users/profiles/minecraft/"):
			name := strings.ToLower(strings.TrimPrefix(p, "/users/profiles/minecraft/"))
			ids := map[string]string{"alice": "0123456789abcdef0123456789abcdef", "bobby": "fedcba9876543210fedcba9876543210"}
			canon := map[string]string{"alice": "Alice", "bobby": "Bobby"}
			if ids[name] == "" {
				w.WriteHeader(404)
				return
			}
			fmt.Fprintf(w, `{"id":%q,"name":%q}`, ids[name], canon[name])
		case strings.HasPrefix(p, "/session/"):
			tex, _ := json.Marshal(map[string]any{"textures": map[string]any{"SKIN": map[string]string{"url": srv.URL + "/texture/abc"}}})
			fmt.Fprintf(w, `{"properties":[{"name":"textures","value":%q}]}`, base64.StdEncoding.EncodeToString(tex))
		case p == "/texture/abc":
			w.Write(buf.Bytes())
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAppBot(t *testing.T) (*Bot, *wlDiscord, *fakeRCON, http.Handler) {
	return newAppBotWith(t, "shh")
}

func newAppBotWith(t *testing.T, secret string) (*Bot, *wlDiscord, *fakeRCON, http.Handler) {
	d := newWLDiscord(t)
	rc := newFakeRCON(t)
	b, err := New(Config{
		Token: "tok", StatePath: filepath.Join(t.TempDir(), "state.json"), Version: "v0.0.0-test",
		APIBase: d.srv.URL, RCONAddr: rc.addr, RCONPassword: "pw", ClientSecret: secret, AdminToken: "admintok",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	b.appID = appID
	mj := fakeMojang(t)
	h := b.app().heads
	h.profileAPI, h.sessionAPI, h.textures = mj.URL+"/users/profiles/minecraft/", mj.URL+"/session/", mj.URL+"/texture/"
	b.store.PutGrant(store.Grant{Player: "StreamerMC", DiscordID: wlStreamer, Kind: store.KindStreamer, Created: time.Now()})
	b.store.SetLink(wlStreamer, "StreamerMC")
	b.store.SetLink(wlAlice, "Alice")
	mux := http.NewServeMux()
	b.routeApp(mux)
	mux.HandleFunc("POST /api/admin/oauth", b.adminOAuth)
	return b, d, rc, mux
}

func appCall(t *testing.T, h http.Handler, method, path, session string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if session != "" {
		req.Header.Set("Authorization", "Bearer "+session)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func signIn(t *testing.T, h http.Handler) string {
	t.Helper()
	code, out := appCall(t, h, "POST", "/whitelist/api/token", "", map[string]string{"code": "good"})
	if code != 200 || out["session"] == nil || out["access_token"] == nil {
		t.Fatalf("sign in: %d %v", code, out)
	}
	return out["session"].(string)
}

func TestWhitelistAppSignsInAndGivesAndTakesSlots(t *testing.T) {
	_, d, rc, h := newAppBot(t)

	if code, _ := appCall(t, h, "GET", "/whitelist/api/me", "", nil); code != 401 {
		t.Fatalf("without a session: %d", code)
	}
	if code, _ := appCall(t, h, "POST", "/whitelist/api/token", "", map[string]string{"code": "bad"}); code != 502 {
		t.Fatalf("a bad code: %d", code)
	}
	s := signIn(t, h)
	code, me := appCall(t, h, "GET", "/whitelist/api/me", s, nil)
	if code != 200 || me["can_give"] != true || me["own"] == nil {
		t.Fatalf("me: %d %v", code, me)
	}

	// find by the linked Minecraft name, with the Mojang preview
	_, found := appCall(t, h, "GET", "/whitelist/api/find?q=ali", s, nil)
	hits := found["hits"].([]any)
	if len(hits) == 0 || hits[0].(map[string]any)["discord_id"] != wlAlice || hits[0].(map[string]any)["state"] != "ok" {
		t.Fatalf("find ali: %v", found)
	}
	_, found = appCall(t, h, "GET", "/whitelist/api/find?q=Alice", s, nil)
	if acc, _ := found["account"].(map[string]any); acc == nil || acc["uuid"] != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("account preview: %v", found)
	}
	// unverified and without a name
	_, found = appCall(t, h, "GET", "/whitelist/api/find?q=carl", s, nil)
	if st := found["hits"].([]any)[0].(map[string]any)["state"]; st != "unverified" {
		t.Fatalf("carl: %v", found)
	}
	_, found = appCall(t, h, "GET", "/whitelist/api/find?q=bobby", s, nil)
	if st := found["hits"].([]any)[0].(map[string]any)["state"]; st != "nolink" {
		t.Fatalf("bobby: %v", found)
	}

	// add Alice by her linked name
	code, out := appCall(t, h, "POST", "/whitelist/api/add", s, map[string]string{"discord_id": wlAlice})
	if code != 200 || !strings.Contains(out["message"].(string), "Alice") {
		t.Fatalf("add alice: %d %v", code, out)
	}
	if !contains(rc.commands(), "kw admin invite StreamerMC Alice") || !d.sent("/users/@me/channels") {
		t.Fatalf("server or DM missing: %v", rc.commands())
	}
	// twice is refused, with a name and not a mention
	code, out = appCall(t, h, "POST", "/whitelist/api/add", s, map[string]string{"discord_id": wlAlice})
	if code != 409 || strings.Contains(out["error"].(string), "<@") {
		t.Fatalf("add alice again: %d %v", code, out)
	}
	// Bob has no linked name: a made up one is refused, a real one goes in with Mojang's spelling
	code, out = appCall(t, h, "POST", "/whitelist/api/add", s, map[string]string{"discord_id": wlBob, "player": "Nobody"})
	if code != 409 || !strings.Contains(out["error"].(string), "gibt es nicht") {
		t.Fatalf("add a name without account: %d %v", code, out)
	}
	code, out = appCall(t, h, "POST", "/whitelist/api/add", s, map[string]string{"discord_id": wlBob, "player": "bobby"})
	if code != 200 || !contains(rc.commands(), "kw admin invite StreamerMC Bobby") {
		t.Fatalf("add bobby: %d %v %v", code, out, rc.commands())
	}
	if players := out["me"].(map[string]any)["players"].([]any); len(players) != 2 {
		t.Fatalf("players after two adds: %v", players)
	}
	code, _ = appCall(t, h, "POST", "/whitelist/api/add", s, map[string]string{"discord_id": wlCarl, "player": "Alice2"})
	if code != 409 {
		t.Fatalf("unverified carl: %d", code)
	}

	// remove
	code, out = appCall(t, h, "POST", "/whitelist/api/remove", s, map[string]string{"player": "Alice"})
	if code != 200 || !containsPrefix(rc.commands(), "kw admin revoke StreamerMC Alice") {
		t.Fatalf("remove: %d %v %v", code, out, rc.commands())
	}

	// more slots: one request a day, with buttons in the control channel
	code, out = appCall(t, h, "POST", "/whitelist/api/more", s, map[string]string{"note": "Mehr Zuschauer"})
	if code != 200 || !d.sent("/channels/"+defaults["channel.control"]+"/messages") || !d.sent(idSlotsAdd+"2:"+wlStreamer) {
		t.Fatalf("more: %d %v", code, out)
	}
	if code, _ = appCall(t, h, "POST", "/whitelist/api/more", s, map[string]string{}); code != 429 {
		t.Fatalf("second request: %d", code)
	}
}

func TestWhitelistAppRefusesPeopleOutsideTheServerAndPlayersWithoutSlots(t *testing.T) {
	_, d, _, h := newAppBot(t)
	d.who = wlStranger
	s := signIn(t, h)
	if code, out := appCall(t, h, "GET", "/whitelist/api/me", s, nil); code != 403 {
		t.Fatalf("stranger: %d %v", code, out)
	}
	d.who = wlAlice
	s = signIn(t, h)
	code, me := appCall(t, h, "GET", "/whitelist/api/me", s, nil)
	if code != 200 || me["can_give"] != false {
		t.Fatalf("alice me: %d %v", code, me)
	}
	if code, _ := appCall(t, h, "GET", "/whitelist/api/find?q=bob", s, nil); code != 403 {
		t.Fatalf("alice find: %d", code)
	}
	if code, _ := appCall(t, h, "POST", "/whitelist/api/add", s, map[string]string{"discord_id": wlBob, "player": "Bobby"}); code != 409 {
		t.Fatalf("alice add: %d", code)
	}
	if code, _ := appCall(t, h, "POST", "/whitelist/api/more", s, map[string]string{}); code != 403 {
		t.Fatalf("alice more: %d", code)
	}
}

func TestWhitelistAppServesFacesAndThePage(t *testing.T) {
	_, _, _, h := newAppBot(t)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	rec := get("/whitelist/head/Alice")
	img, err := png.Decode(rec.Body)
	if err != nil || img.Bounds().Dx() != headSize {
		t.Fatalf("face: %v", err)
	}
	if c := color.NRGBAModel.Convert(img.At(0, 0)).(color.NRGBA); c.B != 220 {
		t.Fatalf("hat pixel not on top: %v", c)
	}
	if c := color.NRGBAModel.Convert(img.At(20, 20)).(color.NRGBA); c.R != 200 {
		t.Fatalf("face pixel: %v", c)
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=3600" {
		t.Fatalf("cache: %q", rec.Header().Get("Cache-Control"))
	}
	if rec := get("/whitelist/head/Nobody"); rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("placeholder: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	page := get("/whitelist/")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "app.js") || !strings.Contains(page.Header().Get("Content-Security-Policy"), "frame-ancestors") {
		t.Fatalf("page: %d", page.Code)
	}
	for _, f := range []string{"app.js", "app.css", "sdk.js", "font.woff2"} {
		if rec := get("/whitelist/" + f); rec.Code != 200 {
			t.Fatalf("%s: %d", f, rec.Code)
		}
	}
	if rec := get("/whitelist"); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("bare path: %d", rec.Code)
	}
}

func TestWhitelistAppWebLoginChecksState(t *testing.T) {
	_, _, _, h := newAppBot(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/whitelist/login", nil))
	loc := rec.Header().Get("Location")
	if rec.Code != 302 || !strings.Contains(loc, "discord.com/oauth2/authorize") || !strings.Contains(loc, "redirect_uri=https%3A%2F%2Fkronwerke.com%2Fwhitelist%2Fcallback") {
		t.Fatalf("login: %d %s", rec.Code, loc)
	}
	cookie := rec.Result().Cookies()[0]
	state := strings.SplitN(strings.SplitN(loc, "state=", 2)[1], "&", 2)[0]

	// a forged callback without the cookie
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/whitelist/callback?code=good&state="+state, nil))
	if !strings.Contains(rec.Header().Get("Location"), "fehler=state") {
		t.Fatalf("forged: %s", rec.Header().Get("Location"))
	}
	req := httptest.NewRequest("GET", "/whitelist/callback?code=good&state="+state, nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	loc = rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/whitelist/#s=") {
		t.Fatalf("callback: %s", loc)
	}
	if code, _ := appCall(t, h, "GET", "/whitelist/api/me", strings.TrimPrefix(loc, "/whitelist/#s="), nil); code != 200 {
		t.Fatalf("session from the web: %d", code)
	}
}

func TestSlotButtonsGiveBonusThatSurvivesANewWorld(t *testing.T) {
	b, _, rc, _ := newAppBot(t)
	n, ok, err := b.store.AddBonus(wlStreamer, 2)
	if err != nil || !ok || n != 2 {
		t.Fatalf("bonus: %d %v %v", n, ok, err)
	}
	if _, ok, _ := b.store.AddBonus(wlAlice, 2); ok {
		t.Fatal("a bonus without a grant")
	}
	// the roster of a new world knows nobody: restore grants again, bonus included
	if _, err := b.restorePlaces(true); err != nil {
		t.Fatal(err)
	}
	if !contains(rc.commands(), "kw admin bonus StreamerMC 2") {
		t.Fatalf("bonus not restored: %v", rc.commands())
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsPrefix(list []string, s string) bool {
	for _, x := range list {
		if strings.HasPrefix(x, s) {
			return true
		}
	}
	return false
}

func TestClientSecretOverTheAdminAPI(t *testing.T) {
	b, _, _, h := newAppBotWith(t, "")
	if code, _ := appCall(t, h, "POST", "/whitelist/api/token", "", map[string]string{"code": "good"}); code != 503 {
		t.Fatalf("without a secret: %d", code)
	}
	if code, _ := appCall(t, h, "POST", "/api/admin/oauth", "wrong", map[string]string{"client_secret": "right"}); code != 404 {
		t.Fatalf("without the admin token: %d", code)
	}
	if code, out := appCall(t, h, "POST", "/api/admin/oauth", "admintok", map[string]string{"client_secret": "wrong"}); code != 400 || b.clientSecret() != "" {
		t.Fatalf("a wrong secret: %d %v", code, out)
	}
	if code, out := appCall(t, h, "POST", "/api/admin/oauth", "admintok", map[string]string{"client_secret": "right"}); code != 200 || out["ready"] != true {
		t.Fatalf("the right secret: %d %v", code, out)
	}
	if b.clientSecret() != "right" {
		t.Fatal("not kept")
	}
}

func TestLinkToAnotherAccountNeedsTheStreamer(t *testing.T) {
	b, d, rc, _ := newAppBot(t)
	ctx := context.Background()
	alice := store.Invite{Player: "Alice", DiscordID: wlAlice, StreamerID: wlStreamer, UUID: "01234567-89ab-cdef-0123-456789abcdef", Created: time.Now()}
	b.store.PutInvite(alice)
	ia := &discord.Interaction{ID: "1", Token: "tok", ApplicationID: appID, GuildID: defaults["guild"]}
	u := discord.User{ID: wlAlice, Username: "alice"}

	// another account: the streamer is asked, nothing moves yet
	b.askMove(ctx, ia, u, alice, "bobby")
	if !d.sent(idMoveYes + wlAlice + ":Bobby") {
		t.Fatal("the streamer was not asked")
	}
	if containsPrefix(rc.commands(), "kw admin invite") {
		t.Fatalf("moved without the streamer: %v", rc.commands())
	}
	// once a day
	before := strings.Count(strings.Join(d.posts, "\n"), idMoveYes)
	b.askMove(ctx, ia, u, alice, "bobby")
	if strings.Count(strings.Join(d.posts, "\n"), idMoveYes) != before {
		t.Fatal("asked twice on one day")
	}
	// someone else pressing the button changes nothing
	other := &discord.Interaction{ID: "2", Token: "tok", ApplicationID: appID, User: &discord.User{ID: wlBob}, Message: &discord.Message{Content: "x"}}
	b.decideMove(ctx, other, wlAlice+":Bobby", true)
	if containsPrefix(rc.commands(), "kw admin invite") {
		t.Fatal("moved by someone who is not the streamer")
	}
	// the streamer agrees
	yes := &discord.Interaction{ID: "3", Token: "tok", ApplicationID: appID, User: &discord.User{ID: wlStreamer}, Message: &discord.Message{Content: "x"}}
	b.decideMove(ctx, yes, wlAlice+":Bobby", true)
	if !contains(rc.commands(), "kw admin invite StreamerMC Bobby") {
		t.Fatalf("not moved: %v", rc.commands())
	}
	if _, ok := b.store.Invite("Bobby"); !ok {
		t.Fatal("the store does not have the new name")
	}

	// the same account under a new name follows without asking
	b.store.PutInvite(store.Invite{Player: "OldName", DiscordID: wlCarl, StreamerID: wlStreamer, UUID: "fedcba98-7654-3210-fedc-ba9876543210", Created: time.Now()})
	n := len(rc.commands())
	held, _ := b.store.Invite("OldName")
	b.askMove(ctx, ia, discord.User{ID: wlCarl, Username: "carl"}, held, "bobby")
	if _, ok := b.store.Invite("OldName"); ok || len(rc.commands()) != n {
		t.Fatalf("a rename should only update the store: %v", rc.commands()[n:])
	}
}

func TestALostRoleWaitsBeforeThePlaceGoes(t *testing.T) {
	b, _, rc, _ := newAppBot(t)
	ctx := context.Background()
	noRole := discord.Member{User: &discord.User{ID: wlStreamer, Username: "streamerin"}, Roles: []string{defaults["role.member"]}}
	b.onMemberUpdate(ctx, noRole)
	g, _ := b.store.Grant(wlStreamer)
	if g.RoleLost.IsZero() || containsPrefix(rc.commands(), "kw admin ungrant") {
		t.Fatalf("the place went at once: %v", rc.commands())
	}
	// the role comes back: nothing happened
	b.onMemberUpdate(ctx, discord.Member{User: noRole.User, Roles: []string{defaults["role.member"], defaults["role.streamer"]}})
	if g, _ := b.store.Grant(wlStreamer); !g.RoleLost.IsZero() {
		t.Fatal("the mark stayed after the role came back")
	}
}

func TestTheBotFollowsChangesMadeInGame(t *testing.T) {
	b, _, _, _ := newAppBot(t)
	b.store.PutGrant(store.Grant{Player: "Other", DiscordID: wlCarl, Kind: store.KindStreamer, UUID: "33333333-3333-3333-3333-333333333333", Created: time.Now()})
	b.store.PutInvite(store.Invite{Player: "Alice", DiscordID: wlAlice, StreamerID: wlStreamer, UUID: "01234567-89ab-cdef-0123-456789abcdef", Created: time.Now()})
	b.store.PutInvite(store.Invite{Player: "Bobby", DiscordID: wlBob, StreamerID: wlStreamer, UUID: "fedcba98-7654-3210-fedc-ba9876543210", Created: time.Now()})
	r := &rosterView{Streamers: []rosterStreamer{
		{UUID: "11111111-1111-1111-1111-111111111111", Name: "StreamerMC", Granted: true, Slots: 7},
		// Bobby was moved to Other in the panel; Alice was taken back in game
		{UUID: "33333333-3333-3333-3333-333333333333", Name: "Other", Granted: true, Slots: 5,
			Invited: []rosterPlayer{{UUID: "fedcba98-7654-3210-fedc-ba9876543210", Name: "Bobby"}}},
	}}
	out := b.followRoster(r)
	if _, ok := b.store.Invite("Alice"); ok {
		t.Fatalf("a slot taken back in game stayed: %s", out)
	}
	if inv, _ := b.store.Invite("Bobby"); inv.StreamerID != wlCarl {
		t.Fatalf("the move was not followed: %+v", inv)
	}
	if g, _ := b.store.Grant(wlStreamer); g.Slots != 7 {
		t.Fatalf("slot count not kept: %d", g.Slots)
	}
	// a streamer the server does not know (a new world) keeps everything
	b.store.PutInvite(store.Invite{Player: "Kept", DiscordID: "810000000000000008", StreamerID: wlStreamer, Created: time.Now()})
	b.followRoster(&rosterView{})
	if _, ok := b.store.Invite("Kept"); !ok {
		t.Fatal("an empty roster removed an invite")
	}
}
