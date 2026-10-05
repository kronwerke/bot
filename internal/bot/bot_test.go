package bot

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kronwerke/bot/internal/discord"
	"github.com/kronwerke/bot/internal/ws"
)

const (
	guildID = "1473242451098472482"
	botID   = "900000000000000001"
	appID   = "900000000000000002"
	userID  = "800000000000000001"
)

type call struct {
	Method, Path string
	Body         string
	Files        []string
}

// fakeDiscord is the REST API and the gateway in one test double.
type fakeDiscord struct {
	t     *testing.T
	rest  *httptest.Server
	gw    *httptest.Server
	mu    sync.Mutex
	calls []call
	next  int
	conn  *ws.ServerConn
	seq   int
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{t: t, next: 700000000000000000}
	f.gw = httptest.NewServer(http.HandlerFunc(f.gateway))
	f.rest = httptest.NewServer(http.HandlerFunc(f.api))
	t.Cleanup(func() { f.rest.Close(); f.gw.Close() })
	return f
}

func (f *fakeDiscord) id() string {
	f.next++
	return fmt.Sprint(f.next)
}

func (f *fakeDiscord) api(w http.ResponseWriter, r *http.Request) {
	c := call{Method: r.Method, Path: r.URL.Path}
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			if p.FormName() == "payload_json" {
				c.Body = string(b)
			} else {
				c.Files = append(c.Files, p.FileName())
			}
		}
	} else {
		b, _ := io.ReadAll(r.Body)
		c.Body = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	id := f.id()
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case p == "/gateway/bot":
		fmt.Fprintf(w, `{"url":%q}`, "ws"+strings.TrimPrefix(f.gw.URL, "http"))
	case r.Method == "GET" && strings.HasSuffix(p, "/messages"):
		w.Write([]byte(`[]`))
	case r.Method == "POST" && strings.HasSuffix(p, "/messages"):
		fmt.Fprintf(w, `{"id":%q}`, id)
	case r.Method == "POST" && strings.HasSuffix(p, "/channels") && strings.HasPrefix(p, "/guilds/"):
		fmt.Fprintf(w, `{"id":%q}`, id)
	case p == "/users/@me/channels":
		fmt.Fprintf(w, `{"id":%q}`, id)
	case r.Method == "PUT" && strings.HasSuffix(p, "/commands"):
		w.Write([]byte(`[]`))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeDiscord) gateway(w http.ResponseWriter, r *http.Request) {
	sc, err := ws.Upgrade(w, r)
	if err != nil {
		return
	}
	sc.Write([]byte(`{"op":10,"d":{"heartbeat_interval":45000}}`))
	_, _, err = sc.Read() // identify
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conn = sc
	f.mu.Unlock()
	f.send("READY", map[string]any{
		"session_id": "s", "resume_gateway_url": "ws" + strings.TrimPrefix(f.gw.URL, "http"),
		"user": map[string]any{"id": botID, "username": "Kronwerke", "bot": true}, "application": map[string]string{"id": appID},
	})
	for {
		if _, _, err := sc.Read(); err != nil {
			return
		}
		sc.Write([]byte(`{"op":11}`))
	}
}

func (f *fakeDiscord) send(event string, d any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	b, _ := json.Marshal(map[string]any{"op": 0, "s": f.seq, "t": event, "d": d})
	f.conn.Write(b)
}

