# 快速开始

5 分钟把 AICLIBridge 跑起来,并用 OpenAI / Anthropic SDK 接入。

## 本页内容

- [前置条件](#前置条件)
- [安装](#安装)
- [最小配置](#最小配置)
- [启动 daemon](#启动-daemon)
- [curl 验证](#curl-验证)
- [用 OpenAI SDK 接入](#用-openai-sdk-接入)
- [用 Anthropic SDK 接入](#用-anthropic-sdk-接入)
- [用 CLI 子命令接入](#用-cli-子命令接入)

## 前置条件

- **Go 1.24+**(从源码构建时需要)
- **至少安装一个 AI coding CLI**,并已在 `PATH` 中可见。可用任意一个或多个:
  - `claude`(Claude Code)
  - `codex`(Codex CLI)
  - `opencode`(OpenCode)
  - `qwen`(Qwen Code)
  - `openclaw`
  - `gemini`(gemini-cli,实验性)
- 验证 CLI 已就绪:

```sh
claude --version    # 或 codex --version / opencode --version / qwen --version
```

> AICLIBridge 会在启动时并行探测这些 CLI(各 10s 超时),缺失的 CLI 仅在 `/v1/agents` 里标记为 `available: false`,不会阻止 daemon 启动。

## 安装

三种方式任选其一:

```sh
# 方式 A: go install(推荐)
go install github.com/tgcz2011/aiclibridge/cmd/aiclibridge@latest

# 方式 B: release 二进制(下载后放到 PATH)
# 见 https://github.com/tgcz2011/aiclibridge/releases

# 方式 C: 从源码构建
git clone https://github.com/tgcz2011/aiclibridge.git
cd aiclibridge
make build          # 产物: ./aiclibridge
```

验证:

```sh
aiclibridge version
```

## 最小配置

AICLIBridge 零配置也能启动(默认 `127.0.0.1:8787`、无鉴权、全部 CLI 探测)。建议写一个最小配置文件以便固定鉴权与按需启停 CLI:

```yaml
# aiclibridge.yaml
listen: 127.0.0.1:8787
api_key: sk-aiclibridge-xxx
data_dir: ./data
log_level: info
agents:
  claude: { enabled: true }
  codex:  { enabled: true }
  opencode: { enabled: false }   # 没装就关掉
  openclaw: { enabled: false }
  qwen:    { enabled: false }
  gemini:  { enabled: false }
```

字段含义见 [configuration.md](./configuration.md)。配置文件查找顺序为:`--config` > `$AICLIBRIDGE_CONFIG` > `./aiclibridge.yaml` > `~/.aiclibridge/config.yaml` > Defaults。

## 启动 daemon

```sh
aiclibridge serve --config ./aiclibridge.yaml
# 或直接(aiclibridge 默认子行为 serve)
aiclibridge --config ./aiclibridge.yaml
```

启动后日志(JSON,stdout)会逐条打印每个 CLI 的探测结果。看到 `aiclibridge listening addr=127.0.0.1:8787` 即就绪。

## curl 验证

```sh
# 健康检查(无需鉴权)
curl -s http://127.0.0.1:8787/healthz
# => {"status":"ok"}

# 模型列表(无需鉴权,OpenAI 形态)
curl -s http://127.0.0.1:8787/v1/models | jq '.data[].id'
# => "claude/anthropic/claude-sonnet-4.5" ...

# 发现已安装的 CLI(需鉴权)
curl -s -H "Authorization: Bearer sk-aiclibridge-xxx" \
  http://127.0.0.1:8787/v1/agents | jq '.agents[] | {name, available}'

# 原生 run,流式输出(SSE)
curl -N -H "Authorization: Bearer sk-aiclibridge-xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude/anthropic/claude-sonnet-4.5","prompt":"用一句话介绍 Go","stream":true}' \
  http://127.0.0.1:8787/v1/runs
```

更多 curl 见 [examples/curl.sh](../examples/curl.sh)。

## 用 OpenAI SDK 接入

把 OpenAI SDK 的 `base_url` 指向 AICLIBridge 即可,model 用 `CLI/provider/model` 形式。流式与非流式都支持。

```python
# pip install openai
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8787/v1",
    api_key="sk-aiclibridge-xxx",
)

# 非流式
resp = client.chat.completions.create(
    model="claude/anthropic/claude-sonnet-4.5",
    messages=[{"role": "user", "content": "用一句话介绍 Go"}],
)
print(resp.choices[0].message.content)

# 流式
for chunk in client.chat.completions.create(
    model="codex/openai/gpt-5",
    messages=[{"role": "user", "content": "写一个 hello world"}],
    stream=True,
):
    delta = chunk.choices[0].delta.content
    if delta:
        print(delta, end="", flush=True)
print()
```

完整脚本见 [examples/openai-python.py](../examples/openai-python.py)。

## 用 Anthropic SDK 接入

```python
# pip install anthropic
from anthropic import Anthropic

client = Anthropic(
    base_url="http://127.0.0.1:8787",
    api_key="sk-aiclibridge-xxx",
)

# 非流式
msg = client.messages.create(
    model="claude/anthropic/claude-sonnet-4.5",
    max_tokens=1024,
    messages=[{"role": "user", "content": "用一句话介绍 Go"}],
)
print(msg.content[0].text)

# 流式
with client.messages.stream(
    model="claude/anthropic/claude-sonnet-4.5",
    max_tokens=1024,
    messages=[{"role": "user", "content": "写一个 hello world"}],
) as stream:
    for text in stream.text_stream:
        print(text, end="", flush=True)
print()
```

> 注意:`/v1/messages` 的流式只下发文本 delta(thinking / tool_use 在兼容层被丢弃);若需要完整事件流(含 thinking、tool_use),改用原生 `/v1/runs`。完整脚本见 [examples/anthropic-python.py](../examples/anthropic-python.py)。

## 用 CLI 子命令接入

`aiclibridge` 自带子命令,可不写代码直接驱动:

```sh
# 单次调用(直接走 adapter,不经过 daemon)
aiclibridge run --model claude/anthropic/claude-sonnet-4.5 --cwd . "重构这个文件"

# 列出 daemon 发现的 CLI(需 daemon 在跑)
aiclibridge agents
aiclibridge models

# 取消 / 查看历史 run(需 daemon 在跑)
aiclibridge cancel <run_id>
aiclibridge get <run_id>
```

子命令细节见 [cli.md](./cli.md)。

## 下一步

- [配置参考](./configuration.md):per-agent 字段、env 覆盖、完整示例
- [API 参考](./api.md):每个端点的请求/响应与 SSE schema
- [架构设计](./architecture.md):并发模型、容错、如何加新 adapter
