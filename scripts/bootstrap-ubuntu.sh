#!/usr/bin/env bash
set -euo pipefail

APP_NAME="${APP_NAME:-dailydocs}"
BINARY_NAME="${BINARY_NAME:-dailydocs}"
APP_USER="${APP_USER:-dailydocs}"
APP_GROUP="${APP_GROUP:-dailydocs}"
APP_DIR="${APP_DIR:-/opt/dailydocs}"
DATA_DIR="${DATA_DIR:-$APP_DIR/data}"
DB_PATH="${DB_PATH:-$DATA_DIR/dailydocs.sqlite}"
APP_ADDR="${APP_ADDR:-127.0.0.1:8080}"
DOMAIN="${DOMAIN:-dailydocs.dev}"

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ "$(id -u)" -ne 0 ]]; then
	echo "Run this script as root." >&2
	exit 1
fi

if ! command -v apt-get >/dev/null 2>&1; then
	echo "This bootstrap script is intended for Ubuntu/Debian systems with apt-get." >&2
	exit 1
fi

echo "==> Installing system packages"
apt-get update
env DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl git golang-go caddy

echo "==> Creating application user"
if ! getent group "$APP_GROUP" >/dev/null 2>&1; then
	groupadd --system "$APP_GROUP"
fi

if ! id "$APP_USER" >/dev/null 2>&1; then
	useradd --system --gid "$APP_GROUP" --home "$APP_DIR" --shell /usr/sbin/nologin "$APP_USER"
fi

echo "==> Preparing application directory"
mkdir -p "$APP_DIR/bin" "$DATA_DIR"
chown -R "$APP_USER:$APP_GROUP" "$DATA_DIR"

echo "==> Building application"
"$REPO_DIR/scripts/build.sh"
install -m 0755 "$REPO_DIR/bin/$BINARY_NAME" "$APP_DIR/bin/$BINARY_NAME"

echo "==> Writing systemd service"
tee "/etc/systemd/system/$APP_NAME.service" >/dev/null <<EOF
[Unit]
Description=DailyDocs web application
After=network.target

[Service]
Type=simple
User=$APP_USER
Group=$APP_GROUP
WorkingDirectory=$APP_DIR
Environment=ADDR=$APP_ADDR
Environment=DB_PATH=$DB_PATH
ExecStart=$APP_DIR/bin/$BINARY_NAME
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

echo "==> Writing Caddy site"
mkdir -p /etc/caddy
cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.$(date +%Y%m%d%H%M%S).bak" 2>/dev/null || true
tee /etc/caddy/Caddyfile >/dev/null <<EOF
$DOMAIN {
	reverse_proxy $APP_ADDR
}
EOF

echo "==> Starting services"
systemctl daemon-reload
systemctl enable "$APP_NAME.service"
systemctl restart "$APP_NAME.service"
systemctl enable --now caddy
caddy validate --config /etc/caddy/Caddyfile
systemctl reload caddy

echo "==> Local smoke check"
curl --fail --silent --show-error "http://$APP_ADDR/health"
echo

cat <<EOF

Bootstrap complete.

Application:
  systemctl status $APP_NAME.service
  journalctl -u $APP_NAME.service -f

Public checks:
  https://$DOMAIN
  https://$DOMAIN/health
EOF
