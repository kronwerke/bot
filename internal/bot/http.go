package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// serveHTTP answers health checks and Prometheus on a loopback address, and takes the
// Minecraft server's link.
func (b *Bot) serveHTTP(ctx context.Context) {
	if b.cfg.HTTPAddr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !b.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"version": b.cfg.Version, "ready": b.ready.Load(),
			"uptime_seconds": int(time.Since(b.started).Seconds()), "reconnects": b.reconnects(),
		})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		ready := 0
		if b.ready.Load() {
			ready = 1
		}
		fmt.Fprintf(w, "# HELP kronwerke_bot_info Build of the running bot.\n# TYPE kronwerke_bot_info gauge\nkronwerke_bot_info{version=%q} 1\n", b.cfg.Version)
		metric := func(name, help, typ string, v any) {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, v)
		}
		metric("kronwerke_bot_ready", "1 when connected to the Discord gateway.", "gauge", ready)
		metric("kronwerke_bot_uptime_seconds", "Seconds since start.", "gauge", int(time.Since(b.started).Seconds()))
		metric("kronwerke_bot_gateway_reconnects_total", "Gateway reconnects since start.", "counter", b.reconnects())
		metric("kronwerke_bot_verifications_total", "Passed captchas since start.", "counter", b.stats.verified.Load())
		metric("kronwerke_bot_verification_failures_total", "Wrong captcha answers since start.", "counter", b.stats.failed.Load())
		metric("kronwerke_bot_applications_total", "Streamer applications since start.", "counter", b.stats.applications.Load())
		metric("kronwerke_bot_invites_total", "Whitelist slots given since start.", "counter", b.stats.invites.Load())
		metric("kronwerke_bot_errors_total", "Errors since start.", "counter", b.stats.errors.Load())
	})
	// the Minecraft server's launcher, the public API, the admin API and the website;
	// the reverse proxy keeps /healthz and /metrics to itself
	mux.Handle("GET /link", b.link)
	mux.HandleFunc("GET /api/status", b.apiStatus)
	mux.HandleFunc("GET /api/admin/bot", b.adminBot)
	mux.HandleFunc("POST /api/admin/link", b.adminLink)
	if b.cfg.CurseForgeKey != "" {
		// with methods, like every other pattern here: a bare path next to "GET /" makes the mux panic
		cp := newCurseProxy(b.cfg.CurseForgeKey)
		mux.Handle("GET /api/curseforge/", cp)
		mux.Handle("POST /api/curseforge/", cp)
	}
	if b.site != nil {
		mux.Handle("GET /", b.site.Handler())
	}
	srv := &http.Server{Addr: b.cfg.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		b.log.Error("http", "err", err)
	}
}

// apiStatus is the public state of the server for the website: online, players, goals.
func (b *Bot) apiStatus(w http.ResponseWriter, r *http.Request) {
	b.statusMu.Lock()
	p := b.lastPing
	b.statusMu.Unlock()
	b.apiMu.Lock()
	goals, goalsAt, season := b.goals, b.goalsAt, b.season
	b.apiMu.Unlock()

	server := map[string]any{"online": false}
	if !p.at.IsZero() && p.err == nil {
		server = map[string]any{"online": true, "players": p.status.Online, "max": p.status.Max,
			"names": p.status.Players, "version": p.status.Version, "checked": p.at.UTC()}
	}
	if i, ok := b.link.Connected(); ok {
		server["state"] = i.State
		server["pack"] = i.Pack
		if p := b.packNow.Load(); p != nil && *p != "" {
			server["pack"] = *p
		}
	}
	out := map[string]any{"server": server}
	if season != nil {
		out["season"] = season
	}
	if goals != nil && time.Since(goalsAt) < 10*time.Minute {
		out["goals"] = goals
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=30")
	json.NewEncoder(w).Encode(out)
}
