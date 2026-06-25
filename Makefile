# doc-db MCP サーバーのビルド・テスト・整合性検証エントリポイント。
# VERSION ファイルが canonical（DES-002 §4.1 / APP-002 VER-01）。
# ローカルビルドで version 値を埋めるためのワンライナーをラップする。

SHELL := /bin/bash

# canonical version を VERSION から読み出す（改行除去）
VERSION := $(shell tr -d '\n' < VERSION)
BIN     := doc-db
PKG     := ./cmd/docdb

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build version test clean help

help:
	@echo "doc-db build targets:"
	@echo "  make build    - $(BIN) を $(VERSION) でビルド（-ldflags -X main.version 経由）"
	@echo "  make version  - VERSION ファイルの値を表示"
	@echo "  make test     - go test ./..."
	@echo "  make clean    - ビルド成果物を削除"

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)
	@echo "built: ./$(BIN) ($(VERSION))"

version:
	@echo "$(VERSION)"

test:
	go test ./...

clean:
	rm -f $(BIN)
