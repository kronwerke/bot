// Package update replaces the running binary with the latest GitHub release.
//
// A release carries the binary (kronwerke-bot-linux-amd64) and SHA256SUMS. The new
// binary is downloaded next to the current one, checked against the sum, run once with
// "version" to prove it starts, then swapped in; the old one stays as .prev. A trial
// file tells deploy/rollback-guard.sh to put .prev back if the new binary keeps dying
// before it reaches Discord.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Asset is the release file name of the binary.
const Asset = "kronwerke-bot-linux-amd64"

// Updater checks one repository.
type Updater struct {
	Repo      string // owner/name
	API       string // https://api.github.com
	Binary    string // path of the running binary
	TrialFile string // written when a new binary is swapped in: "<starts> <tag>"
	SkipFile  string // a tag that was rolled back; never installed again
	Current   string // version of the running binary, like v0.1.0
	HTTP      *http.Client

	etag   string
	latest Release
}

// Release is the part of a GitHub release the updater reads.
type Release struct {
	Tag    string `json:"tag_name"`
	Draft  bool   `json:"draft"`
	Pre    bool   `json:"prerelease"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (u *Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

// Latest asks GitHub for the latest release. It uses the ETag so an unchanged answer
// does not count against the rate limit.
func (u *Updater) Latest(ctx context.Context) (Release, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.API+"/repos/"+u.Repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kronwerke-bot/"+u.Current)
	if u.etag != "" {
		req.Header.Set("If-None-Match", u.etag)
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("update: ask for the latest release: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return u.latest, nil
	case http.StatusNotFound:
		return Release{}, errors.New("update: no release published yet")
	case http.StatusOK:
	default:
		return Release{}, fmt.Errorf("update: GitHub answered %s", resp.Status)
	}
	var r Release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("update: decode release: %w", err)
	}
	u.etag = resp.Header.Get("ETag")
	u.latest = r
	return r, nil
}

// Newer reports whether version a is newer than b (both vX.Y.Z).
func Newer(a, b string) bool {
	pa, oka := parse(a)
	pb, okb := parse(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Check returns the newer release, or ok=false when the running version is current.
func (u *Updater) Check(ctx context.Context) (r Release, ok bool, err error) {
	r, err = u.Latest(ctx)
	if err != nil {
		return r, false, err
	}
	if r.Draft || r.Pre || !Newer(r.Tag, u.Current) || r.Tag == u.Skipped() {
		return r, false, nil
	}
	return r, true, nil
}

func (r Release) asset(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// Install downloads, verifies and swaps in the release. The caller exits afterwards
// so the service manager starts the new binary.
func (u *Updater) Install(ctx context.Context, r Release) error {
	binURL, sumURL := r.asset(Asset), r.asset("SHA256SUMS")
	if binURL == "" || sumURL == "" {
		return fmt.Errorf("update: release %s lacks %s or SHA256SUMS", r.Tag, Asset)
	}
	sums, err := u.get(ctx, sumURL, 1<<16)
	if err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == Asset {
			want = f[0]
		}
	}
	if len(want) != 64 {
		return fmt.Errorf("update: SHA256SUMS of %s has no line for %s", r.Tag, Asset)
	}
	bin, err := u.get(ctx, binURL, 200<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bin)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("update: checksum of %s does not match SHA256SUMS, not installing", r.Tag)
	}

	next := u.Binary + ".new"
	if err := os.WriteFile(next, bin, 0o755); err != nil {
		return fmt.Errorf("update: write %s: %w", next, err)
	}
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, next, "version").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), r.Tag) {
		os.Remove(next)
		return fmt.Errorf("update: the new binary did not start cleanly (%q, %v), keeping %s", strings.TrimSpace(string(out)), err, u.Current)
	}
	prev := u.Binary + ".prev"
	if err := copyFile(u.Binary, prev); err != nil {
		os.Remove(next)
		return fmt.Errorf("update: keep previous binary: %w", err)
	}
	if u.TrialFile != "" {
		os.MkdirAll(filepath.Dir(u.TrialFile), 0o750)
		if err := os.WriteFile(u.TrialFile, []byte("0 "+r.Tag+"\n"), 0o640); err != nil {
			return fmt.Errorf("update: write trial file: %w", err)
		}
	}
	if err := os.Rename(next, u.Binary); err != nil {
		return fmt.Errorf("update: swap binary: %w", err)
	}
	return nil
}

// Rollback puts the previous binary back. The caller exits afterwards.
func (u *Updater) Rollback() error {
	prev := u.Binary + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return errors.New("update: there is no previous binary to go back to")
	}
	if u.TrialFile != "" {
		os.Remove(u.TrialFile)
	}
	u.skip(u.Current)
	return os.Rename(prev, u.Binary)
}

// Skipped returns the tag that was rolled back, or "".
func (u *Updater) Skipped() string {
	if u.SkipFile == "" {
		return ""
	}
	b, _ := os.ReadFile(u.SkipFile)
	return strings.TrimSpace(string(b))
}

func (u *Updater) skip(tag string) {
	if u.SkipFile != "" {
		os.MkdirAll(filepath.Dir(u.SkipFile), 0o750)
		os.WriteFile(u.SkipFile, []byte(tag+"\n"), 0o640)
	}
}

// Settle marks the running binary as good: the trial is over.
func (u *Updater) Settle() {
	if u.TrialFile != "" {
		os.Remove(u.TrialFile)
	}
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "kronwerke-bot/"+u.Current)
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("update: %s is larger than expected", url)
	}
	return b, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755)
}
