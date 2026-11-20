package bot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kronwerke/bot/internal/mcauth"
	"github.com/kronwerke/bot/internal/store"
)

// Sign in on the website with a Minecraft account. The player joins auth.kronwerke.com
// (internal/mcauth, inside this bot), Mojang confirms the account, the disconnect screen
// shows a code, and the code typed on kronwerke.com/anmelden signs the browser in. No
// whitelist place is needed for that. Signed in, the website shows whether the account is
// on the whitelist and, in the quest book, what its team has done.

const (
	webCookie      = "__Host-kw_mc"
	webSessionTTL  = 30 * 24 * time.Hour
	webSessionsPer = 5 // per account; the oldest goes
	codeTTL        = 10 * time.Minute
	codeAlphabet   = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

type pendingCode struct {
	acc mcauth.Account
	exp time.Time
}

type webAuth struct {
	mu       sync.Mutex
	codes    map[string]pendingCode
	tries    map[string][]time.Time // per address, for the code form
	progress map[string]cachedProgress
	roster   cachedRoster
}

type cachedProgress struct {
	at   time.Time
	body json.RawMessage
}

type cachedRoster struct {
	at time.Time
	r  *rosterView
}

func newWebAuth() *webAuth {
	return &webAuth{codes: map[string]pendingCode{}, tries: map[string][]time.Time{}, progress: map[string]cachedProgress{}}
}

// newCode keeps the account under a fresh code for codeTTL.
func (a *webAuth) newCode(acc mcauth.Account) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for c, p := range a.codes {
		if now.After(p.exp) || p.acc.UUID == acc.UUID {
			delete(a.codes, c)
		}
	}
	if len(a.codes) > 5000 {
		return "", errors.New("zu viele offene Codes, versuch es gleich noch einmal")
	}
	for {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
		}
		code := string(b)
		if _, taken := a.codes[code]; !taken {
			a.codes[code] = pendingCode{acc: acc, exp: now.Add(codeTTL)}
			return code, nil
		}
	}
}

func (a *webAuth) takeCode(code string) (mcauth.Account, bool) {
	code = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), " ", ""))
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.codes[code]
	if !ok || time.Now().After(p.exp) {
		return mcauth.Account{}, false
	}
	delete(a.codes, code)
	return p.acc, true
}

// allowTry limits code attempts to 10 a minute per address.
func (a *webAuth) allowTry(addr string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	keep := a.tries[addr][:0]
	for _, t := range a.tries[addr] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 10 {
		a.tries[addr] = keep
		return false
	}
	a.tries[addr] = append(keep, now)
	if len(a.tries) > 10000 {
		a.tries = map[string][]time.Time{}
	}
	return true
}

