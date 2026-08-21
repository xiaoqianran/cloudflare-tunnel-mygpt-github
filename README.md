# cloudflare-tunnel-mygpt-github

A lightweight **local Git workspace backend for Custom GPT**, exposed through Cloudflare Tunnel.

The important difference from a GitHub connector is that repositories are real persistent Git checkouts on your own Linux server:

```text
Custom GPT
    │ HTTPS + Bearer API_TOKEN
    ▼
Cloudflare Tunnel
    │ outbound-only tunnel to the VPS
    ▼
127.0.0.1:8787
MyGPT Git Workspace Agent (Go, root service)
    │
    ├─ /srv/mygpt/repos/owner/repo/.git
    ├─ local file reads / writes
    ├─ local ripgrep search
    ├─ native git fetch / commit / push
    │
    └─ runCommand
         │
         ▼
      persistent OCI sandbox
      root inside container
      /workspace -> real repository (rw)
         │
         ├─ apt / bash / python / npm / uv / go / make ...
         └─ build/test outputs persist in the repository
```

After a repository is cloned, **reading, searching, editing and project command execution no longer uses the GitHub REST API**. GitHub is contacted only for normal Git synchronization such as clone/fetch/push.

## What MyGPT can do

The OpenAPI actions expose:

- `syncRepository` — clone the repository on first use; later fetch and fast-forward a clean worktree
- `inspectRepository` — inspect local branch/HEAD/dirty state and list local files
- `readFiles` — batch-read files directly from server disk
- `searchRepository` — local ripgrep search; no GitHub code-search API
- `readRepositoryPage` — progressively read the complete text repository, including continuation inside large files
- `applyChanges` — write/delete files in the real local worktree
- `runCommand` — run shell commands as root inside a persistent per-repository OCI sandbox
- `gitDiff` — review the local diff
- `commitAndPush` — `git add -A`, commit, and native `git push`; optional force push
- `createRelease` — run host-side `gh release create` to publish a GitHub Release

The systemd API process itself runs as **root**. `runCommand` does **not** execute the requested shell directly on the host: it executes inside a Podman/Docker container. The real repository is the only host workspace mounted into that container at `/workspace`.

## ARM64 / aarch64

ARM servers are supported. The installer builds the Go service natively on the VPS and the CI also cross-builds `linux/arm64`.

The default command image is:

```text
ubuntu:24.04
```

It is multi-architecture. Podman/Docker automatically selects the native `linux/arm64` image on an `aarch64` host.

## Server requirements

Recommended for this project:

```text
Linux VPS
2 vCPU
1 GB RAM
1-2 GB swap
20 GB+ SSD
Git
GitHub CLI (`gh`) for `createRelease`
ripgrep
Go 1.23+
Podman (preferred) or Docker
cloudflared
```

The Go service itself uses only the standard library.

## 1. Install / upgrade the agent on the VPS

```bash
git clone https://github.com/xiaoqianran/cloudflare-tunnel-mygpt-github.git
cd cloudflare-tunnel-mygpt-github
sudo bash ./scripts/install.sh
```

For an existing checkout:

```bash
git pull
sudo bash ./scripts/install.sh
```

The installer:

- detects the host architecture
- installs `ripgrep` and Podman on Ubuntu when neither Podman nor Docker is available
- runs Go tests
- builds the Go binary natively for the current host
- runs the systemd service as `root:root`
- creates `/srv/mygpt/repos` as the persistent Git workspace
- creates/updates `/etc/mygpt-github-agent.env`
- pulls `ubuntu:24.04` using the native host architecture
- starts the service on `127.0.0.1:8787`

It prints a generated `API_TOKEN` on the first install. Save it.

Check locally:

```bash
curl http://127.0.0.1:8787/health
systemctl status mygpt-github-agent
```

Expected health response:

```json
{"ok":true,"service":"cloudflare-tunnel-mygpt-github","version":"0.2.2"}
```

Confirm service identity and architecture:

```bash
systemctl show mygpt-github-agent -p User -p Group
uname -m
```

Typical ARM64 output:

```text
User=root
Group=root
aarch64
```

Confirm the new action exists:

```bash
curl -s http://127.0.0.1:8787/openapi.json | jq -r '.paths | keys[]'
```

You should now see both host and sandbox actions, including:

```text
/v1/github/release
/v1/command/run
```

## 2. Configure Git authentication

Edit:

```bash
sudo nano /etc/mygpt-github-agent.env
```

### Direct GitHub mode

If the VPS can reach GitHub, use:

```dotenv
GIT_REMOTE_BASE_URL=https://github.com
GIT_REMOTE_USERNAME=x-access-token
GITHUB_TOKEN=github_pat_xxxxxxxxx
```

The token stays only on the VPS. It is injected into Git HTTP authentication through the child process environment and is not written into repository remotes. When `createRelease` runs, the same `GITHUB_TOKEN` is exposed to host `gh` as `GH_TOKEN` for that child process only.

For public read-only repositories, clone/fetch can work without a token. Push and private repository access require suitable GitHub credentials.

