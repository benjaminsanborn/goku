#!/usr/bin/env bash
# One-time control-plane host setup for goku. Run as root ON the server:
#   ssh ubuntu 'sudo -n bash -s' < scripts/server-bootstrap.sh
# Idempotent; subsequent deploys need no sudo (see scripts/deploy.sh).
set -euo pipefail

DEPLOY_USER="${SUDO_USER:?run via sudo so \$SUDO_USER is the deploy user}"
HOST_IP="$(hostname -I | awk '{print $1}')"
DOMAIN="goku.host"
ACME_EMAIL="benjaminsanborn@gmail.com"

echo "== packages =="
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates git rsync gnupg

echo "== postgresql 18 (pgdg repo; ubuntu 24.04 ships 16) =="
if [ ! -f /etc/apt/sources.list.d/pgdg.list ]; then
  install -d /usr/share/postgresql-common/pgdg
  curl -fsS -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc
  echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
    > /etc/apt/sources.list.d/pgdg.list
  apt-get update -qq
fi
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql-18

# If an older cluster was already on this box it will hold port 5432 and the
# 18 cluster won't be created. Require explicit human cleanup rather than
# dropping someone's data.
if ! pg_lsclusters -h | awk '$1 == 18' | grep -q .; then
  pg_createcluster 18 main --start > /dev/null
fi
if ! pg_lsclusters -h | awk '$1 == 18 && $3 == 5432' | grep -q .; then
  echo "error: an existing cluster occupies port 5432:" >&2
  pg_lsclusters >&2
  echo "move or drop it (pg_dropcluster --stop <ver> main), then re-run." >&2
  exit 1
fi

echo "== caddy (TLS termination, automatic Let's Encrypt) =="
if [ ! -f /etc/apt/sources.list.d/caddy-stable.list ]; then
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq
fi
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq caddy
cat > /etc/caddy/Caddyfile <<EOF
{
	email $ACME_EMAIL
}

$DOMAIN {
	reverse_proxy localhost:8080
}
EOF
systemctl enable caddy
systemctl restart caddy

echo "== service user + directories =="
id -u goku &>/dev/null || useradd --system --home /var/lib/goku --shell /usr/sbin/nologin goku
mkdir -p /opt/goku/bin /opt/goku/web /var/lib/goku /etc/goku
chown -R "$DEPLOY_USER" /opt/goku            # deploy artifacts: writable by deployer
chown goku:goku /var/lib/goku                # runtime state (bare repos): service-owned

echo "== postgres role + database =="
systemctl enable --now postgresql
sudo -u postgres psql -tAc "select 1 from pg_roles where rolname='goku'" | grep -q 1 \
  || sudo -u postgres createuser goku
sudo -u postgres psql -tAc "select 1 from pg_database where datname='goku'" | grep -q 1 \
  || sudo -u postgres createdb -O goku goku

echo "== config =="
if [ ! -f /etc/goku/gokud.env ]; then
  TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  cat > /etc/goku/gokud.env <<EOF
DATABASE_URL=postgres://goku@/goku?host=/var/run/postgresql
PORT=8080
GOKU_TOKEN=$TOKEN
GOKU_DATA=/var/lib/goku
WEB_DIST=/opt/goku/web/dist
GOKU_BASE_URL=http://$HOST_IP:8080
EOF
  chmod 640 /etc/goku/gokud.env
  chown "goku:$DEPLOY_USER" /etc/goku/gokud.env   # service reads, deployer reads
  # Copy the token where the deploy user (and their tooling) can read it.
  install -o "$DEPLOY_USER" -m 600 /dev/null "/home/$DEPLOY_USER/.goku-token"
  echo "$TOKEN" > "/home/$DEPLOY_USER/.goku-token"
fi

echo "== systemd unit =="
cat > /etc/systemd/system/gokud.service <<'EOF'
[Unit]
Description=goku control plane (API, git, MCP, UI)
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
User=goku
Group=goku
EnvironmentFile=/etc/goku/gokud.env
ExecStart=/opt/goku/bin/gokud
WorkingDirectory=/var/lib/goku
Restart=on-failure
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/goku
ProtectHome=true

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable gokud

echo "== allow the deploy user to restart gokud without a password =="
cat > /etc/sudoers.d/gokud <<EOF
$DEPLOY_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart gokud, /usr/bin/systemctl status gokud, /usr/bin/journalctl -u gokud*
EOF
chmod 440 /etc/sudoers.d/gokud

echo
echo "bootstrap complete."
echo "  LAN URL:    http://$HOST_IP:8080"
echo "  public URL: https://$DOMAIN (once DNS + port forwarding are set up)"
echo "  token:      /home/$DEPLOY_USER/.goku-token (also in /etc/goku/gokud.env)"
echo "  next, from your workstation: scripts/deploy.sh"
