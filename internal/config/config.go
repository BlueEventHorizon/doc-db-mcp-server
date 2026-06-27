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

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: 設定ファイル %q の検証に失敗しました: %w", path, err)
	}

	return &cfg, nil
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
