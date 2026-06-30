# 配置参考

AICLIBridge 通过 YAML 配置文件 + 环境变量配置。两者可混用,env 覆盖 YAML。

## 本页内容

- [配置文件查找顺序](#配置文件查找顺序)
- [顶层字段表](#顶层字段表)
- [per-agent 配置块](#per-agent-配置块)
- [环境变量覆盖](#环境变量覆盖)
- [完整示例](#完整示例)

## 配置文件查找顺序

启动时按以下优先级取第一个命中的配置文件(`--config` 与 `$AICLIBRIDGE_CONFIG` 即使文件不存在也会被原样返回,自动发现路径则跳过缺失项):

| 顺序 | 来源 | 说明 |
|---|---|---|
| 1 | `--config <path>` | 命令行显式指定,优先级最高 |
| 2 | `$AICLIBRIDGE_CONFIG` | 环境变量指定的路径 |
| 3 | `./aiclibridge.yaml` | 当前工作目录下的默认文件 |
| 4 | `~/.aiclibridge/config.yaml` | 用户家目录下的默认文件 |
| 5 | (无文件) | 使用 Defaults,仅 env 覆盖 |

> 任意一份配置都可缺失——AICLIBridge 用内置 Defaults(`127.0.0.1:8787`、`./data`、info 日志、六个 CLI 全 `enabled:true`)启动,env 覆盖始终生效。解析失败的 YAML 才会报错退出。

## 顶层字段表

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `listen` | string | `127.0.0.1:8787` | HTTP 监听地址(`host:port`)。空串会校验失败 |
| `api_key` | string | `""` | 静态 API key。为空时**关闭鉴权**(所有请求放行);非空时要求 `Authorization: Bearer <key>` 或 `x-api-key: <key>` |
| `data_dir` | string | `./data` | 数据目录,SQLite 库 `aiclibridge.db` 存于此。会自动 `mkdir -p` |
| `log_level` | string | `info` | 日志级别,可选 `debug`/`info`/`warn`/`error`。非法值校验失败 |
| `default_timeout_ms` | int | `0` | 预留字段,当前未强制作用于 run(每 run 由请求 `timeout_ms` 决定)。`0` 表示无超时 |
| `agents` | map[string]AgentConfig | 六个 CLI 全 `enabled:true` | per-CLI 配置块,key 必须是 `claude`/`codex`/`opencode`/`openclaw`/`qwen`/`gemini` 之一,否则校验失败 |

> 配置中未列出的已知 CLI 会被自动补成 `enabled:true`(Defaults 行为),因此最小配置里只需写你想覆盖的部分。未知的 agent 名会在 `Validate()` 阶段被拒绝。

## per-agent 配置块

`agents.<cli>` 下的字段(以 `agents.claude` 为例):

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | `true` | 是否暴露该 CLI。`false` 时不在 `/v1/agents` 列出,且 `POST /v1/runs` 拒绝路由到它 |
| `executable_path` | string | `""` | CLI 二进制路径。空表示走 `PATH` 查找(如 `claude`/`codex`/`opencode`) |
| `extra_args` | []string | `[]` | daemon 级默认 CLI 参数,拼在 daemon 自有参数之后、`custom_args` 之前。仅 claude / codex 后端读取 |
| `custom_args` | []string | `[]` | 用户自定义 CLI 参数,拼在最后。请求级 `custom_args` 会追加在其后(请求胜出) |
| `env` | map[string]string | `{}` | 子进程额外环境变量,叠加在 daemon 环境之上 |
| `mcp_config` | MCPConfig | `null` | MCP server 配置,**可直接写内联 YAML**(会被重新编码成 JSON 传给 CLI 的 `--mcp-config`);留空表示不传 |
| `thinking_level` | string | `""` | 推理强度。空表示用 CLI/模型默认值。claude 接受 `low`/`medium`/`high`/`xhigh`/`max`;codex 接受 `none`/`minimal`/`low`/`medium`/`high`/`xhigh`;opencode 接受模型变体名。其它后端忽略此字段 |
| `openclaw_mode` | string | `""`(等同 `local`) | 仅 openclaw 后端生效:`local`(本地 in-process)/`gateway`(走网关)。其它后端忽略 |

### mcp_config 写法

`mcp_config` 支持直接写内联 YAML,无需 base64。加载时被重新编码为 JSON 写入临时文件,再以 `--mcp-config <path>` 传给 CLI:

```yaml
agents:
  claude:
    enabled: true
    mcp_config:
      mcpServers:
        fs:
          command: npx
          args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
```

> 留空(或写 `null`)表示不传 MCP 配置,CLI 会沿用其自身默认(对 claude 而言会继承外层 Claude Code 会话的 MCP)。

## 环境变量覆盖

所有 env 变量以 `AICLIBRIDGE_` 为前缀,**env 永远胜过 YAML**。无法解析的数值/布尔会被静默忽略(保留原值),避免一个拼错的 env 把字段清零。

### 顶层 env

| 环境变量 | 作用 | 示例 |
|---|---|---|
| `AICLIBRIDGE_CONFIG` | 指定配置文件路径 | `/etc/aiclibridge.yaml` |
| `AICLIBRIDGE_LISTEN` | 覆盖 `listen` | `0.0.0.0:8787` |
| `AICLIBRIDGE_API_KEY` | 覆盖 `api_key` | `sk-aiclibridge-xxx` |
| `AICLIBRIDGE_DATA_DIR` | 覆盖 `data_dir` | `/var/lib/aiclibridge` |
| `AICLIBRIDGE_LOG_LEVEL` | 覆盖 `log_level` | `debug` |
| `AICLIBRIDGE_DEFAULT_TIMEOUT_MS` | 覆盖 `default_timeout_ms` | `60000` |

### per-agent env

前缀为 `AICLIBRIDGE_AGENTS_<NAME>_`,`<NAME>` 为大写 CLI 名(`CLAUDE`/`CODEX`/`OPENCODE`/`OPENCLAW`/`QWEN`/`GEMINI`):

| 环境变量 | 作用 | 示例 |
|---|---|---|
| `AICLIBRIDGE_AGENTS_CLAUDE_ENABLED` | 覆盖 `agents.claude.enabled`(布尔) | `false` |
| `AICLIBRIDGE_AGENTS_CLAUDE_EXECUTABLE_PATH` | 覆盖 `agents.claude.executable_path` | `/usr/local/bin/claude` |
| `AICLIBRIDGE_AGENTS_CLAUDE_THINKING_LEVEL` | 覆盖 `agents.claude.thinking_level` | `high` |

> 注意:env 覆盖当前只覆盖 `enabled` / `executable_path` / `thinking_level` 三个 per-agent 字段;`extra_args` / `custom_args` / `env` / `mcp_config` / `openclaw_mode` 不支持 env 覆盖(它们是结构化/复合类型),请用配置文件设置。

### 用 env 快速覆盖示例

```sh
# 临时换监听地址 + 关掉鉴权 + 开 debug
AICLIBRIDGE_LISTEN=0.0.0.0:8787 \
AICLIBRIDGE_API_KEY="" \
AICLIBRIDGE_LOG_LEVEL=debug \
aiclibridge serve

# 临时关掉 gemini
AICLIBRIDGE_AGENTS_GEMINI_ENABLED=false aiclibridge serve
```

## 完整示例

以下示例覆盖所有字段,可直接复制作为模板(也见 [examples/config-full.yaml](../examples/config-full.yaml)):

```yaml
# aiclibridge.yaml — 完整配置示例
listen: 127.0.0.1:8787
api_key: sk-aiclibridge-xxx
data_dir: ./data
log_level: info
default_timeout_ms: 0

agents:
  claude:
    enabled: true
    executable_path: ""              # 空 = 走 PATH
    extra_args: []
    custom_args: []
    env:
      ANTHROPIC_LOG: "debug"
    thinking_level: high            # low|medium|high|xhigh|max
    mcp_config:
      mcpServers:
        fs:
          command: npx
          args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]

  codex:
    enabled: true
    executable_path: ""
    extra_args: []
    custom_args: []
    env: {}
    thinking_level: medium          # none|minimal|low|medium|high|xhigh

  opencode:
    enabled: true
    executable_path: ""
    custom_args: []
    env: {}
    thinking_level: ""              # 空 = 用模型默认

  openclaw:
    enabled: true
    executable_path: ""
    custom_args: []
    env: {}
    openclaw_mode: local            # local|gateway(仅 openclaw 生效)

  qwen:
    enabled: true
    executable_path: ""
    custom_args: []
    env: {}

  gemini:
    enabled: false                  # 实验性,默认关
    executable_path: ""
    custom_args: []
    env: {}
```

校验配置无需启动 daemon——`aiclibridge` 在 `Validate()` 阶段就会对 `listen` 为空、`log_level` 非法、未知 agent 名直接报错退出(退出码 1)。
