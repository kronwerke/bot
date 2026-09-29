// Package mcping asks a Minecraft server for its status with the Server List Ping,
// the same request the multiplayer screen sends. It needs no password and works while
// RCON is down.
package mcping

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// Status is what the server reports.
type Status struct {
	Online    int      `json:"online"`
	Max       int      `json:"max"`
	Players   []string `json:"players"`
	Version   string   `json:"version"`
	MOTD      string   `json:"motd"`
	LatencyMS int64    `json:"latency_ms"`
}

// Ping connects, asks for the status and closes.
func Ping(addr string, timeout time.Duration) (Status, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		host, portStr = addr, "25565"
		addr = net.JoinHostPort(host, portStr)
	}
	port, _ := strconv.Atoi(portStr)
	start := time.Now()
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return Status{}, fmt.Errorf("mcping: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))

	var hs bytes.Buffer
	writeVarInt(&hs, 0x00)
	writeVarInt(&hs, 767) // protocol for 1.21.1; servers answer status for any value
	writeVarInt(&hs, int32(len(host)))
	hs.WriteString(host)
	hs.Write([]byte{byte(port >> 8), byte(port)})
	writeVarInt(&hs, 1) // next state: status
	if err := packet(c, hs.Bytes()); err != nil {
		return Status{}, err
	}
	if err := packet(c, []byte{0x00}); err != nil {
		return Status{}, err
	}

	br := bufio.NewReader(c)
	if _, err := readVarInt(br); err != nil {
		return Status{}, fmt.Errorf("mcping: %w", err)
	}
	id, err := readVarInt(br)
	if err != nil || id != 0 {
		return Status{}, errors.New("mcping: unexpected answer")
	}
	n, err := readVarInt(br)
	if err != nil || n <= 0 || n > 1<<21 {
		return Status{}, errors.New("mcping: bad status length")
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(br, raw); err != nil {
		return Status{}, fmt.Errorf("mcping: %w", err)
	}
	var r struct {
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
		Players struct {
			Max    int `json:"max"`
			Online int `json:"online"`
			Sample []struct {
				Name string `json:"name"`
			} `json:"sample"`
		} `json:"players"`
		Description json.RawMessage `json:"description"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Status{}, fmt.Errorf("mcping: decode: %w", err)
	}
	s := Status{Online: r.Players.Online, Max: r.Players.Max, Version: r.Version.Name, LatencyMS: time.Since(start).Milliseconds()}
	for _, p := range r.Players.Sample {
		s.Players = append(s.Players, p.Name)
	}
	var text string
	if json.Unmarshal(r.Description, &text) == nil {
		s.MOTD = text
	} else {
		var d struct {
			Text string `json:"text"`
		}
		json.Unmarshal(r.Description, &d)
		s.MOTD = d.Text
	}
	return s, nil
}

func packet(w io.Writer, body []byte) error {
	var b bytes.Buffer
	writeVarInt(&b, int32(len(body)))
	b.Write(body)
	_, err := w.Write(b.Bytes())
	return err
}

func writeVarInt(b *bytes.Buffer, v int32) {
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			b.WriteByte(byte(u))
			return
		}
		b.WriteByte(byte(u&0x7F | 0x80))
		u >>= 7
	}
}

func readVarInt(r io.ByteReader) (int32, error) {
	var v uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return int32(v), nil
		}
	}
	return 0, errors.New("mcping: varint too long")
}
