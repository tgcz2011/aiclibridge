# 架构设计

AICLIBridge 的分层、并发模型、容错机制、协议翻译与扩展指南。所有描述基于真实源码,文末标注关键文件位置。

## 本页内容

- [分层架构](#分层架构)
- [ASCII 架构图](#ascii-架构图)
- [并发模型](#并发模型)
- [容错机制](#容错机制)
- [协议翻译](#协议翻译)
- [model name 三段式](#model-name-三段式)
- [SQLite 持久化](#sqlite-持久化)
- [MCP 投递](#mcp-投递)
- [扩展指南:加一个新 adapter](#扩展指南加一个新-adapter)

## 分层架构

AICLIBridge 自上而下分五层,每层职责单一、边界清晰:

| 层 | 包 | 职责 |
|---|---|---|
| **cmd** | `cmd/aiclibridge` | 子命令分发(serve/run/agents/...);本地子命令在进程内组装 stack,HTTP 子命令只读 config |
| **api** | `internal/api` | HTTP 路由、请求解码、响应塑形、middleware(CORS/recover/logging/auth)、OpenAI/Anthropic 兼容层 + 原生层 |
| **facade** | `internal/facade` | 编排:解析路由键、选 adapter、每 run 起 goroutine、聚合事件流、持久化、暴露 cancel/replay |
| **adapter** | `internal/adapter` | 协议翻译:每个 CLI 一个 Backend,把统一请求模型翻译成各 CLI 的 wire 协议 |
| (子进程) | 外部 CLI | claude / codex / opencode / openclaw / qwen / gemini |

横切关注点(不属单一层):

- **store**(`internal/store`):SQLite 持久化,被 facade 调用。
- **detect**(`internal/detect`):CLI 发现与硬编码 catalog,被 facade / api 共享。
- **config**(`internal/config`):YAML + env,被所有层读取。
- **protocol**(`pkg/protocol`):对外稳定的 SSE 事件 schema,被 api 与 facade 共享。

### 调用链(以 `POST /v1/runs` 流式为例)

```
client → api.handleCreateRun          (解码 nativeRunRequest)
       → facade.StartRun              (resolveRoute → 选 adapter → store.CreateRun → safeExecute → 起 forwarder goroutine)
         → adapter.<cli>Backend.Execute   (spawn 子进程,协议翻译,产 Messages/Result channel)
         ← forwardEvents goroutine        (Message→Event 转换,appendEvent 持久化,trySend 到 live channel)
       ← api.streamNativeEvents       (protocol.WriteSSEEvent 写帧,客户端断连则 handle.Cancel)
```

## ASCII 架构图

```
        ┌──────────┐  OpenAI SDK / Anthropic SDK / curl / aiclibridge CLI
        │  client  │
        └────┬─────┘
             │ HTTP (Authorization: Bearer / x-api-key)
┌────────────▼──────────────────────────────────────────────┐
│ api (internal/api)                                          │
│   /v1/chat/completions  /v1/messages  /v1/runs              │
│   /v1/models /v1/agents /v1/providers /healthz             │
│   middleware: CORS → recover → logging → auth → handler     │
├─────────────────────────────────────────────────────────────┤
│ facade (internal/facade)                                    │
│   resolveRoute(CLI/provider/model) → 选 adapter            │
│   每 run 一 goroutine,256-buffered Event channel           │
│   sync.Map 跟踪 live runs(cancel/close 可达)              │
├─────────────────────────────────────────────────────────────┤
│ adapter (internal/adapter)                                  │
│   ┌────────┬───────┬──────────┬──────────┬──────┬───────┐   │
│   │ claude │ codex │ opencode │ openclaw │ qwen │ gemini│   │
│   └───┬────┴───┬───┴────┬─────┴────┬─────┴──┬───┴───┬───┘   │
└───────┼────────┼────────┼──────────┼────────┼───────┼───────┘
        │ exec 子进程 (stdin/stdout 流)
   ┌────▼────────▼────────▼──────────▼────────▼───────▼────┐
   │  各 AI coding CLI  (claude / codex / opencode / ...)  │
   └───────────────────────────────────────────────────────┘
横切:store(SQLite) · detect(CLI 发现+catalog) · config(YAML+env) · protocol(SSE schema)
```

## 并发模型

AICLIBridge **不设全局并发上限**,为高并发调用而设计:

- **每 run 独立 goroutine**:`facade.StartRun` 起 `forwardEvents` goroutine,run 之间互不阻塞。
- **`sync.Map` 跟踪 live runs**:key=runID,value=`*RunHandle`。`CancelRun` / `Close` 通过遍历 map 找到 handle,无需持锁。run 结束时 forwarder `defer f.runs.Delete(runID)`。
- **事件 channel 缓冲 256**:`eventsChanBuffer=256`,与适配器 Message channel 同缓冲。中间事件用 `trySend`(非阻塞)——缓冲满则从 live 流丢弃(store 仍保留用于回放)。
- **终止事件阻塞发送 + 超时兜底**:terminal `EventResult` 用 `select { case ch <- ev: case <-time.After(30s): }`(`terminalSendTimeout`),保证死掉的消费者不会永久 wedge forwarder。
- **SQLite 单写连接**:`store.Open` 设 `SetMaxOpenConns(1)`,modernc.org/sqlite 在连接层串行化写。
- **SSE 长连接无读写超时**:`http.Server` 仅设 `ReadHeaderTimeout=10s`(防慢loris),SSE handler 可无限期持有连接。

## 容错机制

单点故障**绝不**扩散到 daemon 或其它 run:

### 1. panic recover(双重)

- **handler 层**:`recoverMiddleware` 捕获 handler 链任意 panic,转 500 `server_error`,带栈日志。响应仅在未 commit 时写入;流式中途 panic 只能截断。
- **facade 层**:`safeExecute` 包裹 `backend.Execute`,panic 转 error;`forwardEvents` 整体 `defer recover()`,panic 时补发 `error` 事件 + `result{status:failed}` 并关闭 channel。recover 用 LIFO defer 顺序保证在 `close(eventsCh)` 之前执行,仍能向 channel 发事件。

### 2. 超时降级

- `TimeoutMs>0` 时 `deriveContext` 用 `context.WithTimeout`;`0` 用纯 `context.WithCancel`——持续发事件的 session 不会被仅因运行时间长而杀掉。
- 适配器内部还有 watchdog(如 codex 的 semantic inactivity timeout、first-turn no-progress timeout),见各 adapter 文件。

### 3. store 失败不致命

`CreateRun` / `AppendEvent` / `FinishRun` / `SaveSession` 失败仅 `logger.Warn` 并 swallow。run 照常执行,只是失去历史(replay 不可用)。适配器 Session 是 source of truth,store 是 persistence helper。

### 4. 单 CLI 故障隔离

- `detect.Discover` 并行探测每 CLI(各 10s 超时),单个失败标记 `Available=false`,不阻止 daemon 启动。
- `adapter.New` 失败(如路径错误)只让该 agent 不进 facade 的 adapter map,其它 agent 正常。

### 5. deadlock 防护

- 中间事件非阻塞 `trySend`(缓冲满则丢 live,不丢 store)。
- 终止事件阻塞但有 30s 超时兜底。
- `resultWaitTimeout=30s`:Messages 关闭后等 Result 最多 30s,防止适配器关闭 Messages 却不发 Result 的 bug。
- 非流式 run(Stream=false)facade 起一个 `drainEvents` goroutine 消费 live channel,保证 forwarder 的终止发送总有消费者。

### 6. 客户端断连自动 cancel

- `streamNativeEvents` / `streamOpenAIChat` / `streamAnthropicMessages` / `collectEvents` 都 `select` 监听 `r.Context().Done()`;客户端断连触发 `handle.Cancel()`,context 取消传播到适配器子进程。
- 避免无人消费的 run 永久 wedge forwarder。

## 协议翻译

六个 CLI 的上游 wire 协议各异,适配器把它们统一成 `adapter.Message` + `adapter.Result` 流:

| CLI | 上游协议 | 适配器 spawn 命令(关键 flag) | 源码 |
|---|---|---|---|
| **claude** | stream-json | `claude -p --output-format stream-json --input-format stream-json --verbose --strict-mcp-config --permission-mode bypassPermissions --disallowedTools AskUserQuestion [--model X --effort X --max-turns N --append-system-prompt X --resume X]` | `claude.go` |
| **codex** | JSON-RPC 2.0 (app-server) | `codex app-server --listen stdio://`(initialize → thread/start or thread/resume → turn/start) | `codex.go` |
| **opencode** | NDJSON | `opencode run --format json --dangerously-skip-permissions [--dir C --model X --variant X --prompt X --session X] <prompt>` | `opencode.go` |
| **openclaw** | NDJSON + 本地 | `openclaw agent [--local] --json --session-id X [--timeout N --agent X] --message <prompt>` | `openclaw.go` |
| **qwen** | stream-json(Claude SDK schema) | `qwen --bare --output-format stream-json --input-format stream-json --yolo [-m X --max-session-turns N --append-system-prompt X --resume X]` | `qwen.go` |
| **gemini** | stream-json(与 opencode 同源,假设) | `gemini --bare --output-format stream-json --input-format stream-json --yolo [--model X --append-system-prompt X --max-turns N --resume X]` | `gemini.go` |

### 各协议要点

- **claude / qwen / gemini**:stream-json NDJSON I/O。stdin 写 user-turn 帧 `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}`,stdout 按行扫 stream-json 事件。qwen 的 schema 与 Claude Code SDK 一致;gemini 假设与 opencode/qwen 同源,复用 opencode 事件解析器。
- **codex**:JSON-RPC 2.0 over stdin/stdout。daemon 发 `initialize` → `thread/start`(或 `thread/resume` 当有 ResumeSessionID)→ `turn/start`;codex 回 `item/*` / `turn/*` 通知。权限请求(`permissions/requestApproval`)被自动批准。thinking 走 `model_reasoning_effort`(JSON-RPC params,非 CLI flag)。resume 是 JSON-RPC `thread/resume`,**不**走 `--resume` CLI flag。
- **opencode**:NDJSON,`--format json`。MCP 不走 flag,走 `OPENCODE_CONFIG_CONTENT` env。thinking 是 `--variant`(模型变体名)。
- **openclaw**:NDJSON,`--json`。model 不在运行时指定(在 `openclaw agents add/update --model` 时绑定),daemon 用 `--agent <id>` 选择。`--local`(默认)走本地 in-process;`openclaw_mode: gateway` 时去掉 `--local` 走网关。session-id 由 daemon 生成。

### blocked args 防护

每个适配器维护一个 `blockedArgs` 表(`helpers.go` 的 `filterCustomArgs`),用户在 `custom_args` 里写协议关键 flag 会被丢弃并 warn,例如 claude 的 `--output-format` / `--mcp-config` / `--effort` / `-p`。这防止误配置破坏 daemon↔CLI 协议契约。

## model name 三段式

`CLI/provider/model` 是贯穿全系统的路由键:

- **解析**:`detect.ParseModelName` 拆三段,校验非空、CLI 段大小写不敏感归小写,provider/model 段原样保留。
- **路由**:`facade.resolveRoute` 用 CLI 段选 adapter map,provider/model 段透传给适配器(`opts.Model`)。
- **catalog**:`detect.ModelName(cli, provider, model)` 重组,用于 `/v1/models`、`/v1/agents` 输出。
- **裸名回退**:`api.resolveModel` 对不含 `/` 的裸名在 catalog 中按 model 名首配查找并补全。

`supportedCLIs` 顺序(`catalog.go`):claude → codex → opencode → openclaw → qwen → gemini。`DefaultCatalog()` 与 `Discover()` 都按此顺序输出,保证 listing 稳定可测。

## SQLite 持久化

- **驱动**:modernc.org/sqlite(纯 Go,免 CGO,单静态二进制)。
- **库文件**:`<data_dir>/aiclibridge.db`;`run` 子命令用 `:memory:`(一次调用不留文件)。
- **schema**(`schema.sql`,Open 时幂等应用):
  - `runs(id PK, adapter, model, status, started_at, finished_at, cwd, cli_session_id, error)`
  - `events(run_id, seq, type, payload_json, PK(run_id,seq))`——`payload_json` 是 SSE data 行 JSON,`ListEvents` 可直接回放给 EventSource。
  - `sessions(id PK, adapter, cli_session_id, created_at)`——aiclibridge session id ↔ CLI session id(用于 resume)。
  - `idx_events_run` 索引。
- **写入时机**:forwarder 每收到一个 Message 立即 `AppendEvent`;终止后 `FinishRun`(status/cli_session_id/error)+ `SaveSession`(若有 session id)。
- **回放**:`GET /v1/runs/{id}` → `store.GetRun` + `store.ListEvents` → 重组 `RunResult`(含完整 Events 时间线)。

## MCP 投递

MCP server 配置(`agents.<cli>.mcp_config`,内联 YAML,加载时重编码为 JSON)按 CLI 不同方式投递:

| CLI | 投递方式 | 源码 |
|---|---|---|
| claude | 写临时文件,`--mcp-config <path>` | `claude.go` |
| qwen | 写临时文件,`--mcp-config <path>`(qwen 原生支持) | `qwen.go` |
| gemini | 写临时文件,`--mcp-config <path>` | `gemini.go` |
| opencode | `OPENCODE_CONFIG_CONTENT=<json>` env(opencode 无 `--mcp-config` flag) | `opencode.go` |
| codex | 写入 `$CODEX_HOME/config.toml` 的 `[mcp_servers.*]` 块(BEGIN/END 标记包裹,run 后清理) | `codex.go` |
| openclaw | 不支持(忽略 McpConfig) | `openclaw.go` |

> 留空 `mcp_config` 表示不传,CLI 沿用自身默认(claude 会继承外层 Claude Code 会话的 MCP)。

## 扩展指南:加一个新 adapter

以加一个假想 CLI `foo` 为例,需要改 4 处:

### 1. 实现 Backend 接口(`internal/adapter/foo.go`)

```go
type fooBackend struct{ cfg Config }

func (b *fooBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
    // 1. 解析 execPath(cfg.ExecutablePath 或 "foo",exec.LookPath)
    // 2. runContext(ctx, opts.Timeout) 拿 ctx + cancel
    // 3. 组装 args(参考 buildClaudeArgs / buildOpencodeArgs 的模式)
    //    - 定义 fooBlockedArgs,用 filterCustomArgs 过滤用户 custom_args
    // 4. exec.CommandContext,hideAgentWindow + configureProcessGroup
    // 5. cmd.StdoutPipe / cmd.Stderr(newLogWriter 或 newStderrTail)
    // 6. cmd.Start
    // 7. 起一个 goroutine 扫 stdout,把每帧翻译成 adapter.Message,trySend 到 msgCh
    //    结束后 close(msgCh),并把 adapter.Result 发到 resCh
    // 8. 返回 &Session{Messages: msgCh, Result: resCh}
    return &Session{}, nil
}
```

实现要点:
- 用 `runContext`(处理超时)、`trySend`(非阻塞,缓冲满丢弃)、`buildEnv`(合并环境 + 过滤内部 CLAUDECODE_* 标记)。
- 失败时把 stderr tail 纳入 `Result.Error`(`newStderrTail`)。
- panic 由 facade 的 `safeExecute` 兜底,但 goroutine 内 panic 需自行 recover 或保证不发生。

### 2. 在 `adapter.New` 注册(`backend.go`)

```go
case "foo":
    return &fooBackend{cfg: cfg}, nil
```

并在 default 的错误信息里补上 `foo`。

### 3. 加 catalog 条目(`internal/detect/catalog.go`)

- `supportedCLIs` 追加 `"foo"`。
- `hardcodedCatalog` 加 `"foo"` 的 provider/model 表。

`Discover` / `DefaultCatalog` / `ParseModelName` 自动支持新 CLI(它们都遍历 `supportedCLIs`)。

### 4. 加 config 默认(`internal/config/config.go`)

- `KnownAgents` 追加 `"foo"`(`Defaults()` 与 env 覆盖循环都基于它,自动补 `enabled:true`)。

### 可选

- 若新 CLI 有专属 per-agent 字段(如 openclaw 的 `openclaw_mode`),在 `AgentConfig` 加字段并在 `facade.buildExecOptions` 映射到 `ExecOptions`。
- 若新 CLI 的 thinking 语义不同,在 `buildFooArgs` 里消费 `opts.ThinkingLevel`。

无需改 api 层:OpenAI/Anthropic/原生端点都通过 facade 路由,新 adapter 自动获得三套接口支持。
