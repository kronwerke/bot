#!/bin/sh
# One-time installation of kronwerke-bot on a systemd server. Run as root from the
# folder this script is in, next to the files of a release: the binary
# (kronwerke-bot-linux-amd64 or kronwerke-bot), rollback-guard.sh,
# kronwerke-bot.service and config.example.env. Later versions arrive by themselves
# through the bot's self update. Running it again keeps bot.env and the state.
set -eu
cd "$(dirname "$0")"
dir=/srv/projects/kronwerke-bot
bin=kronwerke-bot
[ -f "$bin" ] || bin=kronwerke-bot-linux-amd64

id kronwerke-bot >/dev/null 2>&1 || useradd --system --home-dir "$dir" --shell /usr/sbin/nologin kronwerke-bot
install -d -o kronwerke-bot -g kronwerke-bot -m 750 "$dir" "$dir/bin" "$dir/state"
install -o kronwerke-bot -g kronwerke-bot -m 755 "$bin" "$dir/bin/kronwerke-bot"
install -d -m 755 /usr/local/lib/kronwerke-bot
install -o root -g root -m 755 rollback-guard.sh /usr/local/lib/kronwerke-bot/rollback-guard.sh
install -d -o root -g kronwerke-bot -m 750 /etc/kronwerke-bot
[ -f /etc/kronwerke-bot/bot.env ] || install -o root -g kronwerke-bot -m 640 config.example.env /etc/kronwerke-bot/bot.env
install -o root -g root -m 644 kronwerke-bot.service /etc/systemd/system/kronwerke-bot.service
cat > "$dir/project.manifest" <<MANIFEST
name = kronwerke-bot
owner = private
ports = 13030-13039
service = kronwerke-bot.service
data = $dir/state
config = /etc/kronwerke-bot/bot.env
MANIFEST
chown kronwerke-bot:kronwerke-bot "$dir/project.manifest"
systemctl daemon-reload
echo "installed $("$dir/bin/kronwerke-bot" version)"
