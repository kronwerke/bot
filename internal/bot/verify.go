package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kronwerke/bot/internal/captcha"
	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/store"
)

const (
	idVerify      = "kw:verify"
	idVerifyEnter = "kw:verify:enter"
	idVerifyNew   = "kw:verify:new"
	idVerifyModal = "kw:verify:modal"
	idVerifyCode  = "code"
)

func (b *Bot) onMemberJoin(ctx context.Context, m discord.Member) {
	if m.User == nil || m.User.Bot {
		return
	}
	b.events.add("join %s (%s)", m.User.Username, m.User.ID)
	if b.setting("verify.autorole") != "on" || m.HasRole(b.setting("role.member")) {
		return
	}
	if role := b.setting("role.unverified"); role != "" {
		if err := b.rest.AddRole(ctx, b.guild(), m.User.ID, role, "New member, not verified yet"); err != nil {
			b.fail(ctx, "unverified-Rolle für "+m.User.Username, err)
		}
	}
}

// ensureVerifyPanel posts the verification message once and remembers its id.
func (b *Bot) ensureVerifyPanel(ctx context.Context) {
	ch := b.setting("channel.verify")
	if ch == "" {
		return
	}
	if id := b.store.Setting("msg.verify"); id != "" {
		msgs, err := b.rest.Messages(ctx, ch, 20)
		if err == nil {
			for _, m := range msgs {
				if m.ID == id {
					return
				}
			}
		}
	}
	b.postVerifyPanel(ctx)
}

func (b *Bot) postVerifyPanel(ctx context.Context) {
	ch := b.setting("channel.verify")
	msg, err := b.rest.SendMessage(ctx, ch, discord.MessageSend{
		Embeds: []discord.Embed{{
			Title: "Willkommen bei Kronwerke",
			Description: "Bevor du den Rest des Servers siehst, einmal kurz bestätigen, dass du ein Mensch bist.\n\n" +
				"Klick auf **Verifizieren**, lies den Code aus dem Bild ab und tipp ihn ein. Dauert zehn Sekunden.\n\n" +
				"Klappt etwas nicht, schreib einem Teammitglied.",
			Color: 0xDAA520,
		}},
		Components: []discord.Component{discord.Row(discord.Button(idVerify, "Verifizieren", discord.ButtonSuccess))},
	})
	if err != nil {
		b.fail(ctx, "Verifizierungs-Nachricht", err)
		return
	}
	b.store.SetSetting("msg.verify", msg.ID)
	b.events.add("verify panel posted")
}

// startCaptcha answers the panel button (new ephemeral message) or the "new code"
// button (update that ephemeral message).
func (b *Bot) startCaptcha(ctx context.Context, i *discord.Interaction, update bool) {
	u := i.Actor()
	if i.Member.HasRole(b.setting("role.member")) {
		b.reply(ctx, i, "Du bist schon verifiziert.")
		return
	}
	if until := b.store.LockedUntil(u.ID); time.Now().Before(until) {
		b.reply(ctx, i, fmt.Sprintf("Zu viele falsche Versuche. Probier es <t:%d:R> wieder.", until.Unix()))
		return
	}
	code := captcha.Code(5)
	img, err := captcha.Render(code)
	if err != nil {
		b.fail(ctx, "Captcha-Bild", err)
		b.reply(ctx, i, "Das Bild konnte nicht erzeugt werden. Versuch es gleich nochmal.")
		return
	}
	mins := b.number("captcha.minutes", 10)
	b.store.SetCaptcha(u.ID, store.Captcha{Code: code, Expires: time.Now().Add(time.Duration(mins) * time.Minute)})
	data := discord.MessageSend{
		Flags: discord.FlagEphemeral,
		Embeds: []discord.Embed{{
			Title:       "Welcher Code steht im Bild?",
			Description: fmt.Sprintf("Fünf Zeichen, Groß- oder Kleinschreibung egal. Gilt %d Minuten.", mins),
			Image:       &discord.EmbedImage{URL: "attachment://code.png"},
			Color:       0xDAA520,
		}},
		Components: []discord.Component{discord.Row(
			discord.Button(idVerifyEnter, "Code eingeben", discord.ButtonPrimary),
			discord.Button(idVerifyNew, "Neues Bild", discord.ButtonSecondary),
		)},
		Attachments: []discord.Attachment{{ID: 0, Filename: "code.png"}},
	}
	cb := discord.CallbackMessage
	if update {
		cb = discord.CallbackUpdateMessage
	}
	if err := b.rest.RespondFiles(ctx, i, cb, data, []discord.File{{Name: "code.png", ContentType: "image/png", Data: img}}); err != nil {
		b.fail(ctx, "Captcha senden", err)
	}
}

