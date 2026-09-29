// Package link is the bot's end of the connection to the Minecraft server. The Kronwerke
// launcher on the server dials in over a WebSocket, so the server needs no open port and
// the host needs no API. The bot sends requests (commands, start, stop, logs, files), the
// launcher answers and reports what happens.
//
// A launcher proves who it is with a key it made itself. A key the bot has not seen
// waits until the team accepts its fingerprint in the control channel; only a hash of
// the key is stored.
package link

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kronwerke/bot/internal/ws"
)

// ErrNotConnected is returned by Request while no accepted server is connected.
var ErrNotConnected = errors.New("the Minecraft server is not connected")

// Keys is where accepted keys live (the bot's store).
type Keys interface {
	// LinkKey returns the stored hash for a fingerprint.
	LinkKey(fingerprint string) (hash string, ok bool)
}

// Hooks tell the bot what happened. Every field may be nil.
type Hooks struct {
	Connected    func(info Info)
	Disconnected func(info Info, err error)
	Pending      func(info Info)
	State        func(info Info, state, detail string)
}

// Info describes a connected or waiting launcher.
type Info struct {
	Name        string    `json:"name"`
	Launcher    string    `json:"launcher"`
	Pack        string    `json:"pack"`
	State       string    `json:"state"`
	Fingerprint string    `json:"fingerprint"`
	Remote      string    `json:"remote"`
	Since       time.Time `json:"since"`
}

// Hub accepts launchers and routes requests to the one that is connected.
type Hub struct {
	Keys  Keys
	Hooks Hooks
	Log   *slog.Logger

	mu      sync.Mutex
	cur     *session
	pending map[string]Info
	hashes  map[string]string // fingerprint -> key hash of waiting launchers
	nextID  atomic.Int64
}

type session struct {
	conn    *ws.ServerConn
	info    Info
	mu      sync.Mutex
	waiting map[string]chan reply
	done    chan struct{}
}

type reply struct {
	ok    bool
	data  json.RawMessage
	error string
}

type message struct {
	Type        string          `json:"type"`
	Key         string          `json:"key,omitempty"`
	Name        string          `json:"name,omitempty"`
	Launcher    string          `json:"launcher,omitempty"`
	State       string          `json:"state,omitempty"`
	Pack        string          `json:"pack,omitempty"`
	ID          json.RawMessage `json:"id,omitempty"`
	OK          bool            `json:"ok,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	Error       string          `json:"error,omitempty"`
	Event       string          `json:"event,omitempty"`
	Detail      string          `json:"detail,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
}

// Fingerprint is what the team compares: the first 12 hex characters of the key's hash.
func Fingerprint(key string) string { return Hash(key)[:12] }

// Hash is what the bot stores for an accepted key.
func Hash(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:])
}

// Connected reports the launcher that is connected, if any.
func (h *Hub) Connected() (Info, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil {
		return Info{}, false
	}
	return h.cur.info, true
}

// Pending lists launchers waiting to be accepted.
func (h *Hub) Pending() []Info {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Info
	for _, i := range h.pending {
		out = append(out, i)
	}
	return out
}

// PendingInfo returns a waiting launcher by fingerprint.
func (h *Hub) PendingInfo(fp string) (Info, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i, ok := h.pending[fp]
	return i, ok
}

// Accept returns the key hash of a waiting launcher, for the caller to store. The
// launcher is connected within seconds once the hash is in Keys.
func (h *Hub) Accept(fp string) (hash string, info Info, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	info, ok = h.pending[fp]
	return h.hashes[fp], info, ok
}

// Kick closes the connection of a launcher, for example after its key was revoked.
func (h *Hub) Kick(fp string) {
	h.mu.Lock()
	s := h.cur
	h.mu.Unlock()
	if s != nil && s.info.Fingerprint == fp {
		s.conn.CloseWith(4001)
	}
}

// ServeHTTP upgrades the request and runs the session until it ends.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		http.Error(w, "websocket only", http.StatusBadRequest)
		return
	}
	defer conn.Close()
	remote := r.Header.Get("X-Forwarded-For")
	if remote == "" {
		remote = conn.RemoteAddr()
	}

	// the first message must be a hello
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	_, raw, err := conn.Read()
	if err != nil {
		return
	}
	var hello message
	if json.Unmarshal(raw, &hello) != nil || hello.Type != "hello" || len(hello.Key) < 32 {
		conn.CloseWith(4000)
		return
	}
	info := Info{Name: hello.Name, Launcher: hello.Launcher, Pack: hello.Pack, State: hello.State,
		Fingerprint: Fingerprint(hello.Key), Remote: remote, Since: time.Now()}

	if !h.accepted(hello.Key) {
		h.waitForAcceptance(conn, hello.Key, info)
		return
	}
	h.run(conn, info)
}

