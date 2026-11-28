#!/usr/bin/env bash
# Runs SearXNG on the Mac itself, outside Docker, as a login item (launchd). On Docker Desktop for
# macOS most of SearXNG's engines time out from inside a container (curl_cffi issue #915), while
# the same version works on the host. The version follows the image pinned in compose.yml.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
label="io.kaya.searxng"
plist="$HOME/Library/LaunchAgents/$label.plist"
port="${SEARXNG_MAC_PORT:-8888}"

env_value() {
	sed -n "s/^$1=//p" "$root/.env" 2>/dev/null | tail -n 1
}

data_dir="$(env_value DATA_DIR)"
data_dir="${data_dir:-./data}"
case "$data_dir" in /*) ;; *) data_dir="$root/${data_dir#./}" ;; esac
home="$data_dir/searxng"

if [ "$(uname -s)" != Darwin ]; then
	echo "searxng-mac: only for macOS; elsewhere SearXNG runs in Compose" >&2
	exit 2
fi

pinned_commit() {
	sed -n 's|^ *image: searxng/searxng:[0-9.]*-\([0-9a-f]*\)@.*|\1|p' "$root/compose.yml" | head -n 1
}

install() {
	local commit
	commit="$(pinned_commit)"
	[ -n "$commit" ] || { echo "searxng-mac: no searxng image pinned in compose.yml" >&2; exit 1; }
	mkdir -p "$home"
	if [ ! -d "$home/src/.git" ]; then
		git clone --quiet https://github.com/searxng/searxng.git "$home/src"
	fi
	if [ "$(git -C "$home/src" rev-parse --short=9 HEAD)" != "$commit" ]; then
		git -C "$home/src" fetch --quiet origin
		git -C "$home/src" checkout --quiet "$commit"
		rm -f "$home/installed"
	fi
	if [ ! -f "$home/installed" ]; then
		python3 -m venv "$home/venv"
		"$home/venv/bin/pip" install --quiet --upgrade pip setuptools wheel
		"$home/venv/bin/pip" install --quiet -r "$home/src/requirements.txt" -r "$home/src/requirements-server.txt"
		"$home/venv/bin/pip" install --quiet --no-build-isolation -e "$home/src"
		echo "$commit" >"$home/installed"
	fi
	echo "searxng-mac: SearXNG $commit installed in $home"
}

run() {
	local secret
	secret="$(env_value SEARXNG_SECRET)"
	[ -n "$secret" ] || { echo "searxng-mac: set SEARXNG_SECRET in .env" >&2; exit 1; }
	export SEARXNG_SECRET="$secret"
	export SEARXNG_SETTINGS_PATH="$root/services/searxng/settings.yml"
	export GRANIAN_HOST=127.0.0.1 GRANIAN_PORT="$port" GRANIAN_INTERFACE=wsgi GRANIAN_BLOCKING_THREADS=4
	cd "$home/src"
	exec "$home/venv/bin/granian" searx.webapp:app
}

healthy() {
	curl -fsS --max-time 3 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1
}

start() {
	install
	mkdir -p "$(dirname "$plist")"
	cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>$label</string>
	<key>ProgramArguments</key>
	<array><string>/bin/bash</string><string>$root/scripts/searxng-mac.sh</string><string>run</string></array>
	<key>EnvironmentVariables</key>
	<dict><key>SEARXNG_MAC_PORT</key><string>$port</string></dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>$home/searxng.log</string>
	<key>StandardErrorPath</key><string>$home/searxng.log</string>
</dict>
</plist>
EOF
	launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
	launchctl bootstrap "gui/$(id -u)" "$plist"
	for _ in $(seq 1 60); do
		if healthy; then
			echo "searxng-mac: running on http://127.0.0.1:$port (starts at login; log in $home/searxng.log)"
			return
		fi
		sleep 1
	done
	echo "searxng-mac: did not become healthy; see $home/searxng.log" >&2
	exit 1
}

stop() {
	launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
	rm -f "$plist"
	echo "searxng-mac: stopped and removed from login items"
}

status() {
	if healthy; then echo "searxng-mac: healthy on http://127.0.0.1:$port"; else echo "searxng-mac: not running"; exit 1; fi
}

case "${1:-}" in
install) install ;;
run) run ;;
start) start ;;
stop) stop ;;
status) status ;;
*)
	echo "usage: $0 install|start|stop|status|run" >&2
	exit 2
	;;
esac
