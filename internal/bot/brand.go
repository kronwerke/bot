package bot

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// The pictures Discord shows for the server and for the bot come from the website the
// bot serves, so a new logo is set in one place: the website's release carries the file,
// and this call hands it to Discord. Nothing is uploaded through the API itself.
//
//	POST /api/admin/brand {"icon": "img/brand/icon-512.png", "avatar": "img/brand/icon-512.png"}
//
// icon is the server's icon, avatar the bot's own picture; either may be left out.
func (b *Bot) adminBrand(w http.ResponseWriter, r *http.Request) {
	if !b.adminAllowed(r) {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Icon   string `json:"icon"`
		Avatar string `json:"avatar"`
	}
	if err := readJSON(r, &in); err != nil {
		adminJSON(w, http.StatusBadRequest, map[string]string{"error": "body: " + err.Error()})
		return
	}
	if in.Icon == "" && in.Avatar == "" {
		adminJSON(w, http.StatusBadRequest, map[string]string{"error": "name icon, avatar or both: a picture of the website, like img/brand/icon-512.png"})
		return
	}
	if b.site == nil {
		adminJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "this bot serves no website to take the picture from"})
		return
	}
	done := map[string]any{"ok": true}
	if in.Icon != "" {
		img, err := b.brandImage(in.Icon)
		if err == nil {
			err = b.rest.SetGuildIcon(r.Context(), b.guild(), img, "new server icon from the website ("+in.Icon+")")
		}
		if err != nil {
			adminJSON(w, http.StatusBadGateway, map[string]string{"error": "icon: " + err.Error()})
			return
		}
		done["icon"] = in.Icon
		b.events.add("server icon set from the website: %s", in.Icon)
	}
	if in.Avatar != "" {
		img, err := b.brandImage(in.Avatar)
		if err == nil {
			err = b.rest.SetAvatar(r.Context(), img)
		}
		if err != nil {
			done["ok"] = false
			done["error"] = "avatar: " + err.Error()
			adminJSON(w, http.StatusBadGateway, done)
			return
		}
		done["avatar"] = in.Avatar
		b.events.add("bot avatar set from the website: %s", in.Avatar)
	}
	adminJSON(w, http.StatusOK, done)
}

// brandImage reads a picture of the website and returns it the way Discord takes one.
func (b *Bot) brandImage(p string) (string, error) {
	data, err := b.site.Read(p, 2<<20)
	if err != nil {
		return "", err
	}
	kind := http.DetectContentType(data)
	switch kind {
	case "image/png", "image/jpeg", "image/gif":
	default:
		return "", &brandError{p, strings.TrimPrefix(kind, "application/")}
	}
	return "data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

type brandError struct{ path, kind string }

func (e *brandError) Error() string {
	return e.path + " is " + e.kind + ", Discord takes PNG, JPEG or GIF"
}
