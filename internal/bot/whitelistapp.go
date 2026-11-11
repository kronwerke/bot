package bot

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/wlweb"
)

// The whitelist app: one web page for managing whitelist slots, as a Discord Activity
// (started with /whitelist verwalten) and at kronwerke.com/whitelist/ for everyone outside
// Discord. Both sign in with Discord: inside Discord through the Embedded App SDK, which
// hands over a code; on the website through the usual OAuth redirect. Either way the bot
// swaps the code for the person's Discord id and from then on uses the same rules as the
// slash commands.
//
// Discord serves an Activity from <client id>.discordsays.com and maps "/" to
// kronwerke.com/whitelist, so the page uses relative addresses only and works in both places.

const (
	appPath       = "/whitelist/"
	sessionTTL    = 12 * time.Hour
	sessionMax    = 5000
	memberTTL     = 30 * time.Second
	membersTTL    = 2 * time.Minute
	findPerMinute = 90
	moreEvery     = 24 * time.Hour
	idSlotsAdd    = "kw:slots:add:" // + n + ":" + discord id
	idSlotsNo     = "kw:slots:no:"  // + discord id
)

type appSession struct {
	user  string
	exp   time.Time
	finds []time.Time
}

type cachedMember struct {
	m  discord.Member
	at time.Time
}

// wlApp holds what the whitelist app keeps in memory. Sessions are lost on a restart;
// the page then signs in again by itself.
type wlApp struct {
	mu       sync.Mutex
	sessions map[string]*appSession
	members  map[string]cachedMember
	all      []discord.Member
	allAt    time.Time
	asked    map[string]time.Time // discord id -> last "mehr Plätze" request
	heads    *heads
	oauthAPI string // Discord's API base for the token exchange and /users/@me
}

func (b *Bot) app() *wlApp {
	b.appOnce.Do(func() {
		base := discord.DefaultAPI
		if b.cfg.APIBase != "" {
			base = b.cfg.APIBase
		}
		b.wl = &wlApp{
			sessions: map[string]*appSession{}, members: map[string]cachedMember{},
			asked: map[string]time.Time{}, heads: newHeads(), oauthAPI: base,
		}
	})
	return b.wl
}

func (b *Bot) publicURL() string {
	if b.cfg.PublicURL != "" {
		return strings.TrimRight(b.cfg.PublicURL, "/")
	}
	return "https://kronwerke.com"
}

// routeApp adds the app's pages and API to the mux.
func (b *Bot) routeApp(mux *http.ServeMux) {
	a := b.app()
	mux.HandleFunc("GET /whitelist", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, appPath, http.StatusMovedPermanently)
	})
	mux.Handle("GET /whitelist/", http.StripPrefix("/whitelist", wlweb.Handler()))
	mux.HandleFunc("GET /whitelist/api/config", func(w http.ResponseWriter, r *http.Request) {
		appJSON(w, http.StatusOK, map[string]any{"client_id": b.appID, "ready": b.cfg.ClientSecret != "" && b.appID != ""})
	})
	mux.HandleFunc("POST /whitelist/api/token", b.appToken)
	mux.HandleFunc("GET /whitelist/login", b.appLogin)
	mux.HandleFunc("GET /whitelist/callback", b.appCallback)
	mux.HandleFunc("POST /whitelist/api/logout", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		delete(a.sessions, bearer(r))
		a.mu.Unlock()
		appJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /whitelist/api/me", b.withMember(b.appMe))
	mux.HandleFunc("GET /whitelist/api/find", b.withMember(b.appFind))
	mux.HandleFunc("POST /whitelist/api/add", b.withMember(b.appAdd))
	mux.HandleFunc("POST /whitelist/api/remove", b.withMember(b.appRemove))
	mux.HandleFunc("POST /whitelist/api/more", b.withMember(b.appMore))
	mux.HandleFunc("GET /whitelist/head/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(r.PathValue("name"), ".png")
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		png, found := a.heads.face(ctx, name)
		w.Header().Set("Content-Type", "image/png")
		if found {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		w.Write(png)
	})
}

func appJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func appError(w http.ResponseWriter, status int, msg string) {
	appJSON(w, status, map[string]any{"error": msg})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func randomToken() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---- signing in ----

// exchange swaps an OAuth code for the person's access token and Discord account.
func (b *Bot) exchange(ctx context.Context, code, redirect string) (token string, u discord.User, err error) {
	a := b.app()
	form := url.Values{
		"client_id": {b.appID}, "client_secret": {b.cfg.ClientSecret},
		"grant_type": {"authorization_code"}, "code": {code},
	}
	if redirect != "" {
		form.Set("redirect_uri", redirect)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.oauthAPI+"/oauth2/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", u, err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", u, fmt.Errorf("discord token exchange: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&t); err != nil || t.AccessToken == "" {
		return "", u, errors.New("discord token exchange: no token")
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, a.oauthAPI+"/users/@me", nil)
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	r2, err := client.Do(req)
	if err != nil {
		return "", u, err
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		return "", u, fmt.Errorf("discord /users/@me: %d", r2.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(r2.Body, 8192)).Decode(&u); err != nil || u.ID == "" {
		return "", u, errors.New("discord /users/@me: no user")
	}
	return t.AccessToken, u, nil
}

func (b *Bot) newSession(user string) string {
	a := b.app()
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if len(a.sessions) >= sessionMax {
		for k, s := range a.sessions {
			if now.After(s.exp) {
				delete(a.sessions, k)
			}
		}
		if len(a.sessions) >= sessionMax {
			a.sessions = map[string]*appSession{}
		}
	}
	tok := randomToken()
	a.sessions[tok] = &appSession{user: user, exp: now.Add(sessionTTL)}
	return tok
}

// appToken is the Activity's sign in: the SDK's code in, the access token the SDK needs
// for authenticate and our own session out.
func (b *Bot) appToken(w http.ResponseWriter, r *http.Request) {
	if b.cfg.ClientSecret == "" || b.appID == "" {
		appError(w, http.StatusServiceUnavailable, "Die Whitelist-App ist noch nicht eingerichtet.")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil || in.Code == "" {
		appError(w, http.StatusBadRequest, "Kein Code.")
		return
	}
	tok, u, err := b.exchange(r.Context(), in.Code, "")
	if err != nil {
		b.log.Warn("whitelist app sign in", "err", err)
		appError(w, http.StatusBadGateway, "Die Anmeldung bei Discord ging nicht. Versuch es gleich noch einmal.")
		return
	}
	appJSON(w, http.StatusOK, map[string]any{"access_token": tok, "session": b.newSession(u.ID)})
}

const stateCookie = "__Host-kw_wl_state"

// appLogin sends a browser to Discord to sign in.
func (b *Bot) appLogin(w http.ResponseWriter, r *http.Request) {
	if b.cfg.ClientSecret == "" || b.appID == "" {
		http.Error(w, "Die Whitelist-App ist noch nicht eingerichtet.", http.StatusServiceUnavailable)
		return
	}
	state := randomToken()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/", MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	q := url.Values{
		"client_id": {b.appID}, "response_type": {"code"}, "scope": {"identify"},
		"redirect_uri": {b.publicURL() + "/whitelist/callback"}, "state": {state}, "prompt": {"none"},
	}
	http.Redirect(w, r, "https://discord.com/oauth2/authorize?"+q.Encode(), http.StatusFound)
}

// appCallback is where Discord sends the browser back. The session goes to the page in
// the fragment, which never reaches a server log.
func (b *Bot) appCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Redirect(w, r, appPath+"#fehler=state", http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, appPath+"#fehler=abgebrochen", http.StatusFound)
		return
	}
	_, u, err := b.exchange(r.Context(), code, b.publicURL()+"/whitelist/callback")
	if err != nil {
		b.log.Warn("whitelist app web sign in", "err", err)
		http.Redirect(w, r, appPath+"#fehler=discord", http.StatusFound)
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, appPath+"#s="+b.newSession(u.ID), http.StatusFound)
}

