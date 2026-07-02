// Package config は doc-db の起動時設定を YAML ファイル（`~/.doc-db/doc-db.yaml`）から
// 読み込む（DES-001 §9.1 CFG-01〜03）。
//
// 設計上の方針（DES-001 §9.3）:
//   - YAML ファイルが唯一の正本。環境変数によるオーバーライドは行わない（API キーを除く）
//   - 起動時に 1 回だけ読み込む。稼働中の再読み込みは行わない
//   - パース後にバリデーションを行い、不正値は fail-fast で起動を中止する
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config は doc-db.yaml 全体のルートスキーマ（DES-001 §9.2）。
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Embedding EmbeddingConfig `yaml:"embedding"`
	Rerank    RerankConfig    `yaml:"rerank"`
	Chunker   ChunkerConfig   `yaml:"chunker"`
	BM25      BM25Config      `yaml:"bm25"`
	Fetcher   FetcherConfig   `yaml:"fetcher"`
	Expiry    ExpiryConfig    `yaml:"expiry"`
	// Log は省略可能セクション（CFG-03 の例外。DES-001 §9.3 参照）。
	// 省略時は defaultLogPath() / "info" が適用される。既存の doc-db.yaml に
	// log: セクションが無くても起動が壊れないようにするための後方互換措置。
	Log LogConfig `yaml:"log"`
}

// ServerConfig は server セクション。
type ServerConfig struct {
	Port   int    `yaml:"port"`
	DBPath string `yaml:"db_path"`
}

// EmbeddingConfig は embedding セクション。
type EmbeddingConfig struct {
	Model          string `yaml:"model"`
	Dim            int    `yaml:"dim"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// RerankConfig は rerank セクション。
type RerankConfig struct {
	Model          string `yaml:"model"`
	Factor         int    `yaml:"factor"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// ChunkerConfig は chunker セクション。
type ChunkerConfig struct {
	MaxChunkSize int `yaml:"max_chunk_size"`
}

// BM25Config は bm25 セクション。
type BM25Config struct {
	K1 float64 `yaml:"k1"`
	B  float64 `yaml:"b"`
}

// FetcherConfig は fetcher セクション。
type FetcherConfig struct {
	TimeoutSeconds int  `yaml:"timeout_seconds"`
	AllowPrivate   bool `yaml:"allow_private"`
}

// ExpiryConfig は expiry セクション。
type ExpiryConfig struct {
	TTLDays         int `yaml:"ttl_days"`
	MaxChunks       int `yaml:"max_chunks"`
	IntervalSeconds int `yaml:"interval_seconds"`
}

// LogConfig は log セクション（省略可）。
// Path はログの出力先。通常は絶対パス（`~/` は展開される）。
// 特殊値 "stdout" / "stderr" を指定すると標準出力・標準エラーにそのまま出す
// （フォアグラウンド開発時の用途）。
// Level は "debug" | "info" | "warn" | "error"。
type LogConfig struct {
	Path  string `yaml:"path"`
	Level string `yaml:"level"`
}

// defaultLogPath は log.path 省略時のデフォルト値（展開前）を返す。
func defaultLogPath() string {
	return "~/.doc-db/doc-db.log"
}

const defaultLogLevel = "info"

// DefaultPath は設定ファイルの固定パス（CFG-01）。
// `$HOME/.doc-db/doc-db.yaml` を返す。$HOME が解決できない場合は空文字列を返す。
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".doc-db", "doc-db.yaml")
}

// Load は固定パス `~/.doc-db/doc-db.yaml` から設定を読み込む（CFG-01）。
// ファイルが存在しない、$HOME が解決できない、パース失敗、検証失敗のいずれも fail-fast で error を返す。
func Load() (*Config, error) {
	path := DefaultPath()
	if path == "" {
		return nil, errors.New("config: $HOME が解決できないため設定ファイルパスを決定できません")
	}
	return LoadFrom(path)
}

// LoadFrom は指定パスから設定を読み込む。テストおよび内部実装用（CFG-02）。
// 未知のキーは KnownFields による strict パースで検出する。
// db_path で先頭が `~/` または `~` 単体の場合は $HOME に展開する。
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: 設定ファイル %q を読み込めません: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // 未知のキーをエラーにする（CFG-03）
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: YAML パースに失敗しました %q: %w", path, err)
	}

	// パス系フィールドの `~` を $HOME に展開する（利便性のため）
	expanded, err := expandTilde(cfg.Server.DBPath)
	if err != nil {
		return nil, fmt.Errorf("config: server.db_path のチルダ展開に失敗: %w", err)
	}
	cfg.Server.DBPath = expanded

	// log セクション省略時のデフォルト適用（CFG-03 の例外。DES-001 §9.3）
	if cfg.Log.Path == "" {
		cfg.Log.Path = defaultLogPath()
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = defaultLogLevel
	}
	// "stdout" / "stderr" はチルダ展開・パス扱いしない特殊値
	if cfg.Log.Path != "stdout" && cfg.Log.Path != "stderr" {
		expandedLog, err := expandTilde(cfg.Log.Path)
		if err != nil {
			return nil, fmt.Errorf("config: log.path のチルダ展開に失敗: %w", err)
		}
		cfg.Log.Path = expandedLog
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: 設定ファイル %q の検証に失敗しました: %w", path, err)
	}

	return &cfg, nil
}

