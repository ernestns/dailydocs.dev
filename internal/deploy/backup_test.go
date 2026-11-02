package deploy

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSameSecondBackupsPreserveBothRecoveryPoints(t *testing.T) {
	f := newBackupFixture(t)
	first := f.backup(true)
	firstBytes := f.read(first)
	f.sql("INSERT INTO readings VALUES('later reading');")
	second := f.backup(true)
	if first == second || !bytes.Equal(firstBytes, f.read(first)) {
		t.Fatal("new backup replaced an earlier recovery point", first, second)
	}
	f.requireArchiveRows(first, "checkpointed reading")
	f.requireArchiveRows(second, "checkpointed reading", "later reading")
	f.requirePublishedCount(2)
	for _, path := range []string{first, second} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("backup is not private under caller umask 022", path, err)
		}
	}
	info, err := os.Stat(f.directory)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("new backup directory is not private", err)
	}
}

func TestConcurrentSameSecondBackupsUseDistinctCompleteArchives(t *testing.T) {
	f := newBackupFixture(t)
	type outcome struct {
		output string
		err    error
	}
	const attempts = 4
	done := make(chan outcome, attempts)
	start := make(chan struct{})
	for range attempts {
		go func() {
			<-start
			output, err := f.command().CombinedOutput()
			done <- outcome{strings.TrimSpace(string(output)), err}
		}()
	}
	close(start)
	paths := map[string]bool{}
	for range attempts {
		got := <-done
		if got.err != nil || paths[got.output] {
			t.Fatal("concurrent backup failed or reused a name", got.output, got.err)
		}
		paths[got.output] = true
		f.requireArchiveRows(got.output, "checkpointed reading")
	}
	f.requirePublishedCount(attempts)
}

func TestFailedBackupPreservesExistingArchiveAndRemovesStaging(t *testing.T) {
	for _, stage := range []string{"sqlite", "gzip", "publish"} {
		t.Run(stage, func(t *testing.T) {
			f := newBackupFixture(t)
			existing := f.backup(true)
			before := f.read(existing)
			f.sql("INSERT INTO readings VALUES('later reading');")
			f.backup(false, "BACKUP_TEST_FAIL="+stage)
			if !bytes.Equal(before, f.read(existing)) {
				t.Fatal("failed backup changed the earlier archive")
			}
			f.requireArchiveRows(existing, "checkpointed reading")
			f.requirePublishedCount(1)
		})
	}
}

func TestBackupPublicationDoesNotClobberConcurrentDestination(t *testing.T) {
	for _, kind := range []string{"collision", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newBackupFixture(t)
			existing := f.backup(true)
			before := f.read(existing)
			// Create a destination after the advisory check but before the real
			// hard-link call. It must be a collision, never a directory target.
			f.backup(false, "BACKUP_TEST_FAIL="+kind, "BACKUP_TEST_EXISTING_ARCHIVE="+existing)
			collisionPath := strings.TrimSpace(string(f.read(f.collisionLog)))
			if !bytes.Equal(before, f.read(existing)) {
				t.Fatal("publication changed an earlier archive")
			}
			if kind == "collision" {
				if !bytes.Equal(before, f.read(collisionPath)) {
					t.Fatal("publication replaced the colliding archive")
				}
				f.requireArchiveRows(collisionPath, "checkpointed reading")
			} else {
				files, err := os.ReadDir(collisionPath)
				if err != nil || len(files) != 0 {
					t.Fatal("publication wrote inside a colliding directory", files, err)
				}
			}
			files, err := os.ReadDir(f.directory)
			if err != nil || len(files) != 2 {
				t.Fatal("publication left staging files or removed the collision", files, err)
			}
		})
	}
}

func TestBackupContainsCommittedWALWithoutSidecars(t *testing.T) {
	f := newBackupFixture(t)
	// Reuse the real crashed-writer fixture; it deliberately skips Close to
	// leave a committed WAL rather than simulating its bytes.
	crashed := restoreFixture{t: t, database: f.database}
	crashed.crashAfterCommit()
	archive := f.backup(true)
	f.requireArchiveRows(archive, "checkpointed reading", "committed before crash")
	f.requirePublishedCount(1)
}

