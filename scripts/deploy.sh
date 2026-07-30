#!/usr/bin/env bash
# Deploy the control plane to the server. No sudo needed after bootstrap.
#   scripts/deploy.sh            deploys to host 'ubuntu' (ssh alias)
#   PLATFORM_HOST=myhost scripts/deploy.sh
set -euo pipefail
cd "$(dirname "$0")/.."

HOST="${PLATFORM_HOST:-ubuntu}"
ARCH="$(ssh "$HOST" uname -m)"
case "$ARCH" in
  x86_64) GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *) echo "unsupported server arch: $ARCH" >&2; exit 1 ;;
esac

echo "== build (linux/$GOARCH) =="
GOOS=linux GOARCH=$GOARCH CGO_ENABLED=0 go build -o build/platformd-linux ./cmd/platformd
(cd web && npm run --silent build)

echo "== ship =="
rsync -az build/platformd-linux "$HOST":/opt/platform/bin/platformd.new
rsync -az --delete web/dist/ "$HOST":/opt/platform/web/dist/
ssh "$HOST" 'mv /opt/platform/bin/platformd.new /opt/platform/bin/platformd && sudo -n systemctl restart platformd'

echo "== health check =="
BASE_URL="$(ssh "$HOST" "grep PLATFORM_BASE_URL /etc/platform/platformd.env 2>/dev/null | cut -d= -f2-" || true)"
BASE_URL="${BASE_URL:-http://$HOST:8080}"
for _ in $(seq 1 15); do
  if curl -sf "$BASE_URL/v1/projects" > /dev/null; then
    echo "platformd healthy at $BASE_URL"
    exit 0
  fi
  sleep 1
done
echo "health check failed — inspect with: ssh $HOST sudo -n journalctl -u platformd -n 50" >&2
exit 1