func (h *Hub) accepted(key string) bool {
	if h.Keys == nil {
		return false
	}
	stored, ok := h.Keys.LinkKey(Fingerprint(key))
	return ok && subtle.ConstantTimeCompare([]byte(stored), []byte(Hash(key))) == 1
}

// waitForAcceptance keeps an unknown launcher on the line until the team accepts it
// (then it is served right away) or it goes away.
func (h *Hub) waitForAcceptance(conn *ws.ServerConn, key string, info Info) {
	h.mu.Lock()
	if h.pending == nil {
		h.pending, h.hashes = map[string]Info{}, map[string]string{}
	}
	_, seen := h.pending[info.Fingerprint]
	h.pending[info.Fingerprint] = info
	h.hashes[info.Fingerprint] = Hash(key)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, info.Fingerprint)
		delete(h.hashes, info.Fingerprint)
		h.mu.Unlock()
	}()
	write(conn, map[string]any{"type": "pending", "fingerprint": info.Fingerprint})
	if !seen && h.Hooks.Pending != nil {
		h.Hooks.Pending(info)
	}
	// read in the background so pings are answered and a hang up is noticed
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
			_, raw, err := conn.Read()
			if err != nil {
				return
			}
			var m message
			if json.Unmarshal(raw, &m) == nil && m.Type == "ping" {
				write(conn, map[string]string{"type": "pong"})
			}
		}
	}()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-gone:
			return
		case <-t.C:
			if h.accepted(key) {
				// start over on a fresh connection: the launcher reconnects at once
				conn.CloseWith(4002)
				return
			}
		}
	}
}

func (h *Hub) run(conn *ws.ServerConn, info Info) {
	s := &session{conn: conn, info: info, waiting: map[string]chan reply{}, done: make(chan struct{})}
	h.mu.Lock()
	old := h.cur
	h.cur = s
	h.mu.Unlock()
	if old != nil {
		old.conn.CloseWith(4003) // replaced by a newer connection
	}
	write(conn, map[string]string{"type": "welcome"})
	if h.Hooks.Connected != nil {
		go h.Hooks.Connected(info)
	}

	var err error
	for {
		conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		var raw []byte
		_, raw, err = conn.Read()
		if err != nil {
			break
		}
		var m message
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Type {
		case "ping":
			write(conn, map[string]string{"type": "pong"})
		case "res":
			id := string(m.ID)
			if uq, e := strconv.Unquote(id); e == nil {
				id = uq
			}
			s.mu.Lock()
			ch := s.waiting[id]
			delete(s.waiting, id)
			s.mu.Unlock()
			if ch != nil {
				ch <- reply{ok: m.OK, data: m.Data, error: m.Error}
			}
		case "event":
			if m.Event == "state" {
				h.mu.Lock()
				s.info.State = m.State
				info = s.info
				h.mu.Unlock()
				if h.Hooks.State != nil {
					go h.Hooks.State(info, m.State, m.Detail)
				}
			}
		}
	}
	close(s.done)
	h.mu.Lock()
	if h.cur == s {
		h.cur = nil
	}
	h.mu.Unlock()
	if h.Hooks.Disconnected != nil {
		go h.Hooks.Disconnected(info, err)
	}
}

// Request sends one request to the connected launcher and waits for its answer.
func (h *Hub) Request(ctx context.Context, op string, args any) (json.RawMessage, error) {
	h.mu.Lock()
	s := h.cur
	h.mu.Unlock()
	if s == nil {
		return nil, ErrNotConnected
	}
	id := strconv.FormatInt(h.nextID.Add(1), 10)
	ch := make(chan reply, 1)
	s.mu.Lock()
	s.waiting[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiting, id)
		s.mu.Unlock()
	}()
	if args == nil {
		args = map[string]any{}
	}
	if err := write(s.conn, map[string]any{"type": "req", "id": id, "op": op, "args": args}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if !r.ok {
			return nil, errors.New(r.error)
		}
		return r.data, nil
	case <-s.done:
		return nil, errors.New("the Minecraft server disconnected")
	case <-ctx.Done():
		return nil, fmt.Errorf("no answer from the Minecraft server: %w", ctx.Err())
	}
}

func write(conn *ws.ServerConn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(b)
}
