package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kronwerke/bot/internal/ws"
)

// fakeGateway speaks enough of the Discord gateway protocol for the client under test.
type fakeGateway struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	sessions  int
	got       []payload // everything the client sent, in order
	closeWith int       // close code to send on the first session instead of events
}

func (f *fakeGateway) handler(w http.ResponseWriter, r *http.Request) {
	sc, err := ws.Upgrade(w, r)
	if err != nil {
		f.t.Errorf("upgrade: %v", err)
		return
	}
	f.mu.Lock()
	f.sessions++
	n := f.sessions
	f.mu.Unlock()

	send := func(p any) { b, _ := json.Marshal(p); sc.Write(b) }
	send(map[string]any{"op": opHello, "d": map[string]int{"heartbeat_interval": 50}})

	if n == 1 && f.closeWith != 0 {
		sc.Read()
		sc.CloseWith(f.closeWith)
		return
	}

	for {
		_, msg, err := sc.Read()
		if err != nil {
			return
		}
		var p payload
		json.Unmarshal(msg, &p)
		f.mu.Lock()
		f.got = append(f.got, p)
		f.mu.Unlock()
		switch p.Op {
		case opHeartbeat:
			send(map[string]any{"op": opHeartbeatAck})
		case opIdentify:
			url := "ws" + strings.TrimPrefix(f.srv.URL, "http")
			send(map[string]any{"op": opDispatch, "s": 1, "t": "READY", "d": map[string]any{
				"session_id": "sess-1", "resume_gateway_url": url, "user": map[string]string{"id": "42"},
			}})
			send(map[string]any{"op": opDispatch, "s": 2, "t": "MESSAGE_CREATE", "d": map[string]string{"content": "hello"}})
			if n == 1 {
				time.Sleep(120 * time.Millisecond) // let a heartbeat or two happen
				send(map[string]any{"op": opReconnect})
			}
		case opResume:
			send(map[string]any{"op": opDispatch, "s": 3, "t": "RESUMED", "d": map[string]any{}})
		}
	}
}

func newFake(t *testing.T) *fakeGateway {
	f := &fakeGateway{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.srv.Close)
	return f
}

func TestGatewayIdentifiesDispatchesAndResumesAfterReconnect(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var events []string
	done := make(chan struct{})
	g := &Gateway{
		Token:   "tok",
		Intents: IntentGuilds | IntentGuildMembers,
		URL:     "ws" + strings.TrimPrefix(f.srv.URL, "http"),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handle: func(ev string, _ json.RawMessage) {
			mu.Lock()
			events = append(events, ev)
			if ev == "RESUMED" {
				close(done)
			}
			mu.Unlock()
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go g.Run(ctx)

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("no RESUMED, events: %v", events)
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	want := []string{"READY", "MESSAGE_CREATE", "RESUMED"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", events, want)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var identify, resume *payload
	heartbeats := 0
	for i := range f.got {
		switch f.got[i].Op {
		case opIdentify:
			identify = &f.got[i]
		case opResume:
			resume = &f.got[i]
		case opHeartbeat:
			heartbeats++
		}
	}
	if identify == nil {
		t.Fatal("client never identified")
	}
	var id struct {
		Token   string `json:"token"`
		Intents int    `json:"intents"`
	}
	json.Unmarshal(identify.D, &id)
	if id.Token != "tok" || id.Intents != IntentGuilds|IntentGuildMembers {
		t.Fatalf("identify = %+v", id)
	}
	if resume == nil {
		t.Fatal("client did not resume after op 7")
	}
	var rs struct {
		SessionID string `json:"session_id"`
		Seq       int64  `json:"seq"`
	}
	json.Unmarshal(resume.D, &rs)
	if rs.SessionID != "sess-1" || rs.Seq != 2 {
		t.Fatalf("resume = %+v, want session sess-1 seq 2", rs)
	}
	if heartbeats == 0 {
		t.Fatal("no heartbeats were sent")
	}
}

func TestGatewayStopsOnBadToken(t *testing.T) {
	f := newFake(t)
	f.closeWith = 4004
	g := &Gateway{
		Token: "bad", URL: "ws" + strings.TrimPrefix(f.srv.URL, "http"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := g.Run(ctx)
	var fe *FatalError
	if !errors.As(err, &fe) || fe.Code != 4004 {
		t.Fatalf("want fatal 4004, got %v", err)
	}
}

func TestRouteKeepsMajorParametersAndCollapsesOthers(t *testing.T) {
	a := route("DELETE", "/channels/111111111111111111/messages/222222222222222222")
	b := route("DELETE", "/channels/111111111111111111/messages/333333333333333333")
	c := route("DELETE", "/channels/444444444444444444/messages/222222222222222222")
	if a != b {
		t.Fatalf("messages in one channel should share a route: %s vs %s", a, b)
	}
	if a == c {
		t.Fatal("different channels should not share a route")
	}
}

func TestRESTRetriesAfter429(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bot tok" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		if calls == 1 {
			w.WriteHeader(429)
			w.Write([]byte(`{"retry_after": 0.05, "global": false}`))
			return
		}
		w.Write([]byte(`{"id":"1","username":"kronwerke"}`))
	}))
	defer srv.Close()
	r := NewREST("tok", "test")
	r.Base = srv.URL
	u, err := r.Me(context.Background())
	if err != nil || u.Username != "kronwerke" || calls != 2 {
		t.Fatalf("u=%+v err=%v calls=%d", u, err, calls)
	}
}

func TestRESTReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"Missing Permissions","code":50013}`))
	}))
	defer srv.Close()
	r := NewREST("tok", "test")
	r.Base = srv.URL
	err := r.AddRole(context.Background(), "1", "2", "3", "test")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 50013 || !IsStatus(err, 403) {
		t.Fatalf("got %v", err)
	}
}
