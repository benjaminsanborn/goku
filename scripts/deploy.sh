#!/usr/bin/env bash
# Deploy the goku control plane to the server. No sudo needed after bootstrap.
#   scripts/deploy.sh            deploys to host 'ubuntu' (ssh alias)
#   GOKU_HOST=myhost scripts/deploy.sh
set -euo pipefail
cd "$(dirname "$0")/.."

HOST="${GOKU_HOST:-ubuntu}"
ARCH="$(ssh "$HOST" uname -m)"
case "$ARCH" in
  x86_64) GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *) echo "unsupported server arch: $ARCH" >&2; exit 1 ;;
esac

echo "== build (linux/$GOARCH) =="
GOOS=linux GOARCH=$GOARCH CGO_ENABLED=0 go build -o build/gokud-linux ./cmd/gokud
(cd web && npm run --silent build)

echo "== ship =="
rsync -az build/gokud-linux "$HOST":/opt/goku/bin/gokud.new
rsync -az --delete web/dist/ "$HOST":/opt/goku/web/dist/
ssh "$HOST" 'mv /opt/goku/bin/gokud.new /opt/goku/bin/gokud && sudo -n systemctl restart gokud'

echo "== health check =="
ENVFILE="$(ssh "$HOST" 'cat /etc/goku/gokud.env 2>/dev/null' || true)"
BASE_URL="$(grep '^GOKU_BASE_URL=' <<<"$ENVFILE" | cut -d= -f2- || true)"
TOKEN="$(grep '^GOKU_TOKEN=' <<<"$ENVFILE" | cut -d= -f2- || true)"
BASE_URL="${BASE_URL:-http://$HOST:8080}"
for _ in $(seq 1 15); do
  if curl -sf -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/projects" > /dev/null; then
    echo "gokud healthy at $BASE_URL"
    exit 0
  fi
  sleep 1
done
echo "health check failed — inspect with: ssh $HOST sudo -n journalctl -u gokud -n 50" >&2
exit 1
