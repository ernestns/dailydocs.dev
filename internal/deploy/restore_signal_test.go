package deploy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreSignalAfterSuccessfulStopResumesService(t *testing.T) {
	for _, signal := range []string{"TERM", "HUP"} {
		t.Run(signal, func(t *testing.T) {
			f := newRestoreFixture(t)
			before, err := os.ReadFile(f.database)
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(t.TempDir(), "service-state")
			if err := os.WriteFile(state, []byte("active\n"), 0600); err != nil {
				t.Fatal(err)
			}
			payloadDir := t.TempDir()
			// The fake controller signals only its immediate, disposable restore
			// shell after recording a successful stop. No real service is touched.
			controller := `#!/bin/sh
set -eu
printf '%s\n' "$1" >> "$RESTORE_TEST_SERVICE_LOG"
case "$1" in
  show) cat "$RESTORE_TEST_STATE_FILE" ;;
  stop)
    printf 'inactive\n' > "$RESTORE_TEST_STATE_FILE"
    kill -s "$RESTORE_TEST_INTERRUPT_SIGNAL" "$PPID"
    exit 0
    ;;
  start) printf 'active\n' > "$RESTORE_TEST_STATE_FILE" ;;
  *) exit 90 ;;
esac
`
			if err := os.WriteFile(filepath.Join(filepath.Dir(f.database), "mock-bin", "systemctl"), []byte(controller), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", f.script, f.backup)
			command.Env = append(f.env, "TMPDIR="+payloadDir, "RESTORE_TEST_STATE_FILE="+state, "RESTORE_TEST_INTERRUPT_SIGNAL="+signal)
			output, err := command.CombinedOutput()
			if err == nil || strings.Contains(string(output), "Restored ") {
				t.Fatal("interrupted restore claimed success", err, string(output))
			}
			current, err := os.ReadFile(state)
			if err != nil || strings.TrimSpace(string(current)) != "active" {
				t.Fatal("successfully stopped service was not resumed", string(current), err)
			}
			f.requireServiceActions("show", "stop", "start")
			after, err := os.ReadFile(f.database)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("interruption changed database bytes", err)
			}
			f.requireRows(f.database, "checkpointed reading")
			if len(f.snapshots()) != 0 {
				t.Fatal("interruption advanced to the safety snapshot stage")
			}
			payloads, err := os.ReadDir(payloadDir)
			if err != nil || len(payloads) != 0 {
				t.Fatal("interruption left temporary restore payloads", payloads, err)
			}
		})
	}
}
