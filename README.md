# AICLIBridge

A unified API bridge for AI coding CLIs — call Claude Code, Codex, OpenCode, OpenClaw, and Gemini through one HTTP API.

## Status: early development

This repository is being scaffolded. The Go module, CI, and skeleton directories
are in place. The HTTP API, CLI adapters, and storage layer land in later
milestones.

## Install

TODO: build from source, distribution strategy, single static binary, etc.

## Config

TODO: env vars, config file format, default ports, auth.

## API

TODO: routes, request/response shape, streaming model, auth.

## Supported CLIs

TODO: detection and adapter coverage per CLI:
- Claude Code
- Codex
- OpenCode
- OpenClaw
- Gemini

## Development

```sh
make dev      # go run ./cmd/aiclibridge
make build    # builds ./aiclibridge
make test     # go test ./...
make vet      # go vet ./...
```

## License

Apache-2.0. See [LICENSE](./LICENSE).
