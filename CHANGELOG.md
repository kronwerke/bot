# Changelog

## 0.4.2

- The admin API's `"filter"` also works on answers that are a list of lines, which is what the launcher sends for `logs` and `console`.

## 0.4.1

- Messages carry no emoji any more: status, control channel, whitelist and application lines are plain text, and the goal bar is drawn with block characters.
- The admin API takes `"filter"`, a regular expression; only matching lines of a text answer come back.
- Leaving the Discord also removes the linked Minecraft name, the verification record and captcha state, after the whitelist places are taken back. Decided applications are dropped once their channel is deleted; the decision stays in the log channel.

## 0.4.0

- The bot serves the website: it downloads each new release of `kronwerke/website` (`site.tar.gz`, checked against `SHA256SUMS`), unpacks it next to the old one and switches over. `!site` looks at once, `!status` shows the version. `KW_SITE_REPO` sets the repository, `off` turns it off.
- The admin API: `GET /api/admin/bot` and `POST /api/admin/link` pass whole logs, files and commands to the launcher. It exists only with `KW_ADMIN_TOKEN`, and everything that changes the server is posted to the control channel.
- Caddy now passes everything but `/healthz` and `/metrics` to the bot (see docs/DEPLOY.md).

## 0.3.0

- Commands are English: `/apply` instead of `/bewerben`, and `/whitelist add` takes `player` and `user`.
- `/link` puts streamers and Season 1 players on the whitelist right away, with a place of their own: streamers with Core's default slots, Season 1 players with `s1.slots`. `/dabei` is gone, `/link` does it. Changing the name moves the place, as long as no slot was given yet.
- Accepting a streamer who already linked a name whitelists them at once.
- `!sync` gives the place to everyone who linked while the server was not connected.
- The server link: the [Kronwerke launcher](https://github.com/kronwerke/launcher) on the Minecraft server dials in at `/link`, so the host needs no API and the server no open port. Unknown launchers wait until the team accepts their fingerprint with `!link accept`; only a hash of the key is stored. Commands go through the link when it is up and through RCON otherwise.
- `!mc status|start|stop|restart [update]|cmd|console|logs|ls|cat|launcher-update` in the control channel. Starts, crashes and stops of Minecraft are reported there, and places are synced when the server comes up.
- `GET /api/status` for the website: online, players, launcher state, goals.
- The WebSocket server side answers pings and assembles fragmented messages.
- `tools/linkcli`: a stand-in for the bot, for testing a launcher.

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
