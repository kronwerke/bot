# Changelog

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
