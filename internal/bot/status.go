package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/mcping"
)

type pingResult struct {
	at     time.Time
	status mcping.Status
	err    error
}

// refreshStatus pings the Minecraft server and keeps one message in the status channel
// current. The message is only edited when what it says changes.
func (b *Bot) refreshStatus(ctx context.Context) {
	_, linked := b.link.Connected()
	if b.cfg.MinecraftAddr != "" || linked {
		var st mcping.Status
		var err error
		if b.cfg.MinecraftAddr != "" {
			st, err = mcping.Ping(b.cfg.MinecraftAddr, 5*time.Second)
		} else {
			st, err = b.pingViaLink()
		}
		b.statusMu.Lock()
		was := b.lastPing
		b.lastPing = pingResult{at: time.Now(), status: st, err: err}
		b.statusMu.Unlock()
		if (was.err == nil) != (err == nil) && !was.at.IsZero() {
			if err != nil {
				b.audit(ctx, "Minecraft-Server nicht erreichbar: "+err.Error())
			} else {
				b.audit(ctx, "Minecraft-Server wieder erreichbar.")
			}
		}
		b.postStatus(ctx)
	}
	b.postProgress(ctx)
}

func (b *Bot) postStatus(ctx context.Context) {
	ch := b.setting("channel.status")
	if ch == "" || !b.ready.Load() {
		return
	}
	b.statusMu.Lock()
	p := b.lastPing
	b.statusMu.Unlock()

	var e discord.Embed
	if p.err != nil {
		e = discord.Embed{Title: "Server offline", Description: "Der Minecraft-Server antwortet gerade nicht.", Color: 0xE74C3C}
	} else {
		desc := fmt.Sprintf("**%d / %d** Spieler online", p.status.Online, p.status.Max)
		if len(p.status.Players) > 0 {
			desc += "\n" + strings.Join(p.status.Players, ", ")
		}
		e = discord.Embed{Title: "Server online", Description: desc, Color: 0x2ECC71,
			Fields: []discord.EmbedField{{Name: "Version", Value: p.status.Version, Inline: true}}}
	}
	sig := e.Title + "|" + e.Description
	if b.store.Setting("status.sig") == sig {
		return
	}
	e.Footer = &discord.EmbedFooter{Text: "Stand " + time.Now().In(berlin).Format("02.01. 15:04")}
	msg := discord.MessageSend{Embeds: []discord.Embed{e}, Components: []discord.Component{}}
	if id := b.store.Setting("msg.status"); id != "" {
		if _, err := b.rest.EditMessage(ctx, ch, id, msg); err == nil {
			b.store.SetSetting("status.sig", sig)
			return
		} else if !discord.IsStatus(err, 404) {
			b.log.Warn("status message", "err", err)
			return
		}
	}
	m, err := b.rest.SendMessage(ctx, ch, msg)
	if err != nil {
		b.fail(ctx, "Status-Nachricht", err)
		return
	}
	b.store.SetSetting("msg.status", m.ID)
	b.store.SetSetting("status.sig", sig)
}