// ---- who is asking ----

// member returns a member of the Kronwerke guild, cached for a short while. ok is false
// when the person is not on the server.
func (b *Bot) member(ctx context.Context, id string, fresh bool) (discord.Member, bool, error) {
	a := b.app()
	a.mu.Lock()
	c, has := a.members[id]
	a.mu.Unlock()
	if has && !fresh && time.Since(c.at) < memberTTL {
		return c.m, c.m.User != nil, nil
	}
	m, err := b.rest.Member(ctx, b.guild(), id)
	if err != nil {
		if discord.IsStatus(err, http.StatusNotFound) {
			m = discord.Member{}
		} else {
			return m, false, err
		}
	}
	a.mu.Lock()
	if len(a.members) > 5000 {
		a.members = map[string]cachedMember{}
	}
	a.members[id] = cachedMember{m: m, at: time.Now()}
	a.mu.Unlock()
	return m, m.User != nil, nil
}

type memberHandler func(w http.ResponseWriter, r *http.Request, me *discord.Member, s *appSession)

func (b *Bot) withMember(h memberHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := b.app()
		tok := bearer(r)
		a.mu.Lock()
		s := a.sessions[tok]
		if s != nil && time.Now().After(s.exp) {
			delete(a.sessions, tok)
			s = nil
		}
		a.mu.Unlock()
		if s == nil {
			appError(w, http.StatusUnauthorized, "Bitte neu anmelden.")
			return
		}
		m, ok, err := b.member(r.Context(), s.user, r.Method == http.MethodPost)
		if err != nil {
			appError(w, http.StatusBadGateway, "Discord antwortet gerade nicht. Versuch es gleich noch einmal.")
			return
		}
		if !ok {
			appError(w, http.StatusForbidden, "Du bist nicht auf dem Kronwerke-Discord. Tritt zuerst bei, dann kannst du hier deine Whitelist verwalten.")
			return
		}
		h(w, r, &m, s)
	}
}

// nameOf is how the app names a Discord account: the name it has on the server.
func (b *Bot) nameOf(id string) string {
	a := b.app()
	a.mu.Lock()
	c, ok := a.members[id]
	a.mu.Unlock()
	if ok && c.m.User != nil {
		return displayName(c.m)
	}
	for _, m := range b.allMembers(context.Background(), false) {
		if m.User != nil && m.User.ID == id {
			return displayName(m)
		}
	}
	return "jemand, der nicht mehr auf dem Discord ist"
}

func displayName(m discord.Member) string {
	if m.Nick != "" {
		return m.Nick
	}
	if m.User == nil {
		return ""
	}
	return m.User.Name()
}

// allMembers is the guild's member list, fetched at most every two minutes.
func (b *Bot) allMembers(ctx context.Context, refresh bool) []discord.Member {
	a := b.app()
	a.mu.Lock()
	all, at := a.all, a.allAt
	a.mu.Unlock()
	if all != nil && (!refresh || time.Since(at) < membersTTL) {
		return all
	}
	ms, err := b.rest.Members(ctx, b.guild())
	if err != nil {
		return all
	}
	a.mu.Lock()
	a.all, a.allAt = ms, time.Now()
	a.mu.Unlock()
	return ms
}

// ---- what the page shows ----

type appPlayer struct {
	Player      string `json:"player"`
	DiscordID   string `json:"discord_id"`
	DiscordName string `json:"discord_name"`
	Since       string `json:"since"`
}

func (b *Bot) appMe(w http.ResponseWriter, r *http.Request, me *discord.Member, _ *appSession) {
	appJSON(w, http.StatusOK, b.meView(me))
}

