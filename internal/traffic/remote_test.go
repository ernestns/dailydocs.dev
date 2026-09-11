package traffic

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteRecipeUsesStrictSSHAndPrivateLocalHTML(t *testing.T) {
	script, err := filepath.Abs("../../scripts/traffic-report-remote.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, days, remote string
		failSSH, wantFail  bool
	}{
		{"success", "30", "root@dailydocs.dev", false, false},
		{"bad-days", "999", "root@dailydocs.dev", false, true},
		{"bad-host", "30", "-oProxyCommand=bad", false, true},
		{"ssh-failed", "30", "root@dailydocs.dev", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "mock-bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			body := "#!/bin/sh\nprintf '%s\\n' \"$@\" >\"$TRAFFIC_TEST_ARGS\"\nprintf '<!doctype html><html><body>private report</body></html>\\n'\n"
			if tc.failSSH {
				body += "exit 9\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			argsFile := filepath.Join(dir, "args")
			cmd := exec.Command("sh", script, tc.days)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "REMOTE="+tc.remote, "TRAFFIC_OPEN=0", "TRAFFIC_TEST_ARGS="+argsFile)
			output, err := cmd.CombinedOutput()
			if tc.wantFail {
				if err == nil {
					t.Fatalf("expected failure: %s", output)
				}
				files, _ := filepath.Glob(filepath.Join(dir, ".cache/traffic/*/index.html"))
				if len(files) != 0 {
					t.Fatal("failed report left behind")
				}
				return
			}
			if err != nil {
				t.Fatalf("remote report: %v %s", err, output)
			}
			files, _ := filepath.Glob(filepath.Join(dir, ".cache/traffic/*/index.html"))
			if len(files) != 1 {
				t.Fatal("missing local HTML")
			}
			info, err := os.Stat(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("file mode %v", info.Mode())
			}
			for _, p := range []string{filepath.Dir(files[0]), filepath.Join(dir, ".cache/traffic")} {
				info, err := os.Stat(p)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("directory not private: %s", p)
				}
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "ConnectTimeout=10", "root@dailydocs.dev", "DB_PATH=/opt/dailydocs/data/dailydocs.sqlite /opt/dailydocs/bin/dailydocs traffic-report --days 30 --format html"} {
				if !strings.Contains(string(args), want) {
					t.Fatalf("missing SSH argument: %s", want)
				}
			}
			if !strings.Contains(string(output), files[0]) {
				t.Fatalf("no portable file path: %s", output)
			}
		})
	}
}
