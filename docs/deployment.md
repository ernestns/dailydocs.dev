# Deployment Notes

These notes describe the step-zero deployment target: a publicly viewable hello-world Go application behind Caddy.

## Fresh VPS Bootstrap

On a fresh Ubuntu 26 VPS:

```sh
apt update
apt install -y git
git clone https://github.com/ernestns/daily-docs.git
cd daily-docs
DEPLOY_SSH_PUBLIC_KEY='ssh-ed25519 ...' ./bootstrap.sh
```

Run the bootstrap script as `root`. The application service itself runs as the `dailydocs` system user.
Routine deploys should use the `deploy` user created by the bootstrap script.

If you saved the key in a shell variable first, export it before running bootstrap:

```sh
export DEPLOY_SSH_PUBLIC_KEY
./bootstrap.sh
```

A value can appear in `echo "$DEPLOY_SSH_PUBLIC_KEY"` without being visible to
`./bootstrap.sh` unless it is exported or passed inline as shown above.

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

On Ubuntu 26, `golang-go` installs Go 1.26.

Override defaults with environment variables:

```sh
DOMAIN=example.com APP_ADDR=127.0.0.1:8081 ./bootstrap.sh
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

The deploy script runs the usual manual sequence on the VPS:

```text
git fetch
git pull --ff-only
./scripts/build.sh
sudo /usr/local/bin/dailydocs-install
curl /health
```

## Run Locally

Local secrets can be stored in `.env`, which is ignored by Git:

```sh
cp .env.example .env
```

Edit `.env` and set:

```text
TAVILY_API_KEY=your-key
```

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

## Validate Links

Check active documentation links and disable links after repeated failures:

```sh
DB_PATH=data/dailydocs.sqlite ./bin/dailydocs validate-links
```

## Backup SQLite

Create a compressed backup using SQLite's backup mechanism:

```sh
DB_PATH=/opt/dailydocs/data/dailydocs.sqlite BACKUP_DIR=/opt/dailydocs/backups ./scripts/backup-sqlite.sh
```

The script prints the backup path.

## Restore SQLite

Restore requires an explicit confirmation environment variable because it replaces the database file:

```sh
RESTORE_CONFIRM=replace-dailydocs-db DB_PATH=/opt/dailydocs/data/dailydocs.sqlite ./scripts/restore-sqlite.sh /opt/dailydocs/backups/dailydocs-YYYYMMDDTHHMMSSZ.sqlite.gz
```

The restore script:

- verifies the backup with `PRAGMA integrity_check`
- stops `dailydocs.service` if it exists
- copies the current database to a `.pre-restore-*` file
- installs the restored database
- starts `dailydocs.service` if it exists

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
