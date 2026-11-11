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

// uuidIn finds the account UUID in an answer of Kronwerke Core ("... uuid <uuid>").
var uuidIn = regexp.MustCompile(`uuid ([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

func answerUUID(out string) string {
	if m := uuidIn.FindStringSubmatch(out); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// streamerRef is how Core finds a streamer: by the UUID of their account when the bot knows
// it (names change), else by their linked name.
func (b *Bot) streamerRef(discordID string) string {
	if g, ok := b.store.Grant(discordID); ok && g.UUID != "" {
		return g.UUID
	}
	return b.store.Link(discordID)
}

// grantCommand is Core's grant for a place of one's own, with the kind and, once known, the UUID.
func grantCommand(name string, slots int, kind, uuid string) string {
	if kind == "" {
		kind = store.KindSeason1
	}
	cmd := fmt.Sprintf("kw admin grant %s %d %s", name, slots, kind)
	if uuid != "" {
		cmd += " " + uuid
	}
	return cmd
}

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
	// a slot from a streamer: the same account under a new name follows at once; another
	// account only when the streamer agrees, at most once a day (review R2-09)
	if held := b.store.InvitesOf(u.ID); kind == "" && len(held) > 0 && !strings.EqualFold(held[0].Player, name) {
		b.rest.Respond(ctx, i, discord.CallbackDeferredMessage, map[string]any{"flags": discord.FlagEphemeral})
		b.askMove(ctx, i, u, held[0], name)
		return
	}
	if held := b.store.InvitesOf(u.ID); kind == "" && len(held) > 0 {
		b.reply(ctx, i, fmt.Sprintf("**%s** ist schon auf der Whitelist, über <@%s>.", held[0].Player, held[0].StreamerID))
		return
	}
	if kind == "" {
		b.store.SetLink(u.ID, name)
		b.store.SetLinkName(u.ID, u.Name(), time.Now())
		b.events.add("link %s -> %s", u.Username, name)
		go b.pushLinks()
		b.reply(ctx, i, fmt.Sprintf("Dein Minecraft-Name ist jetzt **%s**. Auf die Whitelist kommst du, wenn dir ein Streamer einen Platz gibt.", name))
		return
	}
	if inv, ok := b.store.Invite(name); ok && inv.DiscordID != u.ID {
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
	b.store.SetLinkName(u.ID, u.Name(), time.Now())
	b.events.add("link %s -> %s", u.Username, name)
	defer func() { go b.pushLinks() }()
	if err := b.promote(ctx, u, name, kind, slots); err != nil {
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

const (
	idMoveYes = "kw:move:yes:" // + discord id + ":" + name
	idMoveNo  = "kw:move:no:"  // + discord id
	moveEvery = 24 * time.Hour
)

// askMove handles /link with a new name for a member who holds a streamer's slot.
func (b *Bot) askMove(ctx context.Context, i *discord.Interaction, u discord.User, inv store.Invite, name string) {
	edit := func(text string) { b.rest.EditReply(ctx, i, discord.MessageSend{Content: text}) }
	lctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	acc, err := b.app().heads.lookup(lctx, name)
	cancel()
	if errors.Is(err, errNoAccount) {
		edit(fmt.Sprintf("Einen Minecraft-Account **%s** gibt es nicht.", name))
		return
	}
	if err == nil && inv.UUID != "" && strings.EqualFold(acc.uuid, inv.UUID) {
		// the same account, renamed: nothing to ask
		b.store.RenameInvite(inv.Player, acc.name, acc.uuid)
		b.store.SetLinkName(u.ID, u.Name(), time.Now())
		go b.pushLinks()
		edit(fmt.Sprintf("Dein Account heißt jetzt **%s**, dein Platz bleibt.", acc.name))
		return
	}
	if err == nil {
		name = acc.name
	}
	a := b.app()
	a.mu.Lock()
	last, asked := a.moves[u.ID]
	if asked && time.Since(last) < moveEvery {
		a.mu.Unlock()
		edit("Du hast heute schon einen Umzug angefragt. Morgen geht es wieder, oder frag deinen Streamer.")
		return
	}
	a.moves[u.ID] = time.Now()
	a.mu.Unlock()
	dm, err := b.rest.CreateDM(ctx, inv.StreamerID)
	if err == nil {
		_, err = b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{
			Content: fmt.Sprintf("**%s** steht über deinen Platz als **%s** auf der Kronwerke-Whitelist und möchte den Platz auf den Minecraft-Account **%s** umziehen. Das ist ein anderer Account. Einverstanden?", u.Name(), inv.Player, name),
			Components: []discord.Component{discord.Row(
				discord.Button(idMoveYes+u.ID+":"+name, "Umziehen", discord.ButtonSuccess),
				discord.Button(idMoveNo+u.ID, "Ablehnen", discord.ButtonSecondary),
			)},
		})
	}
	if err != nil {
		a.mu.Lock()
		delete(a.moves, u.ID)
		a.mu.Unlock()
		edit(fmt.Sprintf("Deinen Streamer <@%s> konnte ich nicht erreichen. Bitte ihn, den Platz mit `/whitelist remove` und `/whitelist add` umzutragen.", inv.StreamerID))
		return
	}
	edit(fmt.Sprintf("**%s** ist ein anderer Account als **%s**. Ich habe <@%s> gefragt, ob der Platz umziehen darf. Du bekommst Bescheid.", name, inv.Player, inv.StreamerID))
}

// decideMove is the streamer's answer in the DM.
func (b *Bot) decideMove(ctx context.Context, i *discord.Interaction, rest string, yes bool) {
	id, name := rest, ""
	if yes {
		var ok bool
		id, name, ok = strings.Cut(rest, ":")
		if !ok || !mcName.MatchString(name) {
			return
		}
	}
	held := b.store.InvitesOf(id)
	content := ""
	if i.Message != nil {
		content = i.Message.Content
	}
	done := func(line string) {
		b.rest.Respond(ctx, i, discord.CallbackUpdateMessage, discord.MessageSend{Content: content + "\n" + line, Components: []discord.Component{}})
	}
	if len(held) == 0 || held[0].StreamerID != i.Actor().ID {
		done("Dieser Platz ist nicht mehr bei dir.")
		return
	}
	if !yes {
		done("Abgelehnt.")
		if dm, err := b.rest.CreateDM(ctx, id); err == nil {
			b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{Content: fmt.Sprintf("Dein Streamer möchte den Platz nicht umziehen, du bleibst als **%s** eingetragen.", held[0].Player)})
		}
		return
	}
	if err := b.moveInvite(ctx, held[0], name); err != nil {
		done("Das ging nicht: " + err.Error())
		return
	}
	go b.pushLinks()
	done(fmt.Sprintf("Umgezogen auf **%s**.", name))
	if dm, err := b.rest.CreateDM(ctx, id); err == nil {
		b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{Content: fmt.Sprintf("Dein Platz steht jetzt auf **%s** statt **%s**.", name, held[0].Player)})
	}
}

// moveInvite puts a streamer's slot onto the holder's new Minecraft name: the old name
// comes off the whitelist, the new one goes on, with the same streamer.
func (b *Bot) moveInvite(ctx context.Context, inv store.Invite, name string) error {
	if other, ok := b.store.Invite(name); ok && other.DiscordID != inv.DiscordID {
		return fmt.Errorf("%s hat schon einen Platz", other.Player)
	}
	if err := b.revoke(ctx, inv, "neuer Name "+name); err != nil {
		return err
	}
	out, err := b.serverCommand(fmt.Sprintf("kw admin invite %s %s", b.streamerRef(inv.StreamerID), name))
	if err != nil {
		// the old name back, so nobody is left without a place
		if back, err2 := b.serverCommand(fmt.Sprintf("kw admin invite %s %s", b.streamerRef(inv.StreamerID), inv.Player)); err2 == nil {
			inv.UUID = answerUUID(back)
			b.store.PutInvite(inv)
		}
		return err
	}
	b.store.PutInvite(store.Invite{Player: name, DiscordID: inv.DiscordID, StreamerID: inv.StreamerID, UUID: answerUUID(out), Created: time.Now()})
	b.store.SetLink(inv.DiscordID, name)
	if role := b.setting("role.player"); role != "" {
		b.rest.AddRole(ctx, b.guild(), inv.DiscordID, role, "Whitelist slot moved to a new name")
	}
	return nil
}

// promote gives a member who plays on a streamer's slot a place of their own (they became
// a streamer, or the Season 1 role came late): the slot goes back to the streamer first,
// then the place is granted. If the grant fails the slot is given back (review R3-03).
func (b *Bot) promote(ctx context.Context, u discord.User, name, kind string, slots int) error {
	inv, invited := b.store.Invite(name)
	if !invited || inv.DiscordID != u.ID {
		return b.grant(ctx, u, name, kind, slots)
	}
	if err := b.revoke(ctx, inv, "hat jetzt einen eigenen Platz"); err != nil {
		return err
	}
	if err := b.grant(ctx, u, name, kind, slots); err != nil {
		if out, err2 := b.serverCommand(fmt.Sprintf("kw admin invite %s %s", b.streamerRef(inv.StreamerID), inv.Player)); err2 == nil {
			inv.UUID = answerUUID(out)
			b.store.PutInvite(inv)
		}
		return err
	}
	if dm, err := b.rest.CreateDM(ctx, inv.StreamerID); err == nil {
		b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{Content: fmt.Sprintf(
			"**%s** hat jetzt einen eigenen Platz auf der Whitelist. Dein Platz, den du an %s vergeben hattest, ist wieder frei.", name, u.Name())})
	}
	return nil
}

// grant gives a member a place of their own on the server and on Discord.
func (b *Bot) grant(ctx context.Context, u discord.User, name, kind string, slots int) error {
	out, err := b.serverCommand(grantCommand(name, slots, kind, ""))
	if err != nil {
		return err
	}
	b.store.PutGrant(store.Grant{Player: name, DiscordID: u.ID, Kind: kind, UUID: answerUUID(out), Created: time.Now()})
	if role := b.setting("role.player"); role != "" {
		b.rest.AddRole(ctx, b.guild(), u.ID, role, "Own whitelist place ("+kind+")")
	}
	b.stats.invites.Add(1)
	line := fmt.Sprintf("**%s** (<@%s>) ist als Streamer auf der Whitelist.", name, u.ID)
	if kind == store.KindSeason1 {
		line = fmt.Sprintf("**%s** (<@%s>) ist aus Season 1 wieder dabei, mit %d eigenen Plätzen.", name, u.ID, slots)
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
		if inv, ok := b.store.Invite(name); ok && inv.DiscordID != m.User.ID {
			continue // the name is someone else's slot
		}
		if err := b.promote(ctx, *m.User, name, kind, slots); err != nil {
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

// userError is a refusal for the person who asked, not a fault of the bot.
type userError string

func (e userError) Error() string { return string(e) }

func mention(id string) string { return "<@" + id + ">" }

// canGive says whether a member may give whitelist slots: streamers, the team and
// everyone with a place of their own (Season 1).
func (b *Bot) canGive(m *discord.Member) bool {
	if m == nil || m.User == nil {
		return false
	}
	_, own := b.store.Grant(m.User.ID)
	return own || m.HasRole(b.setting("role.streamer")) || b.isTeam(m)
}

// checkInvite holds the rules of /whitelist add, for the command and the whitelist app:
// the actor may give slots, the target is verified and has no place yet, and the name
// that counts is the one the target linked. who names a person in the answer.
func (b *Bot) checkInvite(actor, target *discord.Member, player string, who func(string) string) (name, linked string, err error) {
	if !b.canGive(actor) {
		return "", "", userError("Nur Streamer und Season-1-Spieler können Leute auf die Whitelist setzen. Trag dich zuerst mit `/link` ein.")
	}
	if b.store.Link(actor.User.ID) == "" {
		return "", "", userError("Verknüpf zuerst deinen eigenen Minecraft-Namen mit `/link`.")
	}
	if target == nil || target.User == nil {
		return "", "", userError("Diese Person ist nicht auf dem Kronwerke-Discord.")
	}
	id := target.User.ID
	player = strings.TrimSpace(player)
	// the name the person linked counts; a typed one only for someone who linked none
	linked = b.store.Link(id)
	switch {
	case player == "" && linked == "":
		return "", "", userError(fmt.Sprintf("%s hat noch keinen Minecraft-Namen mit `/link` hinterlegt. Bitte sie darum, oder gib den Namen mit an.", who(id)))
	case player == "":
		player = linked
	case linked != "" && !strings.EqualFold(player, linked):
		return "", "", userError(fmt.Sprintf("%s hat **%s** hinterlegt, nicht **%s**. Es gilt der hinterlegte Name, oder sie ändert ihn mit `/link`.", who(id), linked, player))
	}
	if !mcName.MatchString(player) {
		return "", "", userError("Das ist kein gültiger Minecraft-Name.")
	}
	if !target.HasRole(b.setting("role.member")) {
		return "", "", userError("Diese Person ist noch nicht verifiziert. Sie muss erst auf dem Discord das Captcha lösen.")
	}
	if inv, ok := b.store.Invite(player); ok {
		return "", "", userError(fmt.Sprintf("**%s** hat schon einen Platz (von %s).", inv.Player, who(inv.StreamerID)))
	}
	// one Discord account, one place: a second name would be a second place for the same person
	if held := b.store.InvitesOf(id); len(held) > 0 {
		return "", "", userError(fmt.Sprintf("%s hat schon einen Platz als **%s**.", who(id), held[0].Player))
	}
	if g, ok := b.store.Grant(id); ok {
		return "", "", userError(fmt.Sprintf("%s hat schon einen eigenen Platz als **%s**.", who(id), g.Player))
	}
	return player, linked, nil
}

// invite gives a slot after checkInvite: on the server, in the store, the role, the
// whitelist channel and a DM to the new player. out is Core's answer.
func (b *Bot) invite(ctx context.Context, streamer discord.User, target, player, linked string) (out string, err error) {
	out, err = b.serverCommand(fmt.Sprintf("kw admin invite %s %s", b.streamerRef(streamer.ID), player))
	if err != nil {
		return "", err
	}
	b.store.PutInvite(store.Invite{Player: player, DiscordID: target, StreamerID: streamer.ID, UUID: answerUUID(out), Created: time.Now()})
	if linked == "" {
		b.store.SetLink(target, player)
	}
	if role := b.setting("role.player"); role != "" {
		b.rest.AddRole(ctx, b.guild(), target, role, "Whitelisted by "+streamer.Username)
	}
	b.stats.invites.Add(1)
	if ch := b.setting("channel.whitelist"); ch != "" {
		b.rest.SendMessage(ctx, ch, discord.MessageSend{
			Content:         fmt.Sprintf("**%s** (<@%s>) ist auf der Whitelist, eingeladen von <@%s>.", player, target, streamer.ID),
			AllowedMentions: discord.NoMentions,
		})
	}
	if dm, err := b.rest.CreateDM(ctx, target); err == nil {
		b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{Content: fmt.Sprintf(
			"Du bist auf der Kronwerke-Whitelist, eingeladen von %s. Dein Minecraft-Name: **%s**. Wie du das Modpack installierst, steht auf dem Discord im Kanal zur Installation.", streamer.Name(), player)})
	}
	return out, nil
}

func (b *Bot) whitelistAdd(ctx context.Context, i *discord.Interaction, sub discord.CommandOption) {
	var player, target string
	for _, o := range sub.Options {
		switch o.Name {
		case "player":
			player = strings.TrimSpace(o.String())
		case "user":
			target = o.String()
		}
	}
	tm := &discord.Member{User: &discord.User{ID: target}}
	if i.Data.Resolved != nil {
		if m, ok := i.Data.Resolved.Members[target]; ok {
			m.User = &discord.User{ID: target}
			if u, ok := i.Data.Resolved.Users[target]; ok {
				m.User = &u
			}
			tm = &m
		}
	}
	name, linked, err := b.checkInvite(i.Member, tm, player, mention)
	if err != nil {
		b.reply(ctx, i, err.Error())
		return
	}
	b.rest.Respond(ctx, i, discord.CallbackDeferredMessage, map[string]any{"flags": discord.FlagEphemeral})
	out, err := b.invite(ctx, i.Actor(), target, name, linked)
	if err != nil {
		b.rest.EditReply(ctx, i, discord.MessageSend{Content: "Das ging nicht: " + err.Error()})
		return
	}
	b.rest.EditReply(ctx, i, discord.MessageSend{Content: fmt.Sprintf("**%s** ist drin. %s", name, out)})
}

func (b *Bot) whitelistRemove(ctx context.Context, i *discord.Interaction, sub discord.CommandOption) {
	var player string
	for _, o := range sub.Options {
		if o.Name == "player" {
			player = strings.TrimSpace(o.String())
		}
	}
	if _, ok := b.store.Invite(player); !ok {
		if g, ok := b.store.GrantByPlayer(player); ok {
			b.removeGrant(ctx, i, g)
			return
		}
	}
	inv, err := b.checkRemove(i.Member, player)
	if err != nil {
		b.reply(ctx, i, err.Error())
		return
	}
	b.rest.Respond(ctx, i, discord.CallbackDeferredMessage, map[string]any{"flags": discord.FlagEphemeral})
	if err := b.revoke(ctx, inv, "entfernt von "+i.Actor().Name()); err != nil {
		b.rest.EditReply(ctx, i, discord.MessageSend{Content: "Das ging nicht: " + err.Error()})
		return
	}
	b.rest.EditReply(ctx, i, discord.MessageSend{Content: fmt.Sprintf("**%s** ist von der Whitelist entfernt.", inv.Player)})
}

// checkRemove finds the slot a member may take back: one they gave, or any for the team.
func (b *Bot) checkRemove(actor *discord.Member, player string) (store.Invite, error) {
	inv, ok := b.store.Invite(strings.TrimSpace(player))
	if !ok {
		return inv, userError("Für diesen Namen gibt es keinen Platz.")
	}
	if actor == nil || actor.User == nil || (inv.StreamerID != actor.User.ID && !b.isTeam(actor)) {
		return inv, userError("Du kannst nur Leute entfernen, die du selbst eingeladen hast.")
	}
	return inv, nil
}

// slotsOf is "3 von 5 Plätzen vergeben" for a member with a place of their own, from the
// server's roster; empty when the server does not answer or they have none.
func (b *Bot) slotsOf(discordID string) string {
	g, ok := b.store.Grant(discordID)
	if !ok || !b.serverConnected() {
		return ""
	}
	r, err := b.roster()
	if err != nil {
		return ""
	}
	for _, s := range r.Streamers {
		if (g.UUID != "" && strings.EqualFold(s.UUID, g.UUID)) || strings.EqualFold(s.Name, g.Player) {
			return fmt.Sprintf("%d von %d Plätzen vergeben", len(s.Invited), s.Slots)
		}
	}
	return ""
}

// whitelistMe is /whitelist me: where the member stands.
func (b *Bot) whitelistMe(ctx context.Context, i *discord.Interaction) {
	u := i.Actor()
	start := ""
	b.apiMu.Lock()
	season := b.season
	b.apiMu.Unlock()
	if season != nil && season.Start != "" && !season.Running {
		if t, err := time.ParseInLocation("2006-01-02T15:04", season.Start, berlin); err == nil {
			start = fmt.Sprintf(" Der Server öffnet <t:%d:F> (<t:%d:R>).", t.Unix(), t.Unix())
		}
	}
	if g, ok := b.store.Grant(u.ID); ok {
		text := fmt.Sprintf("Du hast einen eigenen Platz als **%s** (%s).", g.Player, g.Label())
		if sl := b.slotsOf(u.ID); sl != "" {
			text += " " + sl + ", mit `/whitelist add` vergibst du sie."
		}
		b.reply(ctx, i, text+start)
		return
	}
	if held := b.store.InvitesOf(u.ID); len(held) > 0 {
		b.reply(ctx, i, fmt.Sprintf("Du stehst als **%s** auf der Whitelist, eingetragen von <@%s>.%s", held[0].Player, held[0].StreamerID, start))
		return
	}
	if name := b.store.Link(u.ID); name != "" {
		b.reply(ctx, i, fmt.Sprintf("Du bist noch nicht auf der Whitelist. Dein hinterlegter Name ist **%s**: ein Streamer kann dich damit eintragen.%s", name, start))
		return
	}
	b.reply(ctx, i, "Du bist noch nicht auf der Whitelist. Hinterleg zuerst deinen Minecraft-Namen mit `/link`, dann kann dich ein Streamer eintragen."+start)
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
	head := b.slotsOf(me)
	if len(invs) == 0 {
		text := "Du hast noch niemanden eingetragen."
		if head != "" {
			text = head + ". " + text
		}
		if _, granted := b.store.Grant(me); !granted && !all {
			text = "Du hast keine Plätze zu vergeben. Ob du selbst drauf bist, sagt `/whitelist me`."
		}
		b.reply(ctx, i, text)
		return
	}
	var lines []string
	if head != "" {
		lines = append(lines, "**"+head+"**")
	}
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
	cmd := fmt.Sprintf("kw admin revoke %s %s", b.streamerRef(inv.StreamerID), inv.Player)
	if inv.UUID != "" {
		cmd += " " + inv.UUID
	}
	if _, err := b.serverCommand(cmd); err != nil {
		return err
	}
	b.store.DeleteInvite(inv.Player)
	_, granted := b.store.Grant(inv.DiscordID)
	if role := b.setting("role.player"); role != "" && len(b.store.InvitesOf(inv.DiscordID)) == 0 && !granted {
		b.rest.RemoveRole(ctx, b.guild(), inv.DiscordID, role, "Whitelist slot removed")
	}
	if ch := b.setting("channel.whitelist"); ch != "" {
		b.rest.SendMessage(ctx, ch, discord.MessageSend{
			Content: fmt.Sprintf("**%s** ist nicht mehr auf der Whitelist (%s).", inv.Player, why), AllowedMentions: discord.NoMentions,
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
	cmd := "kw admin ungrant " + g.Player
	if g.UUID != "" {
		cmd += " " + g.UUID
	}
	if _, err := b.serverCommand(cmd); err != nil {
		return err
	}
	b.store.DeleteGrant(g.DiscordID)
	go b.pushLinks()
	if role := b.setting("role.player"); role != "" && len(b.store.InvitesOf(g.DiscordID)) == 0 {
		b.rest.RemoveRole(ctx, b.guild(), g.DiscordID, role, "Own whitelist place removed")
	}
	if ch := b.setting("channel.whitelist"); ch != "" {
		b.rest.SendMessage(ctx, ch, discord.MessageSend{
			Content: fmt.Sprintf("**%s** ist nicht mehr auf der Whitelist (%s).", g.Player, why), AllowedMentions: discord.NoMentions,
		})
	}
	return nil
}

// onMemberLeave enforces the rule "no Discord, no slot".
func (b *Bot) onMemberLeave(ctx context.Context, u discord.User) {
	b.events.add("leave %s (%s)", u.Username, u.ID)
	if g, ok := b.store.Grant(u.ID); ok {
		if err := b.ungrant(ctx, g, "hat den Discord verlassen"); err != nil {
			b.fail(ctx, fmt.Sprintf("Eigenen Platz von %s nach dem Verlassen entfernen (der Abgleich versucht es wieder)", g.Player), err)
		}
	}
	for _, inv := range b.store.InvitesOf(u.ID) {
		if err := b.revoke(ctx, inv, "hat den Discord verlassen"); err != nil {
			b.fail(ctx, fmt.Sprintf("Whitelist-Platz von %s nach dem Verlassen entfernen (der Abgleich versucht es wieder)", inv.Player), err)
			continue
		}
		if dm, err := b.rest.CreateDM(ctx, inv.StreamerID); err == nil {
			b.rest.SendMessage(ctx, dm.ID, discord.MessageSend{Content: fmt.Sprintf(
				"%s hat den Kronwerke-Discord verlassen, der Whitelist-Platz für **%s** ist wieder frei.", u.Name(), inv.Player)})
		}
	}
}
