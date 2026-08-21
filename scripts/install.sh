#!/usr/bin/env bash
set -euo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run this script as root: ./scripts/install.sh" >&2
  exit 1
fi

for cmd in go git gh rg; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required command: $cmd" >&2
    exit 1
  fi
done

ARCH="$(uname -m)"
echo "Detected architecture: $ARCH"
echo "GitHub CLI: $(gh --version | head -n 1)"

if ! command -v apt-get >/dev/null 2>&1; then
  if ! command -v rg >/dev/null 2>&1; then
    echo "Missing ripgrep and apt-get is unavailable." >&2
    exit 1
  fi
else
  if ! command -v rg >/dev/null 2>&1; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y ripgrep
  fi
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKSPACE="/srv/mygpt/repos"
ENV_FILE="/etc/mygpt-github-agent.env"
BIN="/usr/local/bin/mygpt-github-agent"

# The API service deliberately runs as root because runCommand is a host-side
# command runner and must be able to invoke normal VPS administration commands.
install -d -o root -g root -m 0755 "$WORKSPACE"

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
MAX_COMMAND_OUTPUT_CHARS=120000
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
  grep -q '^MAX_COMMAND_OUTPUT_CHARS=' "$ENV_FILE" || echo 'MAX_COMMAND_OUTPUT_CHARS=120000' >>"$ENV_FILE"
fi

install -o root -g root -m 0644 "$ROOT_DIR/deploy/mygpt-github-agent.service" /etc/systemd/system/mygpt-github-agent.service
systemctl daemon-reload
systemctl enable --now mygpt-github-agent
systemctl restart mygpt-github-agent
systemctl --no-pager --full status mygpt-github-agent || true

echo
echo "Agent process: root"
echo "runCommand: host /bin/bash -lc"
echo "Local health check: curl http://127.0.0.1:8787/health"
echo "OpenAPI check: curl -s http://127.0.0.1:8787/openapi.json | jq -r '.paths | keys[]'"
echo "Next: publish http://localhost:8787 through a Cloudflare Tunnel."
