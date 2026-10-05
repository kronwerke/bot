// Package bot is the Kronwerke Discord bot: verification with a captcha, streamer
// applications, whitelist slots synced with the Minecraft server, live server status,
// a control channel for the team, and self updates.
package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/link"
	"github.com/kronwerke/bot/internal/rcon"
	"github.com/kronwerke/bot/internal/site"
	"github.com/kronwerke/bot/internal/store"
	"github.com/kronwerke/bot/internal/update"
)

// Config comes from the environment (see config.example.env).
type Config struct {
	Token         string
	StatePath     string
	HTTPAddr      string
	MinecraftAddr string // host:port for the status ping, optional
	RCONAddr      string // host:port, optional
	RCONPassword  string
	Version       string
	Updater       *update.Updater // nil disables self updates
	APIBase       string          // Discord REST base for tests; empty is the real API
	AdminToken    string          // bearer token of the admin API; empty turns it off
	Site          *site.Site      // the website served on /; nil serves nothing
}

// ErrRestart asks main to exit so systemd starts the (new) binary.
var ErrRestart = errors.New("restart requested")

// Bot holds everything the handlers share.
type Bot struct {
	cfg     Config
	log     *slog.Logger
	rest    *discord.REST
	gw      atomic.Pointer[discord.Gateway]
	store   *store.Store
	rcon    *rcon.Client
	link    *link.Hub
	site    *site.Site
	started time.Time

	me      discord.User
	appID   string
	ready   atomic.Bool
	restart chan string

	events eventLog
	stats  stats

	statusMu  sync.Mutex
	lastPing  pingResult
	updateMu  sync.Mutex
	lastCheck checkResult

	apiMu    sync.Mutex
	goals    json.RawMessage // the last "kw admin goals json", for the API
	goalsAt  time.Time
	season   *seasonView // the last "kw admin season json"; nil while Core does not answer it
	presence string      // the status line the gateway shows right now

	siteMu  sync.Mutex
	siteErr string // the last website sync error, reported once

	packNow atomic.Pointer[string] // the pack version the launcher reported last
}

type stats struct {
	verified     atomic.Int64
	failed       atomic.Int64
	applications atomic.Int64
	invites      atomic.Int64
	commands     atomic.Int64
	errors       atomic.Int64
}

// New validates the config and opens the store.
func New(cfg Config, log *slog.Logger) (*Bot, error) {
	if cfg.Token == "" || strings.Contains(cfg.Token, "PASTE") {
		return nil, errors.New("DISCORD_TOKEN is empty or still the placeholder. Put the bot token into the env file")
	}
	st, err := store.Open(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	b := &Bot{
		cfg: cfg, log: log, store: st, started: time.Now(),
		rest:    discord.NewREST(cfg.Token, cfg.Version),
		restart: make(chan string, 1),
	}
	if cfg.APIBase != "" {
		b.rest.Base = cfg.APIBase
	}
	b.events.max = 200
	b.events.log = log
	if cfg.RCONAddr != "" {
		b.rcon = &rcon.Client{Addr: cfg.RCONAddr, Password: cfg.RCONPassword}
	}
	b.link = b.newHub()
	b.site = cfg.Site
	if b.site != nil {
		b.site.Load()
	}
	return b, nil
}

// Run connects to Discord and serves until ctx ends, a restart is requested, or the
// gateway fails fatally. Self updates run independently of the gateway, so a broken
// Discord connection can still be fixed by a new release.
func (b *Bot) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go b.serveHTTP(ctx)
	if b.cfg.Updater != nil {
		go b.updateLoop(ctx)
	}
	if b.site != nil {
		go b.siteLoop(ctx)
	}
	go b.connect(ctx)

	go b.loop(ctx, "captcha sweep", time.Minute, func(ctx context.Context) { b.store.SweepCaptchas(time.Now()) })
	go b.loop(ctx, "applications", 30*time.Minute, b.sweepApplications)
	go b.loop(ctx, "privacy", time.Hour, b.sweepPrivacy)
	go b.loop(ctx, "server status", b.duration("status.interval", time.Minute), b.refreshStatus)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case reason := <-b.restart:
		b.log.Info("restarting", "reason", reason)
		b.control(ctx, "Neustart: "+reason)
		return ErrRestart
	}
}

// connect keeps trying to reach Discord. The process never exits because Discord is
// unreachable or the token is wrong: the updater must keep running so a fixed release
// can still arrive, and the health endpoint reports the state.
func (b *Bot) connect(ctx context.Context) {
	wait := 30 * time.Second
	for {
		url, err := b.rest.GatewayURL(ctx)
		if err == nil {
			gw := &discord.Gateway{
				Token:   b.cfg.Token,
				Intents: discord.IntentGuilds | discord.IntentGuildMembers | discord.IntentGuildMessages | discord.IntentMessageContent,
				URL:     url,
				Log:     b.log,
				Handle:  b.dispatch,
			}
			b.gw.Store(gw)
			err = gw.Run(ctx)
			b.ready.Store(false)
		}
		if ctx.Err() != nil {
			return
		}
		if discord.IsStatus(err, 401) {
			err = errors.New("Discord refused the token (401). Put a valid bot token into /etc/kronwerke-bot/bot.env")
		}
		b.stats.errors.Add(1)
		b.events.add("discord unreachable: %v", err)
		b.log.Error("discord", "err", err, "retry_in", wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		if wait < 5*time.Minute {
			wait *= 2
		}
	}
}

func (b *Bot) loop(ctx context.Context, name string, every time.Duration, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer b.recover(name)
				fn(ctx)
			}()
		}
	}
}

