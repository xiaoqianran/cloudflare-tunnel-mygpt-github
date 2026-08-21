# mygpt-cf-tunnel

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
    ├─ runCommand
    │    └─ VPS host /bin/bash -lc (root)
    │
    └─ createRelease
         └─ VPS host gh release create
```

After a repository is cloned, **reading, searching, editing, command execution and Git operations no longer use the GitHub REST API**. GitHub is contacted through normal Git transport for clone/fetch/push and through `gh` for releases.

## What MyGPT can do

The OpenAPI actions expose:

- `syncRepository` — clone the repository on first use; later fetch and fast-forward a clean worktree
- `inspectRepository` — inspect local branch/HEAD/dirty state and list local files
- `readFiles` — batch-read files directly from server disk
- `searchRepository` — local ripgrep search; no GitHub code-search API
- `readRepositoryPage` — progressively read the complete text repository, including continuation inside large files
- `applyChanges` — write/delete files in the real local worktree
- `runCommand` — run **any shell command directly on the VPS host** as the Agent service account
- `gitDiff` — review the local diff
- `commitAndPush` — `git add -A`, commit, and native `git push`; optional force push
- `createRelease` — run host-side `gh release create` to publish a GitHub Release

The systemd API process itself runs as **root**, and `runCommand` intentionally executes directly on the host. There is no repository/container sandbox boundary for command execution anymore.

## ARM64 / aarch64

ARM servers are supported. The installer builds the Go service natively on the VPS and CI also cross-builds `linux/arm64`.

## Server requirements

Recommended for this project:

```text
Linux VPS
2 vCPU
1 GB RAM
1-2 GB swap
20 GB+ SSD
Git
GitHub CLI (`gh`)
ripgrep
Go 1.23+
cloudflared
```

The Go service itself uses only the standard library.

## 1. Install / upgrade the agent on the VPS

```bash
git clone https://github.com/xiaoqianran/mygpt-cf-tunnel.git
cd mygpt-cf-tunnel
sudo bash ./scripts/install.sh
```

For an existing checkout:

```bash
git pull
sudo bash ./scripts/install.sh
```

The installer:

- verifies GitHub CLI (`gh`), Git, Go and ripgrep are installed
- runs Go tests
- builds the Go binary natively for the current host
- runs the systemd service as `root:root`
- creates `/srv/mygpt/repos` as the persistent Git workspace
- creates/updates `/etc/mygpt-github-agent.env`
- starts the service on `127.0.0.1:8787`

It prints a generated `API_TOKEN` on the first install. Save it.

Check locally:

```bash
curl http://127.0.0.1:8787/health
systemctl status mygpt-github-agent
```

Expected health response:

```json
{"ok":true,"service":"cloudflare-tunnel-mygpt-github","version":"0.2.3"}
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

Confirm the actions exist:

```bash
curl -s http://127.0.0.1:8787/openapi.json | jq -r '.paths | keys[]'
```

You should see:

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

The token stays only on the VPS. It is injected into Git HTTP authentication through the child process environment and is not written into repository remotes. `createRelease` also exposes the same token to `gh` as `GH_TOKEN` for that child process only.

Restart after changes:

```bash
sudo systemctl restart mygpt-github-agent
```

## 3. Configure command execution

`runCommand` is now a **host-side root command runner**.

The request body is simply:

```json
{
  "command": "gh auth status",
  "workdir": "/root"
}
```

The service executes:

```text
/bin/bash -lc '<command>'
```

on the VPS host. Pipes, redirects, shell variables, `sudo`, `systemctl`, `gh`, `git`, package managers and arbitrary scripts are available according to the root service environment.

Output remains capped by `MAX_COMMAND_OUTPUT_CHARS`, and execution is limited by `COMMAND_TIMEOUT_SECONDS`.

