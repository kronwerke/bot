package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/store"
)

var mcName = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

var errNoServer = errors.New("der Minecraft-Server ist noch nicht mit dem Bot verbunden")

// placeFor says whether a member gets a place of their own on the whitelist when they
// link a name: streamers with Core's default slots (-1), Season 1 players with s1.slots.
func (b *Bot) placeFor(m *discord.Member) (kind string, slots int) {
	switch {
	case m.HasRole(b.setting("role.streamer")):
		return store.KindStreamer, -1
	case m.HasRole(b.setting("role.season1")):
		return store.KindSeason1, b.number("s1.slots", 2)
	}
	return "", 0
}

// linkMinecraft is /link. Everyone stores their name; streamers and Season 1 players are
// put on the whitelist with slots of their own at the same time.
func (b *Bot) linkMinecraft(ctx context.Context, i *discord.Interaction) {
	u := i.Actor()
	o, _ := i.Data.Option("name")
	name := strings.TrimSpace(o.String())
	if !mcName.MatchString(name) {
		b.reply(ctx, i, "Das ist kein gültiger Minecraft-Name (3 bis 16 Zeichen, Buchstaben, Zahlen, Unterstrich).")
		return
	}
	g, has := b.store.Grant(u.ID)
	if has && strings.EqualFold(g.Player, name) {
		b.reply(ctx, i, fmt.Sprintf("**%s** ist schon auf der Whitelist.", g.Player))
		return
	}
	if has && len(b.store.InvitesBy(u.ID)) > 0 {
		b.reply(ctx, i, fmt.Sprintf("Du bist als **%s** eingetragen und hast schon Plätze vergeben. Einen neuen Namen trägt das Team für dich ein.", g.Player))
		return
	}
	kind, slots := b.placeFor(i.Member)
	if kind == "" {
		b.store.SetLink(u.ID, name)
		b.events.add("link %s -> %s", u.Username, name)
		b.reply(ctx, i, fmt.Sprintf("Dein Minecraft-Name ist jetzt **%s**. Auf die Whitelist kommst du, wenn dir ein Streamer einen Platz gibt.", name))
		return
	}
	if inv, ok := b.store.Invite(name); ok {
		b.reply(ctx, i, fmt.Sprintf("**%s** hat schon einen Platz (von <@%s>).", inv.Player, inv.StreamerID))
		return
	}
	b.rest.Respond(ctx, i, discord.CallbackDeferredMessage, map[string]any{"flags": discord.FlagEphemeral})
	if has {
		if err := b.ungrant(ctx, g, "neuer Name "+name); err != nil {
			b.rest.EditReply(ctx, i, discord.MessageSend{Content: "Das ging nicht: " + err.Error()})
			return
		}
	}
	b.store.SetLink(u.ID, name)
	b.events.add("link %s -> %s", u.Username, name)
	if err := b.grant(ctx, u, name, kind, slots); err != nil {
		if errors.Is(err, errNoServer) {
			b.rest.EditReply(ctx, i, discord.MessageSend{Content: fmt.Sprintf(
				"Dein Minecraft-Name ist jetzt **%s**. Der Server ist noch nicht verbunden. Sobald er läuft, kommst du automatisch auf die Whitelist.", name)})
			return
		}
		b.rest.EditReply(ctx, i, discord.MessageSend{Content: fmt.Sprintf("**%s** ist gespeichert, aber die Whitelist ging nicht: %v", name, err)})
		return
	}
	text := fmt.Sprintf("**%s** ist auf der Whitelist. Mit `/whitelist add` gibst du deine Plätze an deine Leute.", name)
	if kind == store.KindSeason1 {
		text = fmt.Sprintf("Willkommen zurück! **%s** ist auf der Whitelist. Danke fürs Spielen in Season 1: Du hast %d eigene Plätze, die du mit `/whitelist add` vergeben kannst.", name, slots)
	}
	b.rest.EditReply(ctx, i, discord.MessageSend{Content: text})
}