func (b *Bot) meView(me *discord.Member) map[string]any {
	id := me.User.ID
	out := map[string]any{
		"name": displayName(*me), "player": b.store.Link(id), "can_give": b.canGive(me),
		"team": b.isTeam(me), "verified": me.HasRole(b.setting("role.member")),
	}
	b.apiMu.Lock()
	season := b.season
	b.apiMu.Unlock()
	if season != nil && season.Start != "" && !season.Running {
		if t, err := time.ParseInLocation("2006-01-02T15:04", season.Start, berlin); err == nil {
			out["start"] = t.Unix()
		}
	}
	if g, ok := b.store.Grant(id); ok {
		out["own"] = map[string]any{"player": g.Player, "kind": g.Label()}
	}
	if held := b.store.InvitesOf(id); len(held) > 0 {
		out["held"] = map[string]any{"player": held[0].Player, "by": b.nameOf(held[0].StreamerID)}
	}
	var list []appPlayer
	for _, v := range b.store.InvitesBy(id) {
		list = append(list, appPlayer{Player: v.Player, DiscordID: v.DiscordID, DiscordName: b.nameOf(v.DiscordID), Since: v.Created.Format(time.RFC3339)})
	}
	out["players"] = list
	if total, ok := b.slotTotal(id); ok {
		out["slots"] = total
	}
	b.app().mu.Lock()
	if t, ok := b.app().asked[id]; ok && time.Since(t) < moreEvery {
		out["asked"] = t.Unix()
	}
	b.app().mu.Unlock()
	return out
}

// slotTotal is how many slots the member has, from the server's roster.
func (b *Bot) slotTotal(id string) (int, bool) {
	g, ok := b.store.Grant(id)
	if !ok || !b.serverConnected() {
		return 0, false
	}
	r, err := b.roster()
	if err != nil {
		return 0, false
	}
	for _, s := range r.Streamers {
		if (g.UUID != "" && strings.EqualFold(s.UUID, g.UUID)) || strings.EqualFold(s.Name, g.Player) {
			return s.Slots, true
		}
	}
	return 0, false
}

type findHit struct {
	DiscordID   string `json:"discord_id"`
	DiscordName string `json:"discord_name"`
	Username    string `json:"username"`
	Player      string `json:"player,omitempty"` // the name they linked
	State       string `json:"state"`            // ok, nolink, taken, unverified, self
	Note        string `json:"note,omitempty"`
}

