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

ARCH="$(uname -m)"
echo "Detected architecture: $ARCH"

need_apt=0
if ! command -v rg >/dev/null 2>&1; then
  need_apt=1
fi
if ! command -v podman >/dev/null 2>&1 && ! command -v docker >/dev/null 2>&1; then
  need_apt=1
fi

if [[ "$need_apt" == "1" ]]; then
  if ! command -v apt-get >/dev/null 2>&1; then
    echo "Missing podman/docker or ripgrep and apt-get is unavailable." >&2
    exit 1
  fi
  apt-get update
  packages=()
  command -v rg >/dev/null 2>&1 || packages+=(ripgrep)
  if ! command -v podman >/dev/null 2>&1 && ! command -v docker >/dev/null 2>&1; then
    packages+=(podman)
  fi
  if [[ ${#packages[@]} -gt 0 ]]; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y "${packages[@]}"
  fi
fi

if command -v podman >/dev/null 2>&1; then
  SANDBOX_ENGINE=podman
elif command -v docker >/dev/null 2>&1; then
  SANDBOX_ENGINE=docker
else
  echo "A command sandbox engine is required: install podman or docker." >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKSPACE="/srv/mygpt/repos"
ENV_FILE="/etc/mygpt-github-agent.env"
BIN="/usr/local/bin/mygpt-github-agent"
SANDBOX_IMAGE="ubuntu:24.04"

# The API service runs as root so it can manage repositories and the local OCI
# runtime. Commands requested through runCommand execute inside a per-repository
# container; only the repository workspace is mounted into that container.
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
COMMAND_SANDBOX_ENGINE=auto
COMMAND_SANDBOX_IMAGE=$SANDBOX_IMAGE
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
  grep -q '^COMMAND_SANDBOX_ENGINE=' "$ENV_FILE" || echo 'COMMAND_SANDBOX_ENGINE=auto' >>"$ENV_FILE"
  grep -q '^COMMAND_SANDBOX_IMAGE=' "$ENV_FILE" || echo "COMMAND_SANDBOX_IMAGE=$SANDBOX_IMAGE" >>"$ENV_FILE"
  grep -q '^MAX_COMMAND_OUTPUT_CHARS=' "$ENV_FILE" || echo 'MAX_COMMAND_OUTPUT_CHARS=120000' >>"$ENV_FILE"
fi

# Docker/Podman automatically selects the host-native image variant, including
# linux/arm64 on aarch64 hosts.
"$SANDBOX_ENGINE" pull "$SANDBOX_IMAGE"

install -o root -g root -m 0644 "$ROOT_DIR/deploy/mygpt-github-agent.service" /etc/systemd/system/mygpt-github-agent.service
systemctl daemon-reload
systemctl enable --now mygpt-github-agent
systemctl restart mygpt-github-agent
systemctl --no-pager --full status mygpt-github-agent || true

echo
echo "Agent process: root"
echo "Command runtime: $SANDBOX_ENGINE ($SANDBOX_IMAGE)"
echo "Host architecture: $ARCH"
echo "Local health check: curl http://127.0.0.1:8787/health"
echo "OpenAPI check: curl -s http://127.0.0.1:8787/openapi.json | jq -r '.paths | keys[]'"
echo "Next: publish http://localhost:8787 through a Cloudflare Tunnel."
