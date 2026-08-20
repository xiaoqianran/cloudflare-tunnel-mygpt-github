#!/usr/bin/env bash
set -euo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run with sudo: sudo ./scripts/install.sh" >&2
  exit 1
fi

for cmd in go git; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required command: $cmd" >&2
    exit 1
  fi
done

if ! command -v rg >/dev/null 2>&1; then
  echo "ripgrep (rg) is recommended for fast repository search." >&2
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVICE_USER="mygpt-agent"
WORKSPACE="/srv/mygpt/repos"
ENV_FILE="/etc/mygpt-github-agent.env"
BIN="/usr/local/bin/mygpt-github-agent"

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --home /var/lib/mygpt-agent --create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$WORKSPACE"

cd "$ROOT_DIR"
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/mygpt-github-agent ./cmd/mygpt-github-agent
install -o root -g root -m 0755 /tmp/mygpt-github-agent "$BIN"
rm -f /tmp/mygpt-github-agent

if [[ ! -f "$ENV_FILE" ]]; then
  API_TOKEN="$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  cat >"$ENV_FILE" <<EOF
API_TOKEN=$API_TOKEN
WORKSPACE_ROOT=$WORKSPACE
LISTEN_ADDR=127.0.0.1:8787
ALLOWED_REPOS=xiaoqianran/*
GIT_REMOTE_BASE_URL=https://github.com
GIT_REMOTE_USERNAME=x-access-token
GITHUB_TOKEN=
GIT_AUTHOR_NAME=MyGPT Workspace Agent
GIT_AUTHOR_EMAIL=mygpt@localhost
GIT_COMMITTER_NAME=MyGPT Workspace Agent
GIT_COMMITTER_EMAIL=mygpt@localhost
COMMAND_TIMEOUT_SECONDS=300
MAX_READ_FILES=50
MAX_PAGE_CHARS=180000
MAX_WRITE_BYTES=5000000
MAX_DIFF_CHARS=180000
EOF
  chmod 0600 "$ENV_FILE"
  echo "Created $ENV_FILE"
  echo "API_TOKEN=$API_TOKEN"
  echo "Save this token; Custom GPT will use it as Bearer authentication."
else
  echo "Keeping existing $ENV_FILE"
fi

install -o root -g root -m 0644 "$ROOT_DIR/deploy/mygpt-github-agent.service" /etc/systemd/system/mygpt-github-agent.service
systemctl daemon-reload
systemctl enable --now mygpt-github-agent
systemctl --no-pager --full status mygpt-github-agent || true

echo
echo "Local health check: curl http://127.0.0.1:8787/health"
echo "Next: publish http://localhost:8787 through a Cloudflare Tunnel."
