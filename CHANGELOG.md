# Changelog

## 0.9.0

- The website answers without ".html": kronwerke.com/installieren, /faq, /mods and so on; /fragen and /privacy are short names for the FAQ and the privacy page.
- `/link` with another account's name no longer moves a streamer's slot by itself. The same account under a new name follows at once (checked at Mojang); another account needs the streamer's yes in a DM, at most one request a day. (R2-09)
- A streamer or Season 1 role taken away no longer removes the place and every viewer at once. The control channel says when it will happen, with a button that gives the role back; after `role.grace` (24 hours) without the role the place goes as before. (R3-04)
- A viewer on a streamer's slot who becomes a streamer, or gets the Season 1 role, gets a place of their own: the slot goes back to the streamer first, who is told. Before, the grant was refused and the slot stayed used. (R3-03)
- The CurseForge proxy counts by the visitor's address from Cloudflare (`CF-Connecting-IP`, trusted only from Caddy on loopback), not by Cloudflare's edge. (R2-10)

## 0.8.1

- The client secret of the whitelist app can be set over the admin API (`POST /api/admin/oauth {"client_secret": "..."}`), without editing `bot.env`. Discord checks it first; it is kept in the state file and never shown. `bot.env` wins when both are set. `GET /api/admin/bot` says whether the app is ready.

## 0.8.0

- The whitelist app: one page for managing whitelist slots, with Minecraft faces. In Discord it opens as an Activity with `/whitelist verwalten`; outside Discord it is at kronwerke.com/whitelist/. Both sign in with Discord (the Activity through the Embedded App SDK, the website through Discord's OAuth page) and follow the same rules as the slash commands.
  - Your own place and "5 von 8 Plätzen belegt".
  - Your people with their faces, removing with a second click.
  - Search by Minecraft or Discord name, with the state of every hit (free, already has a place, not verified, no name linked). Someone without a linked name gets one typed in, checked at Mojang first.
  - "Mehr Plätze anfragen" posts the request in the control channel with buttons (2 more, 4 more, decline) for the project leads; the answer reaches the asker as a DM. Extra slots are kept with the place, so a new world gets them back.
- Faces come from the bot itself (Mojang profile, skin, face and hat layer, cached), since an Activity loads nothing from other addresses.
- Needs `KW_DISCORD_CLIENT_SECRET` and, in the developer portal, Activities switched on. Without them `/whitelist verwalten` answers with the website link and the page says it is not set up yet. See docs/DEPLOY.md.
- `/whitelist add` and `/whitelist remove` share their checks with the app.

## 0.7.0

Needs Kronwerke Core 0.27.0 (UUIDs and `-1` slots in the admin commands).

- "No Discord, no slot" holds. Every hour, when the bot is ready and when the server connects, the places are compared with the Discord members: who left while the bot restarted, or whose removal failed because the server was down, loses the place then. More than five at once wait for `!sync apply`, so a half answer from Discord cannot empty the whitelist.
- Roles count as they change. A streamer or Season 1 role given to a member who linked a name brings their place at once; a role taken away takes it, with every slot they gave. Team members keep theirs.
- Accounts by UUID. The bot keeps the UUID Kronwerke Core answers with every place and sends it with every invite, revoke and ungrant; streamers are found by UUID too. A renamed account keeps its place, the bot takes the new name from the server.
- One Discord account, one place: `/whitelist add` refuses a member who already has one.
- `/whitelist add` takes the Discord user first and the name they linked with `/link`; `player` is only for someone who linked none, and a typed name that differs from the linked one is refused instead of replacing it.
- `/link` with a new name moves a streamer's slot to it (the old name comes off, the new one goes on, with the same streamer); before, the whitelist kept the old name and the answer said the player had no place.
- `/whitelist me`: whether you are on the whitelist, under which name, through whom, how many of your own places are taken, and when the server opens. `/whitelist list` starts with "3 von 5 Plätzen vergeben" and no longer reads like "you are not on the list" to players without places.
- Updates install only releases whose SHA256SUMS carries the release key's signature (`SHA256SUMS.sig`, Ed25519, the key is pinned in the binary). A release anyone else publishes is refused.
- `/api/status` shows the number of players online, no names.
- CurseForge proxy: the rate limit goes by the address the reverse proxy appended (the last `X-Forwarded-For` entry), and the cache keeps at most 64 MB, oldest first, answers over 2 MB not at all.
- Captcha: a button for members who cannot read the image (the team frees them by hand), at most six new images per ten minutes, the lockout message names the configured time, and the panel is found by its stored id however far it scrolled up. An older panel gets the new button in place.
- What the bot posts in the control channel never runs as a command, whatever text it carries.
- Unknown launchers: at most three wait at once, each for at most 30 minutes, and the same one is announced at most every six hours.
- The preparation text of the status embed is the setting `status.prep`.
- `/api/status` passes on the season's planned start and time zone from Kronwerke Core (`season.start`, `season.timezone`), so the website and the Twitch bot read the date from one place.

## 0.6.0

- Places survive a new world. The bot keeps every own place and every slot a streamer gave, Kronwerke Core keeps them in the world. When the server connects and knows no streamer at all, the bot puts every place back (own places first, then the slots). Otherwise it reports what it holds and the server does not, once, in the control channel, since a streamer may have freed a slot in game.
- `!sync` also compares with the server; `!sync restore` puts the missing places back.

## 0.5.2

- Members who use `/link` are passed to the launcher (Minecraft name, Discord name, streamer or Season 1, when), on every change and when the launcher connects, so its console shows them next to their places.

## 0.5.1

- The CurseForge proxy's routes carry their methods; 0.5.0 stopped at start with a key set, because a bare path next to the website's route makes Go's router panic.

## 0.5.0

- A CurseForge proxy at `/api/curseforge/v1/...` for the launcher's console: it adds the key from `KW_CURSEFORGE_KEY`, so the key never reaches a launcher. Only fingerprint matching, mod info, Minecraft search and files pass; 300 requests per address in ten minutes; reads are kept for ten minutes. Without the key the path does not exist.

## 0.4.8

- `/api/status` passes the obelisk's stage and sleep on in `season` (`tier`, `slumbering`), as Kronwerke Core 0.13.3 prints them; the website shows them.

## 0.4.7

- Before the season starts the server is a work in progress: the status message says so instead of counting players, the bot's status line reads "Work in Progress", and `/api/status` has a `season` field (`running`, `number`, `startedAt`) for the website. The bot asks Kronwerke Core with `kw admin season json`; an older Core that does not know it keeps the player count.
- The status line no longer uses a middle dot.

## 0.4.6

- After Discord asks for a reconnect, which it does every hour or two, the bot resumes at once. It used to wait longer after each one, up to a minute, and buttons like the captcha that were pressed in that minute failed with "Unknown interaction". Only connections that fail again and again back off now.

## 0.4.4

- The pack version on the website and in the status message is the one that runs now, not the one from when the launcher connected.

## 0.4.3

- Without `KW_MC_ADDR` the status comes from the launcher: online while Minecraft runs, players from `list`. The website and the status message showed the server as offline before.

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