// expandTilde はパス文字列の先頭 `~/` または単独 `~` を $HOME に置換する。
// `~user/...` 形式 (他ユーザーの home) は展開せずそのまま返す (POSIX 慣習)。
// 空文字列や `~` を含まないパスはそのまま返す。
func expandTilde(p string) (string, error) {
	if p == "" || (p[0] != '~') {
		return p, nil
	}
	// `~user/...` は非対応 (誤爆防止でそのまま返す)
	if len(p) > 1 && p[1] != '/' {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p, fmt.Errorf("$HOME が解決できません: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	// p は "~/..." の形
	return filepath.Join(home, p[2:]), nil
}

// Validate は Config 全体の値域・必須項目を検証する（CFG-03）。
// 個別セクションの検証ロジックを順に呼び出し、最初のエラーを返す。
func (c *Config) Validate() error {
	if err := c.Server.validate(); err != nil {
		return err
	}
	if err := c.Embedding.validate(); err != nil {
		return err
	}
	if err := c.Rerank.validate(); err != nil {
		return err
	}
	if err := c.Chunker.validate(); err != nil {
		return err
	}
	if err := c.BM25.validate(); err != nil {
		return err
	}
	if err := c.Fetcher.validate(); err != nil {
		return err
	}
	if err := c.Expiry.validate(); err != nil {
		return err
	}
	if err := c.Log.validate(); err != nil {
		return err
	}
	return nil
}

func (s *ServerConfig) validate() error {
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("server.port は 1〜65535 の範囲で指定してください（現在値: %d）", s.Port)
	}
	if s.DBPath == "" {
		return errors.New("server.db_path は必須です")
	}
	return nil
}

func (e *EmbeddingConfig) validate() error {
	if e.Model == "" {
		return errors.New("embedding.model は必須です")
	}
	if e.Dim <= 0 {
		return fmt.Errorf("embedding.dim は正の整数を指定してください（現在値: %d）", e.Dim)
	}
	if e.TimeoutSeconds <= 0 {
		return fmt.Errorf("embedding.timeout_seconds は正の整数を指定してください（現在値: %d）", e.TimeoutSeconds)
	}
	return nil
}

func (r *RerankConfig) validate() error {
	if r.Model == "" {
		return errors.New("rerank.model は必須です")
	}
	if r.Factor <= 0 {
		return fmt.Errorf("rerank.factor は正の整数を指定してください（現在値: %d）", r.Factor)
	}
	if r.TimeoutSeconds <= 0 {
		return fmt.Errorf("rerank.timeout_seconds は正の整数を指定してください（現在値: %d）", r.TimeoutSeconds)
	}
	return nil
}

func (c *ChunkerConfig) validate() error {
	if c.MaxChunkSize <= 0 {
		return fmt.Errorf("chunker.max_chunk_size は正の整数を指定してください（現在値: %d）", c.MaxChunkSize)
	}
	return nil
}

func (b *BM25Config) validate() error {
	if b.K1 <= 0 {
		return fmt.Errorf("bm25.k1 は正の数を指定してください（現在値: %g）", b.K1)
	}
	if b.B < 0 || b.B > 1 {
		return fmt.Errorf("bm25.b は 0〜1 の範囲で指定してください（現在値: %g）", b.B)
	}
	return nil
}

func (f *FetcherConfig) validate() error {
	if f.TimeoutSeconds <= 0 {
		return fmt.Errorf("fetcher.timeout_seconds は正の整数を指定してください（現在値: %d）", f.TimeoutSeconds)
	}
	return nil
}

func (e *ExpiryConfig) validate() error {
	if e.TTLDays <= 0 {
		return fmt.Errorf("expiry.ttl_days は正の整数を指定してください（現在値: %d）", e.TTLDays)
	}
	if e.MaxChunks <= 0 {
		return fmt.Errorf("expiry.max_chunks は正の整数を指定してください（現在値: %d）", e.MaxChunks)
	}
	if e.IntervalSeconds <= 0 {
		return fmt.Errorf("expiry.interval_seconds は正の整数を指定してください（現在値: %d）", e.IntervalSeconds)
	}
	return nil
}

func (l *LogConfig) validate() error {
	if l.Path == "" {
		return errors.New("log.path は必須です")
	}
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf(`log.level は "debug"/"info"/"warn"/"error" のいずれかを指定してください（現在値: %q）`, l.Level)
	}
	return nil
}