// appFind searches the Discord for the person to add: by the Minecraft name they linked
// or by their name on Discord. A valid Minecraft name is also looked up at Mojang, for
// the preview.
func (b *Bot) appFind(w http.ResponseWriter, r *http.Request, me *discord.Member, s *appSession) {
	if !b.canGive(me) {
		appError(w, http.StatusForbidden, "Du hast keine Plätze zu vergeben.")
		return
	}
	a := b.app()
	a.mu.Lock()
	now := time.Now()
	keep := s.finds[:0]
	for _, t := range s.finds {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	s.finds = append(keep, now)
	busy := len(s.finds) > findPerMinute
	a.mu.Unlock()
	if busy {
		appError(w, http.StatusTooManyRequests, "Langsamer bitte, gleich geht es weiter.")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 || len(q) > 40 {
		appJSON(w, http.StatusOK, map[string]any{"hits": []findHit{}})
		return
	}
	lq := strings.ToLower(q)
	type cand struct {
		hit   findHit
		score int
	}
	seen := map[string]*cand{}
	var order []*cand
	add := func(m discord.Member, score int) {
		if m.User == nil || m.User.Bot {
			return
		}
		if c, ok := seen[m.User.ID]; ok {
			if score < c.score {
				c.score = score
			}
			return
		}
		c := &cand{hit: b.hitFor(m, me.User.ID), score: score}
		seen[m.User.ID] = c
		order = append(order, c)
	}
	byID := map[string]discord.Member{}
	for _, m := range b.allMembers(r.Context(), true) {
		if m.User == nil {
			continue
		}
		byID[m.User.ID] = m
		for _, n := range []string{m.Nick, m.User.GlobalName, m.User.Username} {
			ln := strings.ToLower(n)
			if n == "" {
				continue
			}
			if ln == lq {
				add(m, 1)
			} else if strings.HasPrefix(ln, lq) {
				add(m, 3)
			} else if strings.Contains(ln, lq) {
				add(m, 5)
			}
		}
	}
	for _, l := range b.store.Linked() {
		lp := strings.ToLower(l.Player)
		score := 0
		switch {
		case lp == lq:
			score = 0
		case strings.HasPrefix(lp, lq):
			score = 2
		case strings.Contains(lp, lq):
			score = 4
		default:
			continue
		}
		if m, ok := byID[l.DiscordID]; ok {
			add(m, score)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].score < order[j].score })
	hits := []findHit{}
	for i, c := range order {
		if i >= 8 {
			break
		}
		hits = append(hits, c.hit)
	}
	out := map[string]any{"hits": hits}
	if mcName.MatchString(q) {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		e, err := a.heads.lookup(ctx, q)
		cancel()
		switch {
		case err == nil:
			out["account"] = map[string]any{"name": e.name, "uuid": e.uuid}
		case errors.Is(err, errNoAccount):
			out["account"] = nil
		}
	}
	appJSON(w, http.StatusOK, out)
}

// hitFor says whether a member can get a slot from the actor, in the words of the app.
func (b *Bot) hitFor(m discord.Member, actor string) findHit {
	id := m.User.ID
	h := findHit{DiscordID: id, DiscordName: displayName(m), Username: m.User.Username, Player: b.store.Link(id), State: "ok"}
	switch {
	case id == actor:
		h.State, h.Note = "self", "Das bist du."
	case !m.HasRole(b.setting("role.member")):
		h.State, h.Note = "unverified", "Noch nicht verifiziert: Die Person muss erst auf dem Discord das Captcha lösen."
	default:
		if held := b.store.InvitesOf(id); len(held) > 0 {
			h.State, h.Note = "taken", fmt.Sprintf("Hat schon einen Platz als %s, von %s.", held[0].Player, b.nameOf(held[0].StreamerID))
		} else if g, ok := b.store.Grant(id); ok {
			h.State, h.Note = "taken", fmt.Sprintf("Hat einen eigenen Platz als %s (%s).", g.Player, g.Label())
		} else if h.Player == "" {
			h.State, h.Note = "nolink", "Hat noch keinen Minecraft-Namen mit /link hinterlegt. Du kannst ihn hier eintragen."
		} else if inv, ok := b.store.Invite(h.Player); ok {
			h.State, h.Note = "taken", fmt.Sprintf("%s hat schon einen Platz, von %s.", inv.Player, b.nameOf(inv.StreamerID))
		}
	}
	return h
}

// ---- changing the whitelist ----

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(v)
}

func plainName(id string, b *Bot) string { return b.nameOf(id) }

func (b *Bot) appAdd(w http.ResponseWriter, r *http.Request, me *discord.Member, _ *appSession) {
	var in struct {
		DiscordID string `json:"discord_id"`
		Player    string `json:"player"`
	}
	if err := readJSON(r, &in); err != nil || !isID(in.DiscordID) {
		appError(w, http.StatusBadRequest, "Wen denn?")
		return
	}
	if in.DiscordID == me.User.ID {
		appError(w, http.StatusBadRequest, "Dich selbst kannst du nicht eintragen. Dein eigener Platz kommt über /link.")
		return
	}
	target, ok, err := b.member(r.Context(), in.DiscordID, true)
	if err != nil {
		appError(w, http.StatusBadGateway, "Discord antwortet gerade nicht. Versuch es gleich noch einmal.")
		return
	}
	var tm *discord.Member
	if ok {
		tm = &target
	}
	who := func(id string) string { return plainName(id, b) }
	name, linked, err := b.checkInvite(me, tm, in.Player, who)
	if err != nil {
		appError(w, http.StatusConflict, err.Error())
		return
	}
	if linked == "" {
		// a typed name must be a real account, before it reaches the server
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		e, lerr := b.app().heads.lookup(ctx, name)
		cancel()
		if errors.Is(lerr, errNoAccount) {
			appError(w, http.StatusConflict, fmt.Sprintf("Einen Minecraft-Account **%s** gibt es nicht.", name))
			return
		}
		if lerr == nil {
			name = e.name
		}
	}
	if _, err := b.invite(r.Context(), *me.User, in.DiscordID, name, linked); err != nil {
		appError(w, http.StatusBadGateway, "Das ging nicht: "+err.Error())
		return
	}
	b.events.add("whitelist app: %s gives %s a slot", me.User.Username, name)
	appJSON(w, http.StatusOK, map[string]any{"message": fmt.Sprintf("**%s** ist auf der Whitelist.", name), "me": b.meView(me)})
}

