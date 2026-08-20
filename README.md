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
    └─ native git fetch / commit / push
              │
              ▼
            GitHub
```

After a repository is cloned, **reading, searching and editing repository files no longer uses the GitHub REST API**. GitHub is contacted only for normal Git synchronization such as clone/fetch/push.

## What MyGPT can do

The OpenAPI action exposes:

- `syncRepository` — clone the repository on first use; later fetch and fast-forward a clean worktree
- `inspectRepository` — inspect local branch/HEAD/dirty state and list local files
- `readFiles` — batch-read files directly from server disk
- `searchRepository` — local ripgrep search; no GitHub code-search API
- `readRepositoryPage` — progressively read the complete text repository, including continuation inside large files
- `applyChanges` — write/delete files in the real local worktree
- `gitDiff` — review the local diff
- `commitAndPush` — `git add -A`, commit, and native `git push`; optional force push

The service process runs as **root with no systemd filesystem sandboxing**. The current HTTP API still exposes repository-scoped first-class operations rather than an arbitrary shell endpoint, but any bug or future endpoint in this service executes with root privileges.

## Server requirements

Recommended for this project:

```text
Linux VPS
2 vCPU
1 GB RAM
1-2 GB swap
20 GB+ SSD
Git
ripgrep
Go 1.23+
cloudflared
```

The service itself uses only the Go standard library.

## 1. Install the agent on the VPS

```bash
git clone https://github.com/xiaoqianran/cloudflare-tunnel-mygpt-github.git
cd cloudflare-tunnel-mygpt-github
bash ./scripts/install.sh
```

The installer:

- runs tests
- builds a static Go binary
- runs the systemd service as `root:root`
- creates `/srv/mygpt/repos` as a persistent root-owned workspace
- creates `/etc/mygpt-github-agent.env`
- installs and starts a systemd service bound only to `127.0.0.1:8787`

It prints a generated `API_TOKEN` the first time. Save it.

If you are upgrading an older install that used the `mygpt-agent` system user, rerunning `sudo ./scripts/install.sh` replaces the service definition and restarts it as root. The old user can remain unused.

Check locally:

```bash
curl http://127.0.0.1:8787/health
sudo systemctl status mygpt-github-agent
```

Expected health response:

```json
{"ok":true,"service":"cloudflare-tunnel-mygpt-github","version":"0.1.0"}
```

Confirm root mode:

```bash
systemctl show mygpt-github-agent -p User -p Group
```

Expected:

```text
User=root
Group=root
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

The token stays only on the VPS. It is injected into Git HTTP authentication through the child process environment and is not written into repository remotes.

For public read-only repositories, clone/fetch can work without a token. Push and private repository access require suitable GitHub credentials.

Restart after changes:

```bash
sudo systemctl restart mygpt-github-agent
```

### Optional: route Git through the old Worker bridge

If this VPS itself cannot reach `github.com`, the agent can use any Git Smart HTTP gateway instead:

```dotenv
GIT_REMOTE_BASE_URL=https://cloudflare-mygpt-github.wangran.workers.dev/git
GIT_REMOTE_USERNAME=git
GIT_REMOTE_TOKEN=YOUR_GIT_GATEWAY_TOKEN
GITHUB_TOKEN=
```

Then the local repository still lives on the VPS, while only clone/fetch/push traverse the Worker bridge.

## 3. Configure Cloudflare Tunnel

The agent intentionally listens only on:

```text
127.0.0.1:8787
```

Do not open port 8787 to the Internet.

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

Cloudflare Tunnel is outbound-only from the server, so the VPS does not need a public inbound application port.

For a temporary development test only, you can instead run:

```bash
cloudflared tunnel --url http://localhost:8787
```

and use the generated `trycloudflare.com` URL. The URL is temporary; use a named tunnel plus your own Cloudflare-managed domain for production.

## 4. Configure the Custom GPT Action

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

Do **not** put these values into the GPT:

```text
GITHUB_TOKEN
GIT_REMOTE_TOKEN
Cloudflare Tunnel token
```

Only `API_TOKEN` is the GPT-facing credential.

## Recommended MyGPT instruction

