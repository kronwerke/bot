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
			Name: "bewerben", Description: "Als Streamer für Kronwerke bewerben",
			DMPermission: ptr(false),
		},
		{
			Name: "link", Description: "Deinen Minecraft-Namen hinterlegen",
			DMPermission: ptr(false),
			Options: []discord.ApplicationCommandOption{
				{Type: discord.OptString, Name: "name", Description: "Dein Minecraft-Name", Required: true, MinLength: 3, MaxLength: 16},
			},
		},
		{
			Name: "dabei", Description: "Season 1 gespielt? Dann bist du ohne Streamer-Platz dabei",
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
					{Type: discord.OptString, Name: "spieler", Description: "Minecraft-Name", Required: true, MinLength: 3, MaxLength: 16},
					{Type: discord.OptUser, Name: "nutzer", Description: "Die Person auf dem Discord", Required: true},
				}},
				{Type: discord.OptSubCommand, Name: "remove", Description: "Jemanden von der Whitelist nehmen", Options: []discord.ApplicationCommandOption{
					{Type: discord.OptString, Name: "spieler", Description: "Minecraft-Name", Required: true, MinLength: 3, MaxLength: 16},
				}},
				{Type: discord.OptSubCommand, Name: "list", Description: "Wen du eingetragen hast"},
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
	if i.GuildID != b.guild() {
		return
	}
	b.stats.commands.Add(1)
	switch i.Type {
	case discord.InteractionCommand:
		b.events.add("/%s by %s", i.Data.Name, i.Actor().Username)
		switch i.Data.Name {
		case "bewerben":
			b.openApplication(ctx, i)
		case "link":
			b.linkMinecraft(ctx, i)
		case "dabei":
			b.joinSeason1(ctx, i)
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
		case id == idApplyOpen:
			b.openApplication(ctx, i)
		case strings.HasPrefix(id, idApplyAccept):
			b.decideApplication(ctx, i, strings.TrimPrefix(id, idApplyAccept), true)
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
