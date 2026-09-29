package link

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kronwerke/bot/internal/ws"
)

type keys struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *keys) LinkKey(fp string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	h, ok := k.m[fp]
	return h, ok
}

func (k *keys) put(fp, hash string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[fp] = hash
}

const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func dial(t *testing.T, url string) *ws.Conn {
	t.Helper()
	c, err := ws.Dial(context.Background(), "ws"+strings.TrimPrefix(url, "http")+"/link", nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readMsg(t *testing.T, c *ws.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, b, err := c.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func send(t *testing.T, c *ws.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.WriteText(b); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownKeyWaitsThenAcceptedLauncherAnswersRequests(t *testing.T) {
	k := &keys{m: map[string]string{}}
	pending := make(chan Info, 1)
	connected := make(chan Info, 1)
	states := make(chan string, 4)
	h := &Hub{Keys: k, Hooks: Hooks{
		Pending:   func(i Info) { pending <- i },
		Connected: func(i Info) { connected <- i },
		State:     func(i Info, s, d string) { states <- s + " " + d },
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// an unknown key waits
	c := dial(t, srv.URL)
	send(t, c, map[string]any{"type": "hello", "key": key, "name": "test", "launcher": "0.1.0", "state": "running", "pack": "0.4.0"})
	m := readMsg(t, c)
	if m["type"] != "pending" || m["fingerprint"] != Fingerprint(key) {
		t.Fatalf("expected pending, got %v", m)
	}
	i := <-pending
	if i.Fingerprint != Fingerprint(key) || i.Name != "test" {
		t.Fatalf("pending hook: %+v", i)
	}
	if _, err := h.Request(context.Background(), "status", nil); err != ErrNotConnected {
		t.Fatalf("a waiting launcher must not take requests: %v", err)
	}
	// pings are answered while waiting
	send(t, c, map[string]string{"type": "ping"})
	if m := readMsg(t, c); m["type"] != "pong" {
		t.Fatalf("expected pong, got %v", m)
	}

	// accepted: the waiting connection is closed so the launcher comes back
	hash, info, ok := h.Accept(Fingerprint(key))
	if !ok || hash != Hash(key) || info.Name != "test" {
		t.Fatal("accept did not find the waiting launcher")
	}
	k.put(Fingerprint(key), hash)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := c.Read(); err == nil {
		t.Fatal("expected the waiting connection to close")
	}

	c = dial(t, srv.URL)
	defer c.Close(1000)
	send(t, c, map[string]any{"type": "hello", "key": key, "name": "test", "launcher": "0.1.0", "state": "running"})
	if m := readMsg(t, c); m["type"] != "welcome" {
		t.Fatalf("expected welcome, got %v", m)
	}
	<-connected

	// the launcher side: answer every request
	go func() {
		for {
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, b, err := c.Read()
			if err != nil {
				return
			}
			var r map[string]any
			json.Unmarshal(b, &r)
			if r["type"] != "req" {
				continue
			}
			args, _ := r["args"].(map[string]any)
			switch r["op"] {
			case "command":
				send(t, c, map[string]any{"type": "res", "id": r["id"], "ok": true, "data": "OK ran " + args["cmd"].(string)})
			default:
				send(t, c, map[string]any{"type": "res", "id": r["id"], "ok": false, "error": "unknown op"})
			}
		}
	}()
	raw, err := h.Request(context.Background(), "command", map[string]string{"cmd": "list"})
	if err != nil {
		t.Fatal(err)
	}
	var out string
	json.Unmarshal(raw, &out)
	if out != "OK ran list" {
		t.Fatalf("answer %q", out)
	}
	if _, err := h.Request(context.Background(), "nope", nil); err == nil || err.Error() != "unknown op" {
		t.Fatalf("error answer: %v", err)
	}
	// many requests at once are matched to their answers
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			cmd := "say " + strings.Repeat("x", n)
			raw, err := h.Request(context.Background(), "command", map[string]string{"cmd": cmd})
			var got string
			json.Unmarshal(raw, &got)
			if err != nil || got != "OK ran "+cmd {
				t.Errorf("request %d: %q %v", n, got, err)
			}
		}(n)
	}
	wg.Wait()

	send(t, c, map[string]any{"type": "event", "event": "state", "state": "crashed", "detail": "exit code 1"})
	if s := <-states; s != "crashed exit code 1" {
		t.Fatalf("state hook %q", s)
	}
	if i, ok := h.Connected(); !ok || i.State != "crashed" {
		t.Fatalf("connected info not updated: %+v", i)
	}
}

func TestWrongKeyForAKnownFingerprintIsNotAccepted(t *testing.T) {
	k := &keys{m: map[string]string{Fingerprint(key): "not the hash"}}
	h := &Hub{Keys: k}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := dial(t, srv.URL)
	defer c.Close(1000)
	send(t, c, map[string]any{"type": "hello", "key": key, "name": "x"})
	if m := readMsg(t, c); m["type"] != "pending" {
		t.Fatalf("expected pending, got %v", m)
	}
}

func TestGarbageInsteadOfHelloIsClosed(t *testing.T) {
	h := &Hub{Keys: &keys{m: map[string]string{}}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := dial(t, srv.URL)
	send(t, c, map[string]any{"type": "req", "op": "status"})
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := c.Read(); err == nil {
		t.Fatal("expected the connection to close")
	}
}

func TestRequestWithoutServer(t *testing.T) {
	h := &Hub{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := h.Request(ctx, "status", nil); err != ErrNotConnected {
		t.Fatal(err)
	}
}
