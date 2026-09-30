package bot

import "context"

// forget runs after a member left and their places were taken back: the bot keeps nothing
// about them that it no longer needs (see Store.Forget).
func (b *Bot) forget(userID string) {
	if err := b.store.Forget(userID); err != nil {
		b.log.Warn("forget", "err", err)
	}
}

// sweepPrivacy drops decided applications once their channel is gone.
func (b *Bot) sweepPrivacy(ctx context.Context) {
	if n := b.store.DropSweptApplications(); n > 0 {
		b.events.add("dropped %d decided applications", n)
	}
}
