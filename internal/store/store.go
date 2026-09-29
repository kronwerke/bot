// Package store keeps the bot's state in one JSON file, written atomically after
// every change. The data is small (a few hundred members), so a database would only
// add a dependency.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Captcha is a pending verification.
type Captcha struct {
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
}

// Application is a streamer application and its private channel.
type Application struct {
	UserID    string    `json:"user_id"`
	ChannelID string    `json:"channel_id"`
	Status    string    `json:"status"` // open, accepted, declined
	Created   time.Time `json:"created"`
	Closed    time.Time `json:"closed,omitempty"`
	DecidedBy string    `json:"decided_by,omitempty"`
}

// Grant is a place on the whitelist the team gave without a streamer's slot
// (Season 1 players). The player also gets slots of their own.
type Grant struct {
	Player    string    `json:"player"` // Minecraft name
	DiscordID string    `json:"discord_id"`
	Created   time.Time `json:"created"`
}

// Invite is a whitelist slot given by a streamer to a player.
type Invite struct {
	Player     string    `json:"player"` // Minecraft name
	DiscordID  string    `json:"discord_id"`
	StreamerID string    `json:"streamer_id"` // Discord id of the streamer
	Created    time.Time `json:"created"`
}

type data struct {
	Settings     map[string]string      `json:"settings"`
	Captchas     map[string]Captcha     `json:"captchas"`
	Attempts     map[string]int         `json:"attempts"`
	Lockouts     map[string]time.Time   `json:"lockouts"`
	Applications map[string]Application `json:"applications"`
	Links        map[string]string      `json:"links"`   // discord id -> minecraft name
	Invites      map[string]Invite      `json:"invites"` // lower case minecraft name -> invite
	Verified     map[string]time.Time   `json:"verified"`
	Grants       map[string]Grant       `json:"grants"` // discord id -> grant
}

// Store is safe for concurrent use.
type Store struct {
	path string
	mu   sync.Mutex
	d    data
}

// Open loads the file or starts empty when it does not exist.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	default:
		if err := json.Unmarshal(b, &s.d); err != nil {
			return nil, fmt.Errorf("store: %s is not valid JSON, refusing to overwrite it: %w", path, err)
		}
	}
	s.init()
	return s, nil
}

func (s *Store) init() {
	if s.d.Settings == nil {
		s.d.Settings = map[string]string{}
	}
	if s.d.Captchas == nil {
		s.d.Captchas = map[string]Captcha{}
	}
	if s.d.Attempts == nil {
		s.d.Attempts = map[string]int{}
	}
	if s.d.Lockouts == nil {
		s.d.Lockouts = map[string]time.Time{}
	}
	if s.d.Applications == nil {
		s.d.Applications = map[string]Application{}
	}
	if s.d.Links == nil {
		s.d.Links = map[string]string{}
	}
	if s.d.Invites == nil {
		s.d.Invites = map[string]Invite{}
	}
	if s.d.Verified == nil {
		s.d.Verified = map[string]time.Time{}
	}
	if s.d.Grants == nil {
		s.d.Grants = map[string]Grant{}
	}
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("store: write: %w", err)
	}
	return os.Rename(tmp, s.path)
}

// Update runs fn under the lock and saves afterwards.
func (s *Store) update(fn func(d *data)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
	return s.save()
}

func (s *Store) view(fn func(d *data)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
}

// ---- settings ----

func (s *Store) Setting(key string) (v string) {
	s.view(func(d *data) { v = d.Settings[key] })
	return
}

func (s *Store) SetSetting(key, value string) error {
	return s.update(func(d *data) {
		if value == "" {
			delete(d.Settings, key)
		} else {
			d.Settings[key] = value
		}
	})
}

// Settings returns a copy of all settings, sorted keys first.
func (s *Store) Settings() (keys []string, m map[string]string) {
	m = map[string]string{}
	s.view(func(d *data) {
		for k, v := range d.Settings {
			m[k] = v
			keys = append(keys, k)
		}
	})
	sort.Strings(keys)
	return
}

// ---- captcha ----

func (s *Store) SetCaptcha(user string, c Captcha) error {
	return s.update(func(d *data) { d.Captchas[user] = c })
}

func (s *Store) TakeCaptcha(user string) (c Captcha, ok bool) {
	s.update(func(d *data) {
		c, ok = d.Captchas[user]
		delete(d.Captchas, user)
	})
	return
}

