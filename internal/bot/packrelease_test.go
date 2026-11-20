package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestStatusTellsTheInstallPageWhichPackToOffer(t *testing.T) {
	releases := `[
	 {"tag_name":"v0.26.0","assets":[{"name":"Kronwerke-0.26.0.mrpack"}]},
	 {"tag_name":"v0.25.2","prerelease":true,"assets":[{"name":"a.mrpack"},{"name":"a.zip"}]},
	 {"tag_name":"v0.25.1","assets":[{"name":"Kronwerke-0.25.1.mrpack"},{"name":"Kronwerke-0.25.1.zip"}]},
	 {"tag_name":"v0.25.0","assets":[{"name":"Kronwerke-0.25.0.mrpack"},{"name":"Kronwerke-0.25.0.zip"}]}
	]`
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/kronwerke/pack/releases" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, releases)
	}))
	defer gh.Close()
	b, err := New(Config{Token: "tok", StatePath: filepath.Join(t.TempDir(), "state.json"), Version: "v0.0.0-test",
		PackRepo: "kronwerke/pack", PackAPI: gh.URL}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	status := func() map[string]any {
		rec := httptest.NewRecorder()
		b.apiStatus(rec, httptest.NewRequest("GET", "/api/status", nil))
		var out struct {
			Server map[string]any `json:"server"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Server
	}
	if v, ok := status()["download"]; ok {
		t.Fatalf("before GitHub was asked: %v", v)
	}
	b.refreshPack(context.Background())
	// the newest release still lacks its zip and a prerelease is not offered
	if v := status()["download"]; v != "0.25.1" {
		t.Fatalf("download: %v, want 0.25.1", v)
	}
	// the version the server runs wins when its release is complete
	if v := b.packDownload("0.25.0"); v != "0.25.0" {
		t.Fatalf("the server's version: %q", v)
	}
	if v := b.packDownload("0.26.0"); v != "0.25.1" {
		t.Fatalf("a version without its files: %q", v)
	}
	// GitHub down: the last answer stays
	releases = `not json`
	b.refreshPack(context.Background())
	if v := status()["download"]; v != "0.25.1" {
		t.Fatalf("after a failed round: %v", v)
	}
}
