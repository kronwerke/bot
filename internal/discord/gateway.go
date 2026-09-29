package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kronwerke/bot/internal/ws"
)

// Gateway opcodes.
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatAck   = 11
)

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// Handler receives dispatch events by name (READY, MESSAGE_CREATE, ...).
type Handler func(event string, data json.RawMessage)

// FatalError is a close code after which reconnecting is pointless (bad token,
// disallowed intents).
type FatalError struct{ Code int }

func (e *FatalError) Error() string {
	switch e.Code {
	case 4004:
		return "discord gateway: the token was rejected (4004). Put a valid bot token into the env file."
	case 4014:
		return "discord gateway: an intent is not enabled for this bot (4014). Enable Server Members and Message Content in the developer portal."
	default:
		return fmt.Sprintf("discord gateway: closed with %d, not reconnecting", e.Code)
	}
}

// Gateway keeps one connection to the Discord gateway alive and resumes it when it drops.
type Gateway struct {
	Token   string
	Intents int
	URL     string // from /gateway/bot, without query
	Handle  Handler
	Log     *slog.Logger

	seq        atomic.Int64
	sessionID  string
	resumeURL  string
	reconnects atomic.Int64

	mu   sync.Mutex
	conn *ws.Conn
}

// Reconnects counts how often the connection was re-established.
func (g *Gateway) Reconnects() int64 { return g.reconnects.Load() }

// Run connects and stays connected until ctx ends or a fatal close code arrives.
func (g *Gateway) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		err := g.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var fe *FatalError
		if errors.As(err, &fe) {
			return err
		}
		g.reconnects.Add(1)
		g.Log.Warn("gateway disconnected, reconnecting", "err", err, "resume", g.sessionID != "", "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < time.Minute {
			backoff *= 2
		}
		if err == nil {
			backoff = time.Second
		}
	}
}

func (g *Gateway) session(ctx context.Context) error {
	base := g.URL
	resuming := g.sessionID != "" && g.resumeURL != ""
	if resuming {
		base = g.resumeURL
	}
	conn, err := ws.Dial(ctx, base+"/?v=10&encoding=json", nil)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.conn = conn
	g.mu.Unlock()
	// 1000 would invalidate the session; anything else keeps it resumable.
	defer conn.Close(4000)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-sctx.Done()
		if ctx.Err() != nil {
			conn.Close(1000)
		}
	}()

	var acked atomic.Bool
	acked.Store(true)
	hbErr := make(chan error, 1)

	for {
		conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
		_, msg, err := conn.Read()
		if err != nil {
			var ce *ws.CloseError
			if errors.As(err, &ce) {
				switch ce.Code {
				case 4004, 4010, 4011, 4012, 4013, 4014:
					return &FatalError{Code: ce.Code}
				case 4007, 4009:
					g.sessionID = "" // cannot resume
				}
			}
			select {
			case e := <-hbErr:
				return e
			default:
			}
			return err
		}
		var p payload
		if err := json.Unmarshal(msg, &p); err != nil {
			g.Log.Warn("gateway: bad payload", "err", err)
			continue
		}
		if p.S != nil {
			g.seq.Store(*p.S)
		}
		switch p.Op {
		case opHello:
			var h struct {
				HeartbeatInterval int `json:"heartbeat_interval"`
			}
			json.Unmarshal(p.D, &h)
			interval := time.Duration(h.HeartbeatInterval) * time.Millisecond
			go g.heartbeat(sctx, conn, interval, &acked, hbErr)
			if resuming {
				err = g.send(conn, opResume, map[string]any{"token": g.Token, "session_id": g.sessionID, "seq": g.seq.Load()})
			} else {
				err = g.send(conn, opIdentify, map[string]any{
					"token":   g.Token,
					"intents": g.Intents,
					"properties": map[string]string{
						"os": "linux", "browser": "kronwerke-bot", "device": "kronwerke-bot",
					},
				})
			}
			if err != nil {
				return err
			}
		case opHeartbeatAck:
			acked.Store(true)
		case opHeartbeat:
			if err := g.send(conn, opHeartbeat, g.seq.Load()); err != nil {
				return err
			}
		case opReconnect:
			return errors.New("gateway asked to reconnect")
		case opInvalidSession:
			var resumable bool
			json.Unmarshal(p.D, &resumable)
			if !resumable {
				g.sessionID = ""
				g.seq.Store(0)
			}
			time.Sleep(time.Duration(1000+rand.IntN(4000)) * time.Millisecond)
			return errors.New("gateway: invalid session")
		case opDispatch:
			if p.T == "READY" {
				var r struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
				}
				json.Unmarshal(p.D, &r)
				g.sessionID = r.SessionID
				g.resumeURL = r.ResumeGatewayURL
			}
			if g.Handle != nil {
				g.Handle(p.T, p.D)
			}
		}
	}
}

func (g *Gateway) heartbeat(ctx context.Context, conn *ws.Conn, interval time.Duration, acked *atomic.Bool, errc chan<- error) {
	first := time.Duration(rand.Float64() * float64(interval))
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !acked.Load() {
			errc <- errors.New("gateway: heartbeat not acknowledged, connection is a zombie")
			conn.Close(4000)
			return
		}
		acked.Store(false)
		if err := g.send(conn, opHeartbeat, g.seq.Load()); err != nil {
			errc <- err
			return
		}
		t.Reset(interval)
	}
}

func (g *Gateway) send(conn *ws.Conn, op int, d any) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(payload{Op: op, D: raw})
	return conn.WriteText(b)
}

// UpdatePresence sets the bot's status line.
func (g *Gateway) UpdatePresence(text string) error {
	g.mu.Lock()
	c := g.conn
	g.mu.Unlock()
	if c == nil {
		return errors.New("gateway: not connected")
	}
	return g.send(c, 3, map[string]any{
		"since": nil, "afk": false, "status": "online",
		"activities": []map[string]any{{"name": text, "type": 4, "state": text}},
	})
}
