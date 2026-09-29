#!/bin/sh
# Runs before every start of kronwerke-bot (ExecStartPre). After a self update the bot
# writes state/update.trial ("<starts> <tag>"); the bot deletes it once it reaches
# Discord. If a new binary fails three starts in a row, the previous one is put back
# and its tag is written to state/update.skip so it is not installed again.
set -eu
dir=${KW_DIR:-/srv/projects/kronwerke-bot}
trial=$dir/state/update.trial
[ -f "$trial" ] || exit 0
read -r starts tag < "$trial" || true
starts=$(( ${starts:-0} + 1 ))
echo "$starts ${tag:-}" > "$trial"
if [ "$starts" -gt 3 ] && [ -f "$dir/bin/kronwerke-bot.prev" ]; then
  mv "$dir/bin/kronwerke-bot.prev" "$dir/bin/kronwerke-bot"
  echo "${tag:-unknown}" > "$dir/state/update.skip"
  rm -f "$trial"
  echo "rollback-guard: ${tag:-the new binary} failed three starts, the previous binary is back" >&2
fi
exit 0
