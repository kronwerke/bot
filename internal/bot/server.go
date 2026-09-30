package bot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kronwerke/bot/internal/link"
	"github.com/kronwerke/bot/internal/store"
)

// The Minecraft server is reached through the launcher's link when it is connected, and
// through RCON otherwise. serverCommand is the one place that decides.

func (b *Bot) newHub() *link.Hub {
	return &link.Hub{
		Keys: b.store,
		Log:  b.log,
		Hooks: link.Hooks{
			Connected: func(i link.Info) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				b.events.add("link: %s connected from %s (launcher %s, pack %s, %s)", i.Name, i.Remote, i.Launcher, i.Pack, i.State)
				b.control(ctx, fmt.Sprintf("Minecraft-Server **%s** verbunden (Launcher %s, Pack %s, %s).", i.Name, i.Launcher, i.Pack, stateWord(i.State)))
				if i.State == "running" {
					b.syncAfterConnect(ctx)
				}
			},
			Disconnected: func(i link.Info, err error) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				b.events.add("link: %s disconnected: %v", i.Name, err)
				b.control(ctx, fmt.Sprintf("Verbindung zu **%s** getrennt.", i.Name))
			},
			Pending: func(i link.Info) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				b.events.add("link: %s from %s waits, fingerprint %s", i.Name, i.Remote, i.Fingerprint)
				b.control(ctx, fmt.Sprintf("Ein Minecraft-Server will sich verbinden: **%s** von `%s`, Launcher %s, Fingerprint `%s`.\nPasst der Fingerprint zur Konsole des Servers: `!link accept %s`",
					i.Name, i.Remote, i.Launcher, i.Fingerprint, i.Fingerprint))
			},
			State: func(i link.Info, state, detail string) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				b.events.add("minecraft %s %s", state, detail)
				switch state {
				case "running":
					b.control(ctx, "Minecraft läuft.")
					b.syncAfterConnect(ctx)
				case "crashed":
					b.control(ctx, "Minecraft ist abgestürzt: "+detail+". `!mc logs 60` zeigt das Ende des Logs.")
				case "stopped":
					b.control(ctx, "Minecraft ist gestoppt.")
				}
			},
		},
	}
}

// linkFingerprint is what the team compares with the launcher's console.
func (b *Bot) linkFingerprint(key string) string { return link.Fingerprint(key) }

func stateWord(s string) string {
	switch s {
	case "running":
		return "läuft"
	case "stopped":
		return "gestoppt"
	case "starting":
		return "startet"
	case "updating":
		return "aktualisiert das Pack"
	case "stopping":
		return "stoppt"
	case "crashed":
		return "abgestürzt"
	}
	return s
}

// syncAfterConnect gives places to everyone who linked while the server was away.
func (b *Bot) syncAfterConnect(ctx context.Context) {
	out, err := b.syncPlaces(ctx)
	if err != nil {
		b.events.add("sync after connect: %v", err)
		return
	}
	if !strings.HasPrefix(out, "Eigene Plätze nachgetragen: 0") {
		b.control(ctx, out)
	}
}

func (b *Bot) serverConnected() bool {
	_, ok := b.link.Connected()
	return ok || b.rcon != nil
}

// serverCommand runs a Kronwerke Core admin command. Core answers "OK <text>" or
// "ERR <text>".
func (b *Bot) serverCommand(cmd string) (string, error) {
	out, err := b.console(cmd)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	switch {
	case strings.HasPrefix(out, "OK"):
		return strings.TrimSpace(strings.TrimPrefix(out, "OK")), nil
	case strings.HasPrefix(out, "ERR"):
		return "", errors.New(strings.TrimSpace(strings.TrimPrefix(out, "ERR")))
	default:
		return "", fmt.Errorf("unexpected answer from the server: %q", out)
	}
}

// console runs any command on the server and returns its output.
func (b *Bot) console(cmd string) (string, error) {
	if _, ok := b.link.Connected(); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		raw, err := b.link.Request(ctx, "command", map[string]string{"cmd": cmd})
		if err != nil {
			return "", err
		}
		var out string
		json.Unmarshal(raw, &out)
		return out, nil
	}
	if b.rcon == nil {
		return "", errNoServer
	}
	return b.rcon.Command(cmd)
}

// ---- control channel: !link and !mc ----

func (b *Bot) linkControl(ctx context.Context, rest string, by string) string {
	sub, arg, _ := strings.Cut(rest, " ")
	arg = strings.TrimSpace(arg)
	switch sub {
	case "", "status":
		var lines []string
		if i, ok := b.link.Connected(); ok {
			lines = append(lines, fmt.Sprintf("Verbunden: **%s** von `%s` seit %s, Launcher %s, Pack %s, %s, Fingerprint `%s`",
				i.Name, i.Remote, i.Since.Format("02.01. 15:04"), i.Launcher, i.Pack, stateWord(i.State), i.Fingerprint))
		} else {
			lines = append(lines, "Kein Server verbunden.")
		}
		for _, i := range b.link.Pending() {
			lines = append(lines, fmt.Sprintf("Wartet: **%s** von `%s`, Fingerprint `%s`", i.Name, i.Remote, i.Fingerprint))
		}
		keys := b.store.LinkKeys()
		fps := make([]string, 0, len(keys))
		for fp := range keys {
			fps = append(fps, fp)
		}
		sort.Strings(fps)
		for _, fp := range fps {
			k := keys[fp]
			lines = append(lines, fmt.Sprintf("Angenommen: `%s` %s, am %s von %s", fp, k.Name, k.Accepted.Format("02.01.2006"), k.By))
		}
		return strings.Join(lines, "\n")
	case "accept":
		hash, info, ok := b.link.Accept(arg)
		if !ok {
			return "Kein wartender Server mit diesem Fingerprint. `!link` zeigt, wer wartet."
		}
		if err := b.store.PutLinkKey(arg, store.LinkKey{Hash: hash, Name: info.Name, Accepted: time.Now(), By: by}); err != nil {
			return "Speichern ging nicht: " + err.Error()
		}
		b.audit(ctx, fmt.Sprintf("Minecraft-Server %s (`%s`) angenommen von %s.", info.Name, arg, by))
		return fmt.Sprintf("Angenommen. **%s** verbindet sich gleich neu.", info.Name)
	case "revoke":
		ok, err := b.store.DeleteLinkKey(arg)
		if err != nil {
			return "Speichern ging nicht: " + err.Error()
		}
		if !ok {
			return "Diesen Fingerprint gibt es nicht."
		}
		b.link.Kick(arg)
		b.audit(ctx, fmt.Sprintf("Minecraft-Server `%s` entfernt von %s.", arg, by))
		return "Entfernt und getrennt."
	}
	return "So: `!link`, `!link accept <fingerprint>`, `!link revoke <fingerprint>`"
}

