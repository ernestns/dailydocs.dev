#!/usr/bin/env bash
set -euo pipefail

DB_PATH="${DB_PATH:-/opt/dailydocs/data/dailydocs.sqlite}"
BACKUP_FILE="${1:-}"
RESTORE_CONFIRM="${RESTORE_CONFIRM:-}"
APP_NAME="${APP_NAME:-dailydocs}"

if [[ -z "$BACKUP_FILE" ]]; then
	echo "usage: RESTORE_CONFIRM=replace-dailydocs-db $0 path/to/backup.sqlite.gz" >&2
	exit 1
fi

if [[ "$RESTORE_CONFIRM" != "replace-dailydocs-db" ]]; then
	echo "Set RESTORE_CONFIRM=replace-dailydocs-db to restore over $DB_PATH." >&2
	exit 1
fi

if ! command -v sqlite3 >/dev/null 2>&1; then
	echo "sqlite3 is required." >&2
	exit 1
fi

if [[ ! -r "$BACKUP_FILE" ]]; then
	echo "Backup is not readable: $BACKUP_FILE" >&2
	exit 1
fi

if [[ ! -f "$DB_PATH" ]]; then
	echo "Destination database does not exist: $DB_PATH. Provision it with the application user's ownership and permissions before restoring." >&2
	exit 1
fi

restore_tmp="$(mktemp)"
current_backup=""
snapshot_ready=0
service_stopped=0

resume_service() {
	if [[ "$service_stopped" == 1 ]]; then
		service_stopped=0
		if ! systemctl start "$APP_NAME.service"; then
			echo "Unable to restart $APP_NAME.service after restore processing." >&2
			return 1
		fi
	fi
}

cleanup() {
	local status=$?
	if ! resume_service; then
		status=1
	fi
	rm -f "$restore_tmp" "$restore_tmp-wal" "$restore_tmp-shm"
	if [[ -n "$current_backup" && "$snapshot_ready" == 0 ]]; then
		rm -f "$current_backup" "$current_backup-wal" "$current_backup-shm"
	fi
	exit "$status"
}
trap cleanup EXIT

# SQLite dot commands decode C-style escapes inside double-quoted filenames.
sqlite_filename() {
	local value="$1"
	value="${value//\\/\\\\}"
	value="${value//\"/\\\"}"
	value="${value//$'\n'/\\n}"
	value="${value//$'\r'/\\r}"
	value="${value//$'\t'/\\t}"
	printf '"%s"' "$value"
}

case "$BACKUP_FILE" in
	*.gz)
		gzip -dc "$BACKUP_FILE" >"$restore_tmp"
		;;
	*)
		cp "$BACKUP_FILE" "$restore_tmp"
		;;
esac

integrity="$(sqlite3 "$restore_tmp" "PRAGMA integrity_check;")"
if [[ "$integrity" != "ok" ]]; then
	echo "Backup failed integrity check: $integrity" >&2
	exit 1
fi

# SQLite considers zero-byte files and empty databases valid. Neither is a
# usable application backup; restoring one would silently remove every table.
user_tables="$(sqlite3 "$restore_tmp" "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT GLOB 'sqlite_*';")"
if [[ "$user_tables" == "0" ]]; then
	echo "Backup contains no user tables; restore stopped." >&2
	exit 1
fi

if command -v systemctl >/dev/null 2>&1; then
	if ! service_state="$(systemctl show "$APP_NAME.service" --property=ActiveState --value)"; then
		echo "Unable to determine $APP_NAME.service state; restore stopped." >&2
		exit 1
	fi
	case "$service_state" in
		active)
			systemctl stop "$APP_NAME.service"
			service_stopped=1
			;;
		inactive|failed)
			;;
		*)
			echo "$APP_NAME.service is $service_state; wait for a stable service state before restoring." >&2
			exit 1
			;;
	esac
fi

if [[ -f "$DB_PATH" ]]; then
	current_backup="$(mktemp "$DB_PATH.pre-restore-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")"
	# A raw file copy can omit committed WAL data after a crash.
	sqlite3 "$DB_PATH" ".backup $(sqlite_filename "$current_backup")"
	# Keep the new safety snapshot portable as one file, independent of WAL.
	sqlite3 "$current_backup" 'PRAGMA journal_mode=DELETE;' >/dev/null
	if [[ "$(sqlite3 "$current_backup" 'PRAGMA integrity_check;')" != "ok" ]]; then
		echo "Current database snapshot failed integrity check; restore stopped." >&2
		exit 1
	fi
	# Some SQLite builds retain an unused shared-memory file after leaving WAL
	# mode. These belong only to this new, closed, rollback-mode snapshot.
	rm -f "$current_backup-wal" "$current_backup-shm"
	snapshot_ready=1
	echo "Previous database saved to $current_backup"
fi

# Let SQLite replace its contents transactionally, including any existing WAL.
# Keeping the database file also preserves its existing ownership and mode.
umask 077
sqlite3 "$DB_PATH" ".restore $(sqlite_filename "$restore_tmp")"

resume_service

echo "Restored $DB_PATH from $BACKUP_FILE"
