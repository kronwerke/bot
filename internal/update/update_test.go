package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shell script stands in for the binary: "version" prints the tag.
func fakeBinary(tag string) []byte {
	return []byte("#!/bin/sh\necho " + tag + "\n")
}

type fakeGitHub struct {
	srv       *httptest.Server
	tag       string
	bin       []byte
	sums      string
	latestHit int
}

func newGitHub(t *testing.T, tag string, bin []byte, sumOverride string) *fakeGitHub {
	f := &fakeGitHub{tag: tag, bin: bin}
	s := sha256.Sum256(bin)
	f.sums = hex.EncodeToString(s[:]) + "  " + Asset + "\n"
	if sumOverride != "" {
		f.sums = sumOverride
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/kronwerke/bot/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.latestHit++
		if r.Header.Get("If-None-Match") == `"e1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"e1"`)
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": f.tag,
			"assets": []map[string]string{
				{"name": Asset, "browser_download_url": f.srv.URL + "/dl/bin"},
				{"name": "SHA256SUMS", "browser_download_url": f.srv.URL + "/dl/sums"},
			},
		})
	})
	mux.HandleFunc("GET /dl/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(f.bin) })
	mux.HandleFunc("GET /dl/sums", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, f.sums) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func setup(t *testing.T, gh *fakeGitHub) (*Updater, string) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "kronwerke-bot")
	os.WriteFile(bin, fakeBinary("v0.1.0"), 0o755)
	return &Updater{
		Repo: "kronwerke/bot", API: gh.srv.URL, Binary: bin,
		TrialFile: filepath.Join(dir, "state", "update.trial"), SkipFile: filepath.Join(dir, "state", "update.skip"),
		Current: "v0.1.0",
	}, bin
}

func TestNewerComparesNumerically(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.2.0", "v0.1.9", true}, {"v0.10.0", "v0.9.0", true}, {"v1.0.0", "v1.0.0", false},
		{"v0.1.0", "v0.2.0", false}, {"garbage", "v0.1.0", false}, {"v0.2.0", "dev", false},
	}
	for _, c := range cases {
		if Newer(c.a, c.b) != c.want {
			t.Errorf("Newer(%s, %s) != %v", c.a, c.b, c.want)
		}
	}
}

func TestInstallSwapsBinaryKeepsPreviousAndStartsTrial(t *testing.T) {
	gh := newGitHub(t, "v0.2.0", fakeBinary("v0.2.0"), "")
	u, bin := setup(t, gh)
	r, ok, err := u.Check(context.Background())
	if err != nil || !ok {
		t.Fatalf("check: ok=%v err=%v", ok, err)
	}
	if err := u.Install(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(bin)
	if !strings.Contains(string(got), "v0.2.0") {
		t.Fatal("binary was not replaced")
	}
	prev, _ := os.ReadFile(bin + ".prev")
	if !strings.Contains(string(prev), "v0.1.0") {
		t.Fatal("previous binary not kept")
	}
	if _, err := os.Stat(u.TrialFile); err != nil {
		t.Fatal("trial file missing")
	}
	u.Settle()
	if _, err := os.Stat(u.TrialFile); err == nil {
		t.Fatal("Settle left the trial file")
	}
}

func TestChecksumMismatchKeepsRunningBinary(t *testing.T) {
	gh := newGitHub(t, "v0.2.0", fakeBinary("v0.2.0"), strings.Repeat("0", 64)+"  "+Asset+"\n")
	u, bin := setup(t, gh)
	r, _, _ := u.Check(context.Background())
	if err := u.Install(context.Background(), r); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
	got, _ := os.ReadFile(bin)
	if !strings.Contains(string(got), "v0.1.0") {
		t.Fatal("binary changed despite a bad checksum")
	}
}

func TestBinaryThatReportsWrongVersionIsRejected(t *testing.T) {
	gh := newGitHub(t, "v0.2.0", fakeBinary("v0.1.5"), "")
	u, bin := setup(t, gh)
	r, _, _ := u.Check(context.Background())
	if err := u.Install(context.Background(), r); err == nil {
		t.Fatal("installed a binary that reports the wrong version")
	}
	if _, err := os.Stat(bin + ".new"); err == nil {
		t.Fatal(".new left behind")
	}
}

func TestSameVersionIsNotAnUpdateAndETagIsUsed(t *testing.T) {
	gh := newGitHub(t, "v0.1.0", fakeBinary("v0.1.0"), "")
	u, _ := setup(t, gh)
	for i := 0; i < 2; i++ {
		if _, ok, err := u.Check(context.Background()); ok || err != nil {
			t.Fatalf("round %d: ok=%v err=%v", i, ok, err)
		}
	}
	if gh.latestHit != 2 || u.etag != `"e1"` {
		t.Fatalf("hits=%d etag=%s", gh.latestHit, u.etag)
	}
}

func TestRollbackRestoresPrevious(t *testing.T) {
	gh := newGitHub(t, "v0.2.0", fakeBinary("v0.2.0"), "")
	u, bin := setup(t, gh)
	r, _, _ := u.Check(context.Background())
	u.Install(context.Background(), r)
	if err := u.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(bin)
	if !strings.Contains(string(got), "v0.1.0") {
		t.Fatal("rollback did not restore v0.1.0")
	}
}

func TestTrialFileNamesTheTag(t *testing.T) {
	gh := newGitHub(t, "v0.2.0", fakeBinary("v0.2.0"), "")
	u, _ := setup(t, gh)
	r, _, _ := u.Check(context.Background())
	u.Install(context.Background(), r)
	b, _ := os.ReadFile(u.TrialFile)
	if strings.TrimSpace(string(b)) != "0 v0.2.0" {
		t.Fatalf("trial file %q", b)
	}
}

// After a rollback (by command or by the guard script) the bad tag must not come back.
func TestRolledBackTagIsSkipped(t *testing.T) {
	gh := newGitHub(t, "v0.2.0", fakeBinary("v0.2.0"), "")
	u, _ := setup(t, gh)
	os.MkdirAll(filepath.Dir(u.SkipFile), 0o750)
	os.WriteFile(u.SkipFile, []byte("v0.2.0\n"), 0o640)
	if _, ok, _ := u.Check(context.Background()); ok {
		t.Fatal("offered the skipped tag again")
	}
	gh.tag = "v0.2.1"
	u.etag = ""
	if _, ok, _ := u.Check(context.Background()); !ok {
		t.Fatal("a newer tag than the skipped one must be offered")
	}
}
