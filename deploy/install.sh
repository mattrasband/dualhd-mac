#!/bin/sh
# Builds dualhd-mac (from a source checkout) and installs it as a launchd agent that starts at login.
#
#   deploy/install.sh            build, install and (re)start the service
#   deploy/install.sh uninstall  stop and remove the service
#
# Run ./bin/scan first so lineup.json exists.
set -eu

LABEL=com.github.mattrasband.dualhd-mac
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG="$HOME/Library/Logs/dualhd-mac.log"
DOMAIN="gui/$(id -u)"
ROOT=$(cd "$(dirname "$0")/.." && pwd)

stop() {
	launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
}

if [ "${1:-}" = uninstall ]; then
	stop
	rm -f "$PLIST"
	echo "removed $LABEL (logs remain at $LOG)"
	exit 0
fi

if [ ! -f "$ROOT/lineup.json" ]; then
	echo "no lineup.json: run ./bin/scan (with the service stopped) first" >&2
	exit 1
fi

cd "$ROOT"
if [ -f go.mod ]; then
	go build -o bin/hdhr ./cmd/hdhr
elif [ ! -x bin/hdhr ]; then
	echo "bin/hdhr not found" >&2
	exit 1
fi

mkdir -p "$(dirname "$PLIST")"
cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$ROOT/bin/hdhr</string>
		<string>-lineup</string>
		<string>$ROOT/lineup.json</string>
	</array>
	<key>WorkingDirectory</key>
	<string>$ROOT</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>$LOG</string>
	<key>StandardErrorPath</key>
	<string>$LOG</string>
</dict>
</plist>
PLIST
plutil -lint "$PLIST" >/dev/null

stop
launchctl bootstrap "$DOMAIN" "$PLIST"
echo "installed $LABEL; logs: $LOG"
echo "add it to Plex at 127.0.0.1:5004 (Settings > Live TV & DVR)"
