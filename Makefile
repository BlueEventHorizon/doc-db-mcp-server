# doc-db MCP サーバーのビルド・テスト・整合性検証エントリポイント。
# VERSION ファイルが canonical（DES-002 §4.1 / APP-002 VER-01）。
# ローカルビルドで version 値を埋めるためのワンライナーをラップする。

SHELL := /bin/bash

# canonical version を VERSION から読み出す（改行除去）
VERSION := $(shell tr -d '\n' < VERSION)
BIN     := doc-db
PKG     := ./cmd/docdb

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build version test clean verify verify-version verify-tag help show-config show-log

help:
	@echo "doc-db build targets:"
	@echo "  make build            - $(BIN) を $(VERSION) でビルド（-ldflags -X main.version 経由）"
	@echo "  make version          - VERSION ファイルの値を表示"
	@echo "  make test             - go test ./..."
	@echo "  make verify           - verify-version + verify-tag を順に実行"
	@echo "  make verify-version   - CHANGELOG / .version-config.yaml / Formula tag の整合性検証"
	@echo "  make verify-tag       - git tag v\$$(VERSION) と Formula revision の commit SHA 一致検証"
	@echo "  make show-config      - 解決済み設定 (log/db パス等) を表示（doc-db --show-config）"
	@echo "  make show-log         - 設定済みログファイルを tail -f で追跡"
	@echo "  make clean            - ビルド成果物を削除"

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)
	@echo "built: ./$(BIN) ($(VERSION))"

version:
	@echo "$(VERSION)"

test:
	go test ./...

verify: verify-version verify-tag

verify-version:
	@bash scripts/verify_version_consistency.sh

verify-tag:
	@bash scripts/verify_release_tag.sh

# doc-db --show-config はビルド済みバイナリに依存する。未ビルドなら go run で代替。
show-config:
	@if [ -x ./$(BIN) ]; then \
		./$(BIN) --show-config; \
	else \
		go run $(PKG) --show-config; \
	fi

# log.path は "stdout"/"stderr" の特殊値もありうるため、その場合は tail できない
# 旨を案内する（config.log.path はサーバーが決めるため make 側でハードコードしない）。
show-log:
	@log_path=$$( ( [ -x ./$(BIN) ] && ./$(BIN) --show-config || go run $(PKG) --show-config ) \
		| awk -F': *' '/^log.path:/ {print $$2}' ); \
	if [ -z "$$log_path" ]; then \
		echo "エラー: log.path を解決できませんでした。~/.doc-db/doc-db.yaml を確認してください"; \
		exit 1; \
	elif [ "$$log_path" = "stdout" ] || [ "$$log_path" = "stderr" ]; then \
		echo "log.path が \"$$log_path\" のためファイル tail はできません。"; \
		echo "doc-db をフォアグラウンド起動してそのまま端末を確認してください。"; \
		exit 1; \
	elif [ ! -f "$$log_path" ]; then \
		echo "ログファイルがまだ存在しません: $$log_path"; \
		echo "doc-db サーバーを起動すると自動的に作成されます: doc-db &"; \
		exit 1; \
	else \
		echo "tailing: $$log_path"; \
		tail -f "$$log_path"; \
	fi

clean:
	rm -f $(BIN)
