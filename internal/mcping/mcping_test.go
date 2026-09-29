package mcping

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestVarIntRoundTrip(t *testing.T) {
	for _, v := range []int32{0, 1, 127, 128, 255, 25565, 2097151, 2147483647, -1} {
		var b bytes.Buffer
		writeVarInt(&b, v)
		got, err := readVarInt(bufio.NewReader(&b))
		if err != nil || got != v {
			t.Fatalf("%d -> %d, %v", v, got, err)
		}
	}
}

func TestPingReadsStatus(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		for i := 0; i < 2; i++ { // handshake and status request
			n, _ := readVarInt(br)
			io.CopyN(io.Discard, br, int64(n))
		}
		js := `{"version":{"name":"1.21.1","protocol":767},"players":{"max":40,"online":2,"sample":[{"name":"Anna","id":"x"},{"name":"Ben","id":"y"}]},"description":{"text":"Kronwerke"}}`
		var body bytes.Buffer
		writeVarInt(&body, 0)
		writeVarInt(&body, int32(len(js)))
		body.WriteString(js)
		packet(c, body.Bytes())
	}()
	s, err := Ping(ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.Online != 2 || s.Max != 40 || s.Version != "1.21.1" || s.MOTD != "Kronwerke" || len(s.Players) != 2 {
		t.Fatalf("%+v", s)
	}
}
