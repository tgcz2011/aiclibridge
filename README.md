# AICLIBridge

AICLIBridge 是一个统一 AI CLI 桥:用一个 HTTP API 同时驱动 Claude Code、Codex、OpenCode、OpenClaw、Qwen Code、Gemini CLI 六个 AI coding CLI,对外暴露 OpenAI / Anthropic 兼容接口与原生流式接口。

![CI](https://github.com/tgcz2011/aiclibridge/actions/workflows/ci.yml/badge.svg)
![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)
![License](https://img.shields.io/badge/license-Apache--2.0-blue)
![Release](https://img.shields.io/badge/release-v0.5.2-blue)

## 核心特性

- **一键安装**:`curl -fsSL ... | sh`(macOS/Linux)和 `irm ... | iex`(Windows)一行装好,自动探测 GOOS/GOARCH、下载 sha256 校验、装到 `/usr/local/bin` 或 `$HOME\bin`(免管理员);详见 [scripts/install.sh](scripts/install.sh) / [scripts/install.ps1](scripts/install.ps1)
- **自动更新检测**:`aiclibridge update check` 查 GitHub releases,有新版则打印升级提示;`--json` 机器可读、`--quiet` 静默;daemon 启动时异步检查并在日志里打一行 hint,网络/限流失败一律静默
- **三套接口**:OpenAI 兼容 `/v1/chat/completions` + `/v1/models`、Anthropic 兼容 `/v1/messages`、原生 `/v1/runs` SSE 流
- **token / 价格统计**:`/v1/stats/usage`、`/v1/stats/prices`、`/v1/stats/summary` 三端点,per-model token 用量 + USD 估算
- **并发上限 + 排队**:`max_concurrent_runs`(默认 8)+ `queue_timeout_ms`(默认 60s)信号量,超出排队等待,超时返回 `503 + Retry-After: 5`;`/v1/stats/concurrency` 实时查询 active/queued
- **安全加固**:`/debug/pprof/` 在非 loopback 监听(如 `0.0.0.0`)时自动加 API-key 鉴权;Claude 的 `--permission-mode` 改为可配置(`permission_mode`),空值回退 `bypassPermissions` 保持 v0.3 行为
- **全平台 daemon**:Unix(Setsid + PID + SIGTERM/SIGKILL)、Windows(尽力而为:CREATE_NEW_PROCESS_GROUP + CTRL_BREAK_EVENT,无优雅 SIGTERM),`start` / `stop` / `restart` / `upgrade` 跨平台可用
- **schema 迁移框架**:`schema_migrations` 表 + 顺序事务迁移,v0.3 升级自动兼容(检测已有 `usage_json` 列则跳过 ALTER 仅记录 version),告别脆弱的手动 `ALTER TABLE + pragma` 检查
- **CLI 版本检测**:`CheckCLIVersion` + `WarnOnVersion` helpers(`internal/adapter/helpers.go`),适配器启动时打印低于最低版本的 warning,避免 CLI 改 flag 语义后无声失败
- **内存优化**:非流式响应 `collectEvents` 上限 10000 事件,超限丢弃中间事件但保留 terminal `EventResult`,store 仍持久化全部事件供回放
- **去重初始化**:`runDaemonForeground` 与 `runServe` 共用 `serveStack`,消除 ~40 行重复
- **高并发优化**:SQLite WAL 模式 + 连接池(max(4, NumCPU))+ busy_timeout,事件缓冲 256→1024
- **后台 daemon**:`start` / `stop` / `restart` / `upgrade` 子命令,fork 脱离终端,固定端口,PID 文件管理
- **per-request 传参**:`run -- --pure` 用 `--` 分隔符向底层 CLI 透传 flag(如禁用 opencode plugin)
- **六 CLI 统一**:claude / codex / opencode / openclaw / qwen / gemini,model name 形如 `claude/anthropic/claude-sonnet-4.5`
- **强容错**:panic recover、超时降级、单 CLI 故障隔离、store 失败不致命、客户端断连自动取消 run
- **单二进制**:纯 Go + modernc.org/sqlite(纯 Go 驱动,免 CGO),`go install` 即装即用
- **可重放**:每个 run 的完整事件时间线持久化到 SQLite,`GET /v1/runs/{id}` 可随时回放
- **YAML + env 配置**:`./aiclibridge.yaml` 或 `AICLIBRIDGE_*` 环境变量覆盖,零配置也可启动

## 为什么用 AICLIBridge

| 痛点 | AICLIBridge 解法 |
|---|---|
| 每个 CLI 协议不同(stream-json / JSON-RPC / NDJSON) | 统一成一套 HTTP API + 一种 model name |
| OpenAI/Anthropic SDK 想直连 coding agent | 提供兼容层,SDK 零改动接入 |
| 一个 CLI 崩了拖垮整批 | 单 CLI 故障隔离,panic 转 500,daemon 不挂 |
| 并发一高就 OOM / 拖垮机器 | 可配置并发上限 + 排队,超时 503 + Retry-After |
| pprof / 权限 mode 等安全细节疏漏 | 非 loopback pprof 自动加 auth,permission_mode 可配 |
| 安装/升级麻烦,要装 Go 或手动下二进制 | 一行 `curl\|sh` / `irm\|iex` 装好,`aiclibridge update check` 检测新版本 |
| 多工具要装一堆依赖 | 单静态二进制,无 CGO |

## 快速开始

### 一键安装(macOS / Linux)

```sh
curl -fsSL https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.sh | sh
```

> 注意:URL **不要加反引号**(`` ` ``),直接写裸 URL 即可。反引号在 shell 中是命令替换语法,会导致 URL 解析错误。

### 一键安装(Windows,PowerShell)

```powershell
irm https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.ps1 | iex
```

脚本自动探测 GOOS/GOARCH(darwin/linux × amd64/arm64、windows-amd64)、下载对应 tarball/zip、`sha256` 校验、装到 `/usr/local/bin`(不可写则 fallback `~/.local/bin`,Windows 装 `$env:USERPROFILE\bin`)。可选 `--bin` / `--version` / `--force`,详见 `scripts/install.sh -h`。

### 中国大陆网络问题

如果 `github.com` 连接超时或 `api.github.com` 返回 403,有三种解法:

```sh
# 1. 用 GitHub 镜像前缀(脚本会把它加到下载 URL 前)
GITHUB_MIRROR=https://ghproxy.com sh scripts/install.sh
# 或:
curl -fsSL https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.sh | GITHUB_MIRROR=https://ghproxy.com sh

# 2. 走 https_proxy 代理(脚本里的 curl 会自动读取)
https_proxy=http://127.0.0.1:7890 sh scripts/install.sh

# 3. 跳过版本探测,直接指定版本(避免 api.github.com 调用)
curl -fsSL https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.sh | sh -s -- --version v0.5.1
```

脚本内部用 `--http1.1` 避免 HTTP/2 framing 错误,`--retry 3` 应对网络波动,并通过 `github.com/.../releases/latest` 的 302 重定向获取最新版本号(不消耗 API 限额,`api.github.com` 仅作 fallback)。

### 从源码安装

```sh
go install github.com/tgcz2011/aiclibridge/cmd/aiclibridge@latest
```

### 配置 + 启动

```sh
# 最小配置(也可零配置启动)
cat > aiclibridge.yaml <<'EOF'
listen: 127.0.0.1:8787
api_key: sk-aiclibridge-xxx
agents:
  claude: { enabled: true }
  codex:  { enabled: true }
EOF

# 启动(前台)
aiclibridge --config ./aiclibridge.yaml
# 或后台 daemon
aiclibridge start --config ./aiclibridge.yaml

# 验证
curl -s http://127.0.0.1:8787/healthz
curl -s -H "Authorization: Bearer sk-aiclibridge-xxx" http://127.0.0.1:8787/v1/models
```

### 检查更新

```sh
aiclibridge update check             # 打印升级提示(best-effort,失败 exit 0)
aiclibridge update check --json      # 机器可读输出
aiclibridge update check --quiet     # 无更新时静默
```

daemon 启动时也会异步检查一次,有新版本则在日志里打一行 hint。

## 支持的 CLI 矩阵

v0.2 支持 19 个 CLI，分三层：

### Stable（v0.1，已验证）

| CLI | 上游协议 | 接入方式 |
|---|---|---|
| claude | stream-json (`--output-format stream-json`) | PATH / `executable_path` |
| codex | JSON-RPC over stdin/stdout | PATH / `executable_path` |
| opencode | NDJSON (`--output-format stream-json`) | PATH / `executable_path` |
| openclaw | NDJSON + 本地 in-process(`local`/`gateway` 模式) | PATH / `executable_path` |
| qwen | stream-json(Claude SDK schema) | PATH / `executable_path` |

### Experimental（v0.2，基于 AionUi ACP 协议调研）

| CLI | 协议 | 说明 |
|---|---|---|
| gemini | stream-json(与 opencode 同源) | 本机未装，假设性适配 |
| codebuddy | stream-json(Claude SDK schema) | 本机已装 v2.113.0，schema 假设与 qwen 一致 |
| copilot | ACP JSON-RPC | 本机已装 v1.0.65，MCP 用 `--additional-mcp-config` |
| goose | ACP JSON-RPC | `goose acp` 子命令入口 |
| cursor | ACP JSON-RPC | `cursor-agent` 二进制 |
| kimi | ACP JSON-RPC | Moonshot Kimi CLI |
| kiro | ACP JSON-RPC | AWS Kiro |
| qoder | ACP JSON-RPC | Qoder CLI |
| hermes | ACP JSON-RPC | NousResearch hermes-agent |
| auggie | ACP JSON-RPC | Auggie CLI |

### Stub（v0.2，协议未知）

| CLI | 说明 |
|---|---|
| droid | 推测 Factory.ai droid，未在 AionUi TS 代码中发现 |
| snow | 完全无信息，可能仅闭源 aioncore |
| vibe | 完全无信息，可能仅闭源 aioncore |
| aion | 推测 AionUi 自家后端 |

Stub 适配器返回 `ErrNotImplemented`，在 `/v1/agents` 标记为 `available: false`。

## 架构

```
        ┌──────────┐  OpenAI SDK / Anthropic SDK / curl / CLI
        │  client  │
        └────┬─────┘
             │ HTTP (Bearer / x-api-key)
┌────────────▼──────────────────────────────────────┐
│ api: /v1/chat/completions /v1/messages /v1/runs     │  ← 兼容层 + 原生层
│      /v1/models /v1/agents /v1/providers /healthz   │
├────────────────────────────────────────────────────┤
│ facade: 路由 CLI/provider/model → 选 adapter         │  ← 编排 + 持久化
│         每 run 一 goroutine,事件流聚合到 channel    │
├────────────────────────────────────────────────────┤
│ adapter: claude codex opencode openclaw qwen gemini  │  ← 协议翻译
│          codebuddy copilot goose cursor kimi kiro   │
│          qoder hermes auggie droid snow vibe aion    │
└────────────┬───────────────────────────────────────┘
             │ exec 子进程 (stdin/stdout 流)
   ┌─────────▼─────────┐
   │ 各 AI coding CLI  │  ← claude / codex / opencode / ...
   └───────────────────┘
横切:store(SQLite 持久化) · detect(CLI 发现) · config(YAML+env)
```

## 文档

- [快速开始](docs/quickstart.md) — 5 分钟接入,含 SDK 示例
- [配置参考](docs/configuration.md) — 字段表、env 覆盖、完整示例
- [API 参考](docs/api.md) — 所有端点、请求/响应、SSE 事件 schema
- [CLI 子命令](docs/cli.md) — serve / start / stop / restart / upgrade / run / agents / models / cancel / get
- [架构设计](docs/architecture.md) — 分层、并发模型、容错、扩展指南
- 示例:[openai-python](examples/openai-python.py) · [anthropic-python](examples/anthropic-python.py) · [curl](examples/curl.sh) · [完整配置](examples/config-full.yaml)

## 开发

```sh
make dev      # go run ./cmd/aiclibridge
make build    # 构建到 ./aiclibridge
make test     # go test ./...
make vet      # go vet ./...
```

要求 Go 1.24+。CI 在 macOS / Ubuntu 上跑 build + vet + test。

## License

Apache-2.0,详见 [LICENSE](./LICENSE)。