func (b *Bot) askCaptchaCode(ctx context.Context, i *discord.Interaction) {
	if _, ok := b.store.PeekCaptcha(i.Actor().ID); !ok {
		b.reply(ctx, i, "Der Code ist abgelaufen. Klick nochmal auf **Verifizieren**.")
		return
	}
	b.rest.Respond(ctx, i, discord.CallbackModal, map[string]any{
		"custom_id": idVerifyModal,
		"title":     "Code eingeben",
		"components": []discord.Component{discord.Row(
			discord.TextInput(idVerifyCode, "Code aus dem Bild", discord.TextShort, 5, 8, true, "z. B. AC3K7"),
		)},
	})
}

func (b *Bot) checkCaptcha(ctx context.Context, i *discord.Interaction) {
	u := i.Actor()
	answer := i.Data.ModalValue(idVerifyCode)
	c, ok := b.store.TakeCaptcha(u.ID)
	if !ok || time.Now().After(c.Expires) {
		b.reply(ctx, i, "Der Code ist abgelaufen. Klick nochmal auf **Verifizieren**.")
		return
	}
	if !captcha.Match(c.Code, answer) {
		b.stats.failed.Add(1)
		n, locked := b.store.Fail(u.ID, b.number("captcha.attempts", 3), b.duration("captcha.lock", 10*time.Minute))
		b.events.add("captcha wrong for %s (%d)", u.Username, n)
		if locked {
			b.reply(ctx, i, "Das war leider falsch, und das war der letzte Versuch. In zehn Minuten geht es wieder.")
			return
		}
		b.reply(ctx, i, "Das war leider falsch. Klick auf **Verifizieren** für ein neues Bild.")
		return
	}
	g := b.guild()
	if err := b.rest.AddRole(ctx, g, u.ID, b.setting("role.member"), "Passed the captcha"); err != nil {
		b.fail(ctx, "Mitglied-Rolle für "+u.Username, err)
		b.reply(ctx, i, "Richtig, aber die Rolle ließ sich nicht vergeben. Das Team ist informiert.")
		return
	}
	if role := b.setting("role.unverified"); role != "" && i.Member.HasRole(role) {
		b.rest.RemoveRole(ctx, g, u.ID, role, "Passed the captcha")
	}
	b.store.MarkVerified(u.ID)
	b.stats.verified.Add(1)
	b.audit(ctx, fmt.Sprintf("✅ %s (<@%s>) ist verifiziert.", u.Name(), u.ID))
	welcome := b.setting("channel.welcome")
	text := "Richtig, willkommen! Du siehst jetzt den ganzen Server."
	if welcome != "" {
		text += fmt.Sprintf(" Fang am besten in <#%s> an.", welcome)
	}
	b.reply(ctx, i, text)
}

// reply answers an interaction with an ephemeral text.
func (b *Bot) reply(ctx context.Context, i *discord.Interaction, text string) {
	err := b.rest.Respond(ctx, i, discord.CallbackMessage, discord.MessageSend{
		Content: text, Flags: discord.FlagEphemeral, AllowedMentions: discord.NoMentions,
	})
	if err != nil {
		b.log.Warn("reply", "err", err)
	}
}

// syncUnverified gives the unverified role to members who have neither it nor Mitglied.
func (b *Bot) syncUnverified(ctx context.Context) (string, error) {
	members, err := b.rest.Members(ctx, b.guild())
	if err != nil {
		return "", err
	}
	member, unv := b.setting("role.member"), b.setting("role.unverified")
	var fixed []string
	for _, m := range members {
		if m.User == nil || m.User.Bot || m.HasRole(member) || m.HasRole(unv) {
			continue
		}
		if err := b.rest.AddRole(ctx, b.guild(), m.User.ID, unv, "Sync: member without Mitglied"); err != nil {
			return strings.Join(fixed, ", "), err
		}
		fixed = append(fixed, m.User.Username)
	}
	if len(fixed) == 0 {
		return "Alle Mitglieder haben entweder Mitglied oder unverified.", nil
	}
	return "unverified vergeben an: " + strings.Join(fixed, ", "), nil
}
