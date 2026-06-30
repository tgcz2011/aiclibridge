# AICLIBridge

AICLIBridge 是一个统一 AI CLI 桥:用一个 HTTP API 同时驱动 Claude Code、Codex、OpenCode、OpenClaw、Qwen Code、Gemini CLI 六个 AI coding CLI,对外暴露 OpenAI / Anthropic 兼容接口与原生流式接口。

![CI](https://github.com/tgcz2011/aiclibridge/actions/workflows/ci.yml/badge.svg)
![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)
![License](https://img.shields.io/badge/license-Apache--2.0-blue)
![Release](https://img.shields.io/badge/release-v0.1.0-blue)

## 核心特性

- **三套接口**:OpenAI 兼容 `/v1/chat/completions` + `/v1/models`、Anthropic 兼容 `/v1/messages`、原生 `/v1/runs` SSE 流
- **六 CLI 统一**:claude / codex / opencode / openclaw / qwen / gemini,model name 形如 `claude/anthropic/claude-sonnet-4.5`
- **强容错**:panic recover、超时降级、单 CLI 故障隔离、store 失败不致命、客户端断连自动取消 run
- **不限并发**:每个 run 独立 goroutine,`sync.Map` 跟踪 live runs,SSE 长连接不设读写超时
- **单二进制**:纯 Go + modernc.org/sqlite(纯 Go 驱动,免 CGO),`go install` 即装即用
- **可重放**:每个 run 的完整事件时间线持久化到 SQLite,`GET /v1/runs/{id}` 可随时回放
- **YAML + env 配置**:`./aiclibridge.yaml` 或 `AICLIBRIDGE_*` 环境变量覆盖,零配置也可启动

## 为什么用 AICLIBridge

| 痛点 | AICLIBridge 解法 |
|---|---|
| 每个 CLI 协议不同(stream-json / JSON-RPC / NDJSON) | 统一成一套 HTTP API + 一种 model name |
| OpenAI/Anthropic SDK 想直连 coding agent | 提供兼容层,SDK 零改动接入 |
| 一个 CLI 崩了拖垮整批 | 单 CLI 故障隔离,panic 转 500,daemon 不挂 |
| 并发一高就排队 | 不设全局并发上限,每 run 独立 goroutine |
| 多工具要装一堆依赖 | 单静态二进制,无 CGO |

## 快速开始

```sh
# 1. 安装
go install github.com/tgcz2011/aiclibridge/cmd/aiclibridge@latest

# 2. 最小配置(也可零配置启动)
cat > aiclibridge.yaml <<'EOF'
listen: 127.0.0.1:8787
api_key: sk-aiclibridge-xxx
agents:
  claude: { enabled: true }
  codex:  { enabled: true }
EOF

# 3. 启动
aiclibridge --config ./aiclibridge.yaml

# 4. 验证
curl -s http://127.0.0.1:8787/healthz
curl -s -H "Authorization: Bearer sk-aiclibridge-xxx" http://127.0.0.1:8787/v1/models
```

## 支持的 CLI 矩阵

| CLI | 上游协议 | 接入方式 | 状态 |
|---|---|---|---|
| claude | stream-json (`--output-format stream-json`) | PATH / `executable_path` | stable |
| codex | JSON-RPC over stdin/stdout | PATH / `executable_path` | stable |
| opencode | NDJSON (`--output-format stream-json`) | PATH / `executable_path` | stable |
| openclaw | NDJSON + 本地 in-process(`local`/`gateway` 模式) | PATH / `executable_path` | stable |
| qwen | stream-json(Claude SDK schema) | PATH / `executable_path` | stable |
| gemini | stream-json(与 opencode 同源) | PATH / `executable_path` | experimental |

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
│ adapter: claude codex opencode openclaw qwen gemini │  ← 协议翻译
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
- [CLI 子命令](docs/cli.md) — serve / run / agents / models / cancel / get
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