func (b *Bot) recover(where string) {
	if r := recover(); r != nil {
		b.stats.errors.Add(1)
		b.log.Error("panic", "where", where, "panic", r, "stack", string(debug.Stack()))
		b.events.add("panic in %s: %v", where, r)
	}
}

// dispatch routes gateway events. Each event runs in its own goroutine so a slow REST
// call never blocks the read loop (and with it the heartbeats).
func (b *Bot) dispatch(event string, data json.RawMessage) {
	go func() {
		defer b.recover(event)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		switch event {
		case "READY":
			b.onReady(ctx, data)
		case "RESUMED":
			b.events.add("gateway resumed")
		case "GUILD_MEMBER_ADD":
			var m discord.Member
			if json.Unmarshal(data, &m) == nil && m.GuildID == b.guild() {
				b.onMemberJoin(ctx, m)
			}
		case "GUILD_MEMBER_REMOVE":
			var ev struct {
				GuildID string       `json:"guild_id"`
				User    discord.User `json:"user"`
			}
			if json.Unmarshal(data, &ev) == nil && ev.GuildID == b.guild() {
				b.onMemberLeave(ctx, ev.User)
				b.forget(ev.User.ID)
			}
		case "MESSAGE_CREATE":
			var m discord.Message
			if json.Unmarshal(data, &m) == nil {
				b.onMessage(ctx, m)
			}
		case "INTERACTION_CREATE":
			var i discord.Interaction
			if json.Unmarshal(data, &i) == nil {
				b.onInteraction(ctx, &i)
			}
		}
	}()
}

func (b *Bot) onReady(ctx context.Context, data json.RawMessage) {
	var r struct {
		User        discord.User `json:"user"`
		Application struct {
			ID string `json:"id"`
		} `json:"application"`
	}
	json.Unmarshal(data, &r)
	b.me = r.User
	b.appID = r.Application.ID
	first := !b.ready.Swap(true)
	b.events.add("ready as %s", r.User.Username)
	if !first {
		return
	}
	if b.cfg.Updater != nil {
		b.cfg.Updater.Settle()
	}
	if err := b.registerCommands(ctx); err != nil {
		b.fail(ctx, "Slash commands konnten nicht registriert werden", err)
	}
	b.ensureVerifyPanel(ctx)
	b.presence = ""
	b.updatePresence()
	b.control(ctx, fmt.Sprintf("Online: %s. Befehle: `!help`", b.cfg.Version))
	b.refreshStatus(ctx)
}

// ---- logging to Discord ----

// control posts to the control channel.
func (b *Bot) control(ctx context.Context, text string) {
	ch := b.setting("channel.control")
	if ch == "" {
		return
	}
	for _, part := range chunks(text, 1900) {
		if _, err := b.rest.SendMessage(ctx, ch, discord.MessageSend{Content: part, AllowedMentions: discord.NoMentions}); err != nil {
			b.log.Warn("control channel", "err", err)
			return
		}
	}
}

// audit posts a line to the team's log channel.
func (b *Bot) audit(ctx context.Context, text string) {
	b.events.add("%s", text)
	ch := b.setting("channel.logs")
	if ch == "" {
		return
	}
	b.rest.SendMessage(ctx, ch, discord.MessageSend{Content: text, AllowedMentions: discord.NoMentions})
}

// fail logs an error everywhere a person will see it.
func (b *Bot) fail(ctx context.Context, what string, err error) {
	b.stats.errors.Add(1)
	b.log.Error(what, "err", err)
	b.events.add("ERROR %s: %v", what, err)
	b.control(ctx, fmt.Sprintf("Fehler: %s: `%v`", what, err))
}

func chunks(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := strings.LastIndex(s[:n], "\n")
		if cut <= 0 {
			cut = n
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

// ---- recent events, what the bot saw ----

type eventLog struct {
	mu    sync.Mutex
	max   int
	items []string
	log   *slog.Logger // every event also goes to the journal
}

func (e *eventLog) add(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if e.log != nil {
		e.log.Info("event", "what", msg)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	line := time.Now().UTC().Format("01-02 15:04:05") + " " + msg
	e.items = append(e.items, line)
	if len(e.items) > e.max {
		e.items = e.items[len(e.items)-e.max:]
	}
}

func (e *eventLog) last(n int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n > len(e.items) {
		n = len(e.items)
	}
	return append([]string(nil), e.items[len(e.items)-n:]...)
}

func (b *Bot) isTeam(m *discord.Member) bool {
	return m.HasRole(b.setting("role.lead"), b.setting("role.admin"), b.setting("role.staff"))
}

func (b *Bot) isLead(m *discord.Member) bool {
	return m.HasRole(b.setting("role.lead"), b.setting("role.admin"))
}
