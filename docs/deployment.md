# Deployment Notes

These notes describe the single-VPS deployment target: one Go binary, SQLite, systemd, and Caddy.

## Fresh VPS Bootstrap

On a fresh Ubuntu 26 VPS:

```sh
apt update
apt install -y git
git clone https://github.com/ernestns/daily-docs.git
cd daily-docs
DEPLOY_SSH_PUBLIC_KEY='ssh-ed25519 ...' ./scripts/bootstrap-ubuntu.sh
```

Run the bootstrap script as `root`. The application service itself runs as the `dailydocs` system user.
Routine deploys should use the `deploy` user created by the bootstrap script.

If you saved the key in a shell variable first, export it before running bootstrap:

```sh
export DEPLOY_SSH_PUBLIC_KEY
./scripts/bootstrap-ubuntu.sh
```

A value can appear in `echo "$DEPLOY_SSH_PUBLIC_KEY"` without being visible to
`./scripts/bootstrap-ubuntu.sh` unless it is exported or passed inline as shown above.

The script defaults to:

```text
domain: dailydocs.dev
app user: dailydocs
deploy user: deploy
deploy repo: /home/deploy/daily-docs
deploy helper: /usr/local/bin/dailydocs-install
app dir: /opt/dailydocs
database: /opt/dailydocs/data/dailydocs.sqlite
app address: 127.0.0.1:8080
service: dailydocs.service
```

On Ubuntu 26, `golang-go` installs the Go 1.26 series.
The minimum patch version is declared in `go.mod` (currently 1.26.10).
Leave Go's normal automatic toolchain selection enabled so an older installed patch can fetch the required version when building.

Override defaults with environment variables:

```sh
DOMAIN=example.com APP_ADDR=127.0.0.1:8081 ./scripts/bootstrap-ubuntu.sh
```

`DEPLOY_SSH_PUBLIC_KEY` should be the public key from your local machine, for example the
contents of `~/.ssh/id_ed25519.pub`. If it is omitted, bootstrap still creates the
`deploy` user but does not install an SSH key. Root SSH is not changed.

The bootstrap script also installs a root-owned deploy helper and a narrow sudoers rule:

```text
deploy ALL=(root) NOPASSWD: /usr/local/bin/dailydocs-install
```

That helper is the only passwordless root command required for routine deploys.

## Build

The module requires Go 1.26.10 or newer, including the October 2026 standard-library
security fixes. Standard Go toolchain selection downloads the required patch
when the installed Go version is older; it does not replace the host's Go binary.
Run `just pre-deploy` before publishing a release.

```sh
./scripts/build.sh
```

The build output is:

```text
bin/dailydocs
```

## Deploy Existing VPS

After the VPS has been bootstrapped once, deploy from your local machine:

```sh
./scripts/deploy-remote.sh
```

The script defaults to:

```text
ssh host: remote
repo dir: ~/daily-docs
branch: main
service: dailydocs.service
deploy helper: /usr/local/bin/dailydocs-install
health check: http://127.0.0.1:8080/health
```

Override defaults with environment variables:

```sh
REMOTE=dailydocs-vps REPO_DIR=/home/deploy/daily-docs BRANCH=main ./scripts/deploy-remote.sh
```

To require the reviewed commit you just pushed, pass its full commit ID:

```sh
EXPECTED_COMMIT="$(git rev-parse HEAD)" REMOTE=dailydocs-vps REPO_DIR=/home/deploy/daily-docs ./scripts/deploy-remote.sh
```

The deploy script requires a clean deployment checkout, including untracked
files. Ignored local configuration, databases, and build outputs remain allowed.
It fetches the requested branch once, resolves that fetch to a fixed commit,
and stops if `EXPECTED_COMMIT` is provided and does not match. Local commits
absent from the fetched revision also stop deployment; reconcile them without
discarding work before trying again.

The deployment sequence on the VPS is:

```text
require clean checkout
git fetch
verify optional EXPECTED_COMMIT
fast-forward to the fetched commit
./scripts/build.sh
verify clean checkout and exact commit again
sudo /usr/local/bin/dailydocs-install
curl /health
```

The script never resets or force-updates a local branch. Changes made to source
or `HEAD` during the build prevent installation. Deployment guard tests use
temporary Git repositories and a local SSH fixture in `go test ./...`, so they
also run through `just check` without contacting a host.

The health check makes up to five attempts. Each attempt allows two seconds to
connect and five seconds total, so an unresponsive server cannot stall deployment
indefinitely. If all attempts fail, the script exits unsuccessfully and reports
the service status; the installed build remains in place for investigation.

## Run Locally

Local secrets can be stored in `.env`, which is ignored by Git:

```sh
cp .env.example .env
```

Edit `.env` and set:

```text
TAVILY_API_KEY=your-key
OPENAI_API_KEY=your-key
```

