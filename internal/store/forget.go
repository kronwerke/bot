package store

// Forget removes what the bot keeps about a person who left the Discord: the linked
// Minecraft name, when they verified, captcha state, and decided applications whose
// channel is gone. Whitelist places are taken back before this, by the caller.
// An application that is still open or still has its channel stays until it is swept.
func (s *Store) Forget(user string) error {
	return s.update(func(d *data) {
		delete(d.Links, user)
		delete(d.Verified, user)
		delete(d.Attempts, user)
		delete(d.Lockouts, user)
		delete(d.Captchas, user)
		if a, ok := d.Applications[user]; ok && a.Status != "open" && a.ChannelID == "" {
			delete(d.Applications, user)
		}
	})
}

// DropSweptApplications removes decided applications whose channel was deleted. The
// decision itself stays in the team's log channel.
func (s *Store) DropSweptApplications() (n int) {
	s.update(func(d *data) {
		for u, a := range d.Applications {
			if a.Status != "open" && a.ChannelID == "" {
				delete(d.Applications, u)
				n++
			}
		}
	})
	return n
}