func (s *Store) PeekCaptcha(user string) (c Captcha, ok bool) {
	s.view(func(d *data) { c, ok = d.Captchas[user] })
	return
}

// Fail records a wrong answer and returns the number of failures so far.
func (s *Store) Fail(user string, lockAfter int, lockFor time.Duration) (n int, locked bool) {
	s.update(func(d *data) {
		d.Attempts[user]++
		n = d.Attempts[user]
		if n >= lockAfter {
			d.Lockouts[user] = time.Now().Add(lockFor)
			d.Attempts[user] = 0
			locked = true
		}
	})
	return
}

func (s *Store) LockedUntil(user string) (t time.Time) {
	s.view(func(d *data) { t = d.Lockouts[user] })
	return
}

func (s *Store) MarkVerified(user string) error {
	return s.update(func(d *data) {
		d.Verified[user] = time.Now()
		delete(d.Attempts, user)
		delete(d.Lockouts, user)
		delete(d.Captchas, user)
	})
}

func (s *Store) VerifiedCount() (n int) {
	s.view(func(d *data) { n = len(d.Verified) })
	return
}

// SweepCaptchas drops expired captchas and lockouts.
func (s *Store) SweepCaptchas(now time.Time) error {
	return s.update(func(d *data) {
		for u, c := range d.Captchas {
			if now.After(c.Expires) {
				delete(d.Captchas, u)
			}
		}
		for u, t := range d.Lockouts {
			if now.After(t) {
				delete(d.Lockouts, u)
			}
		}
	})
}

// ---- applications ----

func (s *Store) Application(user string) (a Application, ok bool) {
	s.view(func(d *data) { a, ok = d.Applications[user] })
	return
}

func (s *Store) PutApplication(a Application) error {
	return s.update(func(d *data) { d.Applications[a.UserID] = a })
}

func (s *Store) Applications() (out []Application) {
	s.view(func(d *data) {
		for _, a := range d.Applications {
			out = append(out, a)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return
}

// ---- links and invites ----

func (s *Store) Link(discordID string) (mc string) {
	s.view(func(d *data) { mc = d.Links[discordID] })
	return
}

func (s *Store) SetLink(discordID, mc string) error {
	return s.update(func(d *data) {
		if mc == "" {
			delete(d.Links, discordID)
		} else {
			d.Links[discordID] = mc
		}
	})
}

func (s *Store) Invite(player string) (i Invite, ok bool) {
	s.view(func(d *data) { i, ok = d.Invites[lower(player)] })
	return
}

func (s *Store) PutInvite(i Invite) error {
	return s.update(func(d *data) { d.Invites[lower(i.Player)] = i })
}

func (s *Store) DeleteInvite(player string) error {
	return s.update(func(d *data) { delete(d.Invites, lower(player)) })
}

// InvitesBy returns the invites of a streamer, or all when streamer is "".
func (s *Store) InvitesBy(streamer string) (out []Invite) {
	s.view(func(d *data) {
		for _, i := range d.Invites {
			if streamer == "" || i.StreamerID == streamer {
				out = append(out, i)
			}
		}
	})
	sort.Slice(out, func(a, b int) bool { return out[a].Player < out[b].Player })
	return
}

// InvitesOf returns the invites held by a Discord user.
func (s *Store) InvitesOf(discordID string) (out []Invite) {
	s.view(func(d *data) {
		for _, i := range d.Invites {
			if i.DiscordID == discordID {
				out = append(out, i)
			}
		}
	})
	return
}

// ---- grants ----

func (s *Store) Grant(discordID string) (g Grant, ok bool) {
	s.view(func(d *data) { g, ok = d.Grants[discordID] })
	return
}

// GrantByPlayer finds a grant by Minecraft name, ignoring case.
func (s *Store) GrantByPlayer(player string) (g Grant, ok bool) {
	s.view(func(d *data) {
		for _, x := range d.Grants {
			if lower(x.Player) == lower(player) {
				g, ok = x, true
				return
			}
		}
	})
	return
}

func (s *Store) PutGrant(g Grant) error {
	return s.update(func(d *data) { d.Grants[g.DiscordID] = g })
}

func (s *Store) DeleteGrant(discordID string) error {
	return s.update(func(d *data) { delete(d.Grants, discordID) })
}

func (s *Store) Grants() (out []Grant) {
	s.view(func(d *data) {
		for _, g := range d.Grants {
			out = append(out, g)
		}
	})
	sort.Slice(out, func(a, b int) bool { return out[a].Player < out[b].Player })
	return
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
