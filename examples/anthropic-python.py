#!/usr/bin/env python3
# AICLIBridge Anthropic-compatible client example.
#
# Run:  python3 examples/anthropic-python.py
# Deps: pip install anthropic
#
# Points the Anthropic SDK at a running AICLIBridge daemon and exercises:
#   1. listing models (Anthropic /v1/anthropic/models shape)
#   2. non-streaming messages (claude)
#   3. streaming messages via text_stream (claude)
#
# Start the daemon first:
#   aiclibridge serve --config ./aiclibridge.yaml

import sys
import anthropic
from anthropic import Anthropic, APIError, APIConnectionError

BASE_URL = "http://127.0.0.1:8787"
API_KEY = "sk-aiclibridge-xxx"  # set to "" if your daemon runs with no api_key

client = Anthropic(base_url=BASE_URL, api_key=API_KEY or "unused")


def list_models():
    """List models via the Anthropic-compatible endpoint."""
    print("=== /v1/anthropic/models ===")
    # The Anthropic SDK's models.list hits GET /v1/models by default; the
    # bridge exposes the Anthropic shape at /v1/anthropic/models, so we
    # use a raw GET via the SDK's client.
    resp = client.get("/v1/anthropic/models")
    for m in resp.json().get("data", []):
        print(f"  {m['id']}  (display_name={m.get('display_name')})")


def messages_non_stream():
    """One-shot message: returns the full assistant text block."""
    print("\n=== non-streaming messages (claude) ===")
    msg = client.messages.create(
        model="claude/anthropic/claude-sonnet-4.5",
        max_tokens=1024,
        system="You answer in one short sentence.",
        messages=[{"role": "user", "content": "Introduce Go in one sentence."}],
    )
    for block in msg.content:
        if block.type == "text":
            print(block.text)
    print(f"  stop_reason={msg.stop_reason}")


def messages_stream():
    """Streaming messages: prints each text delta as it arrives."""
    print("\n=== streaming messages (claude) ===")
    with client.messages.stream(
        model="claude/anthropic/claude-sonnet-4.5",
        max_tokens=1024,
        messages=[{"role": "user", "content": "Write a one-line hello world in Python."}],
    ) as stream:
        for text in stream.text_stream:
            print(text, end="", flush=True)
    print()  # final newline after the stream


def main() -> int:
    try:
        list_models()
        messages_non_stream()
        messages_stream()
    except APIConnectionError as e:
        print(f"connection error: is the daemon running on {BASE_URL}? {e}", file=sys.stderr)
        return 1
    except APIError as e:
        print(f"api error: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
