```
 _  __                                 _        
| |/ /_ __ ___  _ ____      _____ _ __| | _____ 
| ' /| '__/ _ \| '_ \ \ /\ / / _ \ '__| |/ / _ \
| . \| | | (_) | | | \ V  V /  __/ |  |   <  __/
|_|\_\_|  \___/|_| |_|\_/\_/ \___|_|  |_|\_\___|
                                                
                           b o t
```

**The Discord bot of the Kronwerke server: captcha, streamer applications, whitelist slots and server status, and it updates itself.**

[![ci](https://github.com/kronwerke/bot/actions/workflows/ci.yml/badge.svg)](https://github.com/kronwerke/bot/actions/workflows/ci.yml)
![status](https://img.shields.io/badge/status-early-orange)
![go](https://img.shields.io/badge/go-1.24-blue)
![licence](https://img.shields.io/badge/licence-MIT-green)
[![made by](https://img.shields.io/badge/made%20by-Elchi-black)](https://github.com/Elchi-dev)

## Overview

In Season 1 the Discord and the Minecraft server lived side by side. The old bot's captcha stopped working, applications from streamers got lost in DMs, and whitelisting was done by hand. Season 2 ties them together: you are on the Minecraft server because a streamer gave you a slot, and you keep it only while you are on the Discord.

This bot does the Discord half of that. The Minecraft half is [kronwerke/core](https://github.com/kronwerke/core). The server's [launcher](https://github.com/kronwerke/launcher) dials in to the bot, so the bot can start, stop, update and read the server without any host API; plain RCON works too.

## The trick

After the first install nobody has to log in to the server again. A new version is a GitHub release; the bot finds it, checks it, swaps itself and restarts. If the new version does not come up, the old one comes back on its own.

```
  tag v0.2.0 --> Actions: tests, build, SHA256SUMS --> release
                                                          |
                                                          | every 10 minutes
                                                          v
  bot restarts <-- binary swapped, old one kept <-- download, checksum, run "version"
       |
       +-- fails three starts --> rollback-guard.sh puts the old binary back
                                  and skips that version from then on
```

Everything else the team might want to change (channel ids, roles, timings) is a setting, changed with `!set` in a private control channel, no restart needed.

## Parts

| Part | What it does |
| --- | --- |
| `cmd/kronwerke-bot` | The binary: configuration from the environment, `version` |
| `internal/bot` | Verification, applications, whitelist, status, control channel, settings |
| `internal/discord` | REST with rate limits, the gateway with resume and heartbeats, interactions |
| `internal/ws` | A small WebSocket client for the gateway |
| `internal/captcha` | Captcha codes and the PNG they are drawn into |
| `internal/link` | The server link: accepts the launcher, routes requests, reports its state |
| `internal/rcon` | Minecraft RCON, including answers split over several packets |
| `internal/mcping` | Server List Ping for the status message |
| `internal/store` | State in one JSON file, written atomically |
| `internal/update` | Self update from GitHub releases |
| `internal/site` | The website: downloads each new release of `kronwerke/website` and serves it |
| `deploy/` | systemd unit, installer, rollback guard, example configuration |
| `tools/rconcli` | RCON from the command line, for testing |
| `tools/linkcli` | A stand-in for the bot that a launcher can dial, for testing |

Standard library only. An update is one static binary and nothing else.

## Quick look

```
sh tools/check.sh
go build -o kronwerke-bot ./cmd/kronwerke-bot
DISCORD_TOKEN=... KW_STATE_DIR=./state KW_UPDATE_REPO=off ./kronwerke-bot
```

On a server, see [docs/DEPLOY.md](docs/DEPLOY.md): download a release, run `install.sh`, put in the token, start.

| Command | Who | What |
| --- | --- | --- |
| Button in `verifizierung` | new members | Captcha, then `Mitglied` |
| `/apply` | everyone | Apply as a streamer, opens a private channel with the team |
| `/link <name>` | everyone | Your Minecraft name. Streamers and Season 1 players are whitelisted with it, with slots of their own |
| `/whitelist add <player> <user>` | streamers, Season 1 players | Give one of your slots to someone on the Discord |
| `/whitelist remove <player>` | streamers, Season 1 players | Free it again (a Season 1 name frees its slots too) |
| `/whitelist list` | streamers, Season 1 players | Who has your slots |

In the control channel (team leads, and the bot's own account):

| Command | What |
| --- | --- |
| `!status` | Version, uptime, gateway, Minecraft, update check, counters |
| `!events [n]` | The last things the bot saw and did |
| `!get [prefix]`, `!set key value`, `!unset key` | Settings; `-` switches a default off |
| `!members` | Role counts, members without `Mitglied` |
| `!verify-sync`, `!verify-panel`, `!apply-panel <channel>` | Repair roles, post the panels again |
| `!apps`, `!invites` | Open applications, given slots and own places |
| `!sync` | Whitelist streamers and Season 1 players who linked while the server was away |
| `!rcon <command>` | A command on the Minecraft server |
| `!link`, `!link accept <fingerprint>`, `!link revoke <fingerprint>` | The server link and which launchers may connect |
| `!mc status`, `start`, `stop`, `restart [update]`, `cmd`, `console [n]`, `logs [n] [file]`, `ls`, `cat` | The Minecraft server through its launcher |
| `!update`, `!rollback`, `!restart` | Look for a release now, go back one version, restart |
| `!site` | Look for a new website release now |

## Over HTTP

On `KW_HTTP_ADDR`, behind the reverse proxy:

| Path | What |
| --- | --- |
| `/` | The website, from the latest release of `kronwerke/website` |
| `GET /link` | The launcher's WebSocket |
| `GET /api/status` | Public: server online, players, launcher state, goals |
| `GET /api/admin/bot` | Admin: version, link, website version |
| `POST /api/admin/link` | Admin: `{"op": "logs", "args": {"lines": 5000}}` and the other launcher operations (`status`, `console`, `logs`, `ls`, `read`, `command`, `start`, `stop`, `restart`, `write`, `delete`). `"filter": "<regexp>"` keeps only the matching lines of a text answer. Everything that changes the server is posted to the control channel |
| `/healthz`, `/metrics` | Loopback only; the proxy does not pass them |

The admin paths exist only with `KW_ADMIN_TOKEN` set and answer 404 to anyone without it.

## Planned

- `/whitelist slots` and `/whitelist bonus` for the team, mapping to `/kw admin slots|bonus`.
- A check that every whitelisted name still belongs to someone on the Discord.
- Announcements when a goal reaches its hold point and when it completes.

## Non-goals

- Moderation. Discord AutoMod does that.
- Support tickets. Ticket Tool stays.
- Anything the Minecraft server can do itself. Slots and goals live in Kronwerke Core; the bot only asks.
- Running more than one Discord server.

## Status

Early. Unit tests for every part, and integration tests that run the whole bot against a fake Discord (REST and gateway): verification, applications, whitelist, the control channel, reconnects. The RCON client was run against a dedicated server with the full pack and Kronwerke Core. Self update and the rollback guard were run end to end against a fake release server, including a broken release. Not yet run for a season.

## Docs

| Page | What |
| --- | --- |
| [docs/DEPLOY.md](docs/DEPLOY.md) | Install, configuration, updates, settings, watching it |
| [CHANGELOG.md](CHANGELOG.md) | What changed per version |
| [deploy/config.example.env](deploy/config.example.env) | Every environment variable, explained |

## Licence

MIT. See `LICENSE`.

Made by [Elchi](https://github.com/Elchi-dev)
