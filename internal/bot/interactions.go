package bot

import (
	"context"
	"strings"

	"github.com/kronwerke/bot/internal/discord"
)

func ptr[T any](v T) *T { return &v }

// commands are the slash commands, registered for the guild on every start.
func commands() []discord.ApplicationCommand {
	return []discord.ApplicationCommand{
		{
			Name: "apply", Description: "Als Streamer für Kronwerke bewerben",
			DMPermission: ptr(false),
		},
		{
			Name: "link", Description: "Deinen Minecraft-Namen hinterlegen (Streamer und Season-1-Spieler kommen damit auf die Whitelist)",
			DMPermission: ptr(false),
			Options: []discord.ApplicationCommandOption{
				{Type: discord.OptString, Name: "name", Description: "Dein Minecraft-Name", Required: true, MinLength: 3, MaxLength: 16},
			},
		},
		{
			Name: "whitelist", Description: "Whitelist-Plätze verwalten (Streamer und Season-1-Spieler)",
			DMPermission: ptr(false),
			Options: []discord.ApplicationCommandOption{
				{Type: discord.OptSubCommand, Name: "add", Description: "Jemanden auf die Whitelist setzen", Options: []discord.ApplicationCommandOption{
					{Type: discord.OptUser, Name: "user", Description: "Die Person auf dem Discord", Required: true},
					{Type: discord.OptString, Name: "player", Description: "Minecraft-Name, nur wenn die Person keinen mit /link hinterlegt hat", MinLength: 3, MaxLength: 16},
				}},
				{Type: discord.OptSubCommand, Name: "remove", Description: "Jemanden von der Whitelist nehmen", Options: []discord.ApplicationCommandOption{
					{Type: discord.OptString, Name: "player", Description: "Minecraft-Name", Required: true, MinLength: 3, MaxLength: 16},
				}},
				{Type: discord.OptSubCommand, Name: "list", Description: "Wen du eingetragen hast und wie viele Plätze frei sind"},
				{Type: discord.OptSubCommand, Name: "me", Description: "Bin ich auf der Whitelist? Unter welchem Namen, über wen, ab wann"},
				{Type: discord.OptSubCommand, Name: "verwalten", Description: "Die Whitelist-App öffnen: deine Leute mit Skin-Köpfen, eintragen und entfernen"},
			},
		},
	}
}

func (b *Bot) registerCommands(ctx context.Context) error {
	if b.appID == "" {
		return nil
	}
	return b.rest.SetGuildCommands(ctx, b.appID, b.guild(), commands())
}

func (b *Bot) onInteraction(ctx context.Context, i *discord.Interaction) {
	// the only buttons outside the server: a streamer's answer to a slot move, in a DM
	if i.GuildID == "" && i.Type == discord.InteractionComponent {
		switch id := i.Data.CustomID; {
		case strings.HasPrefix(id, idMoveYes):
			b.decideMove(ctx, i, strings.TrimPrefix(id, idMoveYes), true)
		case strings.HasPrefix(id, idMoveNo):
			b.decideMove(ctx, i, strings.TrimPrefix(id, idMoveNo), false)
		}
		return
	}
	if i.GuildID != b.guild() {
		return
	}
	b.stats.commands.Add(1)
	switch i.Type {
	case discord.InteractionCommand:
		b.events.add("/%s by %s", i.Data.Name, i.Actor().Username)
		switch i.Data.Name {
		case "apply":
			b.openApplication(ctx, i)
		case "link":
			b.linkMinecraft(ctx, i)
		case "whitelist":
			if len(i.Data.Options) == 0 {
				return
			}
			sub := i.Data.Options[0]
			switch sub.Name {
			case "add":
				b.whitelistAdd(ctx, i, sub)
			case "remove":
				b.whitelistRemove(ctx, i, sub)
			case "list":
				b.whitelistList(ctx, i)
			case "me":
				b.whitelistMe(ctx, i)
			case "verwalten":
				b.openApp(ctx, i)
			}
		}
	case discord.InteractionComponent:
		id := i.Data.CustomID
		switch {
		case id == idVerify:
			b.startCaptcha(ctx, i, false)
		case id == idVerifyNew:
			b.startCaptcha(ctx, i, true)
		case id == idVerifyEnter:
			b.askCaptchaCode(ctx, i)
		case id == idVerifyHelp:
			b.verifyHelp(ctx, i)
		case id == idApplyOpen:
			b.openApplication(ctx, i)
		case strings.HasPrefix(id, idApplyAccept):
			b.decideApplication(ctx, i, strings.TrimPrefix(id, idApplyAccept), true)
		case strings.HasPrefix(id, idRoleBack):
			b.giveRoleBack(ctx, i, strings.TrimPrefix(id, idRoleBack))
		case strings.HasPrefix(id, idSlotsAdd):
			b.decideSlots(ctx, i, strings.TrimPrefix(id, idSlotsAdd), true)
		case strings.HasPrefix(id, idSlotsNo):
			b.decideSlots(ctx, i, strings.TrimPrefix(id, idSlotsNo), false)
		case strings.HasPrefix(id, idApplyDecline):
			b.decideApplication(ctx, i, strings.TrimPrefix(id, idApplyDecline), false)
		}
	case discord.InteractionModalSubmit:
		switch i.Data.CustomID {
		case idVerifyModal:
			b.checkCaptcha(ctx, i)
		case idApplyModal:
			b.submitApplication(ctx, i)
		}
	}
}