const mcHelp = "So: `!mc status`, `!mc start|stop`, `!mc restart [update]`, `!mc cmd <befehl>`, `!mc console [n]`, " +
	"`!mc logs [n] [datei]`, `!mc ls [ordner]`, `!mc cat <datei>`, `!mc launcher-update <url> <sha256>`"

func (b *Bot) mcControl(ctx context.Context, rest string) string {
	sub, arg, _ := strings.Cut(rest, " ")
	arg = strings.TrimSpace(arg)
	if _, ok := b.link.Connected(); !ok {
		return "Kein Minecraft-Server über den Launcher verbunden. `!link` zeigt den Stand."
	}
	req := func(op string, args any) (json.RawMessage, error) {
		c, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return b.link.Request(c, op, args)
	}
	switch sub {
	case "status":
		raw, err := req("status", nil)
		if err != nil {
			return "Fehler: " + err.Error()
		}
		var s map[string]any
		json.Unmarshal(raw, &s)
		out := fmt.Sprintf("Minecraft %s seit %v, Pack %v, NeoForge %v, Launcher %v, Java %v, Starts %v",
			stateWord(fmt.Sprint(s["state"])), s["since"], s["pack"], s["neoforge"], s["launcher"], s["java"], s["starts"])
		if d := fmt.Sprint(s["detail"]); d != "" && d != "<nil>" {
			out += "\n" + d
		}
		if p, ok := s["players"].(string); ok {
			out += "\n" + p
		}
		return out
	case "start", "stop", "restart":
		args := map[string]any{"update": arg == "update"}
		raw, err := req(sub, args)
		if err != nil {
			return "Fehler: " + err.Error()
		}
		var s string
		json.Unmarshal(raw, &s)
		return "Launcher: " + s
	case "cmd":
		if arg == "" {
			return mcHelp
		}
		out, err := b.console(arg)
		if err != nil {
			return "Fehler: " + err.Error()
		}
		return fence(out)
	case "console", "logs":
		n := 40
		fields := strings.Fields(arg)
		path := "logs/latest.log"
		if len(fields) > 0 {
			if v, err := strconv.Atoi(fields[0]); err == nil {
				n = v
				fields = fields[1:]
			}
		}
		if len(fields) > 0 {
			path = fields[0]
		}
		var raw json.RawMessage
		var err error
		if sub == "console" {
			raw, err = req("console", map[string]any{"lines": n})
		} else {
			raw, err = req("logs", map[string]any{"lines": n, "path": path})
		}
		if err != nil {
			return "Fehler: " + err.Error()
		}
		var lines []string
		json.Unmarshal(raw, &lines)
		return fence(strings.Join(lines, "\n"))
	case "ls":
		raw, err := req("ls", map[string]any{"path": arg})
		if err != nil {
			return "Fehler: " + err.Error()
		}
		var entries []struct {
			Name string `json:"name"`
			Dir  bool   `json:"dir"`
			Size int64  `json:"size"`
		}
		json.Unmarshal(raw, &entries)
		var lines []string
		for _, e := range entries {
			if e.Dir {
				lines = append(lines, e.Name+"/")
			} else {
				lines = append(lines, fmt.Sprintf("%s  %d", e.Name, e.Size))
			}
		}
		return fence(strings.Join(lines, "\n"))
	case "cat":
		raw, err := req("read", map[string]any{"path": arg})
		if err != nil {
			return "Fehler: " + err.Error()
		}
		var f struct {
			Data string `json:"data"`
		}
		json.Unmarshal(raw, &f)
		body, _ := base64.StdEncoding.DecodeString(f.Data)
		return fence(string(body))
	case "launcher-update":
		p := strings.Fields(arg)
		if len(p) != 2 {
			return mcHelp
		}
		raw, err := req("launcher-update", map[string]any{"url": p[0], "sha256": p[1]})
		if err != nil {
			return "Fehler: " + err.Error()
		}
		var s string
		json.Unmarshal(raw, &s)
		return "Launcher: " + s
	}
	return mcHelp
}

// fence puts text into a code block that fits one message, keeping the end.
func fence(s string) string {
	s = strings.ReplaceAll(s, "```", "'''")
	if s == "" {
		s = "(leer)"
	}
	const max = 1850
	if len(s) > max {
		s = strings.ToValidUTF8(s[len(s)-max:], "")
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < 200 {
			s = s[i+1:]
		}
		s = "...\n" + s
	}
	return "```\n" + s + "\n```"
}
