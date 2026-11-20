package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// packList knows which pack versions can be downloaded: the releases of the pack that
// have both files a launcher wants. The install page used to ask GitHub this from every
// visitor's browser, 60 questions an hour per address; now the bot asks every ten minutes
// and tells the page with /api/status.
type packList struct {
	mu   sync.Mutex
	done []string // versions with .mrpack and .zip, newest first
}

// refreshPack reads the newest releases of the pack. A failed round keeps the last list.
func (b *Bot) refreshPack(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.PackAPI+"/repos/"+b.cfg.PackRepo+"/releases?per_page=15", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kronwerke-bot/"+b.cfg.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.log.Warn("pack releases", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.log.Warn("pack releases", "err", fmt.Sprintf("GitHub answered %s", resp.Status))
		return
	}
	var list []struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		b.log.Warn("pack releases", "err", err)
		return
	}
	var done []string
	for _, r := range list {
		var mrpack, zip bool
		for _, a := range r.Assets {
			mrpack = mrpack || strings.HasSuffix(a.Name, ".mrpack")
			zip = zip || strings.HasSuffix(a.Name, ".zip")
		}
		if !r.Draft && !r.Prerelease && mrpack && zip {
			done = append(done, strings.TrimPrefix(r.Tag, "v"))
		}
	}
	b.packs.mu.Lock()
	b.packs.done = done
	b.packs.mu.Unlock()
}

// packDownload is the version the install page links to: the one the server runs when
// its release has its files, else the newest release that has them. A link that works
// beats one to the right number that leads nowhere. Empty when nothing is known yet.
func (b *Bot) packDownload(server string) string {
	b.packs.mu.Lock()
	defer b.packs.mu.Unlock()
	for _, v := range b.packs.done {
		if v == server {
			return v
		}
	}
	if len(b.packs.done) > 0 {
		return b.packs.done[0]
	}
	return ""
}