Restart after changes:

```bash
sudo systemctl restart mygpt-github-agent
```

### Optional: route Git through the old Worker bridge

If this VPS itself cannot reach `github.com`, the agent can use a Git Smart HTTP gateway instead:

```dotenv
GIT_REMOTE_BASE_URL=https://cloudflare-mygpt-github.wangran.workers.dev/git
GIT_REMOTE_USERNAME=git
GIT_REMOTE_TOKEN=YOUR_GIT_GATEWAY_TOKEN
GITHUB_TOKEN=
```

The repository still lives on the VPS. Only clone/fetch/push traverse the Worker bridge.

## 3. Configure the command sandbox

Default configuration:

```dotenv
COMMAND_TIMEOUT_SECONDS=300
COMMAND_SANDBOX_ENGINE=auto
COMMAND_SANDBOX_IMAGE=ubuntu:24.04
MAX_COMMAND_OUTPUT_CHARS=120000
```

`auto` prefers Podman and falls back to Docker.

Each repository gets one persistent container with a deterministic name such as:

```text
mygpt-xxxxxxxxxxxxxxxx
```

The first `runCommand` call creates and starts it. Later calls reuse it, so packages installed inside the container remain installed.

The mount is:

```text
host:      /srv/mygpt/repos/owner/repo
container: /workspace
mode:      read-write
```

The command container is **not** created with `--privileged`; the host root filesystem, host devices and Podman/Docker socket are not mounted into it.

Example commands inside the sandbox:

```bash
uname -m
apt-get update
apt-get install -y python3 python3-pip git curl
python3 --version
npm test
go test ./...
make
bash scripts/test.sh
```

A command can contain pipes, redirects and multiple shell statements because it is executed by `/bin/bash -lc` **inside the container**.

## 4. Configure Cloudflare Tunnel

The agent intentionally listens only on:

```text
127.0.0.1:8787
```

Do not open port 8787 directly to the Internet.

In the Cloudflare dashboard:

```text
Networking -> Tunnels -> Create Tunnel
```

Create a remotely managed tunnel, for example:

```text
mygpt-github-agent
```

Cloudflare gives an installation command similar to:

```bash
sudo cloudflared service install <TUNNEL_TOKEN>
```

Run it on the VPS.

Then add a **Published application** route:

```text
Hostname:  git-agent.your-domain.com
Service:   http://localhost:8787
```

For a temporary development test only:

```bash
cloudflared tunnel --url http://localhost:8787
```

Use a named tunnel plus your own Cloudflare-managed hostname for production.

## 5. Configure the Custom GPT Action

Open:

```text
https://git-agent.your-domain.com/openapi.json
```

In **GPTs -> Configure -> Actions** import that schema.

Authentication:

```text
Authentication: API Key
Auth type: Bearer
API key: <API_TOKEN from /etc/mygpt-github-agent.env>
```

Do **not** put these into the GPT:

```text
GITHUB_TOKEN
GIT_REMOTE_TOKEN
Cloudflare Tunnel token
```

Only `API_TOKEN` is the GPT-facing credential.

## Recommended MyGPT instruction

```text
For repository tasks, always use cloudflare-tunnel-mygpt-github Actions before any built-in GitHub connector, GitHub search or web browsing.

Workflow:
1. Call inspectRepository. If the repository is not local, call syncRepository.
2. Call syncRepository again only when fresh remote state is required.
3. Use searchRepository and readFiles for targeted investigation.
4. For whole-repository analysis, repeatedly call readRepositoryPage with next_cursor until null.
5. Use applyChanges for direct file edits.
6. Use runCommand for project setup, dependency installation, builds, tests, linters and scripts. Commands execute inside the persistent repository sandbox and /workspace is the real repository.
7. When a command fails, inspect stdout/stderr, fix the code, and rerun it.
8. Call gitDiff after edits and tests.
9. Use commitAndPush only when the result is correct.
10. Use createRelease when a GitHub Release should be published; it runs gh on the VPS host, not in the project sandbox.
11. Do not use built-in GitHub tools for repository file reads when these local actions can answer the request.
```

## API examples

Set the local or Tunnel URL and token:

```bash
export BASE_URL=http://127.0.0.1:8787
export API_TOKEN="$(grep '^API_TOKEN=' /etc/mygpt-github-agent.env | cut -d= -f2-)"
```

Clone a repository onto the VPS:

```bash
curl -s "$BASE_URL/v1/repository/sync" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"xiaoqianran/cloudflare-tunnel-mygpt-github"}'
```

Run an ARM/container sanity check:

```bash
curl -s "$BASE_URL/v1/command/run" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "repo":"xiaoqianran/cloudflare-tunnel-mygpt-github",
    "command":"id; uname -m; pwd"
  }' | jq
```

Expected characteristics:

```text
exit_code: 0
stdout contains uid=0(root)
stdout contains aarch64 on ARM64
stdout contains /workspace
engine is podman or docker
```

Install something and verify the container is persistent:

