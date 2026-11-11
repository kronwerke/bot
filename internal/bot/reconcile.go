package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/store"
)

// reconcileHold is how many places one pass may take on its own. A longer list waits for
// "!sync apply": a half answer from Discord or a role renamed by mistake must not empty
// the whitelist.
const reconcileHold = 5

// reconcile makes the places follow Discord: "no Discord, no slot" for members whose leave
// the bot missed (it was restarting) or whose removal failed (the server was down), places
// for the streamer and Season 1 roles as they are now, and the UUIDs and current names of
// the accounts from Core. It runs when the bot is ready, when the server connects, every
// hour, and with !sync.
func (b *Bot) reconcile(ctx context.Context, apply bool) (string, error) {
	members, err := b.rest.Members(ctx, b.guild())
	if err != nil {
		return "", err
	}
	if len(members) == 0 {
		return "", fmt.Errorf("Discord gab keine Mitglieder zurück")
	}
	byID := map[string]*discord.Member{}
	for k := range members {
		if members[k].User != nil {
			byID[members[k].User.ID] = &members[k]
		}
	}
	var lines []string
	if out := b.learnAccounts(); out != "" {
		lines = append(lines, out)
	}

	type removal struct {
		label string
		do    func() error
	}
	var gone []removal
	for _, g := range b.store.Grants() {
		g := g
		m, ok := byID[g.DiscordID]
		switch {
		case !ok:
			gone = append(gone, removal{g.Player + " (eigener Platz, nicht mehr auf dem Discord)", func() error {
				return b.ungrant(ctx, g, "nicht mehr auf dem Discord")
			}})
		case b.lostRole(m, g):
			if g.RoleLost.IsZero() {
				b.markRoleLost(ctx, g)
				continue
			}
			if time.Since(g.RoleLost) < b.roleGrace() {
				continue
			}
			gone = append(gone, removal{g.Player + " (eigener Platz, Rolle " + g.Label() + " weg)", func() error {
				return b.ungrant(ctx, g, "Rolle "+g.Label()+" entzogen")
			}})
		case !g.RoleLost.IsZero():
			g.RoleLost = time.Time{}
			b.store.PutGrant(g)
		}
	}
	for _, inv := range b.store.InvitesBy("") {
		inv := inv
		if _, ok := byID[inv.DiscordID]; ok {
			continue
		}
		gone = append(gone, removal{inv.Player + " (nicht mehr auf dem Discord)", func() error {
			return b.revoke(ctx, inv, "nicht mehr auf dem Discord")
		}})
	}
	if len(gone) > 0 {
		var names []string
		for _, r := range gone {
			names = append(names, r.label)
		}
		if len(gone) > reconcileHold && !apply {
			lines = append(lines, fmt.Sprintf("%d Plätze müssten weg: %s. Das sind mehr als %d auf einmal, deshalb wartet der Bot auf `!sync apply`.",
				len(gone), strings.Join(names, ", "), reconcileHold))
		} else {
			var done, failed []string
			for _, r := range gone {
				if err := r.do(); err != nil {
					failed = append(failed, fmt.Sprintf("%s: %v", r.label, err))
					continue
				}
				done = append(done, r.label)
			}
			if len(done) > 0 {
				lines = append(lines, "Entfernt: "+strings.Join(done, ", ")+".")
			}
			if len(failed) > 0 {
				lines = append(lines, "Noch nicht entfernt (der nächste Abgleich versucht es wieder): "+strings.Join(failed, "; ")+".")
			}
		}
	}
	if out, err := b.syncPlaces(ctx); err == nil && !strings.HasPrefix(out, "Eigene Plätze nachgetragen: 0") {
		lines = append(lines, out)
	}
	return strings.Join(lines, "\n"), nil
}

const idRoleBack = "kw:roleback:" // + discord id

// roleGrace is how long a place waits for its role to come back (setting role.grace, 24h).
func (b *Bot) roleGrace() time.Duration {
	if d, err := time.ParseDuration(b.setting("role.grace")); err == nil && d >= 0 {
		return d
	}
	return 24 * time.Hour
}

// markRoleLost starts the grace period and tells the team, with a button that gives the role back.
func (b *Bot) markRoleLost(ctx context.Context, g store.Grant) {
	g.RoleLost = time.Now()
	b.store.PutGrant(g)
	n := len(b.store.InvitesBy(g.DiscordID))
	role := b.setting("role.season1")
	if g.Kind == store.KindStreamer {
		role = b.setting("role.streamer")
	}
	ch := b.setting("channel.control")
	if ch == "" {
		return
	}
	b.rest.SendMessage(ctx, ch, discord.MessageSend{
		Content: fmt.Sprintf("**%s** (<@%s>) hat die Rolle %s nicht mehr. Wenn sie bis <t:%d:f> nicht zurück ist, gehen der eigene Platz und %d vergebene Plätze von der Whitelist.",
			g.Player, g.DiscordID, g.Label(), g.RoleLost.Add(b.roleGrace()).Unix(), n),
		AllowedMentions: discord.NoMentions,
		Components: []discord.Component{discord.Row(
			discord.Button(idRoleBack+g.DiscordID+":"+role, "Rolle zurückgeben", discord.ButtonPrimary),
		)},
	})
}