Because the API is exposed through Cloudflare Tunnel, **keep the API token secret and do not expose the origin directly to the Internet**.

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
mygpt-cf-tunnel
```

Cloudflare gives an installation command similar to:

```bash
sudo cloudflared service install <TUNNEL_TOKEN>
```

Then add a Published application route:

```text
Hostname:  git-agent.your-domain.com
Service:   http://localhost:8787
```

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
For repository and VPS tasks, always use cloudflare-tunnel-mygpt-github Actions before any built-in GitHub connector, GitHub search or web browsing.

Workflow:
1. Call inspectRepository. If the repository is not local, call syncRepository.
2. Call syncRepository again only when fresh remote state is required.
3. Use searchRepository and readFiles for targeted investigation.
4. For whole-repository analysis, repeatedly call readRepositoryPage with next_cursor until null.
5. Use applyChanges for direct file edits.
6. Use runCommand for any VPS command, project setup, dependency installation, builds, tests, linters, gh, git and systemctl.
7. When a command fails, inspect stdout/stderr, fix the problem, and rerun it.
8. Call gitDiff after repository edits and tests.
9. Use commitAndPush only when repository changes are correct.
10. Use createRelease when a GitHub Release should be published.
11. Do not use built-in GitHub tools for repository file reads when these local actions can answer the request.
```

## Host-side release example

You can now publish directly from the Agent using either `createRelease` or `runCommand`:

```bash
curl -s http://127.0.0.1:8787/v1/command/run \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"command":"gh release create v0.2.3 --repo xiaoqianran/mygpt-cf-tunnel --title v0.2.3 --generate-notes"}' | jq
```

Or use the structured `createRelease` action for a safer release-specific interface.

## Configuration

`/etc/mygpt-github-agent.env`:

| Variable | Purpose | Default |
|---|---|---|
| `API_TOKEN` | Custom GPT -> agent Bearer credential | required |
| `WORKSPACE_ROOT` | Persistent local repository root | `/srv/mygpt/repos` |
| `LISTEN_ADDR` | Origin HTTP listener | `127.0.0.1:8787` |
| `ALLOWED_REPOS` | Comma-separated repo patterns for repository APIs | empty |
| `GIT_REMOTE_BASE_URL` | Base URL used by native Git | `https://github.com` |
| `GIT_REMOTE_USERNAME` | HTTP Basic username for native Git | `x-access-token` |
| `GITHUB_TOKEN` | Direct GitHub token; server only | empty |
| `GIT_REMOTE_TOKEN` | Overrides `GITHUB_TOKEN` for Git transport | empty |
| `COMMAND_TIMEOUT_SECONDS` | Maximum command/Git operation time | `300` |
| `MAX_COMMAND_OUTPUT_CHARS` | Maximum captured stdout/stderr chars per stream | `180000` |
| `MAX_READ_FILES` | Max files in one `readFiles` call | `50` |
| `MAX_PAGE_CHARS` | Max text budget for one repository page | `180000` |
| `MAX_WRITE_BYTES` | Max bytes written to one file in one action | `5000000` |
| `MAX_DIFF_CHARS` | Max diff returned in one response | `180000` |

## Security model

There is intentionally **no container sandbox around `runCommand` anymore**.

```text
Cloudflare Tunnel
  -> Bearer API authentication
  -> Agent process (root)
  -> /bin/bash -lc command (root on VPS)
```

This is intentionally equivalent to granting the GPT API a remote root shell through the protected tunnel. Keep `API_TOKEN` private, keep the origin bound to `127.0.0.1`, and only expose the service through a trusted Cloudflare Tunnel.

Repository file APIs still enforce their path safety checks and `ALLOWED_REPOS` policy.

## Operational notes

- `runCommand` no longer requires a repository to be cloned first.
- `runCommand` can execute any valid shell command on the VPS host.
- `runCommand` accepts an optional host filesystem `workdir`.
- `runCommand` captures stdout/stderr with a size limit and enforces a timeout.
- `applyChanges` edits local repository files only; it does not silently commit.
- `commitAndPush` stages all local changes with `git add -A`.
- `force: true` intentionally maps to `git push --force`.
- GitHub branch protection/rulesets still apply to pushes.
- `createRelease` uses the host `gh` CLI and may use `GITHUB_TOKEN` as `GH_TOKEN`.