func (b *Bot) appRemove(w http.ResponseWriter, r *http.Request, me *discord.Member, _ *appSession) {
	var in struct {
		Player string `json:"player"`
	}
	if err := readJSON(r, &in); err != nil || !mcName.MatchString(strings.TrimSpace(in.Player)) {
		appError(w, http.StatusBadRequest, "Welcher Name?")
		return
	}
	inv, err := b.checkRemove(me, in.Player)
	if err != nil {
		appError(w, http.StatusConflict, err.Error())
		return
	}
	if err := b.revoke(r.Context(), inv, "entfernt von "+displayName(*me)); err != nil {
		appError(w, http.StatusBadGateway, "Das ging nicht: "+err.Error())
		return
	}
	b.events.add("whitelist app: %s takes the slot of %s back", me.User.Username, inv.Player)
	appJSON(w, http.StatusOK, map[string]any{"message": fmt.Sprintf("**%s** ist von der Whitelist entfernt.", inv.Player), "me": b.meView(me)})
}

// appMore asks the team for more slots: a message in the control channel with buttons.
// Only the project leads decide; the answer reaches the asker as a DM.
func (b *Bot) appMore(w http.ResponseWriter, r *http.Request, me *discord.Member, _ *appSession) {
	id := me.User.ID
	g, ok := b.store.Grant(id)
	if !ok {
		appError(w, http.StatusForbidden, "Mehr Plätze gibt es nur für Streamer und Season-1-Spieler mit eigenem Platz.")
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	readJSON(r, &in)
	note := strings.TrimSpace(in.Note)
	if len([]rune(note)) > 300 {
		note = string([]rune(note)[:300])
	}
	a := b.app()
	a.mu.Lock()
	if t, ok := a.asked[id]; ok && time.Since(t) < moreEvery {
		a.mu.Unlock()
		appError(w, http.StatusTooManyRequests, "Du hast schon gefragt. Das Team meldet sich bei dir.")
		return
	}
	a.asked[id] = time.Now()
	a.mu.Unlock()
	used := len(b.store.InvitesBy(id))
	slots := "?"
	if total, ok := b.slotTotal(id); ok {
		slots = strconv.Itoa(total)
	}
	text := fmt.Sprintf("**%s** (<@%s>, %s als %s) möchte mehr Whitelist-Plätze. Belegt: %d von %s.", displayName(*me), id, g.Label(), g.Player, used, slots)
	if note != "" {
		text += "\n> " + strings.ReplaceAll(note, "\n", "\n> ")
	}
	ch := b.setting("channel.control")
	if ch == "" {
		appError(w, http.StatusServiceUnavailable, "Das Team ist gerade nicht erreichbar.")
		return
	}
	_, err := b.rest.SendMessage(r.Context(), ch, discord.MessageSend{
		Content: text, AllowedMentions: discord.NoMentions,
		Components: []discord.Component{discord.Row(
			discord.Button(idSlotsAdd+"2:"+id, "2 mehr", discord.ButtonSuccess),
			discord.Button(idSlotsAdd+"4:"+id, "4 mehr", discord.ButtonSuccess),
			discord.Button(idSlotsNo+id, "Ablehnen", discord.ButtonSecondary),
		)},
	})
	if err != nil {
		a.mu.Lock()
		delete(a.asked, id)
		a.mu.Unlock()
		appError(w, http.StatusBadGateway, "Die Anfrage ging nicht raus. Versuch es gleich noch einmal.")
		return
	}
	b.events.add("whitelist app: %s asks for more slots", me.User.Username)
	appJSON(w, http.StatusOK, map[string]any{"message": "Gefragt. Das Team entscheidet und schreibt dir auf Discord.", "me": b.meView(me)})
}

// decideSlots is a lead pressing a button under a request for more slots.
func (b *Bot) decideSlots(ctx context.Context, i *discord.Interaction, rest string, give bool) {
	if !b.isLead(i.Member) {
		b.reply(ctx, i, "Über mehr Plätze entscheidet die Projektleitung.")
		return
	}
	n, id := 0, rest
	if give {
		ns, idPart, ok := strings.Cut(rest, ":")
		v, err := strconv.Atoi(ns)
		if !ok || err != nil || v < 1 || v > 16 {
			return
		}
		n, id = v, idPart
	}
	content := ""
	if i.Message != nil {
		content = i.Message.Content
	}
	who := i.Actor()
	if !give {
		b.rest.Respond(ctx, i, discord.CallbackUpdateMessage, discord.MessageSend{Content: content + "\nAbgelehnt von " + who.Name() + ".", Components: []discord.Component{}, AllowedMentions: discord.NoMentions})
		if dm, err := b.rest.CreateDM(ctx, id); err == nil {
			b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{Content: "Zu deiner Frage nach mehr Whitelist-Plätzen: Diesmal gibt es keine zusätzlichen. Bei Fragen meld dich beim Team."})
		}
		return
	}
	g, ok := b.store.Grant(id)
	if !ok {
		b.reply(ctx, i, "Diese Person hat keinen eigenen Platz mehr.")
		return
	}
	b.rest.Respond(ctx, i, discord.CallbackDeferredUpdate, nil)
	out, err := b.console(fmt.Sprintf("kw admin bonus %s %d", g.Player, n))
	if err != nil || !strings.Contains(out, "now has") {
		b.fail(ctx, "Mehr Plätze für "+g.Player, fmt.Errorf("%s %v", strings.TrimSpace(out), errText(err)))
		return
	}
	b.store.AddBonus(id, n)
	total := ""
	if t, ok := b.slotTotal(id); ok {
		total = fmt.Sprintf(" Jetzt hast du %d.", t)
	}
	if i.Message != nil {
		b.rest.EditMessage(ctx, i.ChannelID, i.Message.ID, discord.MessageSend{
			Content: content + fmt.Sprintf("\n%d mehr, von %s.", n, who.Name()), Components: []discord.Component{}, AllowedMentions: discord.NoMentions,
		})
	}
	if dm, err := b.rest.CreateDM(ctx, id); err == nil {
		b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{Content: fmt.Sprintf("Du hast %d Whitelist-Plätze mehr bekommen.%s Vergeben kannst du sie wie bisher mit `/whitelist add` oder in der Whitelist-App.", n, total)})
	}
	b.audit(ctx, fmt.Sprintf("Mehr Plätze: %s (<@%s>) bekommt %d dazu, von %s.", g.Player, id, n, who.Name()))
	b.app().mu.Lock()
	delete(b.app().asked, id)
	b.app().mu.Unlock()
}

func isID(s string) bool {
	if len(s) < 15 || len(s) > 21 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ---- /whitelist verwalten ----

// openApp answers /whitelist verwalten with the Activity. Without Activities switched on
// in the developer portal Discord refuses that; then the answer is the website link.
func (b *Bot) openApp(ctx context.Context, i *discord.Interaction) {
	if b.cfg.ClientSecret != "" {
		if err := b.rest.Respond(ctx, i, discord.CallbackLaunchActivity, nil); err == nil {
			return
		} else {
			b.log.Warn("launch activity", "err", err)
		}
	}
	b.rest.Respond(ctx, i, discord.CallbackMessage, discord.MessageSend{
		Content: "Hier verwaltest du deine Whitelist im Browser, mit Skin-Köpfen und allem:",
		Flags:   discord.FlagEphemeral,
		Components: []discord.Component{discord.Row(
			discord.LinkButton(b.publicURL()+appPath, "Whitelist öffnen"),
		)},
	})
}
