#!/bin/sh
# Serves the web interface with synthetic data, for reviewing the UI.
# Usage: scripts/demo.sh [host:port]    (run inside `nix develop`)
# The default is 127.0.0.1:18090. Pass a VPN address to reach it from another device.
set -eu

addr=${1:-127.0.0.1:18090}
root=$(cd "$(dirname "$0")/.." && pwd)
dir=$(mktemp -d)
pid=
cleanup() {
	[ -n "$pid" ] && kill "$pid" 2>/dev/null
	rm -rf "$dir"
}
trap cleanup EXIT INT TERM

(cd "$root" && go build -o "$dir/kiseki" ./cmd/kiseki)
"$dir/kiseki" token new -name demo -host laptop -sources browser-history,claude-code \
	2>"$dir/tok" >"$dir/entry.json"
printf '[%s]' "$(cat "$dir/entry.json")" >"$dir/tokens.json"
sed 's/^token (shown once): //' "$dir/tok" >"$dir/token"
printf 'demo-read-token' >"$dir/read"

"$dir/kiseki" serve -listen "$addr" -db "$dir/k.db" \
	-tokens-file "$dir/tokens.json" -read-token-file "$dir/read" 2>"$dir/serve.log" &
pid=$!
sleep 1
python3 "$root/scripts/demo-seed.py" "$dir" "$addr" >/dev/null

echo "http://$addr/   login token: demo-read-token   (Ctrl-C to stop)"
wait "$pid"
