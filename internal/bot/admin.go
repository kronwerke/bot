package bot

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// The admin API forwards requests to the Minecraft server's launcher over the link, for
// tools that cannot use the control channel: whole log files, file writes, anything
// longer than a Discord message. It needs KW_ADMIN_TOKEN as a bearer token and does not
// exist without it.
//
//	POST /api/admin/link  {"op": "logs", "args": {"lines": 5000}}  ->  {"data": ...}
//	POST /api/admin/link  {"op": "logs", "args": {...}, "filter": "WARN|ERROR"}
//	GET  /api/admin/bot                                            ->  version, link, site
//
// Everything that changes the server (commands, start, stop, writes) is also posted to
// the control channel, so the team sees what was done.

// adminOps are the launcher operations the API passes on; mutating ones are announced.
var adminOps = map[string]bool{
	"status": false, "console": false, "logs": false, "ls": false, "read": false,
	"command": true, "start": true, "stop": true, "restart": true, "write": true, "delete": true,
}

func (b *Bot) adminAllowed(r *http.Request) bool {
	if b.cfg.AdminToken == "" {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	want := sha256.Sum256([]byte(b.cfg.AdminToken))
	have := sha256.Sum256([]byte(strings.TrimSpace(got)))
	return subtle.ConstantTimeCompare(want[:], have[:]) == 1
}

func adminJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (b *Bot) adminBot(w http.ResponseWriter, r *http.Request) {
	if !b.adminAllowed(r) {
		http.NotFound(w, r)
		return
	}
	out := map[string]any{"version": b.cfg.Version, "ready": b.ready.Load(),
		"uptime_seconds": int(time.Since(b.started).Seconds())}
	if i, ok := b.link.Connected(); ok {
		out["link"] = map[string]any{"name": i.Name, "state": i.State, "pack": i.Pack, "launcher": i.Launcher,
			"since": i.Since.UTC(), "fingerprint": i.Fingerprint}
	}
	if b.site != nil {
		out["site"] = b.site.Tag()
	}
	adminJSON(w, http.StatusOK, out)
}

func (b *Bot) adminLink(w http.ResponseWriter, r *http.Request) {
	if !b.adminAllowed(r) {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Op     string          `json:"op"`
		Args   json.RawMessage `json:"args"`
		Filter string          `json:"filter"` // keeps only matching lines of a text answer
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&in); err != nil {
		adminJSON(w, http.StatusBadRequest, map[string]string{"error": "body: " + err.Error()})
		return
	}
	mutating, ok := adminOps[in.Op]
	if !ok {
		adminJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown op " + in.Op})
		return
	}
	var filter *regexp.Regexp
	if in.Filter != "" {
		var err error
		if filter, err = regexp.Compile(in.Filter); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]string{"error": "filter: " + err.Error()})
			return
		}
	}
	if _, ok := b.link.Connected(); !ok {
		adminJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no Minecraft server connected"})
		return
	}
	var args any
	if len(in.Args) > 0 {
		args = in.Args
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	raw, err := b.link.Request(ctx, in.Op, args)
	b.log.Info("admin api", "op", in.Op, "err", err)
	if mutating {
		b.control(context.Background(), "Admin-API: "+describeOp(in.Op, in.Args, err))
	}
	if err != nil {
		adminJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if filter != nil {
		if raw, err = filterLines(raw, filter); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	adminJSON(w, http.StatusOK, map[string]json.RawMessage{"data": raw})
}

// filterLines keeps the lines of a text answer that match re, so a tool can ask for the
// warnings of a long log without downloading all of it. An answer is either one text or
// a list of lines (what the launcher sends for logs and console).
func filterLines(raw json.RawMessage, re *regexp.Regexp) (json.RawMessage, error) {
	var lines []string
	if err := json.Unmarshal(raw, &lines); err == nil {
		keep := []string{}
		for _, line := range lines {
			if re.MatchString(line) {
				keep = append(keep, line)
			}
		}
		return json.Marshal(keep)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, fmt.Errorf("filter works only on text answers")
	}
	var keep []string
	for _, line := range strings.Split(text, "\n") {
		if re.MatchString(line) {
			keep = append(keep, line)
		}
	}
	return json.Marshal(strings.Join(keep, "\n"))
}

// describeOp is the control channel line for a mutating request. File contents are not
// repeated, only the path.
func describeOp(op string, args json.RawMessage, err error) string {
	var a map[string]any
	json.Unmarshal(args, &a)
	s := op
	switch op {
	case "command":
		s = fmt.Sprintf("Befehl `%v`", a["cmd"])
	case "write", "delete":
		s = fmt.Sprintf("%s `%v`", op, a["path"])
	case "restart", "start":
		if u, _ := a["update"].(bool); u {
			s += " mit Pack-Update"
		}
	}
	if err != nil {
		s += ", Fehler: " + err.Error()
	}
	return s
}