API keys are trimmed before configuration is checked. An empty or whitespace-only
Tavily key pauses web generation while preserving saved requests and readings.
An empty or whitespace-only OpenAI key leaves optional planning and review off;
configured Tavily search uses deterministic filtering and ranking.

Run local commands through the env wrapper:

```sh
./scripts/with-env.sh ./bin/dailydocs
```

Smoke checks:

```sh
curl http://localhost:8080/
curl http://localhost:8080/health
```

`/health` should return:

```text
ok
```

## Import Seed File

Seed files define a topic and its documentation links:

```yaml
topic: sqlite
name: SQLite
pages:
  - title: Write-Ahead Logging
    url: https://sqlite.org/wal.html
    source: SQLite Documentation
    official: true
    estimated_minutes: 12
```

Import a file:

```sh
DB_PATH=data/dailydocs.sqlite ./bin/dailydocs import-file path/to/sqlite.yaml
```

Each seed file must contain exactly one YAML document for one topic. Extra
documents or malformed trailing content are rejected before any catalog changes,
so a partially read file cannot silently replace the existing active page list.

## Validate Links

Check active documentation links and disable links after repeated failures:

```sh
DB_PATH=data/dailydocs.sqlite ./bin/dailydocs validate-links
```

## Backup SQLite

Application connections enforce foreign keys and wait up to five seconds for
brief writer contention. Transactions that may create a daily assignment acquire
the writer lock before reading, while saved readings use read-only transactions.
This preserves one shared assignment when several visitors arrive together.

Create a compressed backup using SQLite's backup mechanism:

```sh
DB_PATH=/opt/dailydocs/data/dailydocs.sqlite BACKUP_DIR=/opt/dailydocs/backups ./scripts/backup-sqlite.sh
```

The script prints the completed backup path, named
`dailydocs-<UTC timestamp>.<unique suffix>.sqlite.gz`. Use that printed path when
restoring. Concurrent backups, including calls within the same second, keep
separate recovery points.

SQLite creates a coherent snapshot, including committed WAL data. The script
requires that snapshot to pass `PRAGMA integrity_check` before compression;
corrupt pages or inconsistent indexes stop the backup with a nonzero exit.
Verification and compression run in a private staging directory on the backup
filesystem. The completed archive is published atomically without replacing an
existing destination. Snapshot, verification, compression, or publication failures
preserve earlier backups and remove the script's staging files. Archives use mode
`0600`, and newly created
backup directories use mode `0700` regardless of the caller's umask.

## Restore SQLite

Restore requires an explicit confirmation environment variable because it replaces the database contents:

```sh
RESTORE_CONFIRM=replace-dailydocs-db DB_PATH=/opt/dailydocs/data/dailydocs.sqlite ./scripts/restore-sqlite.sh /opt/dailydocs/backups/dailydocs-YYYYMMDDTHHMMSSZ.ABC123.sqlite.gz
```

The destination database must already exist with the application user's ownership
and intended permissions. On a fresh host, provision it before restoring; the
script refuses to silently create a root-owned database that the service cannot
use.

The restore script:

- verifies the backup with `PRAGMA integrity_check` and requires at least one
  user table before inspecting or stopping a service; empty files and SQLite
  files without user tables are rejected, while tables with no rows are
  allowed
- stops `dailydocs.service` only if it is active, and rejects uncertain service states
- saves a private, uniquely named `.pre-restore-*` snapshot using SQLite's backup
  mechanism, including any committed WAL data left after a crash
- verifies that snapshot and keeps it self-contained in rollback journal mode
- restores through SQLite rather than replacing the database file, preserving
  existing ownership and permissions and preventing old WAL data from returning
- resumes only the service it stopped, including after a failed restore attempt;
  a previously inactive service stays inactive

The script records an active service's stop before waiting for the stop command,
so interruption immediately after a successful stop still resumes that service.
An ordinary stop failure exits without attempting restoration or a restart.

The snapshot path is printed before restoration begins and remains available if
restoration fails. Restart failures produce a nonzero exit and an explicit error.
Restore integration tests use temporary SQLite databases, a real crashed writer,
and a fake service controller; they never restore production or manage real services.

## Systemd

Example service:

```ini
[Unit]
Description=DailyDocs web application
After=network.target

[Service]
Type=simple
User=dailydocs
Group=dailydocs
WorkingDirectory=/opt/dailydocs
Environment=ADDR=127.0.0.1:8080
ExecStart=/opt/dailydocs/bin/dailydocs
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

## Caddy

Example Caddy site:

```caddyfile
dailydocs.dev {
	reverse_proxy 127.0.0.1:8080
}
```

## Definition of Done

- `https://dailydocs.dev` loads publicly.
- `https://dailydocs.dev/health` returns `ok`.
- Caddy terminates TLS and proxies to the Go app.
- The app runs under systemd or an equivalent supervisor.
