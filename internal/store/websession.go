package store

import "time"

// WebSession is a sign in on the website with a Minecraft account (auth.kronwerke.com).
// Only the hash of the browser's token is kept.
type WebSession struct {
	UUID    string    `json:"uuid"` // Minecraft account, without dashes
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

// WebSession answers the session of a token hash, if it has not expired.
func (s *Store) WebSession(hash string) (w WebSession, ok bool) {
	s.view(func(d *data) { w, ok = d.WebSessions[hash] })
	if ok && time.Now().After(w.Expires) {
		return WebSession{}, false
	}
	return w, ok
}

// PutWebSession keeps a new session and drops expired ones, at most max per account (the
// oldest go first) so a loop of sign ins does not grow the file.
func (s *Store) PutWebSession(hash string, w WebSession, max int) error {
	return s.update(func(d *data) {
		now := time.Now()
		var mine []string
		for h, o := range d.WebSessions {
			if now.After(o.Expires) {
				delete(d.WebSessions, h)
				continue
			}
			if o.UUID == w.UUID {
				mine = append(mine, h)
			}
		}
		for len(mine) >= max && max > 0 {
			oldest := 0
			for i, h := range mine {
				if d.WebSessions[h].Created.Before(d.WebSessions[mine[oldest]].Created) {
					oldest = i
				}
			}
			delete(d.WebSessions, mine[oldest])
			mine = append(mine[:oldest], mine[oldest+1:]...)
		}
		d.WebSessions[hash] = w
	})
}

// DeleteWebSession signs a browser out.
func (s *Store) DeleteWebSession(hash string) error {
	return s.update(func(d *data) { delete(d.WebSessions, hash) })
}

// WebSessionCount is the number of sessions kept, expired ones included.
func (s *Store) WebSessionCount() (n int) {
	s.view(func(d *data) { n = len(d.WebSessions) })
	return n
}

// TouchWebSession moves a session's end out, so a browser in use stays signed in.
func (s *Store) TouchWebSession(hash string, expires time.Time) error {
	return s.update(func(d *data) {
		if w, ok := d.WebSessions[hash]; ok {
			w.Expires = expires
			d.WebSessions[hash] = w
		}
	})
}
