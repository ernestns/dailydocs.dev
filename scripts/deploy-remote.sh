#!/usr/bin/env bash
set -euo pipefail

REMOTE="${REMOTE:-remote}"
REPO_DIR="${REPO_DIR:-~/daily-docs}"
BRANCH="${BRANCH:-main}"
APP_NAME="${APP_NAME:-dailydocs}"
DEPLOY_HELPER="${DEPLOY_HELPER:-/usr/local/bin/$APP_NAME-install}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/health}"
EXPECTED_COMMIT="${EXPECTED_COMMIT:-}"

# Quote values for the remote login shell, including paths containing spaces.
shell_quote() {
	local escaped_quote="'\\''"
	printf "'%s'" "${1//\'/$escaped_quote}"
}

remote_command="REPO_DIR=$(shell_quote "$REPO_DIR") BRANCH=$(shell_quote "$BRANCH") APP_NAME=$(shell_quote "$APP_NAME") DEPLOY_HELPER=$(shell_quote "$DEPLOY_HELPER") HEALTH_URL=$(shell_quote "$HEALTH_URL") EXPECTED_COMMIT=$(shell_quote "$EXPECTED_COMMIT") bash -s"

# shellcheck disable=SC2029
ssh "$REMOTE" "$remote_command" <<'EOF'
set -euo pipefail

case "$REPO_DIR" in
	"~")
		REPO_DIR="$HOME"
		;;
	"~/"*)
		REPO_DIR="$HOME/${REPO_DIR#"~/"}"
		;;
esac

cd "$REPO_DIR"

require_clean_checkout() {
	local status
	status="$(git status --porcelain --untracked-files=normal)"
	if [[ -n "$status" ]]; then
		echo "Deployment checkout contains local changes; preserve or reconcile them before deploying." >&2
		exit 1
	fi
}

git check-ref-format --branch "$BRANCH" >/dev/null
if [[ -n "$EXPECTED_COMMIT" && ! "$EXPECTED_COMMIT" =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]]; then
	echo "EXPECTED_COMMIT must be a full lowercase Git commit ID." >&2
	exit 1
fi
require_clean_checkout

echo "==> Updating source"
git fetch origin "$BRANCH"
release_commit="$(git rev-parse --verify 'FETCH_HEAD^{commit}')"
if [[ -n "$EXPECTED_COMMIT" && "$release_commit" != "$EXPECTED_COMMIT" ]]; then
	echo "Fetched commit $release_commit does not match EXPECTED_COMMIT $EXPECTED_COMMIT; deployment stopped." >&2
	exit 1
fi
if git show-ref --verify --quiet "refs/heads/$BRANCH"; then
	git checkout "$BRANCH"
else
	git checkout -b "$BRANCH" "$release_commit"
fi
if ! git merge-base --is-ancestor HEAD "$release_commit"; then
	echo "Local branch has commits absent from the fetched release; reconcile it before deploying." >&2
	exit 1
fi
git merge --ff-only "$release_commit"
require_clean_checkout

echo "==> Building application at $release_commit"
./scripts/build.sh

# A build hook or another operator must not change the checked source before
# the privileged helper installs the resulting binary.
require_clean_checkout
if [[ "$(git rev-parse --verify HEAD)" != "$release_commit" ]]; then
	echo "Checkout changed during the build; deployment stopped." >&2
	exit 1
fi
echo "==> Installing binary and restarting service"
sudo "$DEPLOY_HELPER"

echo "==> Waiting for health check"
for _ in 1 2 3 4 5; do
	if curl --fail --silent --show-error --connect-timeout 2 --max-time 5 "$HEALTH_URL"; then
		echo
		echo "Deploy complete."
		exit 0
	fi
	sleep 1
done

echo "Health check failed after restart." >&2
systemctl status "$APP_NAME.service" --no-pager >&2 || true
exit 1
EOF
