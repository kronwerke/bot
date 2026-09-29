package bot

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kronwerke/bot/internal/discord"
)

const controlHelp = "**Befehle im Kontrollkanal**\n" +
	"`!status` Version, Laufzeit, Verbindung, Zahlen\n" +
	"`!events [n]` was der Bot zuletzt gesehen hat\n" +
	"`!update` sofort nach einem Release schauen und installieren\n" +
	"`!rollback` zurück auf die vorige Version\n" +
	"`!restart` neu starten\n" +
	"`!get [präfix]`, `!set key wert`, `!unset key` Einstellungen\n" +
	"`!members` Rollen zählen, Unverifizierte zeigen\n" +
	"`!verify-sync` unverified an alle ohne Mitglied\n" +
	"`!verify-panel` Verifizierungs-Nachricht neu posten\n" +
	"`!apply-panel <kanal-id>` Bewerben-Knopf in einen Kanal posten\n" +
	"`!apps` Bewerbungen\n" +
	"`!invites` Whitelist-Plätze\n" +
	"`!rcon <befehl>` Befehl auf dem Minecraft-Server"

func (b *Bot) onMessage(ctx context.Context, m discord.Message) {
	if m.GuildID != "" && m.GuildID != b.guild() {
		return
	}
	if ch := b.setting("channel.trash"); ch != "" && m.ChannelID == ch && m.Author.ID != b.me.ID {
		d := time.Duration(b.number("trash.seconds", 60)) * time.Second
		time.AfterFunc(d, func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			b.rest.DeleteMessage(ctx, m.ChannelID, m.ID, "trash channel")
		})
		return
	}
	if m.ChannelID != b.setting("channel.control") || !strings.HasPrefix(m.Content, "!") {
		return
	}
	// The bot's own messages count: that is how commands arrive through the API.
	if m.Author.ID != b.me.ID && !b.isLead(m.Member) {
		return
	}
	b.events.add("control %q by %s", firstLine(m.Content), m.Author.Username)
	out := b.runControl(ctx, strings.TrimSpace(m.Content))
	if out != "" {
		b.control(ctx, out)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func (b *Bot) runControl(ctx context.Context, line string) string {
	cmd, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	switch cmd {
	case "!help":
		return controlHelp
	case "!status":
		return b.statusText()
	case "!events":
		n := 20
		if v, err := strconv.Atoi(rest); err == nil && v > 0 {
			n = min(v, 100)
		}
		ev := b.events.last(n)
		if len(ev) == 0 {
			return "Noch nichts passiert."
		}
		return "```\n" + strings.Join(ev, "\n") + "\n```"
	case "!update":
		if b.cfg.Updater == nil {
			return "Selbst-Updates sind aus (KW_UPDATE_REPO leer)."
		}
		go b.checkUpdate(context.Background(), true)
		return "Schaue nach einem neuen Release."
	case "!rollback":
		if b.cfg.Updater == nil {
			return "Selbst-Updates sind aus."
		}
		if err := b.cfg.Updater.Rollback(); err != nil {
			return "Rollback geht nicht: " + err.Error()
		}
		b.requestRestart("Rollback auf die vorige Version")
		return ""
	case "!restart":
		b.requestRestart("auf Befehl")
		return ""
	case "!get":
		return b.settingsText(rest)
	case "!set":
		k, v, ok := strings.Cut(rest, " ")
		if !ok || k == "" {
			return "So: `!set key wert` (`-` als Wert schaltet einen Default aus)."
		}
		if err := b.store.SetSetting(k, strings.TrimSpace(v)); err != nil {
			return "Speichern ging nicht: " + err.Error()
		}
		return fmt.Sprintf("`%s` = `%s`", k, b.setting(k))
	case "!unset":
		b.store.SetSetting(rest, "")
		return fmt.Sprintf("`%s` ist wieder `%s` (Default).", rest, defaults[rest])
	case "!members":
		return b.membersText(ctx)
	case "!verify-sync":
		out, err := b.syncUnverified(ctx)
		if err != nil {
			return out + "\nAbgebrochen: " + err.Error()
		}
		return out
	case "!verify-panel":
		b.postVerifyPanel(ctx)
		return "Verifizierungs-Nachricht gepostet."
	case "!apply-panel":
		if rest == "" {
			return "So: `!apply-panel <kanal-id>`"
		}
		_, err := b.rest.SendMessage(ctx, rest, discord.MessageSend{
			Embeds: []discord.Embed{{
				Title:       "Als Streamer dabei sein",
				Description: "Du streamst und willst mit deiner Community auf Kronwerke spielen? Klick auf **Bewerben**, füll das kurze Formular aus, und das Team meldet sich in einem privaten Kanal bei dir.",
				Color:       0x7B68EE,
			}},
			Components: []discord.Component{discord.Row(discord.Button(idApplyOpen, "Bewerben", discord.ButtonPrimary))},
		})
		if err != nil {
			return "Posten ging nicht: " + err.Error()
		}
		return "Bewerben-Knopf gepostet."
	case "!apps":
		apps := b.store.Applications()
		if len(apps) == 0 {
			return "Keine Bewerbungen."
		}
		var lines []string
		for _, a := range apps {
			lines = append(lines, fmt.Sprintf("<@%s> %s seit %s", a.UserID, a.Status, a.Created.Format("02.01. 15:04")))
		}
		return strings.Join(lines, "\n")
	case "!invites":
		invs := b.store.InvitesBy("")
		if len(invs) == 0 {
			return "Keine Whitelist-Plätze vergeben."
		}
		var lines []string
		for _, v := range invs {
			lines = append(lines, fmt.Sprintf("%s (<@%s>) von <@%s>", v.Player, v.DiscordID, v.StreamerID))
		}
		return strings.Join(lines, "\n")
	case "!rcon":
		if b.rcon == nil {
			return "RCON ist nicht konfiguriert (KW_RCON_ADDR)."
		}
		out, err := b.rcon.Command(rest)
		if err != nil {
			return "RCON-Fehler: " + err.Error()
		}
		if out == "" {
			out = "(keine Ausgabe)"
		}
		return "```\n" + out + "\n```"
	}
	return "Unbekannter Befehl. `!help` zeigt alle."
}

func (b *Bot) requestRestart(reason string) {
	select {
	case b.restart <- reason:
	default:
	}
}

func (b *Bot) statusText() string {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	up := time.Since(b.started).Round(time.Second)
	gw := "verbunden"
	if !b.ready.Load() {
		gw = "nicht verbunden"
	}
	var lines []string
	lines = append(lines,
		fmt.Sprintf("**%s**, läuft seit %s, Gateway %s, %d Reconnects", b.cfg.Version, up, gw, b.reconnects()),
		fmt.Sprintf("Speicher %d MiB, Goroutinen %d", ms.Alloc>>20, runtime.NumGoroutine()),
		fmt.Sprintf("Verifiziert %d (seit Start %d, falsch %d), Bewerbungen %d, Whitelist %d, Befehle %d, Fehler %d",
			b.store.VerifiedCount(), b.stats.verified.Load(), b.stats.failed.Load(), b.stats.applications.Load(),
			len(b.store.InvitesBy("")), b.stats.commands.Load(), b.stats.errors.Load()),
	)
	b.updateMu.Lock()
	lc := b.lastCheck
	b.updateMu.Unlock()
	switch {
	case b.cfg.Updater == nil:
		lines = append(lines, "Updates: aus")
	case lc.at.IsZero():
		lines = append(lines, "Updates: noch nicht geprüft")
	case lc.err != nil:
		lines = append(lines, fmt.Sprintf("Updates: letzter Check %s fehlgeschlagen: %v", lc.at.Format("15:04"), lc.err))
	default:
		lines = append(lines, fmt.Sprintf("Updates: letzter Check %s, neuestes Release %s", lc.at.Format("15:04"), lc.latest))
	}
	b.statusMu.Lock()
	p := b.lastPing
	b.statusMu.Unlock()
	switch {
	case b.cfg.MinecraftAddr == "":
		lines = append(lines, "Minecraft: kein Server eingetragen (KW_MC_ADDR)")
	case p.err != nil:
		lines = append(lines, fmt.Sprintf("Minecraft: offline (%v)", p.err))
	case !p.at.IsZero():
		lines = append(lines, fmt.Sprintf("Minecraft: online, %d/%d Spieler, %s, %d ms", p.status.Online, p.status.Max, p.status.Version, p.status.LatencyMS))
	}
	if b.rcon == nil {
		lines = append(lines, "RCON: nicht konfiguriert")
	} else {
		lines = append(lines, "RCON: "+b.cfg.RCONAddr)
	}
	return strings.Join(lines, "\n")
}

func (b *Bot) reconnects() int64 {
	gw := b.gw.Load()
	if gw == nil {
		return 0
	}
	return gw.Reconnects()
}

func (b *Bot) settingsText(prefix string) string {
	keys := map[string]bool{}
	for k := range defaults {
		keys[k] = true
	}
	ks, overrides := b.store.Settings()
	for _, k := range ks {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		if strings.HasPrefix(k, prefix) {
			sorted = append(sorted, k)
		}
	}
	sort.Strings(sorted)
	var lines []string
	for _, k := range sorted {
		mark := ""
		if _, ok := overrides[k]; ok {
			mark = " *"
		}
		lines = append(lines, fmt.Sprintf("%s = %s%s", k, b.setting(k), mark))
	}
	return "```\n" + strings.Join(lines, "\n") + "\n```\n`*` = überschrieben"
}

func (b *Bot) membersText(ctx context.Context) string {
	members, err := b.rest.Members(ctx, b.guild())
	if err != nil {
		return "Mitglieder lesen ging nicht: " + err.Error()
	}
	roles := map[string]string{
		"role.member": "Mitglied", "role.unverified": "unverified", "role.streamer": "Streamer",
		"role.player": "Spieler", "role.staff": "Staff",
	}
	counts := map[string]int{}
	var loose []string
	humans := 0
	for _, m := range members {
		if m.User == nil || m.User.Bot {
			continue
		}
		humans++
		for key, name := range roles {
			if m.HasRole(b.setting(key)) {
				counts[name]++
			}
		}
		if !m.HasRole(b.setting("role.member")) {
			loose = append(loose, m.User.Username)
		}
	}
	out := fmt.Sprintf("%d Menschen: %d Mitglied, %d unverified, %d Streamer, %d Spieler, %d Staff",
		humans, counts["Mitglied"], counts["unverified"], counts["Streamer"], counts["Spieler"], counts["Staff"])
	if len(loose) > 0 {
		out += "\nOhne Mitglied: " + strings.Join(loose, ", ")
	}
	return out
}
