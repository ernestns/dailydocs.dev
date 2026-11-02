#!/usr/bin/env bash
set -euo pipefail

DB_PATH="${DB_PATH:-/opt/dailydocs/data/dailydocs.sqlite}"
BACKUP_DIR="${BACKUP_DIR:-/opt/dailydocs/backups}"
TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"

if ! command -v sqlite3 >/dev/null 2>&1; then
	echo "sqlite3 is required." >&2
	exit 1
fi

if [[ ! -r "$DB_PATH" ]]; then
	echo "Database is not readable: $DB_PATH" >&2
	exit 1
fi

umask 077
mkdir -p "$BACKUP_DIR"

# Stage on the backup filesystem so publication can be atomic. The private
# directory also reserves a unique name for concurrent, same-second backups.
stage_dir="$(mktemp -d "$BACKUP_DIR/.dailydocs-$TIMESTAMP.XXXXXX")"
snapshot="$stage_dir/snapshot.sqlite"
stage_name="${stage_dir##*/}"
archive_name="${stage_name#.}.sqlite.gz"
archive="$stage_dir/$archive_name"
BACKUP_PATH="$BACKUP_DIR/$archive_name"
cleanup() {
	rm -f "$snapshot" "$snapshot-wal" "$snapshot-shm" "$snapshot-journal" "$archive"
	rmdir "$stage_dir"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

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

sqlite3 "$DB_PATH" ".backup $(sqlite_filename "$snapshot")"
# SQLite's backup API copies pages coherently but does not validate their
# contents. Verify the private snapshot before publishing a recovery point.
integrity="$(sqlite3 "$snapshot" 'PRAGMA integrity_check;')" || {
	echo "Backup snapshot integrity check could not complete; backup not published." >&2
	exit 1
}
if [[ "$integrity" != "ok" ]]; then
	echo "Backup snapshot failed integrity check: $integrity" >&2
	exit 1
fi
gzip -c "$snapshot" >"$archive"

if [[ -e "$BACKUP_PATH" || -L "$BACKUP_PATH" ]]; then
	echo "Backup destination already exists; existing backup preserved: $BACKUP_PATH" >&2
	exit 1
fi
# A hard link publishes the completed archive in one operation and fails rather
# than replacing an existing file, including one created after the check above.
ln "$archive" "$BACKUP_DIR/"

echo "$BACKUP_PATH"