// grant gives a member a place of their own on the server and on Discord.
func (b *Bot) grant(ctx context.Context, u discord.User, name, kind string, slots int) error {
	cmd := "kw admin grant " + name
	if slots >= 0 {
		cmd += fmt.Sprintf(" %d", slots)
	}
	if _, err := b.serverCommand(cmd); err != nil {
		return err
	}
	b.store.PutGrant(store.Grant{Player: name, DiscordID: u.ID, Kind: kind, Created: time.Now()})
	if role := b.setting("role.player"); role != "" {
		b.rest.AddRole(ctx, b.guild(), u.ID, role, "Own whitelist place ("+kind+")")
	}
	b.stats.invites.Add(1)
	line := fmt.Sprintf("🎥 **%s** (<@%s>) ist als Streamer auf der Whitelist.", name, u.ID)
	if kind == store.KindSeason1 {
		line = fmt.Sprintf("🎟️ **%s** (<@%s>) ist aus Season 1 wieder dabei, mit %d eigenen Plätzen.", name, u.ID, slots)
	}
	if ch := b.setting("channel.whitelist"); ch != "" {
		b.rest.SendMessage(ctx, ch, discord.MessageSend{Content: line, AllowedMentions: discord.NoMentions})
	}
	b.audit(ctx, fmt.Sprintf("Eigener Platz (%s): %s (<@%s>) als %s.", kind, u.Name(), u.ID, name))
	return nil
}

