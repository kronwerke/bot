# Deploy

The bot runs as a systemd service on a Linux server (amd64). It needs outbound HTTPS to `discord.com`, `gateway.discord.gg`, `api.github.com` and `github.com` (release downloads), and, once the season runs, TCP to the Minecraft server's RCON and game port. It listens only on loopback.

The install is done once. After that, new versions arrive through releases (see Updates).

## Before

- Discord developer portal, the application, **Bot**: turn on **Server Members Intent** and **Message Content Intent**. Reset the token if it was ever shown anywhere.
- On the Discord server, the bot's role must be above `unverified`, `Mitglied`, `Spieler` and `Streamer`, or it cannot give or take them. Administrator is simplest.
- Create a private text channel for the bot (the control channel) and set its id as `channel.control` if it is not the default.

## 1. Download a release

```bash
v=v0.1.0
rm -rf ~/kronwerke-bot && mkdir ~/kronwerke-bot && cd ~/kronwerke-bot \
  && curl -fsSL --remote-name-all https://github.com/kronwerke/bot/releases/download/$v/{kronwerke-bot-linux-amd64,install.sh,rollback-guard.sh,kronwerke-bot.service,config.example.env,SHA256SUMS} \
  && sha256sum -c SHA256SUMS
```

Pass: every file `OK`.

## 2. Install

```bash
cd ~/kronwerke-bot && sudo sh install.sh
```

Pass: `installed v0.1.0`.

This creates the user `kronwerke-bot`, `/srv/projects/kronwerke-bot/{bin,state}`, the rollback guard in `/usr/local/lib/kronwerke-bot`, the unit, and `/etc/kronwerke-bot/bot.env` from the example (an existing one is kept).

## 3. The token

```bash
printf 'Token: ' && read -rs t && echo \
  && printf '%s\n' "$t" | sudo sh -c 'IFS= read -r t; sed -i "s|^DISCORD_TOKEN=.*|DISCORD_TOKEN=$t|" /etc/kronwerke-bot/bot.env' \
  && unset t && sudo grep -c '^DISCORD_TOKEN=.\{50,\}' /etc/kronwerke-bot/bot.env
```

Pass: `1`. The bare token and the form `Bot <token>` both work. The block runs in bash and zsh.

## 4. Start

```bash
sudo systemctl enable --now kronwerke-bot && sleep 5 \
  && systemctl is-active kronwerke-bot && curl -s 127.0.0.1:13030/healthz; echo
```

Pass: `active`, then `{"ready":true,...,"version":"v0.1.0"}`. The control channel shows `Online: v0.1.0`, and the verification message is in `verifizierung`.

## Configuration

`/etc/kronwerke-bot/bot.env` holds what the bot needs before it can reach Discord. Every line is explained in [config.example.env](../deploy/config.example.env). A change there needs `sudo systemctl restart kronwerke-bot`.

Everything else is a setting, changed at run time in the control channel:

```
!get channel.         every channel id
!set trash.seconds 30
!set channel.progress 1554334925635977287
!unset trash.seconds  back to the default
!set channel.trash -  switch a default off
```

| Setting | Default | What |
| --- | --- | --- |
| `guild` | the Kronwerke server | The only server the bot acts on |
| `role.*` | Kronwerke ids | `lead`, `admin`, `staff`, `streamer`, `partner`, `member`, `unverified`, `player`, `eventping` |
| `channel.*` | Kronwerke ids | `control`, `logs`, `verify`, `welcome`, `whitelist`, `status`, `trash`, `progress` (off) |
| `category.team` | Kronwerke id | Where the team's channels are |
| `category.applications` | empty | Created as `Bewerbungen` on the first application |
| `trash.seconds` | `60` | Lifetime of messages in `trash` |
| `update.interval` | `10m` | How often to look for a release |
| `status.interval` | `60s` | How often to ping the Minecraft server; read at start |
| `captcha.minutes`, `captcha.attempts`, `captcha.lock` | `10`, `3`, `10m` | Captcha lifetime, tries, lock after the last wrong try |
| `applications.keep` | `48h` | How long a decided application channel stays |
| `verify.autorole` | `on` | Give `unverified` on join |

## The Minecraft server

In `server.properties`:

```
enable-rcon=true
rcon.port=25575
rcon.password=<long random>
```

Allow the RCON port only from the bot's address. Then in `bot.env`:

```
KW_MC_ADDR=mc.example.net:25565
KW_RCON_ADDR=10.0.0.5:25575
KW_RCON_PASSWORD=<the same>
```

and restart the bot. `!status` shows the Minecraft line, `!rcon list` answers.

## Updates

1. Add a section `## 0.2.0` to `CHANGELOG.md` and push.
2. Push the tag `v0.2.0`, or run the `release` workflow from the Actions tab with the version.
3. The workflow runs the checks, builds, and publishes the release with the binary and `SHA256SUMS`.

The bot looks for a new release 30 seconds after start and then every `update.interval`; `!update` looks at once. It downloads the binary, checks it against `SHA256SUMS`, runs it with `version`, keeps the running one as `kronwerke-bot.prev`, swaps, and exits. systemd starts the new one.

If the new binary fails three starts before it reaches Discord, `rollback-guard.sh` puts `.prev` back and writes the version to `state/update.skip`, so it is not installed again. A later release installs normally. To try a skipped version again, delete that file.

`!rollback` goes back to `.prev` by hand and skips the current version the same way.

The service unit and `bot.env` are not part of a self update. A release that needs a change there says so in its notes.

## Watching it

- The control channel: `Online` after every start, errors as they happen, `!status`, `!events`.
- `journalctl -u kronwerke-bot -f`.
- `curl -s 127.0.0.1:13030/healthz` (503 while not connected to Discord) and `/metrics` for Prometheus.

## Uninstall

```bash
sudo systemctl disable --now kronwerke-bot \
  && sudo rm -f /etc/systemd/system/kronwerke-bot.service && sudo systemctl daemon-reload \
  && sudo rm -rf /usr/local/lib/kronwerke-bot /etc/kronwerke-bot /srv/projects/kronwerke-bot \
  && sudo userdel kronwerke-bot
```

This deletes the state (verifications, applications, slots).
