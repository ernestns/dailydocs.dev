package deploy

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRestoreReplacesCrashWALAndKeepsCompleteSafetySnapshot(t *testing.T) {
	f := newRestoreFixture(t)
	f.crashAfterCommit()
	before, err := os.Stat(f.database)
	if err != nil {
		t.Fatal(err)
	}
	f.restore(true)
	f.requireRows(f.database, "restored reading")
	snapshots := f.snapshots()
	if len(snapshots) != 1 {
		t.Fatalf("expected one coherent safety snapshot, got %v", snapshots)
	}
	f.requireRows(snapshots[0], "checkpointed reading", "committed before crash")
	portable := filepath.Join(t.TempDir(), "portable-snapshot.sqlite")
	contents, err := os.ReadFile(snapshots[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(portable, contents, 0600); err != nil {
		t.Fatal(err)
	}
	f.requireRows(portable, "checkpointed reading", "committed before crash")
	after, err := os.Stat(f.database)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0600 {
		t.Fatal("restore replaced the database file or changed its existing permissions", after.Mode())
	}
	snapshotInfo, err := os.Stat(snapshots[0])
	if err != nil || snapshotInfo.Mode().Perm() != 0600 {
		t.Fatal("safety snapshot is not private", err)
	}
	f.requireServiceActions("show", "stop", "start")
}

func TestRestorePreservesInactiveServiceState(t *testing.T) {
	f := newRestoreFixture(t)
	f.restore(true, "RESTORE_TEST_SERVICE_STATE=inactive")
	f.requireRows(f.database, "restored reading")
	f.requireServiceActions("show")
}

func TestRestoreFailureKeepsCurrentDataAndResumesStoppedService(t *testing.T) {
	for _, stage := range []string{"backup", "restore"} {
		t.Run(stage, func(t *testing.T) {
			f := newRestoreFixture(t)
			f.restore(false, "RESTORE_TEST_FAIL="+stage)
			f.requireRows(f.database, "checkpointed reading")
			f.requireServiceActions("show", "stop", "start")
			snapshots := f.snapshots()
			if stage == "backup" && len(snapshots) != 0 {
				t.Fatal("failed snapshot left a misleading backup", snapshots)
			}
			if stage == "restore" {
				if len(snapshots) != 1 {
					t.Fatal("complete safety snapshot was not retained", snapshots)
				}
				f.requireRows(snapshots[0], "checkpointed reading")
			}
		})
	}
}

func TestRestoreRejectsBeforeChangingDataOrService(t *testing.T) {
	for _, kind := range []string{"confirmation", "corrupt backup", "invalid SQLite", "schema lookup failure", "service lookup failure", "unknown service state", "stop failure"} {
		t.Run(kind, func(t *testing.T) {
			f := newRestoreFixture(t)
			var env, actions []string
			switch kind {
			case "confirmation":
				env = []string{"RESTORE_CONFIRM="}
			case "corrupt backup":
				if err := os.WriteFile(f.backup, []byte("not a gzip or SQLite file"), 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid SQLite":
				f.backup = filepath.Join(filepath.Dir(f.database), "invalid.sqlite")
				if err := os.WriteFile(f.backup, []byte("not a SQLite file"), 0600); err != nil {
					t.Fatal(err)
				}
			case "schema lookup failure":
				env = []string{"RESTORE_TEST_FAIL=schema"}
			case "unknown service state":
				env, actions = []string{"RESTORE_TEST_SERVICE_STATE=activating"}, []string{"show"}
			case "service lookup failure":
				env, actions = []string{"RESTORE_TEST_FAIL=show"}, []string{"show"}
			case "stop failure":
				env, actions = []string{"RESTORE_TEST_FAIL=stop"}, []string{"show", "stop"}
			}
			f.restore(false, env...)
			f.requireRows(f.database, "checkpointed reading")
			f.requireServiceActions(actions...)
			if len(f.snapshots()) != 0 {
				t.Fatal("rejected restore created a snapshot")
			}
		})
	}
}

func TestRestoreRequiresProvisionedDestination(t *testing.T) {
	f := newRestoreFixture(t)
	if err := os.Remove(f.database); err != nil {
		t.Fatal(err)
	}
	output := f.restore(false)
	if !strings.Contains(output, "application user's ownership and permissions") {
		t.Fatal("missing destination lacks actionable ownership guidance", output)
	}
	if _, err := os.Stat(f.database); !os.IsNotExist(err) {
		t.Fatal("restore silently created a caller-owned database", err)
	}
	f.requireServiceActions()
}

func TestRestoreRejectsEmptyBackupsBeforeServiceOrDataChanges(t *testing.T) {
	for _, kind := range []string{"zero-byte", "empty-gzip", "empty-sqlite", "internal-tables-only"} {
		t.Run(kind, func(t *testing.T) {
			f := newRestoreFixture(t)
			f.sql(f.database, "CREATE TABLE daily_readings(reading_date TEXT,title TEXT); INSERT INTO daily_readings VALUES('2026-01-01','checkpointed reading');")
			before, err := os.ReadFile(f.database)
			if err != nil {
				t.Fatal(err)
			}
			f.backup = filepath.Join(filepath.Dir(f.database), "empty.sqlite")
			switch kind {
			case "zero-byte":
				if err := os.WriteFile(f.backup, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "empty-gzip":
				f.backup += ".gz"
				file, err := os.Create(f.backup)
				if err != nil {
					t.Fatal(err)
				}
				compressed := gzip.NewWriter(file)
				if err := compressed.Close(); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			case "empty-sqlite":
				f.sql(f.backup, "PRAGMA user_version=1;")
			case "internal-tables-only":
				f.sql(f.backup, "CREATE TABLE removed(id INTEGER PRIMARY KEY AUTOINCREMENT); DROP TABLE removed;")
			}
			output := f.restore(false)
			if !strings.Contains(output, "Backup contains no user tables") {
				t.Fatal("empty backup rejection lacks a useful diagnostic", output)
			}
			f.requireServiceActions()
			after, err := os.ReadFile(f.database)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected restore changed destination bytes", err)
			}
			f.requireRows(f.database, "checkpointed reading")
			if got := f.sql(f.database, "SELECT reading_date,title FROM daily_readings;"); got != "2026-01-01|checkpointed reading" {
				t.Fatal("rejected restore changed saved reading history", got)
			}
			if len(f.snapshots()) != 0 {
				t.Fatal("rejected empty backup reached the safety snapshot stage")
			}
		})
	}
}

func TestRestoreAcceptsUserTablesWithNoData(t *testing.T) {
	for _, table := range []string{"readings", "sqlitex_readings"} {
		t.Run(table, func(t *testing.T) {
			f := newRestoreFixture(t)
			f.backup = filepath.Join(filepath.Dir(f.database), "empty-data.sqlite")
			f.sql(f.backup, "CREATE TABLE "+table+"(title TEXT);")
			f.restore(true)
			if got := f.sql(f.database, "SELECT count(*) FROM "+table+";"); got != "0" {
				t.Fatal("valid schema with no data was not restored", got)
			}
			f.requireServiceActions("show", "stop", "start")
			snapshots := f.snapshots()
			if len(snapshots) != 1 {
				t.Fatal("valid restore did not retain its safety snapshot", snapshots)
			}
			f.requireRows(snapshots[0], "checkpointed reading")
		})
	}
}

func TestRestoreReportsRestartFailureWithoutRetrying(t *testing.T) {
	f := newRestoreFixture(t)
	output := f.restore(false, "RESTORE_TEST_FAIL=start")
	if !strings.Contains(output, "Unable to restart") {
		t.Fatal("restart failure was not explained", output)
	}
	f.requireRows(f.database, "restored reading")
	f.requireServiceActions("show", "stop", "start")
}

// Exit without closing SQLite to leave a real committed WAL, as a crashed
// process would. The subprocess is restricted to this fixture's temp database.
func TestRestoreFixtureCrashWriter(t *testing.T) {
	database := os.Getenv("DAILYDOCS_RESTORE_FIXTURE_CRASH_DB")
	if database == "" {
		return
	}
	conn, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO readings VALUES('committed before crash');`); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

type restoreFixture struct {
	t                        *testing.T
	database, backup, script string
	serviceLog, sqlite       string
	env                      []string
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("restore integration tests require the sqlite3 command")
	}
	dir := t.TempDir()
	script, err := filepath.Abs("../../scripts/restore-sqlite.sh")
	if err != nil {
		t.Fatal(err)
	}
	f := &restoreFixture{t: t, database: filepath.Join(dir, "current 'quoted\"\\ database.sqlite"), backup: filepath.Join(dir, "backup.sqlite.gz"), script: script, serviceLog: filepath.Join(dir, "service.log"), sqlite: sqlite}
	f.sql(f.database, "CREATE TABLE readings(title TEXT); INSERT INTO readings VALUES('checkpointed reading');")
	if err := os.Chmod(f.database, 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source.sqlite")
	f.sql(source, "CREATE TABLE readings(title TEXT); INSERT INTO readings VALUES('restored reading');")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := os.Create(f.backup)
	if err != nil {
		t.Fatal(err)
	}
	zipper := gzip.NewWriter(compressed)
	if _, err := zipper.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "mock-bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"systemctl": `#!/bin/sh
set -eu
printf '%s\n' "$1" >> "$RESTORE_TEST_SERVICE_LOG"
case "$1" in
  show) [ "${RESTORE_TEST_FAIL:-}" != show ] || exit 74; printf '%s\n' "$RESTORE_TEST_SERVICE_STATE" ;;
  stop|start) [ "${RESTORE_TEST_FAIL:-}" != "$1" ] ;;
  *) exit 90 ;;
esac
`,
		"sqlite3": `#!/bin/sh
set -eu
case "${2:-}" in
  '.backup '*) [ "${RESTORE_TEST_FAIL:-}" != backup ] || exit 70 ;;
  '.restore '*) [ "${RESTORE_TEST_FAIL:-}" != restore ] || exit 71 ;;
  *sqlite_master*) [ "${RESTORE_TEST_FAIL:-}" != schema ] || exit 72 ;;