// waitFor polls the recorded calls until one matches.
func (f *fakeDiscord) waitFor(t *testing.T, what string, match func(call) bool) call {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, c := range f.calls {
			if match(c) {
				f.mu.Unlock()
				return c
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var seen []string
	for _, c := range f.calls {
		seen = append(seen, c.Method+" "+c.Path)
	}
	t.Fatalf("no call for %s; seen:\n%s", what, strings.Join(seen, "\n"))
	return call{}
}

func (f *fakeDiscord) count(match func(call) bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if match(c) {
			n++
		}
	}
	return n
}

func startBot(t *testing.T) (*Bot, *fakeDiscord) {
	f := newFakeDiscord(t)
	b, err := New(Config{
		Token: "tok", StatePath: filepath.Join(t.TempDir(), "state.json"),
		Version: "v0.0.0-test", APIBase: f.rest.URL,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	f.waitFor(t, "online message in control", func(c call) bool {
		return c.Method == "POST" && c.Path == "/channels/"+defaults["channel.control"]+"/messages" && strings.Contains(c.Body, "Online")
	})
	return b, f
}

func member(roles ...string) map[string]any {
	return map[string]any{"user": map[string]any{"id": userID, "username": "anna"}, "roles": roles}
}

func TestStartRegistersCommandsAndPostsVerifyPanel(t *testing.T) {
	_, f := startBot(t)
	c := f.waitFor(t, "slash commands", func(c call) bool {
		return c.Method == "PUT" && c.Path == "/applications/"+appID+"/guilds/"+guildID+"/commands"
	})
	for _, name := range []string{"apply", "whitelist", "link"} {
		if !strings.Contains(c.Body, `"name":"`+name+`"`) {
			t.Errorf("command %s not registered", name)
		}
	}
	f.waitFor(t, "verify panel", func(c call) bool {
		return c.Method == "POST" && c.Path == "/channels/"+defaults["channel.verify"]+"/messages" && strings.Contains(c.Body, idVerify)
	})
}

func TestJoinCaptchaWrongThenRightGivesMitglied(t *testing.T) {
	b, f := startBot(t)
	unv, mem := defaults["role.unverified"], defaults["role.member"]

	f.send("GUILD_MEMBER_ADD", map[string]any{"guild_id": guildID, "user": map[string]any{"id": userID, "username": "anna"}, "roles": []string{}})
	f.waitFor(t, "unverified role on join", func(c call) bool {
		return c.Method == "PUT" && c.Path == "/guilds/"+guildID+"/members/"+userID+"/roles/"+unv
	})

	click := func(id, custom string) {
		f.send("INTERACTION_CREATE", map[string]any{"id": id, "token": "t" + id, "type": 3, "guild_id": guildID,
			"application_id": appID, "member": member(unv), "data": map[string]any{"custom_id": custom, "component_type": 2}})
	}
	submit := func(id, code string) {
		f.send("INTERACTION_CREATE", map[string]any{"id": id, "token": "t" + id, "type": 5, "guild_id": guildID,
			"application_id": appID, "member": member(unv), "data": map[string]any{"custom_id": idVerifyModal,
				"components": []any{map[string]any{"type": 1, "components": []any{map[string]any{"type": 4, "custom_id": idVerifyCode, "value": code}}}}}})
	}

	click("1001", idVerify)
	c := f.waitFor(t, "captcha image", func(c call) bool { return c.Path == "/interactions/1001/t1001/callback" })
	if len(c.Files) != 1 || c.Files[0] != "code.png" || !strings.Contains(c.Body, "attachment://code.png") {
		t.Fatalf("captcha answer: files=%v body=%s", c.Files, c.Body)
	}
	submit("1002", "ZZZZZ")
	f.waitFor(t, "wrong answer reply", func(c call) bool {
		return c.Path == "/interactions/1002/t1002/callback" && strings.Contains(c.Body, "falsch")
	})
	if f.count(func(c call) bool { return strings.HasSuffix(c.Path, "/roles/"+mem) }) != 0 {
		t.Fatal("wrong answer gave the member role")
	}

	click("1003", idVerify)
	f.waitFor(t, "second captcha", func(c call) bool { return c.Path == "/interactions/1003/t1003/callback" })
	cp, ok := b.store.PeekCaptcha(userID)
	if !ok {
		t.Fatal("no captcha stored")
	}
	submit("1004", strings.ToLower(cp.Code))
	f.waitFor(t, "Mitglied role", func(c call) bool {
		return c.Method == "PUT" && c.Path == "/guilds/"+guildID+"/members/"+userID+"/roles/"+mem
	})
	f.waitFor(t, "unverified removed", func(c call) bool {
		return c.Method == "DELETE" && c.Path == "/guilds/"+guildID+"/members/"+userID+"/roles/"+unv
	})
}

func TestControlChannelAcceptsCommandsFromTheBotItselfOnly(t *testing.T) {
	_, f := startBot(t)
	ctl := defaults["channel.control"]
	msg := func(author string, roles []string, content string) {
		f.send("MESSAGE_CREATE", map[string]any{"id": f.id(), "channel_id": ctl, "guild_id": guildID, "content": content,
			"author": map[string]any{"id": author, "username": "x"}, "member": map[string]any{"roles": roles}})
	}
	msg(userID, nil, "!set trash.seconds 1") // a random member: ignored
	msg(botID, nil, "!status")
	f.waitFor(t, "status answer", func(c call) bool {
		return c.Path == "/channels/"+ctl+"/messages" && strings.Contains(c.Body, "v0.0.0-test") && strings.Contains(c.Body, "Gateway")
	})
	if f.count(func(c call) bool { return strings.Contains(c.Body, "trash.seconds") }) != 0 {
		t.Fatal("a member without the lead role could change a setting")
	}
	msg(defaults["role.lead"], []string{defaults["role.lead"]}, "!get trash")
	f.waitFor(t, "lead can read settings", func(c call) bool { return strings.Contains(c.Body, "trash.seconds = 60") })
}

func TestTrashMessagesAreDeleted(t *testing.T) {
	b, f := startBot(t)
	b.store.SetSetting("trash.seconds", "1")
	tr := defaults["channel.trash"]
	f.send("MESSAGE_CREATE", map[string]any{"id": "555555555555555555", "channel_id": tr, "guild_id": guildID,
		"content": "hi", "author": map[string]any{"id": userID}})
	f.waitFor(t, "trash delete", func(c call) bool {
		return c.Method == "DELETE" && c.Path == "/channels/"+tr+"/messages/555555555555555555"
	})
}

func TestApplicationCreatesPrivateChannelAndTeamCanAccept(t *testing.T) {
	b, f := startBot(t)
	mem := defaults["role.member"]
	f.send("INTERACTION_CREATE", map[string]any{"id": "2001", "token": "t2001", "type": 2, "guild_id": guildID,
		"application_id": appID, "member": member(mem), "data": map[string]any{"name": "apply"}})
	c := f.waitFor(t, "application modal", func(c call) bool { return c.Path == "/interactions/2001/t2001/callback" })
	if !strings.Contains(c.Body, `"type":9`) || !strings.Contains(c.Body, idApplyModal) {
		t.Fatalf("expected a modal, got %s", c.Body)
	}

	var comps []any
	for _, fl := range applyFields {
		comps = append(comps, map[string]any{"type": 1, "components": []any{map[string]any{"type": 4, "custom_id": fl.id, "value": "x " + fl.id}}})
	}
	f.send("INTERACTION_CREATE", map[string]any{"id": "2002", "token": "t2002", "type": 5, "guild_id": guildID,
		"application_id": appID, "member": member(mem), "data": map[string]any{"custom_id": idApplyModal, "components": comps}})
	ch := f.waitFor(t, "application channel", func(c call) bool {
		return c.Method == "POST" && c.Path == "/guilds/"+guildID+"/channels" && strings.Contains(c.Body, "bewerbung-anna")
	})
	// @everyone must not see it, the applicant must
	if !strings.Contains(ch.Body, `{"id":"`+guildID+`","type":0,"allow":"0","deny":"1024"}`) || !strings.Contains(ch.Body, `"id":"`+userID+`","type":1`) {
		t.Fatalf("channel permissions wrong: %s", ch.Body)
	}
	f.waitFor(t, "deferred reply edited", func(c call) bool {
		return c.Method == "PATCH" && strings.Contains(c.Path, "/webhooks/"+appID+"/t2002/messages/@original")
	})
	a, ok := b.store.Application(userID)
	if !ok || a.Status != "open" {
		t.Fatal("application not stored")
	}

	// a non team member cannot decide
	f.send("INTERACTION_CREATE", map[string]any{"id": "2003", "token": "t2003", "type": 3, "guild_id": guildID, "application_id": appID,
		"member":  map[string]any{"user": map[string]any{"id": "123456789012345678", "username": "eve"}, "roles": []string{mem}},
		"message": map[string]any{"id": "1", "content": "x"}, "data": map[string]any{"custom_id": idApplyAccept + userID}})
	f.waitFor(t, "refusal", func(c call) bool {
		return c.Path == "/interactions/2003/t2003/callback" && strings.Contains(c.Body, "Nur das Team")
	})

	lead := defaults["role.lead"]
	f.send("INTERACTION_CREATE", map[string]any{"id": "2004", "token": "t2004", "type": 3, "guild_id": guildID, "application_id": appID,
		"member":  map[string]any{"user": map[string]any{"id": "123456789012345679", "username": "samuel"}, "roles": []string{lead}},
		"message": map[string]any{"id": "1", "content": "x"}, "data": map[string]any{"custom_id": idApplyAccept + userID}})
	f.waitFor(t, "streamer role", func(c call) bool {
		return c.Method == "PUT" && c.Path == "/guilds/"+guildID+"/members/"+userID+"/roles/"+defaults["role.streamer"]
	})
	a, _ = b.store.Application(userID)
	if a.Status != "accepted" {
		t.Fatalf("status %s", a.Status)
	}
}

func TestWhitelistWithoutServerSaysSoAndChangesNothing(t *testing.T) {
	b, f := startBot(t)
	b.store.SetLink(userID, "AnnaStreams")
	f.send("INTERACTION_CREATE", map[string]any{"id": "3001", "token": "t3001", "type": 2, "guild_id": guildID, "application_id": appID,
		"member": member(defaults["role.member"], defaults["role.streamer"]),
		"data": map[string]any{"name": "whitelist", "options": []any{map[string]any{"name": "add", "type": 1, "options": []any{
			map[string]any{"name": "player", "type": 3, "value": "Ben_MC"},
			map[string]any{"name": "user", "type": 6, "value": "800000000000000002"},
		}}}, "resolved": map[string]any{"members": map[string]any{"800000000000000002": map[string]any{"roles": []string{defaults["role.member"]}}}}}})
	f.waitFor(t, "no server message", func(c call) bool {
		return c.Method == "PATCH" && strings.Contains(c.Path, "t3001") && strings.Contains(c.Body, "noch nicht")
	})
	if _, ok := b.store.Invite("Ben_MC"); ok {
		t.Fatal("invite stored although the server was not reachable")
	}
}

var _ = discord.FlagEphemeral

// fakeRCON answers Kronwerke Core admin commands and records them.
type fakeRCON struct {
	mu   sync.Mutex
	cmds []string
	addr string
}

func newFakeRCON(t *testing.T) *fakeRCON {
	f := &fakeRCON{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f.addr = ln.Addr().String()
	write := func(w io.Writer, id, typ int32, body string) {
		var b bytes.Buffer
		binary.Write(&b, binary.LittleEndian, int32(len(body)+10))
		binary.Write(&b, binary.LittleEndian, id)
		binary.Write(&b, binary.LittleEndian, typ)
		b.WriteString(body)
		b.Write([]byte{0, 0})
		w.Write(b.Bytes())
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				for {
					var n int32
					if binary.Read(c, binary.LittleEndian, &n) != nil {
						return
					}
					buf := make([]byte, n)
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
					id := int32(binary.LittleEndian.Uint32(buf[0:4]))
					typ := int32(binary.LittleEndian.Uint32(buf[4:8]))
					body := string(bytes.TrimRight(buf[8:], "\x00"))
					switch typ {
					case 3:
						write(c, id, 2, "")
					case 2:
						f.mu.Lock()
						f.cmds = append(f.cmds, body)
						f.mu.Unlock()
						answer := "OK"
						switch {
						case strings.HasPrefix(body, "kw admin grant "):
							answer = "OK granted"
						case strings.HasPrefix(body, "kw admin invite "):
							answer = "OK slots used 1/2"
						}
						write(c, id, 0, answer)
					default:
						write(c, id, 0, "Unknown request 64")
					}
				}
			}(c)
		}
	}()
	return f
}

func (f *fakeRCON) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func TestLinkGivesStreamersAndSeason1PlayersAPlaceAndLeavingTakesItBack(t *testing.T) {
	rc := newFakeRCON(t)
	f := newFakeDiscord(t)
	b, err := New(Config{
		Token: "tok", StatePath: filepath.Join(t.TempDir(), "state.json"),
		Version: "v0.0.0-test", APIBase: f.rest.URL, RCONAddr: rc.addr, RCONPassword: "pw",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	f.waitFor(t, "online", func(c call) bool { return strings.Contains(c.Body, "Online") })

	mem, s1 := defaults["role.member"], defaults["role.season1"]
	link := func(id string, m map[string]any, name string) {
		f.send("INTERACTION_CREATE", map[string]any{"id": id, "token": "t" + id, "type": 2, "guild_id": guildID, "application_id": appID,
			"member": m, "data": map[string]any{"name": "link", "options": []any{map[string]any{"name": "name", "type": 3, "value": name}}}})
	}
	// without a role: the name is stored, no place
	link("4001", member(mem), "Anna_MC")
	f.waitFor(t, "name stored", func(c call) bool {
		return strings.Contains(c.Path, "/interactions/4001/") && strings.Contains(c.Body, "wenn dir ein Streamer")
	})
	if _, ok := b.store.Grant(userID); ok {
		t.Fatal("a place without the Season 1 or streamer role")
	}

	// with the Season 1 role: a place and two slots
	link("4002", member(mem, s1), "Anna_MC")
	f.waitFor(t, "welcome back", func(c call) bool {
		return c.Method == "PATCH" && strings.Contains(c.Path, "t4002") && strings.Contains(c.Body, "Willkommen zur")
	})
	f.waitFor(t, "Spieler role", func(c call) bool {
		return c.Method == "PUT" && c.Path == "/guilds/"+guildID+"/members/"+userID+"/roles/"+defaults["role.player"]
	})
	if g, ok := b.store.Grant(userID); !ok || g.Player != "Anna_MC" || g.Kind != "season1" {
		t.Fatalf("grant not stored: %+v", g)
	}

	// a streamer: a place with Core's default slots
	streamer := map[string]any{"user": map[string]any{"id": "800000000000000009", "username": "carla"}, "roles": []string{mem, defaults["role.streamer"], s1}}
	link("4004", streamer, "CarlaLive")
	f.waitFor(t, "streamer place", func(c call) bool {
		return c.Method == "PATCH" && strings.Contains(c.Path, "t4004") && strings.Contains(c.Body, "CarlaLive** ist auf der Whitelist")
	})

	// the Season 1 player gives one of her slots
	f.send("INTERACTION_CREATE", map[string]any{"id": "4003", "token": "t4003", "type": 2, "guild_id": guildID, "application_id": appID,
		"member": member(mem, s1),
		"data": map[string]any{"name": "whitelist", "options": []any{map[string]any{"name": "add", "type": 1, "options": []any{
			map[string]any{"name": "player", "type": 3, "value": "Ben_MC"},
			map[string]any{"name": "user", "type": 6, "value": "800000000000000002"},
		}}}, "resolved": map[string]any{"members": map[string]any{"800000000000000002": map[string]any{"roles": []string{mem}}}}}})
	f.waitFor(t, "invite done", func(c call) bool {
		return c.Method == "PATCH" && strings.Contains(c.Path, "t4003") && strings.Contains(c.Body, "Ben_MC")
	})

	// she leaves the Discord: her invite goes first, then her own place
	f.send("GUILD_MEMBER_REMOVE", map[string]any{"guild_id": guildID, "user": map[string]any{"id": userID, "username": "anna"}})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := b.store.Grant(userID); !ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	want := []string{"kw admin grant Anna_MC 2", "kw admin grant CarlaLive", "kw admin invite Anna_MC Ben_MC", "kw admin revoke Anna_MC Ben_MC", "kw admin ungrant Anna_MC"}
	var got []string
	for _, c := range rc.commands() {
		if c != "kw admin goals json" && c != "kw admin season json" { // the status loop fetches these for the API
			got = append(got, c)
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("server commands\n got %q\nwant %q", got, want)
	}
	if _, ok := b.store.Grant(userID); ok {
		t.Fatal("grant still stored after leaving")
	}
	if _, ok := b.store.Invite("Ben_MC"); ok {
		t.Fatal("invite still stored after the inviter left")
	}
}

// fakeLauncher is the launcher's side of the link, answering every command with "OK <cmd>".
func fakeLauncher(t *testing.T, addr, key string) *ws.Conn {
	t.Helper()
	var c *ws.Conn
	var err error
	for i := 0; i < 50; i++ {
		c, err = ws.Dial(context.Background(), "ws://"+addr+"/link", nil)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"type": "hello", "key": key, "name": "tavuru", "launcher": "0.1.0", "state": "running", "pack": "0.4.0"})
	c.WriteText(b)
	return c
}

func TestLauncherLinkIsAcceptedInTheControlChannelAndCarriesCommands(t *testing.T) {
	f := newFakeDiscord(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	b, err := New(Config{
		Token: "tok", StatePath: filepath.Join(t.TempDir(), "state.json"),
		Version: "v0.0.0-test", APIBase: f.rest.URL, HTTPAddr: addr,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	f.waitFor(t, "online", func(c call) bool { return strings.Contains(c.Body, "Online") })
	ctl := defaults["channel.control"]
	control := func(content string) {
		f.send("MESSAGE_CREATE", map[string]any{"id": f.id(), "channel_id": ctl, "guild_id": guildID, "content": content,
			"author": map[string]any{"id": botID, "username": "Kronwerke"}})
	}

	const key = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	c := fakeLauncher(t, addr, key)
	fp := b.linkFingerprint(key)
	f.waitFor(t, "pending notice", func(c call) bool {
		return c.Path == "/channels/"+ctl+"/messages" && strings.Contains(c.Body, "!link accept "+fp)
	})
	control("!link accept " + fp)
	f.waitFor(t, "accepted", func(c call) bool { return strings.Contains(c.Body, "verbindet sich gleich neu") })
	c.Close(1000)

	c = fakeLauncher(t, addr, key)
	defer c.Close(1000)
	go func() {
		for {
			_, raw, err := c.Read()
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(raw, &m)
			if m["type"] != "req" {
				continue
			}
			args, _ := m["args"].(map[string]any)
			out, _ := json.Marshal(map[string]any{"type": "res", "id": m["id"], "ok": true, "data": fmt.Sprintf("OK %v %v", m["op"], args["cmd"])})
			c.WriteText(out)
		}
	}()
	f.waitFor(t, "connected notice", func(c call) bool { return strings.Contains(c.Body, "tavuru** verbunden") })
	control("!rcon list")
	f.waitFor(t, "command through the link", func(c call) bool { return strings.Contains(c.Body, "OK command list") })
	if _, err := b.serverCommand("kw admin grant Anna_MC"); err != nil {
		t.Fatalf("serverCommand through the link: %v", err)
	}
	control("!link")
	f.waitFor(t, "link status", func(c call) bool { return strings.Contains(c.Body, "Angenommen: `"+fp) })
}