// authLoop runs the sign in endpoint while the bot runs (setting auth.addr, "-" turns it off).
func (b *Bot) authLoop(ctx context.Context) {
	addr := b.setting("auth.addr")
	if addr == "" {
		return
	}
	s := &mcauth.Server{Addr: addr, Log: b.log, OnAuth: b.web.newCode}
	for ctx.Err() == nil {
		if err := s.ListenAndServe(ctx); err != nil {
			b.log.Warn("minecraft sign in endpoint", "addr", addr, "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Minute):
		}
	}
}

func clientAddr(r *http.Request) string {
	// the bot sits behind Caddy on loopback; Caddy appends the visitor's address last
	if host, _, _ := net.SplitHostPort(r.RemoteAddr); host == "127.0.0.1" || host == "::1" {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			parts := strings.Split(v, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func tokenHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// webUser is the signed in account of the request, if any.
func (b *Bot) webUser(r *http.Request) (store.WebSession, string, bool) {
	c, err := r.Cookie(webCookie)
	if err != nil || len(c.Value) < 20 {
		return store.WebSession{}, "", false
	}
	h := tokenHash(c.Value)
	s, ok := b.store.WebSession(h)
	return s, h, ok
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (b *Bot) routesWebAuth(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/code", b.authCode)
	mux.HandleFunc("POST /api/auth/logout", b.authLogout)
	mux.HandleFunc("GET /api/me", b.apiMe)
	mux.HandleFunc("GET /api/me/quests", b.apiMeQuests)
}

func (b *Bot) authCode(w http.ResponseWriter, r *http.Request) {
	if !b.web.allowTry(clientAddr(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Zu viele Versuche, warte eine Minute."})
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	acc, ok := b.web.takeCode(in.Code)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Der Code stimmt nicht oder ist abgelaufen."})
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no randomness"})
		return
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	if err := b.store.PutWebSession(tokenHash(tok), store.WebSession{UUID: acc.UUID, Name: acc.Name, Created: now, Expires: now.Add(webSessionTTL)}, webSessionsPer); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save the session"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: webCookie, Value: tok, Path: "/", MaxAge: int(webSessionTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	b.log.Info("website sign in", "name", acc.Name)
	writeJSON(w, http.StatusOK, map[string]string{"name": acc.Name, "uuid": acc.UUID})
}

func (b *Bot) authLogout(w http.ResponseWriter, r *http.Request) {
	if _, h, ok := b.webUser(r); ok {
		b.store.DeleteWebSession(h)
	}
	http.SetCookie(w, &http.Cookie{Name: webCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// whitelistOf says whether the account has a place and through whom: the bot's own
// records first (grants and streamer slots), then Core's roster for places the team gave in game.
func (b *Bot) whitelistOf(uuid, name string) map[string]any {
	same := func(u, n string) bool {
		if u != "" && strings.ReplaceAll(strings.ToLower(u), "-", "") == uuid {
			return true
		}
		return n != "" && strings.EqualFold(n, name)
	}
	for _, g := range b.store.Grants() {
		if same(g.UUID, g.Player) {
			return map[string]any{"on": true, "kind": g.Kind, "label": g.Label()}
		}
	}
	if i, ok := b.store.Invite(name); ok && same(i.UUID, i.Player) {
		by := ""
		if g, ok := b.store.Grant(i.StreamerID); ok {
			by = g.Player
		}
		return map[string]any{"on": true, "kind": "invite", "by": by}
	}
	if r := b.cachedRoster(); r != nil {
		for _, s := range r.Streamers {
			if same(s.UUID, s.Name) && s.Granted {
				return map[string]any{"on": true, "kind": s.Kind}
			}
			for _, p := range s.Invited {
				if same(p.UUID, p.Name) {
					return map[string]any{"on": true, "kind": "invite", "by": s.Name}
				}
			}
		}
	}
	return map[string]any{"on": false}
}

func (b *Bot) cachedRoster() *rosterView {
	b.web.mu.Lock()
	c := b.web.roster
	b.web.mu.Unlock()
	if c.r != nil && time.Since(c.at) < 5*time.Minute {
		return c.r
	}
	out, err := b.serverCommand("kw admin roster json")
	if err != nil {
		return c.r
	}
	var r rosterView
	if json.Unmarshal([]byte(out), &r) != nil {
		return c.r
	}
	b.web.mu.Lock()
	b.web.roster = cachedRoster{at: time.Now(), r: &r}
	b.web.mu.Unlock()
	return &r
}

func (b *Bot) apiMe(w http.ResponseWriter, r *http.Request) {
	s, _, ok := b.webUser(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": s.Name, "uuid": s.UUID, "whitelist": b.whitelistOf(s.UUID, s.Name)})
}

// apiMeQuests passes on what Core says about the team's quest progress, kept 30 seconds.
func (b *Bot) apiMeQuests(w http.ResponseWriter, r *http.Request) {
	s, _, ok := b.webUser(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	b.web.mu.Lock()
	c, hit := b.web.progress[s.UUID]
	b.web.mu.Unlock()
	if !hit || time.Since(c.at) > 30*time.Second {
		out, err := b.serverCommand("kw admin quests " + dashed(s.UUID) + " json")
		if err != nil {
			if strings.Contains(err.Error(), "unknown player") {
				writeJSON(w, http.StatusOK, map[string]any{"name": s.Name, "done": []string{}, "started": []string{}, "new": true})
				return
			}
			if hit {
				w.Header().Set("Content-Type", "application/json")
				w.Write(c.body)
				return
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Der Server antwortet gerade nicht."})
			return
		}
		var m map[string]any
		if json.Unmarshal([]byte(out), &m) != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad answer from the server"})
			return
		}
		m["name"] = s.Name
		body, _ := json.Marshal(m)
		c = cachedProgress{at: time.Now(), body: body}
		b.web.mu.Lock()
		if len(b.web.progress) > 2000 {
			b.web.progress = map[string]cachedProgress{}
		}
		b.web.progress[s.UUID] = c
		b.web.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(c.body)
}
