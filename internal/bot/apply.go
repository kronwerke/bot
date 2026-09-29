package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/store"
)

const (
	idApplyOpen    = "kw:apply:open"
	idApplyModal   = "kw:apply:modal"
	idApplyAccept  = "kw:apply:accept:"
	idApplyDecline = "kw:apply:decline:"
)

var applyFields = []struct {
	id, label, placeholder string
	style, min, max        int
	required               bool
}{
	{"channel", "Dein Kanal (Twitch oder YouTube Link)", "https://twitch.tv/...", discord.TextShort, 8, 200, true},
	{"viewers", "Durchschnittliche Zuschauer", "z. B. 25", discord.TextShort, 1, 20, true},
	{"schedule", "Wie oft streamst du?", "z. B. 3 Abende pro Woche", discord.TextShort, 2, 100, true},
	{"modded", "Erfahrung mit modded Minecraft", "Gar keine ist völlig okay, wir fragen nur fürs Onboarding", discord.TextParagraph, 1, 500, true},
	{"why", "Warum Kronwerke?", "", discord.TextParagraph, 1, 800, false},
}

func (b *Bot) openApplication(ctx context.Context, i *discord.Interaction) {
	u := i.Actor()
	if a, ok := b.store.Application(u.ID); ok && a.Status == "open" {
		b.reply(ctx, i, fmt.Sprintf("Du hast schon eine offene Bewerbung: <#%s>", a.ChannelID))
		return
	}
	if i.Member.HasRole(b.setting("role.streamer")) {
		b.reply(ctx, i, "Du bist schon als Streamer dabei.")
		return
	}
	var rows []discord.Component
	for _, f := range applyFields {
		rows = append(rows, discord.Row(discord.TextInput(f.id, f.label, f.style, f.min, f.max, f.required, f.placeholder)))
	}
	if err := b.rest.Respond(ctx, i, discord.CallbackModal, map[string]any{
		"custom_id": idApplyModal, "title": "Bewerbung als Streamer", "components": rows,
	}); err != nil {
		b.fail(ctx, "Bewerbungsformular", err)
	}
}

func (b *Bot) submitApplication(ctx context.Context, i *discord.Interaction) {
	u := i.Actor()
	if a, ok := b.store.Application(u.ID); ok && a.Status == "open" {
		b.reply(ctx, i, fmt.Sprintf("Du hast schon eine offene Bewerbung: <#%s>", a.ChannelID))
		return
	}
	// creating a channel can take longer than the three seconds an answer may take
	b.rest.Respond(ctx, i, discord.CallbackDeferredMessage, map[string]any{"flags": discord.FlagEphemeral})

	cat, err := b.applicationsCategory(ctx)
	if err != nil {
		b.fail(ctx, "Bewerbungs-Kategorie", err)
		b.rest.EditReply(ctx, i, discord.MessageSend{Content: "Das hat nicht geklappt, das Team ist informiert."})
		return
	}
	view := strconv.Itoa(discord.PermViewChannel | discord.PermSendMessages | discord.PermReadMessageHistory | discord.PermAttachFiles)
	ow := []discord.Overwrite{
		{ID: b.guild(), Type: 0, Allow: "0", Deny: strconv.Itoa(discord.PermViewChannel)},
		{ID: u.ID, Type: 1, Allow: view, Deny: "0"},
	}
	for _, r := range []string{b.setting("role.lead"), b.setting("role.staff")} {
		if r != "" {
			ow = append(ow, discord.Overwrite{ID: r, Type: 0, Allow: view, Deny: "0"})
		}
	}
	if b.me.ID != "" {
		ow = append(ow, discord.Overwrite{ID: b.me.ID, Type: 1, Allow: view, Deny: "0"})
	}
	ch, err := b.rest.CreateChannel(ctx, b.guild(), discord.Channel{
		Name: "bewerbung-" + slug(u.Username), Type: 0, ParentID: cat,
		Topic: "Bewerbung von " + u.Name(), PermissionOverwrites: ow,
	}, "Streamer application by "+u.Username)
	if err != nil {
		b.fail(ctx, "Bewerbungskanal anlegen", err)
		b.rest.EditReply(ctx, i, discord.MessageSend{Content: "Das hat nicht geklappt, das Team ist informiert."})
		return
	}

	var fields []discord.EmbedField
	for _, f := range applyFields {
		v := strings.TrimSpace(i.Data.ModalValue(f.id))
		if v == "" {
			v = "(leer)"
		}
		fields = append(fields, discord.EmbedField{Name: f.label, Value: v})
	}
	_, err = b.rest.SendMessage(ctx, ch.ID, discord.MessageSend{
		Content: fmt.Sprintf("Neue Bewerbung von <@%s>. Das Team meldet sich hier.", u.ID),
		Embeds:  []discord.Embed{{Title: "Bewerbung: " + u.Name(), Fields: fields, Color: 0x7B68EE, Timestamp: time.Now().UTC().Format(time.RFC3339)}},
		Components: []discord.Component{discord.Row(
			discord.Button(idApplyAccept+u.ID, "Annehmen", discord.ButtonSuccess),
			discord.Button(idApplyDecline+u.ID, "Ablehnen", discord.ButtonDanger),
		)},
		AllowedMentions: &discord.AllowedMentions{Parse: []string{}, Users: []string{u.ID}},
	})
	if err != nil {
		b.fail(ctx, "Bewerbung posten", err)
	}
	b.store.PutApplication(store.Application{UserID: u.ID, ChannelID: ch.ID, Status: "open", Created: time.Now()})
	b.stats.applications.Add(1)
	b.audit(ctx, fmt.Sprintf("📝 Neue Streamer-Bewerbung von %s: <#%s>", u.Name(), ch.ID))
	b.rest.EditReply(ctx, i, discord.MessageSend{Content: fmt.Sprintf("Danke! Deine Bewerbung liegt in <#%s>.", ch.ID)})
}