```text
For repository tasks, always use the cloudflare-tunnel-mygpt-github Actions before any built-in GitHub connector, web browsing, or GitHub search.

Workflow:
1. Call inspectRepository. If the repository is not local, call syncRepository.
2. Call syncRepository again only when fresh remote state is needed.
3. Use searchRepository and readFiles for targeted investigation.
4. For whole-repository analysis, repeatedly call readRepositoryPage with next_cursor until next_cursor is null.
5. Use applyChanges to edit the real local worktree.
6. Call gitDiff after edits and inspect the result before committing.
7. Use commitAndPush only after the changes are correct.
8. Do not use GitHub API/connector tools for repository file reads when these local actions can answer the request.
```

## API examples

Set the public tunnel URL and your agent token:

```bash
export BASE_URL=https://git-agent.your-domain.com
export API_TOKEN=YOUR_API_TOKEN
```

Clone a repository onto the VPS:

```bash
curl -s "$BASE_URL/v1/repository/sync" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"xiaoqianran/cloudflare-tunnel-mygpt-github"}'
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

Read files locally:

```bash
curl -s "$BASE_URL/v1/files/read" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"xiaoqianran/cloudflare-tunnel-mygpt-github","paths":["README.md","go.mod"]}'
```

## Configuration

`/etc/mygpt-github-agent.env`:

| Variable | Purpose | Default |
|---|---|---|
| `API_TOKEN` | Custom GPT -> agent Bearer credential | required |
| `WORKSPACE_ROOT` | Persistent local repository root | `/srv/mygpt/repos` |
| `LISTEN_ADDR` | Origin HTTP listener | `127.0.0.1:8787` |
| `ALLOWED_REPOS` | Optional comma-separated repo patterns; empty means all | empty |
| `GIT_REMOTE_BASE_URL` | Base URL used by native Git | `https://github.com` |
| `GIT_REMOTE_USERNAME` | HTTP Basic username for native Git | `x-access-token` |
| `GITHUB_TOKEN` | Direct GitHub token; server only | empty |
| `GIT_REMOTE_TOKEN` | Overrides `GITHUB_TOKEN`, useful for a Git gateway | empty |
| `COMMAND_TIMEOUT_SECONDS` | Git/search timeout | `300` |
| `MAX_READ_FILES` | Max files in one `readFiles` call | `50` |
| `MAX_PAGE_CHARS` | Max text budget for one repository page | `180000` |
| `MAX_WRITE_BYTES` | Max bytes written to one file in one action | `5000000` |
| `MAX_DIFF_CHARS` | Max diff returned in one response | `180000` |

`ALLOWED_REPOS` examples:

```dotenv
# all repositories
ALLOWED_REPOS=

# all repos owned by xiaoqianran
ALLOWED_REPOS=xiaoqianran/*

# explicit repositories
ALLOWED_REPOS=xiaoqianran/repo-a,xiaoqianran/repo-b
```

## Why this avoids the GitHub file-read bottleneck

After `syncRepository` has cloned the project:

```text
readFiles          -> VPS local disk
searchRepository   -> VPS local ripgrep
readRepositoryPage -> VPS local disk
applyChanges       -> VPS local disk
gitDiff            -> local .git database
```

None of those operations consumes GitHub REST API requests.

Only synchronization crosses the network:

```text
clone / fetch / push -> Git transport -> GitHub (or your optional Git gateway)
```

This means repository-scale analysis is bounded mainly by VPS disk/CPU, the Tunnel/API response size, and the model context window rather than per-file GitHub API latency.

## Root service mode

The installed systemd service deliberately runs with:

```ini
User=root
Group=root
Environment=HOME=/root
```

The previous restrictions (`NoNewPrivileges`, `ProtectSystem`, `ProtectHome`, `ReadWritePaths`) are removed. The process therefore has normal root access to the host filesystem and to child processes it starts.

The public API remains authenticated with `API_TOKEN` and the current handlers remain repository-scoped. Because the process itself is root, protect the token and keep the origin bound to `127.0.0.1` behind Cloudflare Tunnel.

## Operational notes

- The HTTP API currently does not expose an arbitrary shell endpoint.
- `applyChanges` edits local repository files only; it does not silently commit.
- `commitAndPush` stages all local changes with `git add -A`.
- `force: true` intentionally maps to `git push --force`.
- GitHub branch protection/rulesets still apply to pushes.
- A dirty worktree is never overwritten by `syncRepository`; it fetches remote state but skips checkout/fast-forward until local edits are resolved.
