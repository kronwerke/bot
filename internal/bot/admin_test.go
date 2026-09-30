package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminAPINeedsTheTokenAndForwardsToTheLauncher(t *testing.T) {
	f := newFakeDiscord(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	b, err := New(Config{
		Token: "tok", StatePath: filepath.Join(t.TempDir(), "state.json"),
		Version: "v0.0.0-test", APIBase: f.rest.URL, HTTPAddr: addr, AdminToken: "s3cret-token",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	f.waitFor(t, "online", func(c call) bool { return strings.Contains(c.Body, "Online") })
	ctl := defaults["channel.control"]

	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	c := fakeLauncher(t, addr, key)
	fp := b.linkFingerprint(key)
	f.waitFor(t, "pending notice", func(c call) bool { return strings.Contains(c.Body, "!link accept "+fp) })
	f.send("MESSAGE_CREATE", map[string]any{"id": f.id(), "channel_id": ctl, "guild_id": guildID, "content": "!link accept " + fp,
		"author": map[string]any{"id": botID, "username": "Kronwerke"}})
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
			data := fmt.Sprintf("%v %v", m["op"], args["cmd"])
			if m["op"] == "logs" {
				data = "[INFO] fine\n[WARN] odd\n[ERROR] bad\n[INFO] fine again"
			}
			out, _ := json.Marshal(map[string]any{"type": "res", "id": m["id"], "ok": true, "data": data})
			c.WriteText(out)
		}
	}()
	f.waitFor(t, "connected notice", func(c call) bool { return strings.Contains(c.Body, "tavuru** verbunden") })

	do := func(method, path, token, body string) (int, string) {
		req, _ := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	if code, _ := do("GET", "/api/admin/bot", "", ""); code != http.StatusNotFound {
		t.Fatalf("without a token: %d, want 404", code)
	}
	if code, _ := do("POST", "/api/admin/link", "wrong", `{"op":"status"}`); code != http.StatusNotFound {
		t.Fatalf("wrong token: %d, want 404", code)
	}
	if code, body := do("GET", "/api/admin/bot", "s3cret-token", ""); code != 200 || !strings.Contains(body, `"fingerprint":"`+fp) {
		t.Fatalf("bot info: %d %s", code, body)
	}
	if code, _ := do("POST", "/api/admin/link", "s3cret-token", `{"op":"launcher-update"}`); code != http.StatusBadRequest {
		t.Fatalf("an op the API does not pass on: %d, want 400", code)
	}
	code, body := do("POST", "/api/admin/link", "s3cret-token", `{"op":"command","args":{"cmd":"list"}}`)
	if code != 200 || !strings.Contains(body, "command list") {
		t.Fatalf("command: %d %s", code, body)
	}
	code, body = do("POST", "/api/admin/link", "s3cret-token", `{"op":"logs","filter":"WARN|ERROR"}`)
	if code != 200 || body != `{"data":"[WARN] odd\n[ERROR] bad"}`+"\n" {
		t.Fatalf("filtered logs: %d %q", code, body)
	}
	if code, _ := do("POST", "/api/admin/link", "s3cret-token", `{"op":"logs","filter":"("}`); code != http.StatusBadRequest {
		t.Fatalf("a broken filter: %d, want 400", code)
	}
	f.waitFor(t, "announced in the control channel", func(c call) bool {
		return c.Path == "/channels/"+ctl+"/messages" && strings.Contains(c.Body, "Admin-API: Befehl `list`")
	})
}
