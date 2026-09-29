// Package ws is a small RFC 6455 WebSocket client: enough for the Discord gateway.
// Text and binary messages, fragmentation, ping and pong, close. No extensions.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes from RFC 6455, section 5.2.
const (
	opContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// MaxMessage bounds one assembled message. Discord's READY for a small guild is well below this.
const MaxMessage = 64 << 20

// CloseError is returned by Read when the peer closed the connection.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("ws: closed by peer: %d %s", e.Code, e.Reason)
}

// Conn is a client connection. Read must be called from one goroutine; Write is safe
// for concurrent use.
type Conn struct {
	c  net.Conn
	br *bufio.Reader
	wm sync.Mutex
}

// Dial opens a WebSocket connection to a ws:// or wss:// URL.
func Dial(ctx context.Context, rawURL string, header http.Header) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ws: parse %s: %w", rawURL, err)
	}
	host := u.Host
	secure := false
	switch u.Scheme {
	case "wss":
		secure = true
		if u.Port() == "" {
			host += ":443"
		}
	case "ws":
		if u.Port() == "" {
			host += ":80"
		}
	default:
		return nil, fmt.Errorf("ws: unsupported scheme %q", u.Scheme)
	}

	d := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	var nc net.Conn
	if secure {
		td := tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}
		nc, err = td.DialContext(ctx, "tcp", host)
	} else {
		nc, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("ws: dial %s: %w", host, err)
	}

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		nc.Close()
		return nil, fmt.Errorf("ws: key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)

	path := u.RequestURI()
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", u.Host)
	b.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", key)
	for k, vs := range header {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")

	if dl, ok := ctx.Deadline(); ok {
		nc.SetDeadline(dl)
	} else {
		nc.SetDeadline(time.Now().Add(20 * time.Second))
	}
	if _, err := io.WriteString(nc, b.String()); err != nil {
		nc.Close()
		return nil, fmt.Errorf("ws: handshake write: %w", err)
	}
	br := bufio.NewReaderSize(nc, 64<<10)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("ws: handshake read: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		nc.Close()
		return nil, fmt.Errorf("ws: handshake: server answered %s", resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		nc.Close()
		return nil, errors.New("ws: handshake: no upgrade to websocket")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != AcceptKey(key) {
		nc.Close()
		return nil, errors.New("ws: handshake: wrong Sec-WebSocket-Accept")
	}
	nc.SetDeadline(time.Time{})
	return &Conn{c: nc, br: br}, nil
}

// AcceptKey computes Sec-WebSocket-Accept for a Sec-WebSocket-Key.
func AcceptKey(key string) string {
	h := sha1.Sum([]byte(key + acceptGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// SetReadDeadline sets the deadline for the next Read.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.c.SetReadDeadline(t) }

// Read returns the next complete text or binary message. Pings are answered
// on the way; a close frame is answered and returned as *CloseError.
func (c *Conn) Read() (op int, msg []byte, err error) {
	var buf []byte
	msgOp := -1
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			ce := &CloseError{Code: 1005}
			if len(payload) >= 2 {
				ce.Code = int(binary.BigEndian.Uint16(payload))
				ce.Reason = string(payload[2:])
			}
			c.writeFrame(opClose, payload[:min(2, len(payload))])
			c.c.Close()
			return 0, nil, ce
		case OpText, OpBinary:
			if msgOp != -1 {
				return 0, nil, errors.New("ws: new message inside a fragmented one")
			}
			msgOp = op
			buf = append(buf[:0], payload...)
		case opContinuation:
			if msgOp == -1 {
				return 0, nil, errors.New("ws: continuation without a message")
			}
			buf = append(buf, payload...)
		default:
			return 0, nil, fmt.Errorf("ws: unknown opcode %d", op)
		}
		if len(buf) > MaxMessage {
			return 0, nil, errors.New("ws: message too large")
		}
		if fin {
			return msgOp, buf, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op int, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		err = errors.New("ws: reserved bits set, no extension was negotiated")
		return
	}
	op = int(h[0] & 0x0F)
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var e [2]byte
		if _, err = io.ReadFull(c.br, e[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err = io.ReadFull(c.br, e[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(e[:])
	}
	if n > MaxMessage {
		err = errors.New("ws: frame too large")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// WriteText sends one text message.
func (c *Conn) WriteText(p []byte) error { return c.writeFrame(OpText, p) }

// Close sends a close frame with the code and closes the connection.
func (c *Conn) Close(code int) error {
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(code))
	c.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	c.writeFrame(opClose, p[:])
	return c.c.Close()
}

func (c *Conn) writeFrame(op int, p []byte) error {
	c.wm.Lock()
	defer c.wm.Unlock()
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|byte(op))
	n := len(p)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	hdr = append(hdr, mask[:]...)
	body := make([]byte, n)
	for i := range p {
		body[i] = p[i] ^ mask[i%4]
	}
	c.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.c.Write(append(hdr, body...)); err != nil {
		return fmt.Errorf("ws: write: %w", err)
	}
	return nil
}
