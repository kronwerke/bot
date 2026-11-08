package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/kronwerke/bot/internal/store"
)

// lastRestore is the last place difference posted after a connect, so it is posted once.
var lastRestore atomic.Value

// rosterView is what Kronwerke Core prints for "kw admin roster json".
type rosterView struct {
	DefaultSlots int `json:"defaultSlots"`
	Streamers    []struct {
		Name    string `json:"name"`
		Granted bool   `json:"granted"`
		Invited []struct {
			Name string `json:"name"`
		} `json:"invited"`
	} `json:"streamers"`
}

func (b *Bot) roster() (*rosterView, error) {
	out, err := b.serverCommand("kw admin roster json")
	if err != nil {
		return nil, err
	}
	var r rosterView
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return nil, fmt.Errorf("roster: %w", err)
	}
	return &r, nil
}

// missing lists the places the bot keeps that the server does not know: own places
// (grants) and slots streamers gave (invites). The bot's store outlives a new world, Core's
// slot data does not, so after a world reset everything shows up here.
func (b *Bot) missing(r *rosterView) (grants []store.Grant, invites []store.Invite) {
	granted := map[string]bool{}
	invited := map[string]bool{} // "streamer/player", lower case
	for _, s := range r.Streamers {
		if s.Granted {
			granted[strings.ToLower(s.Name)] = true
		}
		for _, i := range s.Invited {
			invited[strings.ToLower(s.Name)+"/"+strings.ToLower(i.Name)] = true
		}
	}
	for _, g := range b.store.Grants() {
		if !granted[strings.ToLower(g.Player)] {
			grants = append(grants, g)
		}
	}
	for _, i := range b.store.InvitesBy("") {
		streamer := b.store.Link(i.StreamerID)
		if streamer == "" || !invited[strings.ToLower(streamer)+"/"+strings.ToLower(i.Player)] {
			invites = append(invites, i)
		}
	}
	return
}

// restorePlaces compares the bot's places with the server's. With apply it puts every
// missing place back, own places first so the streamers exist before their slots are used.
func (b *Bot) restorePlaces(apply bool) (string, error) {
	r, err := b.roster()
	if err != nil {
		return "", err
	}
	grants, invites := b.missing(r)
	if len(grants) == 0 && len(invites) == 0 {
		return "Server und Bot stimmen überein.", nil
	}
	var done, failed, open []string
	for _, g := range grants {
		label := g.Player + " (eigener Platz)"
		if !apply {
			open = append(open, label)
			continue
		}
		cmd := "kw admin grant " + g.Player
		if g.Kind != store.KindStreamer {
			cmd += fmt.Sprintf(" %d", b.number("s1.slots", 2))
		}
		if _, err := b.serverCommand(cmd); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		done = append(done, label)
	}
	for _, i := range invites {
		streamer := b.store.Link(i.StreamerID)
		label := fmt.Sprintf("%s (von %s)", i.Player, streamer)
		if streamer == "" {
			failed = append(failed, fmt.Sprintf("%s: der Streamer <@%s> hat keinen Namen verknüpft", i.Player, i.StreamerID))
			continue
		}
		if !apply {
			open = append(open, label)
			continue
		}
		if _, err := b.serverCommand(fmt.Sprintf("kw admin invite %s %s", streamer, i.Player)); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		done = append(done, label)
	}
	var lines []string
	if len(open) > 0 {
		lines = append(lines, fmt.Sprintf("Beim Bot, aber nicht auf dem Server: %s. `!sync restore` trägt sie nach.", strings.Join(open, ", ")))
	}
	if len(done) > 0 {
		lines = append(lines, fmt.Sprintf("Auf dem Server nachgetragen: %s.", strings.Join(done, ", ")))
	}
	if len(failed) > 0 {
		lines = append(lines, "Nicht nachgetragen: "+strings.Join(failed, "; ")+".")
	}
	return strings.Join(lines, "\n"), nil
}

// restoreAfterConnect runs when the server connects. A server that knows no streamer at
// all has a new world, and gets every place back at once; otherwise differences are only
// reported, once each, since a streamer may have freed a slot in game.
func (b *Bot) restoreAfterConnect(ctx context.Context) {
	r, err := b.roster()
	if err != nil {
		b.events.add("restore: %v", err)
		return
	}
	fresh := len(r.Streamers) == 0 && len(b.store.Grants()) > 0
	out, err := b.restorePlaces(fresh)
	if err != nil {
		b.events.add("restore: %v", err)
		return
	}
	if out == "Server und Bot stimmen überein." {
		lastRestore.Store("")
		return
	}
	if fresh {
		out = "Neue Welt erkannt.\n" + out
	}
	if prev, _ := lastRestore.Load().(string); prev == out {
		return
	}
	lastRestore.Store(out)
	b.control(ctx, out)
}
