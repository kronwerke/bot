// Package rcon is a Minecraft RCON client (the Source RCON protocol). Long answers
// arrive in several packets; a trailing empty request marks where one answer ends.
package rcon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	typeResponse = 0
	typeCommand  = 2
	typeAuth     = 3
)

// Client runs commands over one RCON connection, reconnecting when it drops.
type Client struct {
	Addr     string
	Password string
	Timeout  time.Duration

	mu   sync.Mutex
	conn net.Conn
	id   int32
}

// ErrAuth means the server refused the password.
var ErrAuth = errors.New("rcon: the server refused the password. Check KW_RCON_PASSWORD against rcon.password in server.properties")

// Command runs one command and returns its output with colour codes removed.
func (c *Client) Command(cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, err := c.command(cmd)
	if err != nil && !errors.Is(err, ErrAuth) {
		// one retry on a fresh connection
		c.drop()
		out, err = c.command(cmd)
	}
	if err != nil {
		c.drop()
	}
	return StripColors(out), err
}

func (c *Client) drop() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *Client) timeout() time.Duration {
	if c.Timeout == 0 {
		return 10 * time.Second
	}
	return c.Timeout
}

func (c *Client) connect() error {
	if c.conn != nil {
		return nil
	}
	nc, err := net.DialTimeout("tcp", c.Addr, c.timeout())
	if err != nil {
		return fmt.Errorf("rcon: connect %s: %w", c.Addr, err)
	}
	c.conn = nc
	c.id++
	nc.SetDeadline(time.Now().Add(c.timeout()))
	if err := write(nc, c.id, typeAuth, c.Password); err != nil {
		c.drop()
		return err
	}
	for {
		id, typ, _, err := read(nc)
		if err != nil {
			c.drop()
			return fmt.Errorf("rcon: auth: %w", err)
		}
		if id == -1 {
			c.drop()
			return ErrAuth
		}
		if typ == 2 && id == c.id { // SERVERDATA_AUTH_RESPONSE
			return nil
		}
	}
}

func (c *Client) command(cmd string) (string, error) {
	if err := c.connect(); err != nil {
		return "", err
	}
	c.conn.SetDeadline(time.Now().Add(c.timeout()))
	c.id++
	cmdID := c.id
	c.id++
	endID := c.id
	if err := write(c.conn, cmdID, typeCommand, cmd); err != nil {
		return "", err
	}
	// Minecraft's RCON reads one packet per read and drops the connection when two
	// arrive together, so the end marker goes out only after the first answer packet.
	// The server answers the marker (an unknown packet type) after the rest of the
	// command's answer, which tells us where a long, split answer ends.
	var out strings.Builder
	sentEnd := false
	for {
		id, _, body, err := read(c.conn)
		if err != nil {
			return out.String(), fmt.Errorf("rcon: read: %w", err)
		}
		if id == endID {
			return out.String(), nil
		}
		if id == cmdID {
			out.WriteString(body)
			if !sentEnd {
				if err := write(c.conn, endID, 100, ""); err != nil {
					return out.String(), err
				}
				sentEnd = true
			}
		}
	}
}

func write(w io.Writer, id int32, typ int32, body string) error {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, int32(len(body)+10))
	binary.Write(&b, binary.LittleEndian, id)
	binary.Write(&b, binary.LittleEndian, typ)
	b.WriteString(body)
	b.Write([]byte{0, 0})
	_, err := w.Write(b.Bytes())
	return err
}

func read(r io.Reader) (id, typ int32, body string, err error) {
	var n int32
	if err = binary.Read(r, binary.LittleEndian, &n); err != nil {
		return
	}
	if n < 10 || n > 1<<20 {
		err = fmt.Errorf("rcon: bad packet length %d", n)
		return
	}
	buf := make([]byte, n)
	if _, err = io.ReadFull(r, buf); err != nil {
		return
	}
	id = int32(binary.LittleEndian.Uint32(buf[0:4]))
	typ = int32(binary.LittleEndian.Uint32(buf[4:8]))
	body = string(bytes.TrimRight(buf[8:], "\x00"))
	return
}

// StripColors removes Minecraft section sign formatting codes.
func StripColors(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] == '§' && i+1 < len(rs) {
			i++
			continue
		}
		b.WriteRune(rs[i])
	}
	return b.String()
}
