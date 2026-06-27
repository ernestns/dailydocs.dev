# Deployment Notes

These notes describe the step-zero deployment target: a publicly viewable hello-world Go application behind Caddy.

## Fresh VPS Bootstrap

On a fresh Ubuntu 26 VPS:

```sh
apt update
apt install -y git
git clone https://github.com/ernestns/daily-docs.git
cd daily-docs
./bootstrap.sh
```

Run the bootstrap script as `root`. The application service itself runs as the `dailydocs` system user.

The script defaults to:

```text
domain: dailydocs.dev
app user: dailydocs
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

## Build

```sh
./scripts/build.sh
```

The build output is:

```text
bin/dailydocs
```

## Run Locally

```sh
ADDR=:8080 DB_PATH=data/dailydocs.sqlite ./bin/dailydocs
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
