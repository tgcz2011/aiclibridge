# API 参考

AICLIBridge HTTP API 完整参考,覆盖原生 `/v1/runs`、OpenAI 兼容 `/v1/chat/completions`、Anthropic 兼容 `/v1/messages` 与发现端点。所有 schema 取自真实源码,不编造字段。

## 本页内容

- [通用约定](#通用约定)
- [鉴权](#鉴权)
- [model name 三段式](#model-name-三段式)
- [SSE 事件 schema(原生 /v1/runs)](#sse-事件-schema原生-v1runs)
- [端点](#端点)
  - [GET /healthz](#get-healthz)
  - [GET /v1/models](#get-v1models)
  - [GET /v1/anthropic/models](#get-v1anthropicmodels)
  - [POST /v1/chat/completions](#post-v1chatcompletions)
  - [POST /v1/chat/completions/{id}/cancel](#post-v1chatcompletionsidcancel)
  - [POST /v1/messages](#post-v1messages)
  - [POST /v1/messages/{id}/cancel](#post-v1messagesidcancel)
  - [POST /v1/runs](#post-v1runs)
  - [GET /v1/runs/{id}](#get-v1runsid)
  - [POST /v1/runs/{id}/cancel](#post-v1runsidcancel)
  - [GET /v1/agents](#get-v1agents)
  - [GET /v1/agents/{cli}](#get-v1agentscli)
  - [GET /v1/providers](#get-v1providers)
- [错误码](#错误码)

## 通用约定

- 监听地址由配置 `listen` 决定,默认 `127.0.0.1:8787`。
- 所有 JSON 请求体上限 10MB(`maxBodyBytes`),超出按 400 拒绝。
- 响应 `Content-Type` 除 SSE 外均为 `application/json`。
- CORS 全开:`Access-Control-Allow-Origin: *`,允许 `GET/POST/OPTIONS/DELETE`,允许头 `Authorization, Content-Type, x-api-key`;OPTIONS 预检返回 204。
- SSE 响应头:`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`、`X-Accel-Buffering: no`。
- SSE 长连接不设读写超时(仅 `ReadHeaderTimeout=10s` 防慢loris);客户端断连时正在流式输出的 run 会被自动 cancel。

## 鉴权

鉴权由配置 `api_key` 控制:

- `api_key` 为空时**关闭鉴权**,所有请求放行。
- `api_key` 非空时,需在请求头携带以下任一形式:
  - `Authorization: Bearer <key>`(OpenAI 客户端常用)
  - `x-api-key: <key>`(Anthropic 客户端常用)
- key 不匹配返回 `401`,错误体见[错误码](#错误码)。

**免鉴权端点**:`GET /healthz`、`GET /v1/models`、`OPTIONS *`(因 OpenAI 客户端常无凭据列模型,且健康探针须始终可达)。其余端点均需鉴权。

## model name 三段式

所有需要 `model` 的端点统一使用 `CLI/provider/model` 形式,作为路由键同时选定 CLI 与模型:

```
claude/anthropic/claude-sonnet-4.5
codex/openai/gpt-5
opencode/google/gemini-2.5-pro
qwen/alibaba/qwen3-coder-plus
gemini/google/gemini-2.5-pro
openclaw/bytedance/doubao-seedream-4-0
```

- CLI 段大小写不敏感(归一为小写);provider/model 段大小写敏感原样透传给上游 CLI。
- **裸模型名**(不含 `/`)会在 catalog 中按模型名首配查找并自动补全为三段式;找不到返回 `404 model_not_found_error`。
- catalog 由 `internal/detect/catalog.go` 的硬编码表提供,详见 `GET /v1/agents`。

## SSE 事件 schema(原生 /v1/runs)

原生 `/v1/runs` 流式输出使用 `pkg/protocol.Event` 结构,每帧为标准 SSE:

```
event: <type>
data: <json>

```

### EventType 取值

| type | 含义 |
|---|---|
| `text` | 文本增量(assistant 输出) |
| `thinking` | 推理/思考内容 |
| `tool_use` | 工具调用发起 |
| `tool_result` | 工具调用结果 |
| `status` | 状态变更(含 session_id) |
| `error` | 错误(非致命) |
| `log` | 日志(带 level) |
| `result` | 终止事件,标志 run 结束 |

### Event 字段

```json
{
  "type": "text",
  "seq": 3,
  "content": "...",
  "tool": "",
  "call_id": "",
  "input": null,
  "output": "",
  "status": "",
  "level": "",
  "session_id": "",
  "result": null
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `type` | string | 事件类型(见上表) |
| `seq` | int | 单调递增序号,从 0 起;`omitempty` |
| `content` | string | text/thinking/error/log 的文本内容 |
| `tool` | string | tool_use/tool_result 的工具名 |
| `call_id` | string | tool_use/tool_result 的调用 ID |
| `input` | object(raw) | tool_use 的输入参数(JSON) |
| `output` | string | tool_result 的输出 |
| `status` | string | status 事件的状态串 |
| `level` | string | log 事件的日志级别 |
| `session_id` | string | status 事件携带的 CLI 会话 ID(可用于 resume) |
| `result` | object | 仅 `type=result` 时存在,见 ResultPayload |

> 所有零值字段在 JSON 序列化时 `omitempty` 省略,故常见 text 帧仅 `{"type":"text","seq":N,"content":"..."}`。

### ResultPayload(终止事件)

```json
{
  "status": "completed",
  "output": "最终文本",
  "error": "",
  "duration_ms": 12345,
  "session_id": "abc...",
  "usage": {
    "claude-sonnet-4.5": {
      "input_tokens": 100,
      "output_tokens": 50,
      "cache_read_tokens": 0,
      "cache_write_tokens": 0
    }
  }
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `status` | string | `completed` / `failed` / `cancelled` / `timeout` |
| `output` | string | 聚合后的最终输出(可空) |
| `error` | string | 失败时的错误信息 |
| `duration_ms` | int64 | 运行时长(毫秒) |
| `session_id` | string | CLI 会话 ID(用于 resume) |
| `usage` | map | 按模型名分桶的 token 计量;可省略 |

`result` 事件是流的最后一帧,收到即代表 run 完全结束。

## 端点

### GET /healthz

存活探针,无需鉴权。

- **鉴权**:否
- **响应**:`200`
  ```json
  {"status":"ok"}
  ```
- **说明**:返回 200 仅表示进程在跑、mux 在服务,**不**代表下游 CLI 已安装。

```sh
curl -s http://127.0.0.1:8787/healthz
```

---

### GET /v1/models

OpenAI 形态的模型列表,无需鉴权。

- **鉴权**:否
- **响应**:`200`,OpenAI `list` 形态

```json
{
  "object": "list",
  "data": [
    {"id":"claude/anthropic/claude-sonnet-4.5","object":"model","created":1,"owned_by":"anthropic"},
    {"id":"codex/openai/gpt-5","object":"model","created":1,"owned_by":"openai"}
  ]
}
```

| 字段 | 说明 |
|---|---|
| `id` | `CLI/provider/model` 三段式 |
| `object` | 固定 `"model"` |
| `created` | 固定 `1`(避免响应不可缓存) |
| `owned_by` | provider 名 |

```sh
curl -s http://127.0.0.1:8787/v1/models | jq '.data[].id'
```

---

### GET /v1/anthropic/models

Anthropic 形态的模型列表,需鉴权。

- **鉴权**:是
- **响应**:`200`,`{"data":[...]}` 形态

```json
{
  "data": [
    {"id":"claude/anthropic/claude-sonnet-4.5","display_name":"Claude Sonnet 4.5","type":"model"}
  ]
}
```

| 字段 | 说明 |
|---|---|
| `id` | `CLI/provider/model` |
| `display_name` | catalog 中的 DisplayName;为空时回退到 model 名 |
| `type` | 固定 `"model"` |

```sh
curl -s -H "x-api-key: sk-aiclibridge-xxx" \
  http://127.0.0.1:8787/v1/anthropic/models
```

---

### POST /v1/chat/completions

OpenAI 兼容 Chat Completions。一次 completion 对应一个底层 run(completion id == run id)。

- **鉴权**:是
- **请求体**:

```json
{
  "model": "claude/anthropic/claude-sonnet-4.5",
  "messages": [
    {"role":"system","content":"你是 Go 专家"},
    {"role":"user","content":"用一句话介绍 Go"}
  ],
  "stream": false,
  "max_tokens": 1024,
  "temperature": 0.7,
  "top_p": 1.0,
  "user": "u-1"
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `model` | string | 必填,三段式或裸名(自动解析) |
| `messages` | array | 必填,非空;`content` 可为 string 或 `[{type,text}]` 数组(非文本 part 被丢弃) |
| `stream` | bool | `true` 走 SSE;`false`(默认)返回完整对象 |
| `max_tokens` | int | 接受但忽略 |
| `temperature` | float | 接受但忽略 |
| `top_p` | float | 接受但忽略 |
| `user` | string | 接受但忽略 |

> 消息折叠规则:最后一条 `user` 消息成为 prompt;之前的 system/user/assistant 消息折叠进 system prompt 作为对话上下文。

#### 非流式响应(stream=false)

```json
{
  "id": "<run-id>",
  "object": "chat.completion",
  "created": 1700000000,
  "model": "claude/anthropic/claude-sonnet-4.5",
  "choices": [
    {"index":0,"message":{"role":"assistant","content":"..."},"finish_reason":"stop"}
  ],
  "usage": {"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}
}
```

- `finish_reason`:`stop` / `failed` / `cancelled` / `timeout`(取自终止事件)。
- `content`:优先取终止事件的 `output`;为空时回退为所有 text 事件拼接。
- `usage`:适配器上报时累加,否则全 0。

#### 流式响应(stream=true)

SSE,每帧 `data: <json>\n\n`,事件序列:

1. 首帧:role-only chunk(`delta.role="assistant"`,无 content)。
2. 每个 text 事件 → `delta.content` 增量;thinking 事件 → `delta.reasoning_content`;tool_use → `delta.tool_calls`(整块发送,不分片)。
3. 通道关闭后:终止帧 `finish_reason="stop"`。
4. 最后:`data: [DONE]`。

> status / log / error / tool_result 事件在流中被丢弃(无 OpenAI 表示)。

chunk schema:

```json
{
  "id":"<run-id>",
  "object":"chat.completion.chunk",
  "created":1700000000,
  "model":"claude/anthropic/claude-sonnet-4.5",
  "choices":[{
    "index":0,
    "delta":{"content":"增量文本"},
    "finish_reason":null
  }]
}
```

```sh
# 非流式
curl -s -H "Authorization: Bearer sk-aiclibridge-xxx" -H "Content-Type: application/json" \
  -d '{"model":"claude/anthropic/claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}' \
  http://127.0.0.1:8787/v1/chat/completions

# 流式
curl -N -H "Authorization: Bearer sk-aiclibridge-xxx" -H "Content-Type: application/json" \
  -d '{"model":"codex/openai/gpt-5","messages":[{"role":"user","content":"hi"}],"stream":true}' \
  http://127.0.0.1:8787/v1/chat/completions
```

---

### POST /v1/chat/completions/{id}/cancel

取消一个 chat completion。`{id}` 即 run id,委托给原生 cancel 逻辑。

- **鉴权**:是
- **路径参数**:`id` = run id
- **响应**:`200` `{"cancelled":true,"id":"<id>"}`;run 不存在(已结束)返回 `404`。

```sh
curl -s -X POST -H "Authorization: Bearer sk-aiclibridge-xxx" \
  http://127.0.0.1:8787/v1/chat/completions/<id>/cancel
```

---

### POST /v1/messages

Anthropic 兼容 Messages API。一次 message 对应一个底层 run(message id == run id)。

- **鉴权**:是
- **请求体**:

```json
{
  "model": "claude/anthropic/claude-sonnet-4.5",
  "messages": [{"role":"user","content":"用一句话介绍 Go"}],
  "system": "你是 Go 专家",
  "max_tokens": 1024,
  "stream": false,
  "temperature": 0.7,
  "top_p": 1.0
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `model` | string | 必填,三段式或裸名 |
| `messages` | array | 必填,非空;`content` 同 OpenAI 规则 |
| `system` | string \| array | 字符串或 `[{type,text}]` 数组,折叠为单一 system prompt |
| `stream` | bool | `true` 走 SSE;`false` 返回完整对象 |
| `max_tokens` / `temperature` / `top_p` | - | 接受但忽略 |

#### 非流式响应(stream=false)

```json
{
  "id":"<run-id>",
  "type":"message",
  "role":"assistant",
  "model":"claude/anthropic/claude-sonnet-4.5",
  "content":[{"type":"text","text":"..."}],
  "stop_reason":"end_turn",
  "stop_sequence":null,
  "usage":{"input_tokens":0,"output_tokens":0}
}
```

- `stop_reason` 固定 `end_turn`(v1 不区分失败/取消的 stop_reason,真实状态见原生流)。
- `content` 仅含单个 text block。

#### 流式响应(stream=true)

SSE,每帧 `event: <type>\ndata: <json>\n\n`,固定事件序列:

1. `message_start`:message 信封(空 content,`stop_reason:null`,usage 全 0)。
2. `content_block_start`:开启 index 0 的 text block。
3. `content_block_delta`(每个 text 事件一帧):`delta.text` 为增量文本。
4. `content_block_stop`:关闭 text block。
5. `message_delta`:`stop_reason:"end_turn"`。
6. `message_stop`:终止哨兵。

> **重要**:`/v1/messages` 流式只下发文本 delta;thinking 与 tool_use 事件被丢弃。若需完整事件流(含 thinking、tool_use),改用原生 `/v1/runs`。

```sh
# 非流式
curl -s -H "x-api-key: sk-aiclibridge-xxx" -H "Content-Type: application/json" \
  -d '{"model":"claude/anthropic/claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}' \
  http://127.0.0.1:8787/v1/messages

# 流式
curl -N -H "x-api-key: sk-aiclibridge-xxx" -H "Content-Type: application/json" \
  -d '{"model":"claude/anthropic/claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"stream":true}' \
  http://127.0.0.1:8787/v1/messages
```

---

### POST /v1/messages/{id}/cancel

取消一个 message。`{id}` 即 run id,委托给原生 cancel 逻辑。

- **鉴权**:是
- **响应**:`200` `{"cancelled":true,"id":"<id>"}`;不存在返回 `404`。

```sh
curl -s -X POST -H "x-api-key: sk-aiclibridge-xxx" \
  http://127.0.0.1:8787/v1/messages/<id>/cancel
```

---

### POST /v1/runs

原生 AICLIBridge run。支持流式 SSE(`stream=true`)与同步等待(`stream=false`)。

- **鉴权**:是
- **请求体**(`nativeRunRequest`):

```json
{
  "model": "claude/anthropic/claude-sonnet-4.5",
  "prompt": "用一句话介绍 Go",
  "cwd": "/tmp/proj",
  "system_prompt": "你是 Go 专家",
  "resume_session_id": "",
  "max_turns": 0,
  "timeout_ms": 0,
  "custom_args": [],
  "custom_env": {},
  "stream": true
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `model` | string | 三段式或裸名;空表示用首个 enabled agent 的默认模型 |
| `prompt` | string | 用户输入,必填 |
| `cwd` | string | 子进程工作目录;空=继承 daemon cwd |
| `system_prompt` | string | developer/system 指令(部分后端忽略) |
| `resume_session_id` | string | 恢复之前的 CLI 会话 |
| `max_turns` | int | 0=不限 |
| `timeout_ms` | int64 | 硬超时;0=无超时 |
| `custom_args` | []string | 追加在 agent 配置的 custom_args 之后(请求胜出) |
| `custom_env` | map | 请求级 env 覆盖(注:当前适配器层未实现 per-run env,字段已接受待支持) |
| `stream` | bool | `true`=SSE 流;`false`=聚合后返回 JSON |

> 无论 `stream` 真假,run 内部都以 `Stream=true` 启动,handler 是 live channel 的唯一消费者。

#### 流式响应(stream=true)

SSE,使用原生 `protocol.Event` schema(见[SSE 事件 schema](#sse-事件-schema原生-v1runs))。每帧 `event: <type>\ndata: <json>\n\n`,直到 `result` 事件(终止)后通道关闭。客户端断连会自动 cancel 该 run。

```sh
curl -N -H "Authorization: Bearer sk-aiclibridge-xxx" -H "Content-Type: application/json" \
  -d '{"model":"claude/anthropic/claude-sonnet-4.5","prompt":"hi","stream":true}' \
  http://127.0.0.1:8787/v1/runs
```

#### 非流式响应(stream=false)

收集完整事件时间线后返回 `RunResult` JSON:

```json
{
  "ID":"<run-id>",
  "Status":"completed",
  "Output":"最终文本",
  "Error":"",
  "DurationMs":12345,
  "SessionID":"abc...",
  "Usage":null,
  "Events":[ /* 完整 protocol.Event 时间线 */ ]
}
```

| 字段 | 说明 |
|---|---|
| `ID` | run id(32 字符 hex) |
| `Status` | `completed`/`failed`/`cancelled`/`timeout` |
| `Output` | 终止事件的 output |
| `Error` | 失败时的错误 |
| `DurationMs` | 运行时长 |
| `SessionID` | CLI 会话 ID |
| `Usage` | 按模型名分桶的 token 计量 |
| `Events` | 完整事件时间线(含 text/thinking/tool_use/result 等) |

```sh
curl -s -H "Authorization: Bearer sk-aiclibridge-xxx" -H "Content-Type: application/json" \
  -d '{"model":"codex/openai/gpt-5","prompt":"hi","stream":false}' \
  http://127.0.0.1:8787/v1/runs
```

---

### GET /v1/runs/{id}

回放一个 run 的存储时间线(从 SQLite 读取,source of truth)。

- **鉴权**:是
- **路径参数**:`id` = run id
- **响应**:`200`,`RunResult`(同 `POST /v1/runs` 非流式响应,含完整 `Events`)。
- run 不存在返回 `404 not_found_error`。

```sh
curl -s -H "Authorization: Bearer sk-aiclibridge-xxx" \
  http://127.0.0.1:8787/v1/runs/<id> | jq .
```

---

### POST /v1/runs/{id}/cancel

取消一个 live run。

- **鉴权**:是
- **响应**:`200` `{"cancelled":true,"id":"<id>"}`。
- run 不在 live map(已结束)返回 `404 not_found_error`。

> cancel 仅对 live run 幂等;取消已结束的 run 视为客户端错误并返回 404。

```sh
curl -s -X POST -H "Authorization: Bearer sk-aiclibridge-xxx" \
  http://127.0.0.1:8787/v1/runs/<id>/cancel
```

---

### GET /v1/agents

列出全部 CLI catalog,含每个 CLI 的 providers/models 树。

- **鉴权**:是
- **响应**:`200` `{"agents":[CLIInfo,...]}`

```json
{
  "agents": [
    {
      "name":"claude",
      "version":"2.1.100",
      "available":true,
      "path":"/usr/local/bin/claude",
      "providers":[
        {"name":"anthropic","models":[
          {"name":"claude-sonnet-4.5","display_name":"Claude Sonnet 4.5"}
        ]}
      ]
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `name` | CLI 名(lowercase) |
| `version` | `<cli> --version` 报告的版本;缺失/探测失败时为空 |
| `available` | 二进制在 PATH 且 version 探测通过 |
| `path` | `exec.LookPath` 解析的路径;未找到为空 |
| `providers` | provider/model 树(缺失 CLI 仍返回硬编码 catalog 以供预览) |

```sh
curl -s -H "Authorization: Bearer sk-aiclibridge-xxx" \
  http://127.0.0.1:8787/v1/agents | jq '.agents[] | {name,available}'
```

---

### GET /v1/agents/{cli}

返回单个 CLI 的 catalog 条目。

- **鉴权**:是
- **路径参数**:`cli` = CLI 名(claude/codex/opencode/openclaw/qwen/gemini)
- **响应**:`200`,单个 `CLIInfo` 对象;未知 CLI 返回 `404 not_found_error`。

```sh
curl -s -H "Authorization: Bearer sk-aiclibridge-xxx" \
  http://127.0.0.1:8787/v1/agents/claude
```

---

### GET /v1/providers

返回所有 CLI 中去重后的 provider 名列表(便捷汇总)。

- **鉴权**:是
- **响应**:`200` `{"providers":["anthropic","openai","google","bytedance","alibaba"]}`

```sh
curl -s -H "Authorization: Bearer sk-aiclibridge-xxx" \
  http://127.0.0.1:8787/v1/providers
```

## 错误码

所有错误响应使用 OpenAI 风格信封:

```json
{
  "error": {
    "message": "...",
    "type": "...",
    "details": "..."   // 仅当有底层 cause 时
  }
}
```

| HTTP | type | 触发场景 |
|---|---|---|
| 400 | `invalid_request_error` | JSON 解析失败、messages 为空、缺 run id、缺 cli |
| 401 | `authentication_error` | api_key 不匹配 |
| 404 | `model_not_found_error` | 裸模型名在 catalog 中找不到 |
| 404 | `not_found_error` | run 不存在(cancel/get)、CLI 不存在(/v1/agents/{cli}) |
| 500 | `server_error` | handler panic(已被 recover 捕获)、streaming 不被 ResponseWriter 支持 |
| 502 | `upstream_error` | facade 启动 run 失败、ListAgents 失败 |

> handler 任意位置的 panic 都会被 `recoverMiddleware` 捕获并转为 500 `server_error`,daemon 进程不会崩溃。
