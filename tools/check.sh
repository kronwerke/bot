#!/bin/sh
# What CI checks, runnable before a push: formatting, vet, tests with the race
# detector, a static build, and the shell scripts parse.
set -eu
cd "$(dirname "$0")/.."
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "not formatted, run gofmt -w:" >&2
  echo "$unformatted" >&2
  exit 1
fi
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/kronwerke-bot
for f in deploy/*.sh tools/*.sh; do sh -n "$f"; done
echo ok