// giveRoleBack is the team's button: the role comes back, and with it the place stays.
func (b *Bot) giveRoleBack(ctx context.Context, i *discord.Interaction, rest string) {
	if !b.isTeam(i.Member) {
		b.reply(ctx, i, "Das kann nur das Team.")
		return
	}
	id, role, ok := strings.Cut(rest, ":")
	if !ok {
		return
	}
	if err := b.rest.AddRole(ctx, b.guild(), id, role, "Role given back by "+i.Actor().Username); err != nil {
		b.reply(ctx, i, "Die Rolle ließ sich nicht zurückgeben: "+err.Error())
		return
	}
	if g, ok := b.store.Grant(id); ok {
		g.RoleLost = time.Time{}
		b.store.PutGrant(g)
	}
	content := ""
	if i.Message != nil {
		content = i.Message.Content
	}
	b.rest.Respond(ctx, i, discord.CallbackUpdateMessage, discord.MessageSend{Content: content + "\nRolle zurückgegeben von " + i.Actor().Name() + ", der Platz bleibt.", Components: []discord.Component{}, AllowedMentions: discord.NoMentions})
}

// lostRole says whether a place of one's own no longer has the role it was given for.
// Team members keep theirs; a kind whose role is not set up is left alone.
func (b *Bot) lostRole(m *discord.Member, g store.Grant) bool {
	if b.isTeam(m) {
		return false
	}
	role := b.setting("role.season1")
	if g.Kind == store.KindStreamer {
		role = b.setting("role.streamer")
	}
	if role == "" {
		return false
	}
	if m.HasRole(role) {
		return false
	}
	// a Season 1 player who became a streamer keeps the place, and the other way round
	return !m.HasRole(b.setting("role.streamer")) && !m.HasRole(b.setting("role.season1"))
}

// learnAccounts takes the UUIDs and current names from Core's roster: entries from before
// the bot kept UUIDs get theirs, and a renamed account keeps its place under the new name.
func (b *Bot) learnAccounts() string {
	r, err := b.roster()
	if err != nil {
		return ""
	}
	return b.learnFrom(r)
}

func (b *Bot) learnFrom(r *rosterView) string {
	byName := map[string]string{} // lower name -> uuid
	nameOf := map[string]string{} // uuid -> current name
	for _, s := range r.Streamers {
		byName[strings.ToLower(s.Name)] = strings.ToLower(s.UUID)
		nameOf[strings.ToLower(s.UUID)] = s.Name
		for _, i := range s.Invited {
			byName[strings.ToLower(i.Name)] = strings.ToLower(i.UUID)
			nameOf[strings.ToLower(i.UUID)] = i.Name
		}
	}
	var renamed []string
	for _, g := range b.store.Grants() {
		changed := false
		if g.UUID == "" {
			if id := byName[strings.ToLower(g.Player)]; id != "" {
				g.UUID, changed = id, true
			}
		}
		if now := nameOf[g.UUID]; g.UUID != "" && now != "" && now != g.Player {
			renamed = append(renamed, g.Player+" heißt jetzt "+now)
			g.Player, changed = now, true
			b.store.SetLink(g.DiscordID, now)
		}
		if changed {
			b.store.PutGrant(g)
		}
	}
	for _, inv := range b.store.InvitesBy("") {
		id := inv.UUID
		if id == "" {
			id = byName[strings.ToLower(inv.Player)]
		}
		if id == "" {
			continue
		}
		now := nameOf[id]
		if now == "" {
			now = inv.Player
		}
		if id != inv.UUID || now != inv.Player {
			if now != inv.Player {
				renamed = append(renamed, inv.Player+" heißt jetzt "+now)
			}
			b.store.RenameInvite(inv.Player, now, id)
		}
	}
	if len(renamed) == 0 {
		return ""
	}
	return "Neue Minecraft-Namen übernommen: " + strings.Join(renamed, ", ") + "."
}

// reconcileNow runs a pass and posts what it changed.
func (b *Bot) reconcileNow(ctx context.Context) {
	if !b.serverConnected() || !b.ready.Load() {
		return
	}
	out, err := b.reconcile(ctx, false)
	if err != nil {
		b.events.add("reconcile: %v", err)
		return
	}
	if out != "" {
		b.control(ctx, "Abgleich mit dem Discord:\n"+out)
	}
}

// onMemberUpdate follows role changes at once: a streamer or Season 1 role given to a
// member who linked a name brings their place, a role taken away takes it.
func (b *Bot) onMemberUpdate(ctx context.Context, m discord.Member) {
	if m.User == nil || m.User.Bot || !b.serverConnected() {
		return
	}
	if g, ok := b.store.Grant(m.User.ID); ok {
		// a role taken away by mistake, or one synced from Twitch that flickers, must not
		// throw out a streamer and all their viewers at once: the place goes only when the
		// role is still missing after the grace period (review R3-04)
		switch lost := b.lostRole(&m, g); {
		case lost && g.RoleLost.IsZero():
			b.markRoleLost(ctx, g)
		case !lost && !g.RoleLost.IsZero():
			g.RoleLost = time.Time{}
			b.store.PutGrant(g)
			b.control(ctx, fmt.Sprintf("%s (<@%s>) hat die Rolle wieder, der Platz bleibt.", g.Player, g.DiscordID))
		}
		return
	}
	name := b.store.Link(m.User.ID)
	if name == "" {
		return
	}
	kind, slots := b.placeFor(&m)
	if kind == "" {
		return
	}
	if inv, ok := b.store.Invite(name); ok && inv.DiscordID != m.User.ID {
		return
	}
	if err := b.promote(ctx, *m.User, name, kind, slots); err != nil {
		b.fail(ctx, fmt.Sprintf("Eigenen Platz für %s nach der neuen Rolle geben", name), err)
	}
}
