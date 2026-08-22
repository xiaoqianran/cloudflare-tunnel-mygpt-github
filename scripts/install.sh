#!/usr/bin/env bash
set -euo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run this script as root: ./scripts/install.sh" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "Missing required build tool: go (1.23+)" >&2
  exit 1
fi

ARCH="$(uname -m)"
echo "Detected architecture: $ARCH"
echo "Go: $(go version)"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="/etc/mygpt-github-agent.env"
BIN="/usr/local/bin/mygpt-github-agent"

cd "$ROOT_DIR"
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/mygpt-github-agent ./cmd/mygpt-github-agent
install -o root -g root -m 0755 /tmp/mygpt-github-agent "$BIN"
rm -f /tmp/mygpt-github-agent

if [[ ! -f "$ENV_FILE" ]]; then
  API_TOKEN="$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  cat >"$ENV_FILE" <<EOF
API_TOKEN=$API_TOKEN
LISTEN_ADDR=127.0.0.1:8787
COMMAND_TIMEOUT_SECONDS=1800
MAX_COMMAND_OUTPUT_CHARS=180000

# Optional. gh also reads its normal /root config. If GITHUB_TOKEN is set and
# GH_TOKEN is not, runCommand automatically exposes it to child commands as GH_TOKEN.
GITHUB_TOKEN=
EOF
  chmod 0600 "$ENV_FILE"
  echo "Created $ENV_FILE"
  echo "API_TOKEN=$API_TOKEN"
  echo "Save this token; Custom GPT will use it as Bearer authentication."
else
  echo "Keeping existing $ENV_FILE"
  grep -q '^COMMAND_TIMEOUT_SECONDS=' "$ENV_FILE" || echo 'COMMAND_TIMEOUT_SECONDS=1800' >>"$ENV_FILE"
  grep -q '^MAX_COMMAND_OUTPUT_CHARS=' "$ENV_FILE" || echo 'MAX_COMMAND_OUTPUT_CHARS=180000' >>"$ENV_FILE"
fi

install -o root -g root -m 0644 "$ROOT_DIR/deploy/mygpt-github-agent.service" /etc/systemd/system/mygpt-github-agent.service
systemctl daemon-reload
systemctl enable --now mygpt-github-agent
systemctl restart mygpt-github-agent
systemctl --no-pager --full status mygpt-github-agent || true

echo
echo "Agent process: root"
echo "Action surface: runCommand only"
echo "Executor: /bin/bash -lc on the VPS host"
echo "Local health check: curl http://127.0.0.1:8787/health"
echo "OpenAPI check: curl -s http://127.0.0.1:8787/openapi.json"
echo "Use runCommand to install or invoke gh, modal, kaggle, apt packages and any other host tools."
