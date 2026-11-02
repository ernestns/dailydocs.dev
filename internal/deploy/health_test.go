package deploy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestDeploymentRetriesStalledHealthResponse(t *testing.T) {
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("health integration test requires curl")
	}
	f := newFixture(t)
	var calls atomic.Int32
	first := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(first)
			// Accept the connection but never send response headers. The real
			// curl deadline must cancel this request before the script retries.
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer server.Close()
	f.write(filepath.Join(f.dir, "mock-bin", "curl"), "#!/bin/sh\nexec \"$DEPLOY_TEST_REAL_CURL\" --noproxy '*' \"$@\"\n", 0700)
	f.write(filepath.Join(f.dir, "mock-bin", "systemctl"), "#!/bin/sh\nexit 0\n", 0700)
	cmd := exec.Command("bash", f.script)
	cmd.Env = append(append([]string{}, f.env...), "HEALTH_URL="+server.URL, "DEPLOY_TEST_REAL_CURL="+realCurl)
	// Only fixture shells, fake SSH and local curl belong to this process group.
	// The watchdog must clean all of them up if a regression removes the bound.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	defer func() {
		if !finished {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}()
	select {
	case <-first:
	case err := <-done:
		finished = true
		t.Fatalf("deployment exited before health probe: %v %s", err, output.String())
	case <-time.After(5 * time.Second):
		t.Fatal("fixture did not reach health check")
	}
	select {
	case err := <-done:
		finished = true
		if err != nil || calls.Load() != 2 || !strings.Contains(output.String(), "Deploy complete.") {
			t.Fatalf("health retry did not recover: calls=%d err=%v output=%s", calls.Load(), err, output.String())
		}
	case <-time.After(9 * time.Second):
		t.Fatalf("stalled health probe did not time out and retry: calls=%d", calls.Load())
	}
}

func TestDeploymentHealthFailureExhaustsBoundedAttempts(t *testing.T) {
	for _, failureCode := range []string{"22", "28"} {
		t.Run("curl-exit-"+failureCode, func(t *testing.T) {
			f := newFixture(t)
			healthLog := filepath.Join(f.dir, "health.log")
			serviceLog := filepath.Join(f.dir, "service.log")
			f.write(filepath.Join(f.dir, "mock-bin", "curl"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$DEPLOY_TEST_HEALTH_LOG\"\nexit "+failureCode+"\n", 0700)
			f.write(filepath.Join(f.dir, "mock-bin", "sleep"), "#!/bin/sh\nexit 0\n", 0700)
			f.write(filepath.Join(f.dir, "mock-bin", "systemctl"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$DEPLOY_TEST_SERVICE_LOG\"\nprintf 'fixture service status\\n'\n", 0700)
			const healthURL = "http://127.0.0.1/fixture-health"
			output := f.deploy(false, "HEALTH_URL="+healthURL, "APP_NAME=dailydocs-health-fixture", "DEPLOY_TEST_HEALTH_LOG="+healthLog, "DEPLOY_TEST_SERVICE_LOG="+serviceLog)
			f.requireMessage(output, "Health check failed after restart.")
			f.requireMessage(output, "fixture service status")
			if strings.Contains(output, "Deploy complete.") {
				t.Fatal("failed health check reported success")
			}
			attempts := strings.Split(strings.TrimSpace(f.read(healthLog)), "\n")
			if len(attempts) != 5 {
				t.Fatalf("health failure made %d attempts, want 5", len(attempts))
			}
			want := []string{"--fail", "--silent", "--show-error", "--connect-timeout", "2", "--max-time", "5", healthURL}
			for _, attempt := range attempts {
				if !reflect.DeepEqual(strings.Fields(attempt), want) {
					t.Fatal("health probe changed URL or lost its deadlines", attempt)
				}
			}
			if got := strings.TrimSpace(f.read(serviceLog)); got != "status dailydocs-health-fixture.service --no-pager" {
				t.Fatal("failure handling changed service operations", got)
			}
			if f.read(f.installed) != f.read(f.built) {
				t.Fatal("health failure changed the installed build")
			}
		})
	}
}
