#!/usr/bin/env python3
# AICLIBridge OpenAI-compatible client example.
#
# Run:  python3 examples/openai-python.py
# Deps: pip install openai
#
# Points the OpenAI SDK at a running AICLIBridge daemon and exercises:
#   1. non-streaming chat completion (claude)
#   2. streaming chat completion (codex/openai/gpt-5)
#   3. listing models (OpenAI /v1/models shape)
#
# Start the daemon first:
#   aiclibridge serve --config ./aiclibridge.yaml

import sys
from openai import OpenAI, APIError, APIConnectionError

BASE_URL = "http://127.0.0.1:8787/v1"
API_KEY = "sk-aiclibridge-xxx"  # set to "" if your daemon runs with no api_key

client = OpenAI(base_url=BASE_URL, api_key=API_KEY or "unused")


def list_models():
    """List every CLI/provider/model routing key the bridge can serve."""
    print("=== /v1/models ===")
    resp = client.models.list()
    for m in resp.data:
        print(f"  {m.id}  (owned_by={m.owned_by})")


def chat_non_stream():
    """One-shot chat completion: returns the full assistant message."""
    print("\n=== non-streaming chat (claude) ===")
    resp = client.chat.completions.create(
        model="claude/anthropic/claude-sonnet-4.5",
        messages=[
            {"role": "system", "content": "You answer in one short sentence."},
            {"role": "user", "content": "Introduce Go in one sentence."},
        ],
    )
    print(resp.choices[0].message.content)
    print(f"  finish_reason={resp.choices[0].finish_reason}")


def chat_stream():
    """Streaming chat completion: prints each text delta as it arrives."""
    print("\n=== streaming chat (codex/openai/gpt-5) ===")
    stream = client.chat.completions.create(
        model="codex/openai/gpt-5",
        messages=[{"role": "user", "content": "Write a one-line hello world in Python."}],
        stream=True,
    )
    for chunk in stream:
        delta = chunk.choices[0].delta
        if delta and delta.content:
            print(delta.content, end="", flush=True)
    print()  # final newline after the stream


def main() -> int:
    try:
        list_models()
        chat_non_stream()
        chat_stream()
    except APIConnectionError as e:
        print(f"connection error: is the daemon running on {BASE_URL}? {e}", file=sys.stderr)
        return 1
    except APIError as e:
        print(f"api error: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