type backupFixture struct {
	t                                   *testing.T
	database, directory, script, sqlite string
	collisionLog                        string
	env                                 []string
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("backup integration tests require the sqlite3 command")
	}
	gzipPath, err := exec.LookPath("gzip")
	if err != nil {
		t.Fatal(err)
	}
	linkPath, err := exec.LookPath("ln")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../scripts/backup-sqlite.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := &backupFixture{t: t, database: filepath.Join(dir, "current.sqlite"), directory: filepath.Join(dir, "backup 'quoted\"\\ directory"), script: script, sqlite: sqlite, collisionLog: filepath.Join(dir, "collision.log")}
	f.sql("CREATE TABLE readings(title TEXT); INSERT INTO readings VALUES('checkpointed reading');")
	bin := filepath.Join(dir, "mock-bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"date": "#!/bin/sh\nprintf '20261102T063000Z\\n'\n",
		"sqlite3": `#!/bin/sh
set -eu
if [ "${BACKUP_TEST_FAIL:-}" = sqlite ]; then exit 70; fi
exec "$BACKUP_TEST_SQLITE3" "$@"
`,
		"gzip": `#!/bin/sh
set -eu
if [ "${BACKUP_TEST_FAIL:-}" = gzip ]; then
  printf 'partial compressed bytes'
  exit 42
fi
exec "$BACKUP_TEST_GZIP" "$@"
`,
		"ln": `#!/bin/sh
set -eu
case "${BACKUP_TEST_FAIL:-}" in
  publish) exit 72 ;;
  collision|directory|symlink)
    destination="${2%/}/${1##*/}"
    case "$BACKUP_TEST_FAIL" in
      collision) cp "$BACKUP_TEST_EXISTING_ARCHIVE" "$destination" ;;
      directory) mkdir "$destination" ;;
      symlink)
        mkdir "$BACKUP_TEST_SYMLINK_TARGET"
        "$BACKUP_TEST_LN" -s "$BACKUP_TEST_SYMLINK_TARGET" "$destination"
        ;;
    esac
    printf '%s\n' "$destination" > "$BACKUP_TEST_COLLISION_LOG"
    ;;
esac
exec "$BACKUP_TEST_LN" "$@"
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "DB_PATH="+f.database, "BACKUP_DIR="+f.directory, "BACKUP_TEST_FAIL=", "BACKUP_TEST_SQLITE3="+sqlite, "BACKUP_TEST_GZIP="+gzipPath, "BACKUP_TEST_LN="+linkPath, "BACKUP_TEST_COLLISION_LOG="+f.collisionLog, "BACKUP_TEST_SYMLINK_TARGET="+filepath.Join(dir, "symlink target"))
	return f
}

func (f *backupFixture) command(extraEnv ...string) *exec.Cmd {
	cmd := exec.Command("bash", "-c", `umask 022; exec bash "$1"`, "backup-fixture", f.script)
	cmd.Env = append(append([]string{}, f.env...), extraEnv...)
	return cmd
}

func (f *backupFixture) backup(success bool, extraEnv ...string) string {
	f.t.Helper()
	output, err := f.command(extraEnv...).CombinedOutput()
	if (err == nil) != success {
		f.t.Fatalf("backup success=%v, err=%v: %s", success, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (f *backupFixture) sql(query string) {
	f.t.Helper()
	if output, err := exec.Command(f.sqlite, f.database, query).CombinedOutput(); err != nil {
		f.t.Fatal("SQLite fixture failed", err, string(output))
	}
}

func (f *backupFixture) read(path string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *backupFixture) requireArchiveRows(path string, expected ...string) {
	f.t.Helper()
	zipper, err := gzip.NewReader(bytes.NewReader(f.read(path)))
	if err != nil {
		f.t.Fatal("backup is not a complete gzip archive", err)
	}
	data, err := io.ReadAll(zipper)
	if err != nil {
		f.t.Fatal("backup gzip contents are incomplete", err)
	}
	if err := zipper.Close(); err != nil {
		f.t.Fatal(err)
	}
	// Restore only the compressed main database into a separate directory:
	// no source or staging sidecars are available to hide an incomplete copy.
	snapshot := filepath.Join(f.t.TempDir(), "snapshot.sqlite")
	if err := os.WriteFile(snapshot, data, 0600); err != nil {
		f.t.Fatal(err)
	}
	output, err := exec.Command(f.sqlite, snapshot, "SELECT title FROM readings ORDER BY rowid;").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != strings.Join(expected, "\n") {
		f.t.Fatalf("backup contents: got %q want %q err=%v", output, expected, err)
	}
}

func (f *backupFixture) requirePublishedCount(expected int) {
	f.t.Helper()
	files, err := os.ReadDir(f.directory)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(files) != expected {
		f.t.Fatalf("backup left unexpected or missing files: %v", files)
	}
	for _, file := range files {
		if file.IsDir() || !strings.HasPrefix(file.Name(), "dailydocs-20261102T063000Z.") || !strings.HasSuffix(file.Name(), ".sqlite.gz") {
			f.t.Fatal("private staging or unexpected output remained", file.Name())
		}
	}
}
