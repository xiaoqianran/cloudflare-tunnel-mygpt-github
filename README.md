# mygpt-cf-tunnel

把 Custom GPT 连接到一台真实 VPS 的 **通用 root shell**。整个 Action 面只有一个操作：`runCommand`。

它不是 GitHub 工具集合，也不是预装 CLI 的固定能力列表。模型可以直接使用服务器已有能力；缺什么，就通过 root shell 安装什么，然后继续组合完成工作流。

> **推荐 GPT 描述**：连接远程 VPS 的 root shell。通过唯一的 `runCommand` 自主安装所需软件、调用任意 CLI、读写文件、运行脚本与服务，并完成服务器能够执行的任意工作流。

可直接粘贴到 GPT Builder 的完整指令见 [`GPT_INSTRUCTIONS.md`](./GPT_INSTRUCTIONS.md)。

## 架构

```text
Custom GPT
    │  HTTPS + Bearer API_TOKEN
    ▼
Cloudflare Tunnel
    ▼
127.0.0.1:8787
Universal VPS Root Shell Agent (systemd, root)
    │
    └── runCommand
          │
          └── /bin/bash -lc <command>
                 │
                 ├── 使用服务器已有程序
                 ├── apt / pip / npm / cargo / go install / curl ...
                 ├── 安装新的运行时、CLI、库或系统包
                 ├── 写脚本 / 编译程序 / 调 API / 管服务
                 └── 组合成服务器能够执行的任意非交互式工作流
```

`gh`、`git`、`modal`、`kaggle`、Python、Node、Go、Docker、数据库客户端或云平台 CLI 都只是示例。**它们不是能力边界。**

## 设计原则

### 一个 Action，而不是一堆专用 API

OpenAPI 只公开：

```text
POST /v1/command/run
```

过去的 `syncRepository`、`readFiles`、`applyChanges`、`gitDiff`、`commitAndPush`、`createRelease` 等专用 API 已从代码中删除。

仓库任务直接由 shell 完成，例如：

```bash
gh repo clone owner/repo /srv/work/repo
cd /srv/work/repo
rg 'target'
python3 scripts/change.py
go test ./...
git diff --check
git add -A
git commit -m 'change'
git push
```

如果 `gh` 不存在，可以先安装；如果项目需要另一个运行时，也可以先安装那个运行时。工具选择属于工作流的一部分，而不是 Agent API 的一部分。

### 能力可以在运行时扩展

`runCommand` 是 root shell，所以模型可以先获取能力，再完成任务。例如：

```bash
apt-get update && apt-get install -y jq
python3 -m pip install --user some-cli
npm install -g some-cli
cargo install some-cli
go install example.com/tool@latest
curl -fsSL https://example.com/install.sh | bash
```

上述方式仍然只是示例。只要 VPS 的操作系统、网络和权限允许，可以采用适合目标的安装或构建方式。

### 真实主机，不是仓库 sandbox

`workdir` 可以是 root 能访问的任意真实主机目录，默认 `/root`。命令可以管理系统包、进程、systemd 服务、文件、网络请求、代码、数据和部署目标。

因此 `runCommand` 在 OpenAPI 中明确标记为：

```json
"x-openai-isConsequential": true
```

它具有真实系统副作用，不应伪装成只读或非 consequential 操作。

## Action 请求

```json
{
  "command": "set -euo pipefail\nuname -a\ncommand -v jq || apt-get update && apt-get install -y jq\njq --version",
  "workdir": "/root",
  "stdin": "",
  "timeout_seconds": 300
}
```

执行语义：

```text
root user
  -> /bin/bash -lc
  -> real VPS filesystem / network / processes / credentials
```

返回：

```json
{
  "workdir": "/root",
  "exit_code": 0,
  "stdout": "...",
  "stderr": "...",
  "timed_out": false,
  "truncated": false,
  "duration_ms": 123
}
```

`stdin` 可用于非交互式 CLI 输入、脚本、payload 或文件内容。

`timeout_seconds` 只能缩短本次调用的时间，不能突破服务器端 `COMMAND_TIMEOUT_SECONDS`。超时后 Agent 会终止整个 shell 进程组，避免留下意外的子进程。

## 环境与凭据

systemd 服务以 `root:root` 运行，并提供常见工具安装路径：

```text
/root/.local/bin
/root/.cargo/bin
/root/go/bin
/usr/local/go/bin
/usr/local/sbin
/usr/local/bin
/usr/sbin
/usr/bin
/sbin
/bin
/snap/bin
```

同时保留宿主机已有 PATH，并使用 root 的 login shell 配置。

`/etc/mygpt-github-agent.env` 中除了 Agent 自己的配置外，其他环境变量也会被 `runCommand` 子进程继承。这样第三方 CLI 的 token 或非秘密配置可以留在 VPS，而不需要写进 GPT 指令。

不要让模型为了“检查配置”而输出完整 `env`、token 文件或密钥。认证状态应尽量通过对应 CLI 的 status/auth 命令检查。

## 安装 / 升级

当前仍保留历史二进制和 systemd 单元名称 `mygpt-github-agent`，这是为了兼容已部署服务器的原地升级；它们不再代表 Agent 的能力边界。

服务本身构建只要求 Go 1.23+：

```bash
git clone https://github.com/xiaoqianran/mygpt-cf-tunnel.git
cd mygpt-cf-tunnel
bash ./scripts/install.sh
```

默认配置：

```dotenv
API_TOKEN=<random secret>
LISTEN_ADDR=127.0.0.1:8787
COMMAND_TIMEOUT_SECONDS=1800
MAX_COMMAND_OUTPUT_CHARS=180000
```

安装后检查：

```bash
curl http://127.0.0.1:8787/health
curl -s http://127.0.0.1:8787/openapi.json | jq '.paths'
systemctl status mygpt-github-agent
```

## Cloudflare Tunnel

Agent 默认只监听：

```text
127.0.0.1:8787
```

Cloudflare Tunnel 的 Published application 指向：

```text
http://localhost:8787
```

然后在 Custom GPT Actions 中导入：

```text
https://<你的域名>/openapi.json
```

认证使用 Bearer API Key，值为 `/etc/mygpt-github-agent.env` 中的 `API_TOKEN`。

## 安全模型

这是有意设计的高权限执行入口：

```text
Cloudflare Tunnel
  -> Bearer API_TOKEN
  -> root systemd service
  -> /bin/bash -lc
  -> VPS root 权限
```

因此：

- `API_TOKEN` 等价于高权限远程执行凭据，必须保密。
- Agent origin 应继续只监听 loopback，不要直接暴露公网。
- root shell 可以安装软件、修改系统、删除文件、停止服务、访问 root 可读取的凭据；这是设计目标，不是 sandbox。
- OpenAPI 把操作标记为 consequential，准确反映它的真实副作用。
- 输出会按 `MAX_COMMAND_OUTPUT_CHARS` 截断；超时由 `COMMAND_TIMEOUT_SECONDS` 控制。