// syncPlaces gives every streamer and Season 1 player who linked a name but has no place
// yet (because the server was not connected at the time) their place now.
func (b *Bot) syncPlaces(ctx context.Context) (string, error) {
	members, err := b.rest.Members(ctx, b.guild())
	if err != nil {
		return "", err
	}
	var done, failed []string
	for _, m := range members {
		if m.User == nil || m.User.Bot {
			continue
		}
		name := b.store.Link(m.User.ID)
		if name == "" {
			continue
		}
		if _, ok := b.store.Grant(m.User.ID); ok {
			continue
		}
		kind, slots := b.placeFor(&m)
		if kind == "" {
			continue
		}
		if _, ok := b.store.Invite(name); ok {
			continue
		}
		if err := b.grant(ctx, *m.User, name, kind, slots); err != nil {
			if errors.Is(err, errNoServer) {
				return "", err
			}
			failed = append(failed, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		done = append(done, name)
	}
	out := fmt.Sprintf("Eigene Plätze nachgetragen: %d", len(done))
	if len(done) > 0 {
		out += " (" + strings.Join(done, ", ") + ")"
	}
	if len(failed) > 0 {
		out += "\nFehlgeschlagen: " + strings.Join(failed, ", ")
	}
	return out, nil
}

func (b *Bot) whitelistAdd(ctx context.Context, i *discord.Interaction, sub discord.CommandOption) {
	_, season1 := b.store.Grant(i.Actor().ID)
	if !i.Member.HasRole(b.setting("role.streamer")) && !b.isTeam(i.Member) && !season1 {
		b.reply(ctx, i, "Nur Streamer und Season-1-Spieler können Leute auf die Whitelist setzen. Trag dich zuerst mit `/link` ein.")
		return
	}
	streamer := i.Actor()
	streamerMC := b.store.Link(streamer.ID)
	if streamerMC == "" {
		b.reply(ctx, i, "Verknüpf zuerst deinen eigenen Minecraft-Namen mit `/link`.")
		return
	}
	var player, target string
	for _, o := range sub.Options {
		switch o.Name {
		case "player":
			player = strings.TrimSpace(o.String())
		case "user":
			target = o.String()
		}
	}
	if !mcName.MatchString(player) {
		b.reply(ctx, i, "Das ist kein gültiger Minecraft-Name.")
		return
	}
	var tm discord.Member
	if i.Data.Resolved != nil {
		if m, ok := i.Data.Resolved.Members[target]; ok {
			tm = m
		}
	}
	if !tm.HasRole(b.setting("role.member")) {
		b.reply(ctx, i, "Diese Person ist noch nicht verifiziert. Sie muss erst auf dem Discord das Captcha lösen.")
		return
	}
	if inv, ok := b.store.Invite(player); ok {
		b.reply(ctx, i, fmt.Sprintf("**%s** hat schon einen Platz (von <@%s>).", inv.Player, inv.StreamerID))
		return
	}
	b.rest.Respond(ctx, i, discord.CallbackDeferredMessage, map[string]any{"flags": discord.FlagEphemeral})
	out, err := b.serverCommand(fmt.Sprintf("kw admin invite %s %s", streamerMC, player))
	if err != nil {
		b.rest.EditReply(ctx, i, discord.MessageSend{Content: "Das ging nicht: " + err.Error()})
		return
	}
	b.store.PutInvite(store.Invite{Player: player, DiscordID: target, StreamerID: streamer.ID, Created: time.Now()})
	b.store.SetLink(target, player)
	if role := b.setting("role.player"); role != "" {
		b.rest.AddRole(ctx, b.guild(), target, role, "Whitelisted by "+streamer.Username)
	}
	b.stats.invites.Add(1)
	if ch := b.setting("channel.whitelist"); ch != "" {
		b.rest.SendMessage(ctx, ch, discord.MessageSend{
			Content:         fmt.Sprintf("➕ **%s** (<@%s>) ist auf der Whitelist, eingeladen von <@%s>.", player, target, streamer.ID),
			AllowedMentions: discord.NoMentions,
		})
	}
	if dm, err := b.rest.CreateDM(ctx, target); err == nil {
		b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{Content: fmt.Sprintf(
			"Du bist auf der Kronwerke-Whitelist, eingeladen von %s. Dein Minecraft-Name: **%s**. Wie du das Modpack installierst, steht auf dem Discord im Kanal zur Installation.", streamer.Name(), player)})
	}
	b.rest.EditReply(ctx, i, discord.MessageSend{Content: fmt.Sprintf("**%s** ist drin. %s", player, out)})
}

func (b *Bot) whitelistRemove(ctx context.Context, i *discord.Interaction, sub discord.CommandOption) {
	var player string
	for _, o := range sub.Options {
		if o.Name == "player" {
			player = strings.TrimSpace(o.String())
		}
	}
	inv, ok := b.store.Invite(player)
	if !ok {
		if g, ok := b.store.GrantByPlayer(player); ok {
			b.removeGrant(ctx, i, g)
			return
		}
		b.reply(ctx, i, "Für diesen Namen gibt es keinen Platz.")
		return
	}
	if inv.StreamerID != i.Actor().ID && !b.isTeam(i.Member) {
		b.reply(ctx, i, "Du kannst nur Leute entfernen, die du selbst eingeladen hast.")
		return
	}
	b.rest.Respond(ctx, i, discord.CallbackDeferredMessage, map[string]any{"flags": discord.FlagEphemeral})
	if err := b.revoke(ctx, inv, "entfernt von "+i.Actor().Name()); err != nil {
		b.rest.EditReply(ctx, i, discord.MessageSend{Content: "Das ging nicht: " + err.Error()})
		return
	}
	b.rest.EditReply(ctx, i, discord.MessageSend{Content: fmt.Sprintf("**%s** ist von der Whitelist entfernt.", inv.Player)})
}

func (b *Bot) whitelistList(ctx context.Context, i *discord.Interaction) {
	me := i.Actor().ID
	all := b.isTeam(i.Member)
	var invs []store.Invite
	if all {
		invs = b.store.InvitesBy("")
	} else {
		invs = b.store.InvitesBy(me)
	}
	if len(invs) == 0 {
		b.reply(ctx, i, "Noch niemand eingetragen.")
		return
	}
	var lines []string
	for _, v := range invs {
		line := fmt.Sprintf("• **%s** (<@%s>)", v.Player, v.DiscordID)
		if all {
			line += fmt.Sprintf(", von <@%s>", v.StreamerID)
		}
		lines = append(lines, line)
	}
	b.reply(ctx, i, strings.Join(lines, "\n"))
}

// revoke takes a slot back on the server and on Discord.
func (b *Bot) revoke(ctx context.Context, inv store.Invite, why string) error {
	streamerMC := b.store.Link(inv.StreamerID)
	if _, err := b.serverCommand(fmt.Sprintf("kw admin revoke %s %s", streamerMC, inv.Player)); err != nil {
		return err
	}
	b.store.DeleteInvite(inv.Player)
	_, granted := b.store.Grant(inv.DiscordID)
	if role := b.setting("role.player"); role != "" && len(b.store.InvitesOf(inv.DiscordID)) == 0 && !granted {
		b.rest.RemoveRole(ctx, b.guild(), inv.DiscordID, role, "Whitelist slot removed")
	}
	if ch := b.setting("channel.whitelist"); ch != "" {
		b.rest.SendMessage(ctx, ch, discord.MessageSend{
			Content: fmt.Sprintf("➖ **%s** ist nicht mehr auf der Whitelist (%s).", inv.Player, why), AllowedMentions: discord.NoMentions,
		})
	}
	return nil
}

// removeGrant is /whitelist remove on a place of one's own: the team or the player themselves.
func (b *Bot) removeGrant(ctx context.Context, i *discord.Interaction, g store.Grant) {
	if g.DiscordID != i.Actor().ID && !b.isTeam(i.Member) {
		b.reply(ctx, i, "Diesen Platz kann nur das Team oder der Spieler selbst zurückgeben.")
		return
	}
	b.rest.Respond(ctx, i, discord.CallbackDeferredMessage, map[string]any{"flags": discord.FlagEphemeral})
	if err := b.ungrant(ctx, g, "entfernt von "+i.Actor().Name()); err != nil {
		b.rest.EditReply(ctx, i, discord.MessageSend{Content: "Das ging nicht: " + err.Error()})
		return
	}
	b.rest.EditReply(ctx, i, discord.MessageSend{Content: fmt.Sprintf("**%s** ist von der Whitelist entfernt, mit allen Plätzen, die vergeben waren.", g.Player)})
}

// ungrant takes a place of one's own back: first every slot the player gave, then the place.
func (b *Bot) ungrant(ctx context.Context, g store.Grant, why string) error {
	for _, inv := range b.store.InvitesBy(g.DiscordID) {
		if err := b.revoke(ctx, inv, why); err != nil {
			return err
		}
	}
	if _, err := b.serverCommand("kw admin ungrant " + g.Player); err != nil {
		return err
	}
	b.store.DeleteGrant(g.DiscordID)
	if role := b.setting("role.player"); role != "" && len(b.store.InvitesOf(g.DiscordID)) == 0 {
		b.rest.RemoveRole(ctx, b.guild(), g.DiscordID, role, "Own whitelist place removed")
	}
	if ch := b.setting("channel.whitelist"); ch != "" {
		b.rest.SendMessage(ctx, ch, discord.MessageSend{
			Content: fmt.Sprintf("➖ **%s** ist nicht mehr auf der Whitelist (%s).", g.Player, why), AllowedMentions: discord.NoMentions,
		})
	}
	return nil
}

// onMemberLeave enforces the rule "no Discord, no slot".
func (b *Bot) onMemberLeave(ctx context.Context, u discord.User) {
	b.events.add("leave %s (%s)", u.Username, u.ID)
	if g, ok := b.store.Grant(u.ID); ok {
		if err := b.ungrant(ctx, g, "hat den Discord verlassen"); err != nil {
			b.fail(ctx, fmt.Sprintf("Eigenen Platz von %s nach dem Verlassen entfernen", g.Player), err)
		}
	}
	for _, inv := range b.store.InvitesOf(u.ID) {
		if err := b.revoke(ctx, inv, "hat den Discord verlassen"); err != nil {
			b.fail(ctx, fmt.Sprintf("Whitelist-Platz von %s nach dem Verlassen entfernen", inv.Player), err)
			continue
		}
		if dm, err := b.rest.CreateDM(ctx, inv.StreamerID); err == nil {
			b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{Content: fmt.Sprintf(
				"%s hat den Kronwerke-Discord verlassen, der Whitelist-Platz für **%s** ist wieder frei.", u.Name(), inv.Player)})
		}
	}
}
