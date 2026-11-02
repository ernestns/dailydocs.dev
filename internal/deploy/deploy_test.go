package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeploymentInstallsExactFetchedCommit(t *testing.T) {
	f := newFixture(t)
	wanted := f.advanceRemote()
	// Existing deployment configuration and build output are intentionally ignored.
	f.write(filepath.Join(f.checkout, ".env"), "private-local-config\n", 0600)
	f.write(filepath.Join(f.checkout, "bin", "old-build"), "old binary\n", 0700)
	f.deploy(true, "EXPECTED_COMMIT="+wanted)
	if got := f.read(f.installed); got != wanted+"\n" {
		t.Fatalf("installed %q, expected fetched commit %s", got, wanted)
	}
	if got := f.git(f.checkout, "rev-parse", "HEAD"); got != wanted {
		t.Fatalf("checkout %s, expected %s", got, wanted)
	}
}

func TestDeploymentRejectsDirtySource(t *testing.T) {
	for _, name := range []string{"tracked", "untracked", "staged"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			file := "tracked.txt"
			if name == "untracked" {
				file = "local-work.txt"
			}
			f.write(filepath.Join(f.checkout, file), "preserve local work\n", 0600)
			if name == "staged" {
				f.git(f.checkout, "add", file)
			}
			before := f.git(f.checkout, "rev-parse", "HEAD")
			f.advanceRemote()
			f.requireMessage(f.deploy(false), "Deployment checkout contains local changes")
			f.assertNotBuiltOrInstalled()
			if f.read(filepath.Join(f.checkout, file)) != "preserve local work\n" || f.git(f.checkout, "rev-parse", "HEAD") != before {
				t.Fatal("deployment altered local work")
			}
		})
	}
}

func TestDeploymentRejectsUnexpectedRemoteCommit(t *testing.T) {
	f := newFixture(t)
	old := f.git(f.checkout, "rev-parse", "HEAD")
	f.advanceRemote()
	f.requireMessage(f.deploy(false, "EXPECTED_COMMIT="+old), "does not match EXPECTED_COMMIT")
	f.assertNotBuiltOrInstalled()
	if got := f.git(f.checkout, "rev-parse", "HEAD"); got != old {
		t.Fatal("mismatched release changed checkout", got)
	}
}

func TestDeploymentRejectsLocalCommits(t *testing.T) {
	for _, diverged := range []bool{false, true} {
		name := "ahead"
		if diverged {
			name = "diverged"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.git(f.checkout, "commit", "--allow-empty", "-m", "Local work")
			before := f.git(f.checkout, "rev-parse", "HEAD")
			if diverged {
				f.advanceRemote()
			}
			f.requireMessage(f.deploy(false), "Local branch has commits absent from the fetched release")
			f.assertNotBuiltOrInstalled()
			if f.git(f.checkout, "rev-parse", "HEAD") != before {
				t.Fatal("deployment overwrote a local commit")
			}
		})
	}
}

func TestDeploymentRejectsSourceChangesDuringBuild(t *testing.T) {
	for _, change := range []string{"dirty", "commit"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			message := "Deployment checkout contains local changes"
			if change == "commit" {
				message = "Checkout changed during the build"
			}
			f.requireMessage(f.deploy(false, "DEPLOY_TEST_CHANGE="+change), message)
			if _, err := os.Stat(f.built); err != nil {
				t.Fatal("build did not run", err)
			}
			if _, err := os.Stat(f.installed); !os.IsNotExist(err) {
				t.Fatal("changed source was installed")
			}
		})
	}
}

func TestDeploymentCanSelectNewRemoteBranch(t *testing.T) {
	f := newFixture(t)
	f.git(f.source, "switch", "-c", "release/reviewed")
	f.git(f.source, "commit", "--allow-empty", "-m", "Reviewed release")
	wanted := f.git(f.source, "rev-parse", "HEAD")
	f.git(f.source, "push", "origin", "release/reviewed")
	f.deploy(true, "BRANCH=release/reviewed", "EXPECTED_COMMIT="+wanted)
	if f.read(f.installed) != wanted+"\n" {
		t.Fatal("wrong branch installed")
	}
}

