# Changelog

## 0.2.0

- `/dabei <name>` for everyone with the Season 1 role who is not a streamer: a place on the whitelist without a streamer's slot, and `s1.slots` (2) slots of their own to give with `/whitelist add`. Needs Kronwerke Core's `kw admin grant|ungrant`.
- Leaving the Discord also takes a Season 1 place back, together with every slot that player gave. `/whitelist remove` on a Season 1 name does the same for the team or the player.
- Every event the bot records also goes to the journal, so `journalctl -u kronwerke-bot` shows what `!events` shows.
- `!invites` and `!status` count Season 1 places.

## 0.1.0

First version.

- Verification with a captcha image: new members get `unverified`, solve the captcha in `verifizierung` and get `Mitglied`. Three wrong answers lock for ten minutes.
- Streamer applications with `/bewerben`: a form, then a private channel with the applicant and the team, accept and decline buttons. Accepting gives the `Streamer` role.
- Whitelist slots with `/whitelist add|remove|list` and `/link`, through Kronwerke Core over RCON. A log line per change in `whitelist`. Leaving the Discord frees the slot.
- Server status message, edited only when something changes. A goal board for `fortschritt`, off until the season.
- Messages in `trash` disappear after a minute.
- A control channel for the team: status, recent events, settings, RCON, update, rollback, restart.
- Self update from GitHub releases with a checksum check, a start test of the new binary, and a rollback guard that puts the previous binary back when the new one fails three starts.
- `/healthz` and Prometheus `/metrics` on loopback.
