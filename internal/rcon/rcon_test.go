package rcon

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeServer answers like Minecraft: auth, then commands; long answers in 4096 byte
// packets; unknown packet types with "Unknown request". Like Minecraft's RconClient it
// reads at most 1460 bytes at a time and hangs up when a read holds more than exactly
// one packet.
func fakeServer(t *testing.T, password string, answers map[string]string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1460)
				for {
					n, err := c.Read(buf)
					if err != nil || n < 14 {
						return
					}
					if int(binary.LittleEndian.Uint32(buf[:4])) != n-4 {
						return // two packets in one read: Minecraft closes here
					}
					id, typ, body, err := read(bytes.NewReader(buf[:n]))
					if err != nil {
						return
					}
					switch typ {
					case typeAuth:
						if body != password {
							write(c, -1, 2, "")
							return
						}
						write(c, id, 2, "")
					case typeCommand:
						ans := answers[body]
						for len(ans) > 4096 {
							write(c, id, typeResponse, ans[:4096])
							ans = ans[4096:]
						}
						write(c, id, typeResponse, ans)
						time.Sleep(5 * time.Millisecond) // lets a pipelined marker arrive in the same read, as on a real network
					default:
						write(c, id, typeResponse, "Unknown request 64")
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestCommandReturnsAnswerWithoutColors(t *testing.T) {
	addr := fakeServer(t, "pw", map[string]string{"list": "§6There are §r2 players"})
	c := &Client{Addr: addr, Password: "pw"}
	out, err := c.Command("list")
	if err != nil || out != "There are 2 players" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestLongAnswerIsAssembledFromSeveralPackets(t *testing.T) {
	long := strings.Repeat("abcdefghij", 1500) // 15000 bytes, four packets
	addr := fakeServer(t, "pw", map[string]string{"kw admin goals json": long})
	c := &Client{Addr: addr, Password: "pw"}
	out, err := c.Command("kw admin goals json")
	if err != nil || out != long {
		t.Fatalf("got %d bytes, err %v", len(out), err)
	}
	// and the connection is still usable
	if _, err := c.Command("kw admin goals json"); err != nil {
		t.Fatal(err)
	}
}

func TestWrongPasswordIsReported(t *testing.T) {
	addr := fakeServer(t, "pw", nil)
	c := &Client{Addr: addr, Password: "nope"}
	if _, err := c.Command("list"); err != ErrAuth {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}