func (b *Bot) decideApplication(ctx context.Context, i *discord.Interaction, userID string, accept bool) {
	if !b.isTeam(i.Member) {
		b.reply(ctx, i, "Nur das Team kann Bewerbungen entscheiden.")
		return
	}
	a, ok := b.store.Application(userID)
	if !ok || a.Status != "open" {
		b.reply(ctx, i, "Diese Bewerbung ist schon entschieden.")
		return
	}
	who := i.Actor()
	a.DecidedBy, a.Closed = who.ID, time.Now()
	var text string
	if accept {
		if err := b.rest.AddRole(ctx, b.guild(), userID, b.setting("role.streamer"), "Application accepted by "+who.Username); err != nil {
			b.fail(ctx, "Streamer-Rolle vergeben", err)
			b.reply(ctx, i, "Die Streamer-Rolle ließ sich nicht vergeben, siehe Kontrollkanal.")
			return
		}
		a.Status = "accepted"
		text = fmt.Sprintf("✅ <@%s>, du bist dabei! %s hat deine Bewerbung angenommen. Als Nächstes: `/link` mit deinem Minecraft-Namen, damit kommst du auf die Whitelist. Dann trägst du mit `/whitelist add` deine Leute ein.", userID, who.Name())
		if name := b.store.Link(userID); name != "" {
			if _, has := b.store.Grant(userID); !has {
				if m, err := b.rest.Member(ctx, b.guild(), userID); err == nil && m.User != nil && b.grant(ctx, *m.User, name, store.KindStreamer, -1) == nil {
					text = fmt.Sprintf("✅ <@%s>, du bist dabei! %s hat deine Bewerbung angenommen. **%s** ist auf der Whitelist, und mit `/whitelist add` trägst du deine Leute ein.", userID, who.Name(), name)
				}
			}
		}
	} else {
		a.Status = "declined"
		text = fmt.Sprintf("<@%s>, diesmal hat es leider nicht gepasst. Danke fürs Bewerben, und du bist als Zuschauer jederzeit willkommen.", userID)
	}
	b.store.PutApplication(a)
	// the applicant can still read, not write; the channel is removed after applications.keep
	b.rest.Do(ctx, "PUT", "/channels/"+a.ChannelID+"/permissions/"+userID, map[string]any{
		"type": 1, "allow": strconv.Itoa(discord.PermViewChannel | discord.PermReadMessageHistory), "deny": strconv.Itoa(discord.PermSendMessages),
	}, nil, "Application decided")
	content := ""
	if i.Message != nil {
		content = i.Message.Content
	}
	// keep the application text, drop the buttons
	b.rest.Respond(ctx, i, discord.CallbackUpdateMessage, discord.MessageSend{Content: content, Components: []discord.Component{}})
	b.rest.SendMessage(ctx, a.ChannelID, discord.MessageSend{Content: text, AllowedMentions: &discord.AllowedMentions{Parse: []string{}, Users: []string{userID}}})
	b.audit(ctx, fmt.Sprintf("Bewerbung von <@%s>: %s durch %s", userID, a.Status, who.Name()))
}

// applicationsCategory returns the category for application channels, creating it.
func (b *Bot) applicationsCategory(ctx context.Context) (string, error) {
	if id := b.setting("category.applications"); id != "" {
		return id, nil
	}
	view := strconv.Itoa(discord.PermViewChannel)
	ow := []discord.Overwrite{{ID: b.guild(), Type: 0, Allow: "0", Deny: view}}
	for _, r := range []string{b.setting("role.lead"), b.setting("role.staff")} {
		if r != "" {
			ow = append(ow, discord.Overwrite{ID: r, Type: 0, Allow: view, Deny: "0"})
		}
	}
	c, err := b.rest.CreateChannel(ctx, b.guild(), discord.Channel{Name: "📝 | Bewerbungen", Type: 4, PermissionOverwrites: ow}, "Category for streamer applications")
	if err != nil {
		return "", err
	}
	b.store.SetSetting("category.applications", c.ID)
	return c.ID, nil
}

// sweepApplications removes decided application channels after applications.keep.
func (b *Bot) sweepApplications(ctx context.Context) {
	keep := b.duration("applications.keep", 48*time.Hour)
	for _, a := range b.store.Applications() {
		if a.Status == "open" || a.ChannelID == "" || a.Closed.IsZero() || time.Since(a.Closed) < keep {
			continue
		}
		if err := b.rest.DeleteChannel(ctx, a.ChannelID, "Application decided "+a.Closed.Format("2006-01-02")); err != nil && !discord.IsStatus(err, 404) {
			b.fail(ctx, "Bewerbungskanal löschen", err)
			continue
		}
		a.ChannelID = ""
		b.store.PutApplication(a)
	}
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '.' || r == '-':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "neu"
	}
	return b.String()
}
