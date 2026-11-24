# doc-db MCP サーバーのビルド・テスト・整合性検証エントリポイント。
# VERSION ファイルが canonical（DES-002 §4.1 / APP-002 VER-01）。
# Rust バイナリは VERSION をコンパイル時に読み込む。

SHELL := /bin/bash

# canonical version を VERSION から読み出す（改行除去）
VERSION := $(shell tr -d '\n' < VERSION)
BIN     := doc-db

.PHONY: build version test test-go clean verify verify-version verify-tag help show-config show-log

help:
	@echo "doc-db build targets:"
	@echo "  make build            - Rust 版 $(BIN) ($(VERSION)) をビルド"
	@echo "  make version          - VERSION ファイルの値を表示"
	@echo "  make test             - cargo test --locked"
	@echo "  make test-go          - 移植前の Go 版の比較用テスト"
	@echo "  make verify           - verify-version + verify-tag を順に実行"
	@echo "  make verify-version   - CHANGELOG / .version-config.yaml / Formula tag の整合性検証"
	@echo "  make verify-tag       - main の第1親履歴とタグ時点のバージョン、Formula の固定 SHA を検証"
	@echo "  make show-config      - 解決済み設定 (log/db パス等) を表示（doc-db --show-config）"
	@echo "  make show-log         - 設定済みログファイルを tail -f で追跡"
	@echo "  make clean            - ビルド成果物を削除"

build:
	cargo build --release --locked
	install -m 755 target/release/$(BIN) $(BIN)
	@echo "built: ./$(BIN) ($(VERSION))"

version:
	@echo "$(VERSION)"

test:
	cargo test --locked

test-go:
	go test ./...

verify: verify-version verify-tag

verify-version:
	@bash scripts/verify_version_consistency.sh

verify-tag:
	@bash scripts/verify_release_tag.sh

show-config:
	@cargo run --locked --quiet -- --show-config

# log.path は "stdout"/"stderr" の特殊値もありうるため、その場合は tail できない
# 旨を案内する（config.log.path はサーバーが決めるため make 側でハードコードしない）。
show-log:
	@config_output=$$(cargo run --locked --quiet -- --show-config) || exit; \
	log_path=$$( printf '%s\n' "$$config_output" | sed -n 's/^log\.path:[[:space:]]*//p' ); \
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
