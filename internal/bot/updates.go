package bot

import (
	"context"
	"fmt"
	"time"
)

type checkResult struct {
	at     time.Time
	latest string
	err    error
}

func (b *Bot) updateLoop(ctx context.Context) {
	// first check shortly after start, so a broken release is replaced quickly
	select {
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return
	}
	for {
		b.checkUpdate(ctx, false)
		select {
		case <-time.After(b.duration("update.interval", 10*time.Minute)):
		case <-ctx.Done():
			return
		}
	}
}

// checkUpdate installs a newer release and asks for a restart. verbose reports
// "nothing new" too (for !update).
func (b *Bot) checkUpdate(ctx context.Context, verbose bool) {
	defer b.recover("update")
	u := b.cfg.Updater
	r, newer, err := u.Check(ctx)
	b.updateMu.Lock()
	prevErr := b.lastCheck.err
	b.lastCheck = checkResult{at: time.Now(), latest: r.Tag, err: err}
	b.updateMu.Unlock()
	if err != nil {
		b.log.Warn("update check", "err", err)
		if verbose || prevErr == nil {
			b.control(ctx, "Update-Check fehlgeschlagen: "+err.Error())
		}
		return
	}
	if !newer {
		if verbose {
			b.control(ctx, fmt.Sprintf("Kein neues Release, %s ist aktuell.", b.cfg.Version))
		}
		return
	}
	b.control(ctx, fmt.Sprintf("Installiere %s (laufend: %s).", r.Tag, b.cfg.Version))
	if err := u.Install(ctx, r); err != nil {
		b.fail(ctx, "Update auf "+r.Tag, err)
		return
	}
	b.requestRestart("Update auf " + r.Tag)
}

// siteLoop keeps the website on its latest release: once shortly after start, then with
// the update interval.
func (b *Bot) siteLoop(ctx context.Context) {
	select {
	case <-time.After(20 * time.Second):
	case <-ctx.Done():
		return
	}
	for {
		b.syncSite(ctx, false)
		select {
		case <-time.After(b.duration("update.interval", 10*time.Minute)):
		case <-ctx.Done():
			return
		}
	}
}

// syncSite installs a new website release. The loop posts what happened to the control
// channel itself; !site (verbose) gets the line back and posts it as its answer.
func (b *Bot) syncSite(ctx context.Context, verbose bool) string {
	defer b.recover("site")
	tag, changed, err := b.site.Sync(ctx)
	switch {
	case err != nil:
		b.log.Warn("site sync", "err", err)
		b.siteMu.Lock()
		prev := b.siteErr
		b.siteErr = err.Error()
		b.siteMu.Unlock()
		msg := "Website-Update fehlgeschlagen: " + err.Error()
		if !verbose && prev != err.Error() {
			b.control(ctx, msg)
		}
		return msg
	case changed:
		b.siteMu.Lock()
		b.siteErr = ""
		b.siteMu.Unlock()
		msg := "Website " + tag + " ist live."
		if !verbose {
			b.control(ctx, msg)
		}
		return msg
	}
	b.siteMu.Lock()
	b.siteErr = ""
	b.siteMu.Unlock()
	if tag == "" {
		return "Noch kein Website-Release."
	}
	return "Website " + tag + " ist aktuell."
}
