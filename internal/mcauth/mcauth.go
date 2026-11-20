// Package mcauth is the sign in endpoint on auth.kronwerke.com: a "server" that speaks only
// the first steps of the Minecraft protocol. A player adds it in the multiplayer screen and
// joins; the client logs in with Mojang as for any online mode server, this side asks
// Mojang's session server whether that really happened (hasJoined), and then disconnects the
// player with a short code. The code, typed on the website, signs the browser in as that
// account. No world, no game, a few kilobytes per connection, so it runs inside the bot.
//
// Supported: clients from 1.20.2 (protocol 764) on, the login format of today's clients.
// Older ones get a plain message in their language-neutral form.
package mcauth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Account is a Minecraft account Mojang vouched for.
type Account struct {
	UUID string // without dashes, as Mojang answers it
	Name string
}

// Server is the endpoint. OnAuth makes the code for an account; its answer is shown to
// the player in the disconnect screen.
type Server struct {
	Addr       string
	Log        *slog.Logger
	OnAuth     func(Account) (code string, err error)
	SessionURL string        // Mojang's hasJoined; empty is the real one
	HTTP       *http.Client  // for the session server; nil is a client with a 10 s timeout
	PerMinute  int           // connections per address and minute; 0 is 20
	Timeout    time.Duration // per connection; 0 is 20 s

	key  *rsa.PrivateKey
	pub  []byte
	mu   sync.Mutex
	hits map[string][]time.Time
	ln   net.Listener
}

const minProtocol = 764 // 1.20.2: login start carries the uuid, encryption response has no salt

// ListenAndServe runs until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context) error {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return err
	}
	s.key = key
	s.pub, err = x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	s.hits = map[string][]time.Time{}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go s.handle(c)
	}
}

// Listening is the bound address once the listener is up (for tests).
func (s *Server) Listening() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *Server) allow(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	limit := s.PerMinute
	if limit <= 0 {
		limit = 20
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	keep := s.hits[host][:0]
	for _, t := range s.hits[host] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= limit {
		s.hits[host] = keep
		return false
	}
	s.hits[host] = append(keep, now)
	if len(s.hits) > 10000 {
		s.hits = map[string][]time.Time{}
	}
	return true
}

func (s *Server) logf(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Info(msg, args...)
	}
}

type conn struct {
	c  net.Conn
	r  io.Reader
	w  io.Writer
	br *bufio.Reader
}

func (s *Server) handle(nc net.Conn) {
	defer nc.Close()
	t := s.Timeout
	if t == 0 {
		t = 20 * time.Second
	}
	nc.SetDeadline(time.Now().Add(t))
	if !s.allow(nc.RemoteAddr().String()) {
		return
	}
	br := bufio.NewReader(nc)
	c := &conn{c: nc, r: br, w: nc, br: br}

	id, body, err := c.read()
	if err != nil || id != 0x00 {
		return
	}
	proto, _ := readVarInt(body)
	readString(body, 255) // address
	body.Next(2)          // port
	next, _ := readVarInt(body)
	switch next {
	case 1:
		s.status(c, int(proto))
	case 2, 3:
		s.login(c, int(proto))
	}
}

func (s *Server) status(c *conn, proto int) {
	id, _, err := c.read()
	if err != nil || id != 0x00 {
		return
	}
	st, _ := json.Marshal(map[string]any{
		"version":     map[string]any{"name": "Kronwerke Anmeldung", "protocol": proto},
		"players":     map[string]any{"max": 0, "online": 0},
		"description": map[string]any{"text": "Kronwerke: Anmelden für kronwerke.com", "color": "gold"},
	})
	var b bytes.Buffer
	writeString(&b, string(st))
	c.write(0x00, b.Bytes())
	id, body, err := c.read()
	if err == nil && id == 0x01 {
		c.write(0x01, body.Bytes())
	}
}

func (s *Server) login(c *conn, proto int) {
	if proto < minProtocol {
		s.kick(c, text("Bitte nutze Minecraft 1.20.2 oder neuer.\nPlease use Minecraft 1.20.2 or newer.", "red"))
		return
	}
	id, body, err := c.read()
	if err != nil || id != 0x00 {
		return
	}
	name, err := readString(body, 16)
	if err != nil {
		return
	}
	token := make([]byte, 4)
	rand.Read(token)
	var b bytes.Buffer
	writeString(&b, "")
	writeVarInt(&b, int32(len(s.pub)))
	b.Write(s.pub)
	writeVarInt(&b, int32(len(token)))
	b.Write(token)
	if proto >= 766 { // 1.20.5: the server says whether the client must log in with Mojang
		b.WriteByte(1)
	}
	if err := c.write(0x01, b.Bytes()); err != nil {
		return
	}
	id, body, err = c.read()
	if err != nil || id != 0x01 {
		return
	}
	encSecret, err := readBytes(body, 256)
	if err != nil {
		return
	}
	encToken, err := readBytes(body, 256)
	if err != nil {
		return
	}
	secret, err := rsa.DecryptPKCS1v15(rand.Reader, s.key, encSecret)
	if err != nil || len(secret) != 16 {
		return
	}
	got, err := rsa.DecryptPKCS1v15(rand.Reader, s.key, encToken)
	if err != nil || !bytes.Equal(got, token) {
		return
	}
	// from here on everything is encrypted, both ways
	block, _ := aes.NewCipher(secret)
	c.r = cipher.StreamReader{S: newCFB8(block, secret, true), R: c.br}
	c.w = cipher.StreamWriter{S: newCFB8(block, secret, false), W: c.c}

	acc, err := s.hasJoined(name, serverHash("", secret, s.pub))
	if err != nil {
		s.logf("mc sign in failed", "name", name, "err", err)
		s.kick(c, text("Anmeldung bei Mojang nicht bestätigt. Versuch es gleich noch einmal.\nMojang did not confirm the login. Please try again.", "red"))
		return
	}
	code, err := s.OnAuth(acc)
	if err != nil {
		s.kick(c, text("Gerade geht es nicht: "+err.Error(), "red"))
		return
	}
	s.logf("mc sign in", "name", acc.Name)
	s.kick(c, codeMessage(acc.Name, code))
}

