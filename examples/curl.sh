#!/usr/bin/env bash
# AICLIBridge curl cookbook — every common call in one script.
#
# Run:    bash examples/curl.sh
# Needs:  a running daemon (aiclibridge serve --config ./aiclibridge.yaml)
#         and the jq tool is recommended (some commands pipe through it).
#
# Set BASE/API_KEY to match your daemon. If your daemon runs with no
# api_key (api_key: ""), the Authorization header is simply ignored.
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:8787}"
API_KEY="${API_KEY:-sk-aiclibridge-xxx}"
AUTH="Authorization: Bearer ${API_KEY}"
XKEY="x-api-key: ${API_KEY}"
JSON="Content-Type: application/json"

hr() { printf '\n=== %s ===\n' "$*"; }

# ── discovery (healthz + models are unauthenticated) ──

hr "GET /healthz (no auth)"
curl -s "${BASE}/healthz"; echo

hr "GET /v1/models (no auth, OpenAI shape)"
curl -s "${BASE}/v1/models" | jq -r '.data[].id'

# ── agents / providers (auth required) ──

hr "GET /v1/agents"
curl -s -H "${AUTH}" "${BASE}/v1/agents" | jq '.agents[] | {name,available,version}'

hr "GET /v1/agents/claude"
curl -s -H "${AUTH}" "${BASE}/v1/agents/claude" | jq '{name,available,providers}'

hr "GET /v1/providers"
curl -s -H "${AUTH}" "${BASE}/v1/providers" | jq '.providers'

hr "GET /v1/anthropic/models (auth)"
curl -s -H "${XKEY}" "${BASE}/v1/anthropic/models" | jq -r '.data[].id'

# ── native /v1/runs ──
# Stream=true prints live SSE events; Stream=false waits and returns JSON.

hr "POST /v1/runs (stream=true, SSE)"
curl -N -s -H "${AUTH}" -H "${JSON}" \
  -d '{"model":"claude/anthropic/claude-sonnet-4.5","prompt":"Introduce Go in one sentence.","stream":true}' \
  "${BASE}/v1/runs"

hr "POST /v1/runs (stream=false, sync JSON)"
RUN=$(curl -s -H "${AUTH}" -H "${JSON}" \
  -d '{"model":"codex/openai/gpt-5","prompt":"Write hello world in one line.","stream":false}' \
  "${BASE}/v1/runs")
echo "${RUN}" | jq '{ID,Status,Output,DurationMs}'
RUN_ID=$(echo "${RUN}" | jq -r '.ID')

# ── OpenAI-compatible /v1/chat/completions ──

hr "POST /v1/chat/completions (non-stream)"
curl -s -H "${AUTH}" -H "${JSON}" \
  -d '{"model":"claude/anthropic/claude-sonnet-4.5","messages":[{"role":"user","content":"Say hi"}]}' \
  "${BASE}/v1/chat/completions" | jq '.choices[0].message.content'

hr "POST /v1/chat/completions (stream=true, SSE chunks)"
curl -N -s -H "${AUTH}" -H "${JSON}" \
  -d '{"model":"codex/openai/gpt-5","messages":[{"role":"user","content":"Say hi"}],"stream":true}' \
  "${BASE}/v1/chat/completions"

# ── Anthropic-compatible /v1/messages ──

hr "POST /v1/messages (non-stream)"
curl -s -H "${XKEY}" -H "${JSON}" \
  -d '{"model":"claude/anthropic/claude-sonnet-4.5","max_tokens":256,"messages":[{"role":"user","content":"Say hi"}]}' \
  "${BASE}/v1/messages" | jq '.content[0].text'

hr "POST /v1/messages (stream=true, typed SSE)"
curl -N -s -H "${XKEY}" -H "${JSON}" \
  -d '{"model":"claude/anthropic/claude-sonnet-4.5","max_tokens":256,"messages":[{"role":"user","content":"Say hi"}],"stream":true}' \
  "${BASE}/v1/messages"

# ── replay + cancel (use the run id from the sync /v1/runs above) ──

hr "GET /v1/runs/{id} (replay)"
curl -s -H "${AUTH}" "${BASE}/v1/runs/${RUN_ID}" | jq '{ID,Status,Events:[.Events[].Type]}'

hr "POST /v1/runs/{id}/cancel"
# Cancel an already-finished run returns 404; start a fresh long run to
# observe a real cancel. Here we just demonstrate the call shape.
curl -s -X POST -H "${AUTH}" "${BASE}/v1/runs/${RUN_ID}/cancel" || true
echo

echo; echo "done."