```bash
curl -s "$BASE_URL/v1/command/run" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"xiaoqianran/cloudflare-tunnel-mygpt-github","command":"apt-get update && apt-get install -y jq"}'

curl -s "$BASE_URL/v1/command/run" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"xiaoqianran/cloudflare-tunnel-mygpt-github","command":"jq --version"}'
```

List local files:

```bash
curl -s "$BASE_URL/v1/repository/inspect" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"xiaoqianran/cloudflare-tunnel-mygpt-github","limit":1000}'
```

Search locally:

```bash
curl -s "$BASE_URL/v1/repository/search" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"xiaoqianran/cloudflare-tunnel-mygpt-github","query":"commitAndPush"}'
```

## Configuration

`/etc/mygpt-github-agent.env`:

| Variable | Purpose | Default |
|---|---|---|
| `API_TOKEN` | Custom GPT -> agent Bearer credential | required |
| `WORKSPACE_ROOT` | Persistent local repository root | `/srv/mygpt/repos` |
| `LISTEN_ADDR` | Origin HTTP listener | `127.0.0.1:8787` |
| `ALLOWED_REPOS` | Comma-separated repo patterns; empty means all | empty |
| `GIT_REMOTE_BASE_URL` | Base URL used by native Git | `https://github.com` |
| `GIT_REMOTE_USERNAME` | HTTP Basic username for native Git | `x-access-token` |
| `GITHUB_TOKEN` | Direct GitHub token; server only | empty |
| `GIT_REMOTE_TOKEN` | Overrides `GITHUB_TOKEN`, useful for a Git gateway | empty |
| `COMMAND_TIMEOUT_SECONDS` | Maximum command/Git operation time | `300` |
| `COMMAND_SANDBOX_ENGINE` | `auto`, `podman`, or `docker` | `auto` |
| `COMMAND_SANDBOX_IMAGE` | Persistent command-container image | `ubuntu:24.04` |
| `MAX_COMMAND_OUTPUT_CHARS` | Maximum captured stdout/stderr chars per stream | `180000` |
| `MAX_READ_FILES` | Max files in one `readFiles` call | `50` |
| `MAX_PAGE_CHARS` | Max text budget for one repository page | `180000` |
| `MAX_WRITE_BYTES` | Max bytes written to one file in one action | `5000000` |
| `MAX_DIFF_CHARS` | Max diff returned in one response | `180000` |

## Why this avoids the GitHub file-read bottleneck

After `syncRepository` has cloned a project:

```text
readFiles          -> VPS local disk
searchRepository   -> VPS local ripgrep
readRepositoryPage -> VPS local disk
applyChanges       -> VPS local disk
runCommand         -> local OCI container + mounted repo
gitDiff            -> local .git database
```

None of those operations consumes GitHub REST API requests.

Only synchronization crosses the Git network path:

```text
clone / fetch / push -> Git transport -> GitHub (or optional Git gateway)
```

## Security boundary

There are intentionally two privilege domains:

```text
Host API process
  root on VPS

runCommand process
  root inside OCI container
  /workspace = selected repository (rw)
  no host / mount
  no container-engine socket mount
  no --privileged
```

This gives the coding agent broad project-level command capability without turning the public API into a direct host-root shell.

Keep `API_TOKEN` secret and keep the origin bound to `127.0.0.1` behind Cloudflare Tunnel.

## Operational notes

- `runCommand` requires the repository to be cloned first with `syncRepository`.
- Each repository gets a persistent command container; installed packages remain until that container is removed.
- `applyChanges` edits local repository files only; it does not silently commit.
- `commitAndPush` stages all local changes with `git add -A`.
- `force: true` intentionally maps to `git push --force`.
- GitHub branch protection/rulesets still apply to pushes.
- A dirty worktree is never overwritten by `syncRepository`; it fetches remote state but skips checkout/fast-forward until local edits are resolved.

## Host-side GitHub Release action

`createRelease` is intentionally different from `runCommand`: it executes the fixed GitHub CLI command `gh release create` directly on the VPS host. It is **not** a general host shell. The request is still protected by the agent Bearer token and `ALLOWED_REPOS`.

The systemd service runs as root, so verify GitHub CLI availability as root:

```bash
sudo gh --version
sudo gh auth status
```

If `GITHUB_TOKEN` is configured in `/etc/mygpt-github-agent.env`, no interactive `gh auth login` is required; the agent passes it to the `gh` child process as `GH_TOKEN`. Otherwise authenticate the root account once:

```bash
sudo gh auth login
```

Create a release through the local API:

```bash
curl -s http://127.0.0.1:8787/v1/github/release \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "repo":"xiaoqianran/cloudflare-tunnel-mygpt-github",
    "tag":"v0.2.2",
    "title":"v0.2.2",
    "notes":"Host-side GitHub Release support via gh.",
    "target":"main"
  }' | jq
```

If the tag does not already exist on GitHub, `gh release create` can create it from `target`. If the tag already exists, GitHub uses that tag. `draft` and `prerelease` are optional booleans.
