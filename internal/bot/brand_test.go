package bot

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kronwerke/bot/internal/site"
)

// The server icon and the bot's picture are taken from a file of the website and handed
// to Discord as a data URI; anything that is not a picture, or not of the website, is refused.
func TestBrandSetsIconAndAvatarFromTheWebsite(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.Method+" "+r.URL.Path] = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer discord.Close()

	dir := t.TempDir()
	root := filepath.Join(dir, "v-v1.0.0")
	os.MkdirAll(filepath.Join(root, "img", "brand"), 0o755)
	var pic bytes.Buffer
	png.Encode(&pic, image.NewNRGBA(image.Rect(0, 0, 4, 4)))
	os.WriteFile(filepath.Join(root, "img", "brand", "icon-512.png"), pic.Bytes(), 0o644)
	os.WriteFile(filepath.Join(root, "index.html"), []byte("<!doctype html><title>x</title>"), 0o644)
	os.WriteFile(filepath.Join(dir, "current"), []byte("v1.0.0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.png"), pic.Bytes(), 0o644)

	b, err := New(Config{
		Token: "tok", StatePath: filepath.Join(t.TempDir(), "state.json"), Version: "v0.0.0-test",
		APIBase: discord.URL, AdminToken: "admintok", Site: &site.Site{Dir: dir},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	call := func(token, body string) (int, map[string]any) {
		req := httptest.NewRequest("POST", "/api/admin/brand", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		b.adminBrand(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, _ := call("", `{"icon":"img/brand/icon-512.png"}`); code != 404 {
		t.Fatalf("without the admin token: %d", code)
	}
	if code, out := call("admintok", `{}`); code != 400 {
		t.Fatalf("nothing named: %d %v", code, out)
	}
	for _, bad := range []string{"index.html", "../secret.png", "img/brand/none.png", "img/brand"} {
		if code, out := call("admintok", `{"icon":"`+bad+`"}`); code != 502 {
			t.Fatalf("%s should be refused: %d %v", bad, code, out)
		}
	}
	mu.Lock()
	if len(got) != 0 {
		t.Fatalf("a refused picture reached Discord: %v", got)
	}
	mu.Unlock()

	code, out := call("admintok", `{"icon":"img/brand/icon-512.png","avatar":"img/brand/icon-512.png"}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("brand: %d %v", code, out)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pic.Bytes())
	mu.Lock()
	defer mu.Unlock()
	var icon, avatar map[string]string
	json.Unmarshal([]byte(got["PATCH /guilds/"+defaults["guild"]]), &icon)
	json.Unmarshal([]byte(got["PATCH /users/@me"]), &avatar)
	if icon["icon"] != want {
		t.Fatalf("the server icon did not arrive as the file: %v", got)
	}
	if avatar["avatar"] != want {
		t.Fatalf("the avatar did not arrive as the file: %v", got)
	}
}
