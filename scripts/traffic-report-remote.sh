#!/usr/bin/env sh
# Render a private local HTML report through the established SSH route.
set -eu

if [ "$#" -gt 1 ]; then
	echo "usage: traffic-report-remote.sh [days]" >&2
	exit 2
fi
DAYS="${1:-30}"
case "$DAYS" in
	''|*[!0-9]*|???*) echo "days must be an integer from 1 to 90" >&2; exit 2 ;;
esac
if [ "$DAYS" -lt 1 ] || [ "$DAYS" -gt 90 ]; then
	echo "days must be an integer from 1 to 90" >&2
	exit 2
fi
if [ -z "${REMOTE:-}" ]; then
	echo "Set REMOTE to the existing SSH login, for example REMOTE=root@dailydocs.dev just traffic" >&2
	exit 2
fi
case "$REMOTE" in
	-*|*[!a-zA-Z0-9_.@:-]*) echo "REMOTE must be an SSH host or user@host" >&2; exit 2 ;;
esac

umask 077
if [ -L .cache ] || [ -L .cache/traffic ]; then
	echo "Refusing a symlinked report directory" >&2
	exit 2
fi
mkdir -p .cache/traffic
chmod 0700 .cache/traffic
report_dir="$(mktemp -d .cache/traffic/report-XXXXXXXX)"
report="$report_dir/index.html"
cleanup() { if [ -n "$report" ]; then rm -f "$report"; rmdir "$report_dir"; fi; }
trap cleanup EXIT HUP INT TERM
# The remote command has fixed paths and a validated numeric argument only.
# shellcheck disable=SC2029
ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=10 "$REMOTE" \
	"DB_PATH=/opt/dailydocs/data/dailydocs.sqlite /opt/dailydocs/bin/dailydocs traffic-report --days $DAYS --format html" >"$report"
chmod 0600 "$report"
result="$report"
report=''
printf 'Private traffic report: %s/%s\n' "$PWD" "$result"
if [ "${TRAFFIC_OPEN:-1}" != "0" ]; then
	if command -v open >/dev/null 2>&1; then
		open "$result" >/dev/null 2>&1 || true
	elif command -v xdg-open >/dev/null 2>&1; then
		xdg-open "$result" >/dev/null 2>&1 || true
	fi
fi
