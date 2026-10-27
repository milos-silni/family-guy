#!/usr/bin/env bash
# Build Family Guy and deploy it to the Ubuntu server.
#
#   ./deploy.sh          # test, build, deploy
#   ./deploy.sh --fast   # skip the tests
#
# Settings (environment variables):
#   DEPLOY_HOST  ssh target               (default: milos@darkstar)
#   DEPLOY_URL   public URL to check      (default: https://family.milosm.com)
#
# The server must already run the app as the systemd service "family-guy";
# paths, user and port are read from it. You'll be asked for your sudo password on the server.
set -euo pipefail

HOST="${DEPLOY_HOST:-milos@darkstar}"
URL="${DEPLOY_URL:-https://family.milosm.com}"

FAST=0
[ "${1:-}" = "--fast" ] && FAST=1

cd "$(dirname "$0")"
step() { printf '\n\033[1;32m==>\033[0m %s\n' "$*"; }
start=$SECONDS

# Short path: macOS limits ssh control socket paths to 104 characters.
build_dir="$(mktemp -d /tmp/fg.XXXXXX)"
# One shared SSH connection for every ssh/scp below (log in once, much faster).
SSH_OPTS=(-o ControlMaster=auto -o "ControlPath=$build_dir/ssh-%C" -o ControlPersist=120)
cleanup() {
  ssh "${SSH_OPTS[@]}" -O exit "$HOST" 2>/dev/null || true
  rm -rf "$build_dir"
}
trap cleanup EXIT

if [ "$FAST" = 1 ]; then
  step "Skipping tests (--fast)"
else
  step "Running checks and tests"
  go vet ./...
  go test ./...
fi

step "Checking server architecture ($HOST)"
case "$(ssh "${SSH_OPTS[@]}" "$HOST" uname -m)" in
  x86_64)  arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) echo "Unsupported server architecture"; exit 1 ;;
esac
echo "linux/$arch"

step "Building"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$build_dir/family-guy" .

# Runs on the server as root.
cat > "$build_dir/install.sh" <<'EOF'
set -euo pipefail
SERVICE=family-guy
NEW=/tmp/family-guy-deploy/family-guy
stamp="$(date +%Y%m%d-%H%M%S)"

# Read paths, user and port from the systemd service instead of assuming them.
prop() { systemctl show -p "$1" --value "$SERVICE"; }
envvar() { for kv in $(prop Environment); do case "$kv" in "$1="*) echo "${kv#*=}" ;; esac; done; }

APP="$(prop ExecStart | sed -n 's/.*path=\([^ ;]*\).*/\1/p')"
[ -n "$APP" ] || { echo "Can't find $SERVICE.service. Is it installed?" >&2; exit 1; }
RUN_USER="$(prop User)"; RUN_USER="${RUN_USER:-root}"
WORKDIR="$(prop WorkingDirectory)"; WORKDIR="${WORKDIR:-/}"
DB="$(envvar DB_PATH)"; DB="${DB:-family.db}"
case "$DB" in /*) ;; *) DB="$WORKDIR/$DB" ;; esac
ADDR="$(envvar ADDR)"; PORT="${ADDR##*:}"; PORT="${PORT:-8080}"
BACKUPS="$(dirname "$DB")/backups"
echo "Service runs $APP as $RUN_USER, database $DB, port $PORT"

install -m 755 -o "$RUN_USER" "$NEW" "$APP.new"
if [ -f "$APP" ]; then cp -p "$APP" "$APP.prev"; fi

# From here on the app is stopped: if anything fails, start it again.
stopped=0
finish() {
  code=$?
  rm -rf /tmp/family-guy-deploy "$APP.new"
  if [ "$code" -ne 0 ] && [ "$stopped" = 1 ]; then
    echo "Deploy failed; starting the app again." >&2
    systemctl start "$SERVICE" || true
  fi
}
trap finish EXIT

systemctl stop "$SERVICE"
stopped=1

# Back up the database while the app is stopped, so the copy is consistent.
mkdir -p "$BACKUPS"
for f in "$DB" "$DB-wal" "$DB-shm"; do
  if [ -f "$f" ]; then cp -p "$f" "$BACKUPS/$(basename "$f").$stamp"; fi
done
chown -R "$RUN_USER:" "$BACKUPS"
# Keep the 10 most recent backups.
name="$(basename "$DB")"
{ ls -1t "$BACKUPS/$name".* 2>/dev/null || true; } | tail -n +11 | while read -r old; do
  ts="${old##*.}"
  rm -f "$old" "$BACKUPS/$name-wal.$ts" "$BACKUPS/$name-shm.$ts"
done

mv "$APP.new" "$APP"
systemctl start "$SERVICE"

# Give it a few seconds to come up (the first tries may fail while it starts).
for _ in $(seq 20); do
  if curl -fs -o /dev/null "http://127.0.0.1:$PORT/login"; then
    echo "App is running."
    exit 0
  fi
  sleep 0.5
done

echo "New version didn't start. Rolling back." >&2
journalctl -u "$SERVICE" -n 20 --no-pager >&2 || true
if [ -f "$APP.prev" ]; then
  mv "$APP.prev" "$APP"
  systemctl restart "$SERVICE"
  echo "Previous version restored." >&2
fi
exit 1
EOF

step "Uploading"
mkdir "$build_dir/family-guy-deploy"
mv "$build_dir/family-guy" "$build_dir/install.sh" "$build_dir/family-guy-deploy/"
ssh "${SSH_OPTS[@]}" "$HOST" "rm -rf /tmp/family-guy-deploy"
scp "${SSH_OPTS[@]}" -q -r "$build_dir/family-guy-deploy" "$HOST:/tmp/"

step "Installing and restarting (sudo password may be asked)"
ssh "${SSH_OPTS[@]}" -t "$HOST" "sudo bash /tmp/family-guy-deploy/install.sh"

step "Checking $URL"
code="$(curl -s -o /dev/null -w '%{http_code}' "$URL/login" || true)"
if [ "$code" = "200" ]; then
  echo "Live: $URL  (took $((SECONDS - start))s)"
else
  echo "Public URL answered HTTP $code. The app runs on the server; check cloudflared." >&2
  exit 1
fi