// pingViaLink asks the launcher when there is no address to ping (KW_MC_ADDR empty):
// the server counts as online while Minecraft runs, and "list" gives the players.
func (b *Bot) pingViaLink() (mcping.Status, error) {
	i, ok := b.link.Connected()
	if !ok {
		return mcping.Status{}, errNoServer
	}
	if i.State != "running" {
		return mcping.Status{}, fmt.Errorf("Minecraft %s", stateWord(i.State))
	}
	// the launcher's status has the pack that runs now (the link info has the one from
	// when it connected) and the answer to "list"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := b.link.Request(ctx, "status", nil)
	if err != nil {
		return mcping.Status{}, err
	}
	var s struct {
		State   string `json:"state"`
		Pack    string `json:"pack"`
		Players string `json:"players"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return mcping.Status{}, err
	}
	if s.State != "running" {
		return mcping.Status{}, fmt.Errorf("Minecraft %s", stateWord(s.State))
	}
	st, err := parseList(s.Players)
	st.Version = "Pack " + s.Pack
	b.packNow.Store(&s.Pack)
	return st, err
}

var listLine = regexp.MustCompile(`There are (\d+) of a max of (\d+) players online:?(.*)`)

// parseList reads the answer of the vanilla "list" command.
func parseList(out string) (mcping.Status, error) {
	m := listLine.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return mcping.Status{}, fmt.Errorf("unexpected answer to list: %q", out)
	}
	var st mcping.Status
	st.Online, _ = strconv.Atoi(m[1])
	st.Max, _ = strconv.Atoi(m[2])
	for _, n := range strings.Split(m[3], ",") {
		if n = strings.TrimSpace(n); n != "" {
			st.Players = append(st.Players, n)
		}
	}
	return st, nil
}

var berlin = func() *time.Location {
	l, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return time.UTC
	}
	return l
}()

// goalView is what Kronwerke Core prints for "kw admin goals json".
type goalView struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	State   string `json:"state"` // locked, active, held, done
	Percent int    `json:"percent"`
	Pillars []struct {
		Title string `json:"title"`
		Items []struct {
			Item   string `json:"item"`
			Name   string `json:"name"`
			Have   int64  `json:"have"`
			Target int64  `json:"target"`
		} `json:"items"`
	} `json:"pillars"`
	Top []struct {
		Name   string `json:"name"`
		Amount int64  `json:"amount"`
	} `json:"top"`
}

// postProgress fetches the goals for the API and keeps a live board of the active
// community goal, when a channel is set.
func (b *Bot) postProgress(ctx context.Context) {
	if !b.serverConnected() {
		return
	}
	out, err := b.serverCommand("kw admin goals json")
	if err != nil {
		b.events.add("progress: %v", err)
		return
	}
	if json.Valid([]byte(out)) {
		b.apiMu.Lock()
		b.goals, b.goalsAt = json.RawMessage(out), time.Now()
		b.apiMu.Unlock()
	}
	ch := b.setting("channel.progress")
	if ch == "" || !b.ready.Load() {
		return
	}
	var goals []goalView
	if err := json.Unmarshal([]byte(out), &goals); err != nil {
		b.events.add("progress: bad json: %v", err)
		return
	}
	var g *goalView
	for k := range goals {
		if goals[k].State == "active" || goals[k].State == "held" {
			g = &goals[k]
			break
		}
	}
	if g == nil {
		return
	}
	e := discord.Embed{Title: g.Title, Color: 0xDAA520}
	bar := progressBar(g.Percent)
	e.Description = fmt.Sprintf("%s **%d%%**", bar, g.Percent)
	if g.State == "held" {
		e.Description += "\nFast geschafft. Der Rest kommt beim gemeinsamen Event rein, Termin in den Ankündigungen."
		e.Color = 0x9B59B6
	}
	for _, p := range g.Pillars {
		var lines []string
		for _, it := range p.Items {
			name := it.Name
			if name == "" {
				name = it.Item
			}
			lines = append(lines, fmt.Sprintf("%s: %d / %d", name, it.Have, it.Target))
		}
		e.Fields = append(e.Fields, discord.EmbedField{Name: p.Title, Value: strings.Join(lines, "\n"), Inline: true})
	}
	if len(g.Top) > 0 {
		var lines []string
		for k, t := range g.Top {
			if k >= 5 {
				break
			}
			lines = append(lines, fmt.Sprintf("%d. %s (%d)", k+1, t.Name, t.Amount))
		}
		e.Fields = append(e.Fields, discord.EmbedField{Name: "Am meisten beigetragen", Value: strings.Join(lines, "\n")})
	}
	sig := fmt.Sprintf("%s|%d|%s", g.ID, g.Percent, g.State)
	for _, f := range e.Fields {
		sig += "|" + f.Value
	}
	if b.store.Setting("progress.sig") == sig {
		return
	}
	e.Footer = &discord.EmbedFooter{Text: "Stand " + time.Now().In(berlin).Format("02.01. 15:04")}
	msg := discord.MessageSend{Embeds: []discord.Embed{e}, Components: []discord.Component{}}
	if id := b.store.Setting("msg.progress"); id != "" {
		if _, err := b.rest.EditMessage(ctx, ch, id, msg); err == nil {
			b.store.SetSetting("progress.sig", sig)
			return
		}
	}
	m, err := b.rest.SendMessage(ctx, ch, msg)
	if err != nil {
		b.fail(ctx, "Fortschritts-Nachricht", err)
		return
	}
	b.store.SetSetting("msg.progress", m.ID)
	b.store.SetSetting("progress.sig", sig)
}

func progressBar(pct int) string {
	pct = max(0, min(100, pct))
	full := pct / 10
	return "`" + strings.Repeat("█", full) + strings.Repeat("░", 10-full) + "`"
}
