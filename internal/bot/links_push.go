package bot

import (
	"context"
	"time"
)

// pushLinks tells the launcher who linked a Minecraft name on Discord, so its console can show
// streamers and Season 1 players next to their places. Launchers before 0.4 do not know the
// op and answer with an error, which is fine.
func (b *Bot) pushLinks() {
	if b.link == nil {
		return
	}
	if _, ok := b.link.Connected(); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := b.link.Request(ctx, "discord-links", map[string]any{"links": b.store.Linked()}); err != nil {
		b.log.Debug("discord-links", "err", err)
	}
}
