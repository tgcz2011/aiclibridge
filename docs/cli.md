# CLI 子命令

AICLIBridge 是单二进制 + 子命令分发模型:`aiclibridge <command> [flags] [args]`。每个子命令独立解析 flag、独立管理生命周期。本文档覆盖 serve / run / agents / models / cancel / get / version 七个子命令,所有 flag 取自真实源码。

## 本页内容

- [总览](#总览)
- [退出码](#退出码)
- [aiclibridge serve](#aiclibridge-serve)
- [aiclibridge run](#aiclibridge-run)
- [aiclibridge agents](#aiclibridge-agents)
- [aiclibridge models](#aiclibridge-models)
- [aiclibridge cancel](#aiclibridge-cancel)
- [aiclibridge get](#aiclibridge-get)
- [aiclibridge version](#aiclibridge-version)
- [顶层 flag](#顶层-flag)

## 总览

```
aiclibridge is a unified bridge for AI coding CLIs.

Usage:
  aiclibridge <command> [flags] [args]

Commands:
  serve    Start the HTTP daemon (the original main.go behaviour).
  run      Run a single prompt against a CLI without a long-lived daemon.
  agents   List detected CLIs and their providers/models (local detect).
  models   List every CLI/provider/model routing key (local detect).
  cancel   Cancel a running run via the daemon's HTTP API.
  get      Fetch a run's history via the daemon's HTTP API.
  version  Print the aiclibridge version and exit.
```

子命令分两类:

- **本地子命令**(`run` / `agents` / `models`):在进程内构造完整 stack(config → logger → 内存 SQLite store → detect → facade),不依赖 daemon。`run` 用 `:memory:` store,一次调用不留 SQLite 文件。
- **HTTP 子命令**(`cancel` / `get`):仅加载 config(取 `listen` + `api_key`),向已运行的 daemon 发 HTTP 请求。daemon 不在跑会报连接错误。
- **daemon 子命令**(`serve`):启动长驻 HTTP daemon,使用文件 SQLite store。

## 退出码

### run 子命令(由 run 终止状态映射)

| 状态 | 退出码 | 说明 |
|---|---|---|
| `completed` | 0 | 成功完成 |
| `failed` | 1 | 通用失败 |
| `cancelled` | 130 | 被 cancel(128 + SIGINT(2),与 bash 中 Ctrl-C 杀进程一致) |
| `timeout` | 124 | 超时(与 `timeout(1)` 约定一致,区别于 failed) |
| 未知 | 1 | 流关闭但无终止事件(回退为 failed) |

### 其它子命令

| 退出码 | 说明 |
|---|---|
| 0 | 成功 |
| 1 | 运行错误(daemon 连接失败、facade 构建失败、CLI 探测失败、HTTP 4xx/5xx 等) |
| 2 | flag 解析错误 / 用法错误(如缺少必填位置参数) |

> `cancel` / `get` 依赖 daemon:daemon 未运行时连接失败返回 **1**(非 2)。flag 解析错误才返回 2。

## aiclibridge serve

启动 HTTP daemon。这是原 `main.go` 行为,`serve` 是默认子命令(`aiclibridge` 不带子命令等价于 `serve`)。

### 用法

```
aiclibridge serve [--config <path>] [--listen <addr>]
```

### flags

| flag | 默认 | 说明 |
|---|---|---|
| `--config <path>` | 搜索顺序 | 配置文件路径,见 [configuration.md](./configuration.md) |
| `--listen <addr>` | 取自 config | 覆盖监听地址(优先级高于配置文件中的 `listen`) |

### 行为

1. 加载并校验 config。
2. `mkdir -p` 数据目录,打开 `<data_dir>/aiclibridge.db` 文件 SQLite store。
3. 并行探测六个 CLI(各 10s 超时),失败回退硬编码 catalog,不阻止启动。
4. 构造 facade,启动 HTTP server(仅 `ReadHeaderTimeout=10s`,SSE 长连接无读写超时)。
5. 阻塞等待 `SIGINT` / `SIGTERM`;收到信号后按序优雅关闭:HTTP(拒绝新请求)→ facade(取消 in-flight run)→ store(释放 SQLite)。

### 示例

```sh
aiclibridge serve --config ./aiclibridge.yaml
aiclibridge serve --listen 0.0.0.0:8787
aiclibridge --config ./aiclibridge.yaml   # 等价于 serve
```

### 退出码

`0`(信号优雅关闭)/ `1`(config/store/facade/监听失败)/ `2`(flag 解析)。

## aiclibridge run

一次性运行单个 prompt,**不依赖 daemon**。在进程内构造 facade,直接 spawn 适配器子进程,run 结束后进程退出。

### 用法

```
aiclibridge run [flags] [prompt...]
```

### flags

| flag | 类型 | 默认 | 说明 |
|---|---|---|---|
| `--config <path>` | string | 搜索顺序 | 配置文件路径 |
| `--model <m>` | string | 首个 enabled agent 的默认模型 | `CLI/provider/model` 路由键 |
| `--cwd <dir>` | string | 继承 | 子进程工作目录 |
| `--system-prompt <s>` | string | `""` | developer/system 指令(部分 process-stdin CLI 忽略) |
| `--max-turns <n>` | int | 0 | 限制 agent 轮数;0=不限 |
| `--timeout <d>` | duration | 0 | 硬超时,如 `30s`/`2m`;0=无 |
| `--resume <session_id>` | string | `""` | 恢复之前的 CLI 会话 |
| `--no-stream` | bool | false | 关闭实时流式,运行结束后打印聚合输出 |

> 注:`mcp_config` / `thinking_level` / `custom_args` / `custom_env` 是 **per-agent 配置字段**(写在 `aiclibridge.yaml` 的 `agents.<cli>` 块里),**不是** `run` 的 flag。`run` 启动时会读取配置文件应用这些字段。详见 [configuration.md](./configuration.md)。

### prompt 输入的两种模式

`collectPrompt` 按以下顺序决定 prompt:

1. **位置参数**:`run` 之后的所有非 flag 参数用单个空格拼接。例如 `run "fix the bug"` → prompt=`fix the bug`。
2. **stdin 管道**:无位置参数且 stdin **不是 TTY**(有管道输入)时,读取全部 stdin 并去除首尾空白。例如 `echo "refactor this" | aiclibridge run`。
3. **TTY 且无参数**:无位置参数且 stdin 是 TTY(终端)时,prompt 为空 → 报错 `prompt is required`,退出码 2。

TTY 检测用 `os.Stdin.Stat()` + `os.ModeCharDevice`(纯标准库,无外部依赖)。

### 默认流式模式(不带 --no-stream)

`streamEvents` 把事件分流:

- **stdout**(原始文本,无前缀):`text` + `thinking` 内容。
- **stderr**(带 `[type]` 前缀,行式):`tool_use` / `tool_result` / `status` / `error` / `log` / `result`。

这样 stdout 可直接管道给下一个工具,stderr 保留进度/工具信息。终止 `result` 事件打印到 stderr,含 `status` / `duration_ms` / `session_id` / `error`。

```
[tool_use] tool=Read call_id=... input={...}
[status] running session_id=abc
这是助手输出文本
[result] status=completed duration_ms=1234 session_id=abc
```

### --no-stream 聚合模式

`drainAndSummarize` 不输出实时事件:读取全部事件到切片,运行结束后:

- stdout:打印终止事件的 `Output`(无 Output 时回退为所有 text 事件拼接),末尾补换行。
- stderr:打印单行 `[result] status=... duration_ms=... session_id=...`。

### 信号处理

`SIGINT` / `SIGTERM` 取消 run 的 context,传播到适配器子进程,终止事件为 `cancelled`(退出码 130)。第二次 Ctrl-C 走默认 disposition 直接杀进程。

### 示例

```sh
# 位置参数
aiclibridge run --model claude/anthropic/claude-sonnet-4.5 --cwd . "fix the bug"

# stdin 管道
echo "refactor this" | aiclibridge run --model codex/openai/gpt-5

# 聚合输出(适合脚本)
aiclibridge run --no-stream --model qwen/alibaba/qwen3-coder-plus "写个 fizzbuzz" > out.txt

# 带超时与系统提示
aiclibridge run --model opencode/google/gemini-2.5-pro \
  --timeout 60s --system-prompt "你是 Go 专家" --max-turns 5 "解释 goroutine"

# 恢复会话
aiclibridge run --model claude/anthropic/claude-sonnet-4.5 --resume <session_id> "继续"
```

### 退出码

`0`(completed)/ `1`(failed 或启动错误)/ `130`(cancelled)/ `124`(timeout)/ `2`(flag 或 prompt 缺失)。

## aiclibridge agents

列出全部已探测 CLI 的可用性、版本、路径、provider 数及完整 provider/model 树。**本地探测,不需 daemon**。

### 用法

```
aiclibridge agents [--config <path>]
```

### flags

| flag | 默认 | 说明 |
|---|---|---|
| `--config <path>` | 搜索顺序 | 配置文件路径 |

### 输出

Tab 分隔的 CLI 概要行 + 缩进的 `  cli/provider/model` 列表:

```
claude	available=yes	version=2.1.100	path=/usr/local/bin/claude	providers=1
  claude/anthropic/claude-sonnet-4.5
  claude/anthropic/claude-opus-4.1
codex	available=no	providers=2
  codex/openai/gpt-5
```

### 示例

```sh
aiclibridge agents
aiclibridge agents | grep available=yes
```

### 退出码

`0` / `1`(config 或探测失败)/ `2`(flag 解析)。

## aiclibridge models

列出所有 `CLI/provider/model` 路由键,每行一个。是 `agents` 的扁平形态,输出顺序稳定(`supportedCLIs` 顺序)。**本地探测,不需 daemon**。

### 用法

```
aiclibridge models [--config <path>]
```

### flags

| flag | 默认 | 说明 |
|---|---|---|
| `--config <path>` | 搜索顺序 | 配置文件路径 |

### 输出

```
claude/anthropic/claude-sonnet-4.5
claude/anthropic/claude-opus-4.1
codex/openai/gpt-5
```

### 示例

```sh
aiclibridge models
aiclibridge models | grep claude
```

### 退出码

`0` / `1` / `2`。

## aiclibridge cancel

通过 daemon 的 HTTP API 取消一个运行中的 run。**必须依赖正在运行的 daemon**(本地不构造 facade)。

### 用法

```
aiclibridge cancel <run-id> [--config <path>] [--listen <addr>]
```

### flags

| flag | 默认 | 说明 |
|---|---|---|
| `--config <path>` | 搜索顺序 | 用于取 `listen` + `api_key` |
| `--listen <addr>` | 取自 config | 覆盖 daemon 地址(指向非默认 daemon) |

位置参数:`<run-id>` 必填。

### 行为

向 `http://<addr>/v1/runs/<id>/cancel` 发 POST,带 `Authorization: Bearer <api_key>`(api_key 为空时不带)。成功打印响应体到 stdout;HTTP 4xx/5xx 打印到 stderr。

### 示例

```sh
aiclibridge cancel abc123def456...
aiclibridge cancel abc123 --listen 127.0.0.1:9999
```

### 退出码

`0`(成功)/ `1`(连接失败或 HTTP 错误,含 daemon 未运行)/ `2`(flag 解析或缺 run-id)。

## aiclibridge get

通过 daemon 的 HTTP API 获取一个 run 的存储历史。**必须依赖正在运行的 daemon**。

### 用法

```
aiclibridge get <run-id> [--config <path>] [--listen <addr>]
```

### flags

| flag | 默认 | 说明 |
|---|---|---|
| `--config <path>` | 搜索顺序 | 用于取 `listen` + `api_key` |
| `--listen <addr>` | 取自 config | 覆盖 daemon 地址 |

位置参数:`<run-id>` 必填。

### 行为

向 `http://<addr>/v1/runs/<id>` 发 GET,带 `Authorization: Bearer <api_key>`。响应若为合法 JSON 则 pretty-print(缩进 2 空格)到 stdout;否则原样打印。HTTP 4xx/5xx 打印到 stderr。

### 示例

```sh
aiclibridge get abc123def456...
aiclibridge get abc123 | jq '.Status'
```

### 退出码

`0` / `1`(连接失败或 HTTP 错误)/ `2`(flag 解析或缺 run-id)。

## aiclibridge version

打印版本横幅到 stdout 并退出。忽略参数。

### 用法

```
aiclibridge version
```

### 输出

```
aiclibridge 0.1.0
```

当通过 `-ldflags` 注入 `Build` / `Commit` 时额外打印:

```
aiclibridge 0.1.0
build:  2026-06-30
commit: abc1234
```

### 退出码

`0`。

## 顶层 flag

| flag | 说明 |
|---|---|
| `-h`, `--help` | 打印总览用法到 stdout,退出 0 |
| `-v`, `--version` | 等价于 `version` 子命令 |

不带任何参数也等价于 `--help`(退出 0);未知子命令打印 `unknown command %q` + 用法到 stderr,退出 2。
