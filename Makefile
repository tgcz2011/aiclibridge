SHELL := /bin/sh
GO    ?= go
BIN   ?= aiclibridge

.PHONY: dev build test vet clean

dev:
	$(GO) run ./cmd/aiclibridge

build:
	$(GO) build -o $(BIN) ./cmd/aiclibridge

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

clean:
	rm -f $(BIN)
