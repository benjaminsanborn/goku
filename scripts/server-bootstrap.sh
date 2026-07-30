#!/usr/bin/env bash
# One-time control-plane host setup. Run as root ON the server:
#   ssh -t ubuntu 'sudo bash -s' < scripts/server-bootstrap.sh
# Idempotent; subsequent deploys need no sudo (see scripts/deploy.sh).
set -euo pipefail

DEPLOY_USER="${SUDO_USER:?run via sudo so \$SUDO_USER is the deploy user}"
HOST_IP="$(hostname -I | awk '{print $1}')"

echo "== packages =="
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql git rsync

echo "== service user + directories =="
id -u platform &>/dev/null || useradd --system --home /var/lib/platform --shell /usr/sbin/nologin platform
mkdir -p /opt/platform/bin /opt/platform/web /var/lib/platform /etc/platform
chown -R "$DEPLOY_USER" /opt/platform          # deploy artifacts: writable by deployer
chown platform:platform /var/lib/platform      # runtime state (bare repos): service-owned

echo "== postgres =="
systemctl enable --now postgresql
sudo -u postgres psql -tAc "select 1 from pg_roles where rolname='platform'" | grep -q 1 \
  || sudo -u postgres createuser platform
sudo -u postgres psql -tAc "select 1 from pg_database where datname='platform'" | grep -q 1 \
  || sudo -u postgres createdb -O platform platform

echo "== config =="
if [ ! -f /etc/platform/platformd.env ]; then
  TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  cat > /etc/platform/platformd.env <<EOF
DATABASE_URL=postgres://platform@/platform?host=/var/run/postgresql
PORT=8080
PLATFORM_TOKEN=$TOKEN
PLATFORM_DATA=/var/lib/platform
WEB_DIST=/opt/platform/web/dist
PLATFORM_BASE_URL=http://$HOST_IP:8080
EOF
  chmod 640 /etc/platform/platformd.env
  chown "platform:$DEPLOY_USER" /etc/platform/platformd.env   # service reads, deployer reads
  # Copy the token where the deploy user (and their tooling) can read it.
  install -o "$DEPLOY_USER" -m 600 /dev/null "/home/$DEPLOY_USER/.platform-token"
  echo "$TOKEN" > "/home/$DEPLOY_USER/.platform-token"
fi

echo "== systemd unit =="
cat > /etc/systemd/system/platformd.service <<'EOF'
[Unit]
Description=platform control plane (API, git, MCP, UI)
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
User=platform
Group=platform
EnvironmentFile=/etc/platform/platformd.env
ExecStart=/opt/platform/bin/platformd
WorkingDirectory=/var/lib/platform
Restart=on-failure
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/platform
ProtectHome=true

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable platformd

echo "== allow the deploy user to restart platformd without a password =="
cat > /etc/sudoers.d/platformd <<EOF
$DEPLOY_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart platformd, /usr/bin/systemctl status platformd, /usr/bin/journalctl -u platformd*
EOF
chmod 440 /etc/sudoers.d/platformd

echo
echo "bootstrap complete."
echo "  control plane URL: http://$HOST_IP:8080"
echo "  token: /home/$DEPLOY_USER/.platform-token (also in /etc/platform/platformd.env)"
echo "  next, from your workstation: scripts/deploy.sh"
