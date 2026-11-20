package bot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/store"
)

// Linking Discord and Minecraft on the website: someone signed in with their Minecraft
// account (auth.kronwerke.com) signs in with Discord as well, and the bot links the two the
// way /link does in Discord, with the same rules for places on the whitelist. Discord sends
// the browser back to the whitelist app's callback (the one address the app has registered);
// a state that starts with linkState sends it on to webLinkCallback.

const linkState = "mclink."

// discordOf is the Discord account linked to a Minecraft name: by /link, or by a place
// given on Discord.
func (b *Bot) discordOf(name string) (id, shown string) {
	for _, m := range b.store.Linked() {
		if strings.EqualFold(m.Player, name) {
			return m.DiscordID, m.Name
		}
	}
	for _, g := range b.store.Grants() {
		if strings.EqualFold(g.Player, name) {
			return g.DiscordID, b.nameOf(g.DiscordID)
		}
	}
	return "", ""
}

// webLinkStart sends a signed in browser to Discord.
func (b *Bot) webLinkStart(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := b.webUser(r); !ok {
		http.Redirect(w, r, "/anmelden", http.StatusFound)
		return
	}
	if b.clientSecret() == "" || b.appID == "" {
		http.Redirect(w, r, "/anmelden#discord="+url.QueryEscape("Die Discord-Anmeldung ist gerade nicht eingerichtet."), http.StatusFound)
		return
	}
	state := linkState + randomToken()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/", MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	q := url.Values{
		"client_id": {b.appID}, "response_type": {"code"}, "scope": {"identify"},
		"redirect_uri": {b.publicURL() + "/whitelist/callback"}, "state": {state},
	}
	http.Redirect(w, r, "https://discord.com/oauth2/authorize?"+q.Encode(), http.StatusFound)
}

// webLinkCallback finishes it; the state was checked by appCallback.
func (b *Bot) webLinkCallback(w http.ResponseWriter, r *http.Request) {
	back := func(ok bool, msg string) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		k := "discord"
		if !ok {
			k = "discordfehler"
		}
		http.Redirect(w, r, "/anmelden#"+k+"="+url.QueryEscape(msg), http.StatusFound)
	}
	s, _, ok := b.webUser(r)
	if !ok {
		back(false, "Deine Minecraft-Anmeldung ist abgelaufen. Melde dich neu an und versuch es noch einmal.")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		back(false, "Die Discord-Anmeldung wurde abgebrochen.")
		return
	}
	_, u, err := b.exchange(r.Context(), code, b.publicURL()+"/whitelist/callback")
	if err != nil {
		b.log.Warn("website discord link", "err", err)
		back(false, "Die Anmeldung bei Discord ging nicht. Versuch es gleich noch einmal.")
		return
	}
	m, in, err := b.member(r.Context(), u.ID, true)
	if err != nil {
		back(false, "Discord antwortet gerade nicht. Versuch es gleich noch einmal.")
		return
	}
	if !in {
		back(false, "Dieser Discord-Account ist nicht auf dem Kronwerke-Discord. Tritt zuerst bei, dann klappt die Verknüpfung.")
		return
	}
	if m.User == nil {
		m.User = &u
	}
	msg, ok := b.linkWeb(r.Context(), u, &m, s.Name)
	b.log.Info("website discord link", "name", s.Name, "ok", ok)
	back(ok, msg)
}

// linkWeb is /link for the website. The one case it leaves to Discord is moving a
// streamer's place to another account, which needs the streamer to agree there.
func (b *Bot) linkWeb(ctx context.Context, u discord.User, m *discord.Member, name string) (string, bool) {
	if id, _ := b.discordOf(name); id != "" && id != u.ID {
		return fmt.Sprintf("%s ist schon mit einem anderen Discord-Account verknüpft. Melde dich dort mit /link um, oder frag das Team.", name), false
	}
	g, has := b.store.Grant(u.ID)
	if has && strings.EqualFold(g.Player, name) {
		b.store.SetLink(u.ID, name)
		return fmt.Sprintf("Verknüpft. %s ist schon auf der Whitelist.", g.Player), true
	}
	if has && len(b.store.InvitesBy(u.ID)) > 0 {
		return fmt.Sprintf("Du bist als %s eingetragen und hast schon Plätze vergeben. Einen neuen Namen trägt das Team für dich ein.", g.Player), false
	}
	kind, slots := b.placeFor(m)
	if held := b.store.InvitesOf(u.ID); kind == "" && len(held) > 0 {
		if !strings.EqualFold(held[0].Player, name) {
			return fmt.Sprintf("Dein Platz gehört zu %s. Um ihn auf %s umzuziehen, nutze /link im Discord, dann fragt der Bot deinen Streamer.", held[0].Player, name), false
		}
		b.store.SetLink(u.ID, name)
		b.store.SetLinkName(u.ID, displayName(*m), time.Now())
		go b.pushLinks()
		return fmt.Sprintf("Verknüpft. %s ist schon auf der Whitelist.", held[0].Player), true
	}
	if kind == "" {
		b.store.SetLink(u.ID, name)
		b.store.SetLinkName(u.ID, displayName(*m), time.Now())
		b.events.add("link (website) %s -> %s", u.Username, name)
		go b.pushLinks()
		return "Verknüpft. Auf die Whitelist kommst du, wenn dir ein Streamer einen Platz gibt.", true
	}
	if inv, ok := b.store.Invite(name); ok && inv.DiscordID != u.ID {
		return fmt.Sprintf("%s hat schon einen Platz von jemand anderem.", inv.Player), false
	}
	if has {
		if err := b.ungrant(ctx, g, "neuer Name "+name); err != nil {
			return "Das ging nicht: " + err.Error(), false
		}
	}
	b.store.SetLink(u.ID, name)
	b.store.SetLinkName(u.ID, displayName(*m), time.Now())
	b.events.add("link (website) %s -> %s", u.Username, name)
	defer func() { go b.pushLinks() }()
	if err := b.promote(ctx, u, name, kind, slots); err != nil {
		if errors.Is(err, errNoServer) {
			return "Verknüpft. Der Server ist noch nicht verbunden, sobald er läuft, kommst du automatisch auf die Whitelist.", true
		}
		return fmt.Sprintf("Verknüpft, aber die Whitelist ging nicht: %v", err), false
	}
	if kind == store.KindSeason1 {
		return fmt.Sprintf("Willkommen zurück! Verknüpft, und %s ist auf der Whitelist. Du hast %d eigene Plätze, die du im Discord mit /whitelist add vergibst.", name, slots), true
	}
	return "Verknüpft. Du bist auf der Whitelist und vergibst deine Plätze im Discord mit /whitelist add.", true
}
