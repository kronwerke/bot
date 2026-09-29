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

// serverCommand runs a Kronwerke Core admin command over RCON. Core answers
// "OK <text>" or "ERR <text>".
func (b *Bot) serverCommand(cmd string) (string, error) {
	if b.rcon == nil {
		return "", errNoServer
	}
	out, err := b.rcon.Command(cmd)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	switch {
	case strings.HasPrefix(out, "OK"):
		return strings.TrimSpace(strings.TrimPrefix(out, "OK")), nil
	case strings.HasPrefix(out, "ERR"):
		return "", errors.New(strings.TrimSpace(strings.TrimPrefix(out, "ERR")))
	default:
		return "", fmt.Errorf("unexpected answer from the server: %q", out)
	}
}

func (b *Bot) linkMinecraft(ctx context.Context, i *discord.Interaction) {
	o, _ := i.Data.Option("name")
	name := strings.TrimSpace(o.String())
	if !mcName.MatchString(name) {
		b.reply(ctx, i, "Das ist kein gültiger Minecraft-Name (3 bis 16 Zeichen, Buchstaben, Zahlen, Unterstrich).")
		return
	}
	b.store.SetLink(i.Actor().ID, name)
	b.events.add("link %s -> %s", i.Actor().Username, name)
	b.reply(ctx, i, fmt.Sprintf("Dein Minecraft-Name ist jetzt **%s**.", name))
}

func (b *Bot) whitelistAdd(ctx context.Context, i *discord.Interaction, sub discord.CommandOption) {
	if !i.Member.HasRole(b.setting("role.streamer")) && !b.isTeam(i.Member) {
		b.reply(ctx, i, "Nur Streamer können Leute auf die Whitelist setzen.")
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
		case "spieler":
			player = strings.TrimSpace(o.String())
		case "nutzer":
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
		if o.Name == "spieler" {
			player = strings.TrimSpace(o.String())
		}
	}
	inv, ok := b.store.Invite(player)
	if !ok {
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
	if role := b.setting("role.player"); role != "" && len(b.store.InvitesOf(inv.DiscordID)) == 0 {
		b.rest.RemoveRole(ctx, b.guild(), inv.DiscordID, role, "Whitelist slot removed")
	}
	if ch := b.setting("channel.whitelist"); ch != "" {
		b.rest.SendMessage(ctx, ch, discord.MessageSend{
			Content: fmt.Sprintf("➖ **%s** ist nicht mehr auf der Whitelist (%s).", inv.Player, why), AllowedMentions: discord.NoMentions,
		})
	}
	return nil
}

// onMemberLeave enforces the rule "no Discord, no slot".
func (b *Bot) onMemberLeave(ctx context.Context, u discord.User) {
	b.events.add("leave %s (%s)", u.Username, u.ID)
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
