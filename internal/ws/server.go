package ws

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// ServerConn is the server side of a connection. It exists so the gateway client can
// be tested against a fake gateway; the bot itself never accepts connections.
type ServerConn struct {
	c  net.Conn
	br *bufio.Reader
	wm sync.Mutex
}

// Upgrade turns an HTTP request into a server side WebSocket connection.
func Upgrade(w http.ResponseWriter, r *http.Request) (*ServerConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("ws: not a websocket request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("ws: cannot hijack")
	}
	nc, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + AcceptKey(key) + "\r\n\r\n"
	if _, err := io.WriteString(nc, resp); err != nil {
		nc.Close()
		return nil, err
	}
	return &ServerConn{c: nc, br: brw.Reader}, nil
}

// Write sends one unmasked text frame.
func (s *ServerConn) Write(p []byte) error { return s.frame(OpText, p, true) }

// WriteFragmented sends a text message split into two frames.
func (s *ServerConn) WriteFragmented(p []byte) error {
	h := len(p) / 2
	if err := s.frame(OpText, p[:h], false); err != nil {
		return err
	}
	return s.frame(opContinuation, p[h:], true)
}

// Ping sends a ping frame.
func (s *ServerConn) Ping(p []byte) error { return s.frame(opPing, p, true) }

// CloseWith sends a close frame with a code and closes.
func (s *ServerConn) CloseWith(code int) error {
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(code))
	s.frame(opClose, p[:], true)
	return s.c.Close()
}

func (s *ServerConn) frame(op int, p []byte, fin bool) error {
	s.wm.Lock()
	defer s.wm.Unlock()
	b0 := byte(op)
	if fin {
		b0 |= 0x80
	}
	hdr := []byte{b0}
	n := len(p)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	_, err := s.c.Write(append(hdr, p...))
	return err
}

// Read returns the next message from the client, unmasked. Pongs are skipped.
func (s *ServerConn) Read() (op int, msg []byte, err error) {
	c := &Conn{c: s.c, br: s.br}
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		if op == opPong {
			continue
		}
		if op == opClose {
			return op, payload, io.EOF
		}
		if !fin {
			return 0, nil, errors.New("ws: test server does not assemble client fragments")
		}
		return op, payload, nil
	}
}

// Close drops the connection without a close frame.
func (s *ServerConn) Close() error { return s.c.Close() }
