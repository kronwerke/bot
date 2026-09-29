package bot

import (
	"strconv"
	"time"
)

// defaults are the ids of the Kronwerke Discord server. Any of them can be changed at
// run time with !set in the control channel; the store keeps the override.
var defaults = map[string]string{
	"guild": "1473242451098472482",

	"role.lead":       "1473246853406658777", // Projektleitung
	"role.admin":      "1473247461601444083", // *
	"role.staff":      "1473246973858676777",
	"role.streamer":   "1473247037163044874",
	"role.partner":    "1473247106843279401",
	"role.member":     "1473247277039489136", // Mitglied
	"role.unverified": "1473259494220763332",
	"role.player":     "1554464523224617051", // Spieler, has a whitelist slot
	"role.eventping":  "1473250017455243355",

	"channel.control":       "1554464518506156043",
	"channel.logs":          "1473259886077939794",
	"channel.verify":        "1473243893326811189",
	"channel.welcome":       "1473243109943935091",
	"channel.whitelist":     "1484847936692031510",
	"channel.status":        "1473244279676604487",
	"channel.trash":         "1473256133895389226",
	"channel.progress":      "", // off until the season: a live goal board
	"category.team":         "1473246632161316906",
	"category.applications": "",

	"trash.seconds":     "60",
	"update.interval":   "10m",
	"status.interval":   "60s",
	"captcha.minutes":   "10",
	"captcha.attempts":  "3",
	"captcha.lock":      "10m",
	"applications.keep": "48h",
	"verify.autorole":   "on", // give new members the unverified role on join
}

// setting returns the override from the store or the default.
func (b *Bot) setting(key string) string {
	if v := b.store.Setting(key); v != "" {
		if v == "-" {
			return ""
		}
		return v
	}
	return defaults[key]
}

func (b *Bot) duration(key string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(b.setting(key))
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func (b *Bot) number(key string, fallback int) int {
	n, err := strconv.Atoi(b.setting(key))
	if err != nil {
		return fallback
	}
	return n
}

func (b *Bot) guild() string { return b.setting("guild") }
