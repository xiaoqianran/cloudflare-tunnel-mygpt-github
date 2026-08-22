#!/usr/bin/env bash
set -euo pipefail

SERVICE="mygpt-github-agent"
UNIT_FILE="${SERVICE}.service"
ENV_FILE="/etc/mygpt-github-agent.env"
BIN="/usr/local/bin/mygpt-github-agent"

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run this script as root: ./scripts/install.sh" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "Missing required build tool: go (1.23+)" >&2
  echo "Install Go first, then rerun this installer." >&2
  exit 1
fi

ARCH="$(uname -m)"
echo "Detected architecture: $ARCH"
echo "Go: $(go version)"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
COMMAND_TIMEOUT_SECONDS=86400
MAX_COMMAND_OUTPUT_CHARS=180000
EOF
  chmod 0600 "$ENV_FILE"
  echo "Created $ENV_FILE"
  echo "API_TOKEN=$API_TOKEN"
  echo "Save this token; Custom GPT uses it as Bearer authentication."
else
  echo "Keeping existing $ENV_FILE, including any provider-specific variables or credentials."
  grep -q '^COMMAND_TIMEOUT_SECONDS=' "$ENV_FILE" || echo 'COMMAND_TIMEOUT_SECONDS=86400' >>"$ENV_FILE"
  grep -q '^MAX_COMMAND_OUTPUT_CHARS=' "$ENV_FILE" || echo 'MAX_COMMAND_OUTPUT_CHARS=180000' >>"$ENV_FILE"
fi

install -o root -g root -m 0644 "$ROOT_DIR/deploy/mygpt-github-agent.service" "/etc/systemd/system/$UNIT_FILE"
systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null

# runCommand is powerful enough to upgrade this agent itself. Restarting the
# service synchronously from one of its own child processes would kill the
# installer before it can return a response, so schedule the restart outside the
# current service cgroup in that case.
if grep -q "${UNIT_FILE}" /proc/$$/cgroup 2>/dev/null; then
  restart_unit="mygpt-agent-restart-$(date +%s)-$$"
  systemd-run --quiet --unit="$restart_unit" --on-active=2s /bin/systemctl restart "$SERVICE"
  echo "Installed successfully; scheduled $SERVICE restart in 2 seconds."
else
  systemctl restart "$SERVICE"
  systemctl --no-pager --full status "$SERVICE" || true
fi

echo
echo "Agent role: universal VPS root shell"
echo "Action surface: synchronous runCommand + asynchronous command jobs"
echo "Executor: /bin/bash -lc as root on the real host"
echo "Capability model: use installed tools or install/bootstrap new ones as needed"
echo "Local health: curl http://127.0.0.1:8787/health"
echo "OpenAPI: curl -s http://127.0.0.1:8787/openapi.json"
