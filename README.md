# mygpt-cf-tunnel

一个刻意极简的 **Custom GPT -> Cloudflare Tunnel -> VPS root shell** 后端。

从 `v0.3.0` 开始，给 Custom GPT 暴露的 Action **只保留 `runCommand`**。不再要求 GPT 分别调用 `syncRepository`、`readFiles`、`applyChanges`、`commitAndPush`、`createRelease` 等专用动作；这些事情都可以直接通过远程主机上的 shell 和 CLI 完成。

```text
Custom GPT
    │ HTTPS + Bearer API_TOKEN
    ▼
Cloudflare Tunnel
    ▼
127.0.0.1:8787
MyGPT VPS Root Shell Agent (systemd, root)
    │
    └─ runCommand
         └─ /bin/bash -lc "..."
              ├─ gh / git
              ├─ apt / apt-get / systemctl
              ├─ modal
              ├─ kaggle
              ├─ curl / wget
              ├─ python / pip / uv
              ├─ node / npm
              └─ 任何 root 能执行的命令或脚本
```

## 核心理念

只要 VPS 上能通过命令行完成，就不需要为 Custom GPT 再设计一个专门的 Action。

例如 GitHub 仓库任务可以直接：

```bash
mkdir -p /srv/mygpt/repos/xiaoqianran
cd /srv/mygpt/repos/xiaoqianran
[ -d mygpt-cf-tunnel/.git ] \
  && git -C mygpt-cf-tunnel pull --ff-only \
  || gh repo clone xiaoqianran/mygpt-cf-tunnel
```

修改、测试和推送也全部走同一个入口：

```bash
cd /srv/mygpt/repos/xiaoqianran/mygpt-cf-tunnel
git status --short
# sed/python/cat 等方式修改文件
go test ./...
git diff --check
git add -A
git commit -m 'change'
git push
```

其他平台同理：

```bash
apt-get update && apt-get install -y jq

gh auth status

gh repo view xiaoqianran/mygpt-cf-tunnel

modal token show
modal run app.py

kaggle datasets list -s imagenet
```

`runCommand` 运行在宿主机，不在容器或仓库 sandbox 内；systemd 服务用户是 `root`，所以它本质上是通过受保护 API 暴露的远程 root shell。

## Action 请求

OpenAPI 只公开：

```text
POST /v1/command/run
```

请求示例：

```json
{
  "command": "gh auth status && uname -a",
  "workdir": "/root",
  "timeout_seconds": 120
}
```

服务执行：

```text
/bin/bash -lc '<command>'
```

返回值包含：

```json
{
  "command": "...",
  "workdir": "/root",
  "exit_code": 0,
  "stdout": "...",
  "stderr": "...",
  "timed_out": false,
  "truncated": false,
  "duration_ms": 123
}
```

`timeout_seconds` 只能缩短单次命令超时，不能超过服务器端 `COMMAND_TIMEOUT_SECONDS`。

## CLI 环境

服务固定提供适合 root CLI 的基础环境：

```text
HOME=/root
USER=root
LOGNAME=root
SHELL=/bin/bash
PATH=/root/.local/bin:/root/.cargo/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin
```

因此安装到 `/usr/local/bin`、`/usr/bin` 等常见位置的 CLI 可以直接使用。

`modal`、`kaggle` 等工具可以读取 root 用户自己的配置目录，例如 `/root/.modal`、`/root/.kaggle`。

如果 `/etc/mygpt-github-agent.env` 中设置了 `GITHUB_TOKEN` 且没有设置 `GH_TOKEN`，`runCommand` 会自动为子进程补充同值的 `GH_TOKEN`，便于 `gh` CLI 使用。也可以直接通过 `gh auth login` 在 root 用户环境完成认证。

## 安装 / 升级

服务本身的构建只要求 Go 1.23+。`git`、`gh`、`modal`、`kaggle`、`rg` 等不再是安装 agent 的强制依赖；需要什么工具，可以之后通过 `runCommand` 安装。

```bash
git clone https://github.com/xiaoqianran/mygpt-cf-tunnel.git
cd mygpt-cf-tunnel
bash ./scripts/install.sh
```

已有 checkout：

```bash
git pull
bash ./scripts/install.sh
```

安装脚本会：

- 运行 `go test ./...`
- 构建当前架构的静态 Go 二进制
- 安装到 `/usr/local/bin/mygpt-github-agent`
- 创建 `/etc/mygpt-github-agent.env`（首次安装）
- 以 `root:root` 启动 systemd 服务
- 默认只监听 `127.0.0.1:8787`

默认配置：

```dotenv
API_TOKEN=<random secret>
LISTEN_ADDR=127.0.0.1:8787
COMMAND_TIMEOUT_SECONDS=1800
MAX_COMMAND_OUTPUT_CHARS=180000
GITHUB_TOKEN=
```

检查：

```bash
curl http://127.0.0.1:8787/health
curl -s http://127.0.0.1:8787/openapi.json | jq '.paths'
systemctl status mygpt-github-agent
```

## Cloudflare Tunnel

保持 origin 仅监听：

```text
127.0.0.1:8787
```

Cloudflare Tunnel 的 Published application 指向：

```text
http://localhost:8787
```

例如域名：

```text
git-agent.example.com
```

然后在 Custom GPT Actions 中导入：

```text
https://git-agent.example.com/openapi.json
```

认证选择 Bearer API Key，值为 `/etc/mygpt-github-agent.env` 中的 `API_TOKEN`。

不要把 Cloudflare Tunnel token、GitHub token、Modal token、Kaggle 凭据放进 GPT 指令。让这些凭据留在 VPS/root 用户环境中。

## 推荐 GPT 指令

```text
所有服务器、GitHub、部署、构建和文件操作优先使用 runCommand。
把 runCommand 视为远程 VPS root shell：通过 /bin/bash -lc 执行命令。

GitHub 仓库任务不需要专用 GitHub Action：直接使用 gh 和 git。
需要仓库时自行 clone/fetch/pull；读取文件用 cat/sed/rg；修改文件可用 python/sed/cat；测试后用 git diff 检查，再 commit/push。

需要其他能力时优先调用服务器上已有 CLI；缺少工具时可以使用 apt/apt-get、pip、npm 等安装，然后继续执行。
例如可以使用 gh、modal、kaggle、systemctl、curl、python、node 等。

命令失败时读取 exit_code/stdout/stderr，修复后重试。
不要因为存在内置 GitHub、网页搜索或其他连接器就绕过 runCommand；只要 VPS shell 可以完成，就优先使用 runCommand。
```

## 安全模型

这不是“有限命令执行器”，而是有意设计成：

```text
Cloudflare Tunnel
  -> Bearer API_TOKEN
  -> root systemd service
  -> /bin/bash -lc
  -> VPS root 权限
```

因此：

- `API_TOKEN` 等价于高权限远程执行凭据，必须保密。
- 不要把 `127.0.0.1:8787` 直接暴露公网。
- 建议只通过受控 Cloudflare Tunnel 暴露。
- root shell 可以修改系统、安装软件、删除文件、停止服务或改写凭据；这是本项目的设计目标，不是 sandbox。
- 输出会按 `MAX_COMMAND_OUTPUT_CHARS` 截断，命令会受 `COMMAND_TIMEOUT_SECONDS` 限制。