esac
exec "$RESTORE_TEST_SQLITE3" "$@"
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "DB_PATH="+f.database, "RESTORE_CONFIRM=replace-dailydocs-db", "APP_NAME=dailydocs-fixture", "RESTORE_TEST_SERVICE_LOG="+f.serviceLog, "RESTORE_TEST_SQLITE3="+sqlite, "RESTORE_TEST_SERVICE_STATE=active", "RESTORE_TEST_FAIL=")
	return f
}

func (f *restoreFixture) sql(database, query string) string {
	f.t.Helper()
	output, err := exec.Command(f.sqlite, database, query).CombinedOutput()
	if err != nil {
		f.t.Fatal("SQLite fixture failed", err, string(output))
	}
	return strings.TrimSpace(string(output))
}

func (f *restoreFixture) requireRows(database string, expected ...string) {
	f.t.Helper()
	if got := f.sql(database, "SELECT title FROM readings ORDER BY rowid;"); got != strings.Join(expected, "\n") {
		f.t.Fatalf("database contents changed: got %q want %q", got, expected)
	}
}

func (f *restoreFixture) crashAfterCommit() {
	f.t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestoreFixtureCrashWriter$")
	cmd.Env = append(os.Environ(), "DAILYDOCS_RESTORE_FIXTURE_CRASH_DB="+f.database)
	if output, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatal("crash fixture failed", err, string(output))
	}
	if _, err := os.Stat(f.database + "-wal"); err != nil {
		f.t.Fatal("crash fixture did not leave its committed WAL", err)
	}
}

func (f *restoreFixture) restore(success bool, extraEnv ...string) string {
	f.t.Helper()
	cmd := exec.Command("bash", f.script, f.backup)
	cmd.Env = append(append([]string{}, f.env...), extraEnv...)
	output, err := cmd.CombinedOutput()
	if (err == nil) != success {
		f.t.Fatalf("restore success=%v, err=%v: %s", success, err, output)
	}
	return string(output)
}

func (f *restoreFixture) snapshots() []string {
	f.t.Helper()
	files, err := os.ReadDir(filepath.Dir(f.database))
	if err != nil {
		f.t.Fatal(err)
	}
	var paths []string
	for _, file := range files {
		if strings.HasPrefix(file.Name(), filepath.Base(f.database)+".pre-restore-") {
			paths = append(paths, filepath.Join(filepath.Dir(f.database), file.Name()))
		}
	}
	return paths
}

func (f *restoreFixture) requireServiceActions(expected ...string) {
	f.t.Helper()
	contents, err := os.ReadFile(f.serviceLog)
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	if got := strings.TrimSpace(string(contents)); got != strings.Join(expected, "\n") {
		f.t.Fatalf("unexpected service actions: got %q want %q", got, expected)
	}
}