func TestDeploymentQuotesRemoteConfiguration(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.dir, "deploy's checkout")
	if err := os.Rename(f.checkout, path); err != nil {
		t.Fatal(err)
	}
	f.checkout = path
	f.deploy(true, "REPO_DIR="+path)
	if f.read(f.installed) != f.git(path, "rev-parse", "HEAD")+"\n" {
		t.Fatal("quoted checkout was not installed")
	}
}

type fixture struct {
	t                             *testing.T
	dir, source, checkout, script string
	built, installed              string
	env                           []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	script, err := filepath.Abs("../../scripts/deploy-remote.sh")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t: t, dir: dir, source: filepath.Join(dir, "source"),
		checkout: filepath.Join(dir, "deployment checkout"), script: script,
		built: filepath.Join(dir, "built"), installed: filepath.Join(dir, "installed"),
	}
	bin := filepath.Join(dir, "mock-bin")
	f.env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Deployment test", "GIT_AUTHOR_EMAIL=deploy@example.invalid",
		"GIT_COMMITTER_NAME=Deployment test", "GIT_COMMITTER_EMAIL=deploy@example.invalid",
		"PATH="+bin+":"+os.Getenv("PATH"), "REMOTE=fixture",
		"REPO_DIR="+f.checkout, "BRANCH=main", "EXPECTED_COMMIT=",
		"DEPLOY_TEST_BUILD="+f.built, "DEPLOY_TEST_INSTALL="+f.installed,
	)
	f.write(filepath.Join(bin, "ssh"), "#!/bin/sh\n[ \"$1\" = fixture ] || exit 90\nexec sh -c \"$2\"\n", 0700)
	f.write(filepath.Join(bin, "sudo"), "#!/bin/sh\ngit rev-parse HEAD >\"$DEPLOY_TEST_INSTALL\"\n", 0700)
	f.write(filepath.Join(bin, "curl"), "#!/bin/sh\nprintf 'ok\\n'\n", 0700)
	f.git(dir, "init", "--bare", "--initial-branch=main", "remote.git")
	f.git(dir, "init", "--initial-branch=main", "source")
	f.write(filepath.Join(f.source, ".gitignore"), ".env\n/bin/\n", 0600)
	f.write(filepath.Join(f.source, "tracked.txt"), "initial\n", 0600)
	f.write(filepath.Join(f.source, "scripts", "build.sh"), `#!/bin/sh
set -eu
git rev-parse HEAD >"$DEPLOY_TEST_BUILD"
mkdir -p bin
printf binary >bin/dailydocs
case "${DEPLOY_TEST_CHANGE:-}" in
  dirty) printf changed >>tracked.txt ;;
  commit) git -c commit.gpgsign=false commit --allow-empty -m 'Changed during build' ;;
esac
`, 0700)
	f.git(f.source, "add", ".")
	f.git(f.source, "commit", "-m", "Initial")
	f.git(f.source, "remote", "add", "origin", filepath.Join(dir, "remote.git"))
	f.git(f.source, "push", "-u", "origin", "main")
	f.git(dir, "clone", filepath.Join(dir, "remote.git"), f.checkout)
	return f
}

func (f *fixture) advanceRemote() string {
	f.t.Helper()
	f.write(filepath.Join(f.source, "release.txt"), "remote update\n", 0600)
	f.git(f.source, "add", "release.txt")
	f.git(f.source, "commit", "-m", "Remote update")
	f.git(f.source, "push", "origin", "main")
	return f.git(f.source, "rev-parse", "HEAD")
}

func (f *fixture) deploy(wantSuccess bool, env ...string) string {
	f.t.Helper()
	cmd := exec.Command("bash", f.script)
	cmd.Env = append(append([]string{}, f.env...), env...)
	output, err := cmd.CombinedOutput()
	if (err == nil) != wantSuccess {
		f.t.Fatalf("deploy success=%v, wanted=%v: %v\n%s", err == nil, wantSuccess, err, output)
	}
	return string(output)
}

func (f *fixture) requireMessage(output, message string) {
	f.t.Helper()
	if !strings.Contains(output, message) {
		f.t.Fatalf("expected deployment diagnostic %q, got:\n%s", message, output)
	}
}

func (f *fixture) assertNotBuiltOrInstalled() {
	f.t.Helper()
	for _, path := range []string{f.built, f.installed} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			f.t.Fatalf("deployment unexpectedly created %s", path)
		}
	}
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = f.env
	output, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (f *fixture) write(path, data string, mode os.FileMode) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}