func (s *Server) hasJoined(name, hash string) (Account, error) {
	base := s.SessionURL
	if base == "" {
		base = "https://sessionserver.mojang.com/session/minecraft/hasJoined"
	}
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Get(base + "?username=" + url.QueryEscape(name) + "&serverId=" + url.QueryEscape(hash))
	if err != nil {
		return Account{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Account{}, fmt.Errorf("session server answered %s", resp.Status)
	}
	var p struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&p); err != nil {
		return Account{}, err
	}
	if len(p.ID) != 32 || p.Name == "" {
		return Account{}, errors.New("session server gave no account")
	}
	return Account{UUID: strings.ToLower(p.ID), Name: p.Name}, nil
}

func (s *Server) kick(c *conn, component any) {
	j, _ := json.Marshal(component)
	var b bytes.Buffer
	writeString(&b, string(j))
	c.write(0x00, b.Bytes())
	time.Sleep(200 * time.Millisecond) // let the client read it before the socket closes
}

func text(s, color string) map[string]any { return map[string]any{"text": s, "color": color} }

func codeMessage(name, code string) map[string]any {
	return map[string]any{"text": "", "extra": []any{
		map[string]any{"text": "Kronwerke\n\n", "color": "gold", "bold": true},
		map[string]any{"text": "Hallo " + name + ", dein Code / your code:\n\n", "color": "white"},
		map[string]any{"text": code, "color": "yellow", "bold": true},
		map[string]any{"text": "\n\nGib ihn auf kronwerke.com/anmelden ein.\nEnter it at kronwerke.com/anmelden.\n\nGültig 10 Minuten / valid for 10 minutes.", "color": "gray"},
	}}
}

// serverHash is Minecraft's login hash: SHA-1 over server id, shared secret and public key,
// printed as a signed (two's complement) hex number.
func serverHash(serverID string, secret, pub []byte) string {
	h := sha1.New()
	h.Write([]byte(serverID))
	h.Write(secret)
	h.Write(pub)
	sum := h.Sum(nil)
	n := new(big.Int).SetBytes(sum)
	if sum[0]&0x80 != 0 {
		n.Sub(n, new(big.Int).Lsh(big.NewInt(1), uint(len(sum)*8)))
	}
	return n.Text(16)
}

// ---- packets ----------------------------------------------------------------------------

func (c *conn) read() (int32, *bytes.Buffer, error) {
	n, err := readVarIntFrom(c.r)
	if err != nil {
		return 0, nil, err
	}
	if n <= 0 || n > 1<<16 {
		return 0, nil, errors.New("bad packet length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return 0, nil, err
	}
	b := bytes.NewBuffer(buf)
	id, err := readVarInt(b)
	return id, b, err
}

func (c *conn) write(id int32, body []byte) error {
	var p bytes.Buffer
	writeVarInt(&p, id)
	p.Write(body)
	var out bytes.Buffer
	writeVarInt(&out, int32(p.Len()))
	out.Write(p.Bytes())
	_, err := c.w.Write(out.Bytes())
	return err
}

func readVarIntFrom(r io.Reader) (int32, error) {
	var v uint32
	b := make([]byte, 1)
	for i := 0; i < 5; i++ {
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, err
		}
		v |= uint32(b[0]&0x7f) << (7 * i)
		if b[0]&0x80 == 0 {
			return int32(v), nil
		}
	}
	return 0, errors.New("varint too long")
}

func readVarInt(b *bytes.Buffer) (int32, error) { return readVarIntFrom(b) }

func writeVarInt(b *bytes.Buffer, v int32) {
	u := uint32(v)
	for {
		if u&^0x7f == 0 {
			b.WriteByte(byte(u))
			return
		}
		b.WriteByte(byte(u&0x7f | 0x80))
		u >>= 7
	}
}

func readString(b *bytes.Buffer, max int) (string, error) {
	n, err := readVarInt(b)
	if err != nil || n < 0 || int(n) > max*4 || int(n) > b.Len() {
		return "", errors.New("bad string")
	}
	return string(b.Next(int(n))), nil
}

func readBytes(b *bytes.Buffer, max int) ([]byte, error) {
	n, err := readVarInt(b)
	if err != nil || n < 0 || int(n) > max || int(n) > b.Len() {
		return nil, errors.New("bad byte array")
	}
	return b.Next(int(n)), nil
}

func writeString(b *bytes.Buffer, s string) {
	writeVarInt(b, int32(len(s)))
	b.WriteString(s)
}

// ---- AES/CFB8, the stream cipher of Minecraft's connection ----------------------------

type cfb8 struct {
	b       cipher.Block
	sr      []byte
	out     []byte
	decrypt bool
}

func newCFB8(b cipher.Block, iv []byte, decrypt bool) cipher.Stream {
	sr := make([]byte, len(iv))
	copy(sr, iv)
	return &cfb8{b: b, sr: sr, out: make([]byte, b.BlockSize()), decrypt: decrypt}
}

func (x *cfb8) XORKeyStream(dst, src []byte) {
	for i := range src {
		x.b.Encrypt(x.out, x.sr)
		in := src[i]
		o := in ^ x.out[0]
		dst[i] = o
		copy(x.sr, x.sr[1:])
		if x.decrypt {
			x.sr[len(x.sr)-1] = in
		} else {
			x.sr[len(x.sr)-1] = o
		}
	}
}
