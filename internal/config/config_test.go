package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandTilde_HomeSlash(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no HOME")
	}
	got, err := expandTilde("~/doc-db/docdb.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "doc-db/docdb.sqlite")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpandTilde_HomeOnly(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no HOME")
	}
	got, err := expandTilde("~")
	if err != nil {
		t.Fatal(err)
	}
	if got != home {
		t.Errorf("got %q, want %q", got, home)
	}
}

func TestExpandTilde_NoTilde_PassThrough(t *testing.T) {
	for _, p := range []string{"", "/abs/path", "./rel/path", "relative/path.sqlite"} {
		got, err := expandTilde(p)
		if err != nil {
			t.Errorf("expandTilde(%q) err=%v", p, err)
		}
		if got != p {
			t.Errorf("expandTilde(%q) = %q, want unchanged", p, got)
		}
	}
}

func TestExpandTilde_TildeUser_NotExpanded(t *testing.T) {
	// ~otheruser/... 形式は誤爆防止でそのまま返す
	got, err := expandTilde("~otheruser/data.db")
	if err != nil {
		t.Fatal(err)
	}
	if got != "~otheruser/data.db" {
		t.Errorf("~user should not expand: got %q", got)
	}
}

func TestLoadFrom_TildeExpandsDBPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no HOME")
	}
	// テスト用に一時 YAML を作成
	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "doc-db.yaml")
	yaml := `server:
  port: 58080
  db_path: "~/.doc-db/docdb.sqlite"

embedding:
  model: "text-embedding-3-large"
  dim: 3072
  timeout_seconds: 60

rerank:
  model: "gpt-4o-mini"
  factor: 3
  timeout_seconds: 30

chunker:
  max_chunk_size: 8192

bm25:
  k1: 1.5
  b: 0.75

fetcher:
  timeout_seconds: 30
  allow_private: false

expiry:
  ttl_days: 30
  max_chunks: 10000
  interval_seconds: 3600
`
	if err := os.WriteFile(yamlPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(yamlPath)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	wantPrefix := home + "/"
	if !strings.HasPrefix(cfg.Server.DBPath, wantPrefix) {
		t.Errorf("DBPath = %q, want prefix %q", cfg.Server.DBPath, wantPrefix)
	}
	if !strings.HasSuffix(cfg.Server.DBPath, "docdb.sqlite") {
		t.Errorf("DBPath suffix wrong: %q", cfg.Server.DBPath)
	}
}

const baseYAMLWithoutLog = `server:
  port: 58080
  db_path: "~/.doc-db/docdb.sqlite"

embedding:
  model: "text-embedding-3-large"
  dim: 3072
  timeout_seconds: 60

rerank:
  model: "gpt-4o-mini"
  factor: 3
  timeout_seconds: 30

chunker:
  max_chunk_size: 8192

bm25:
  k1: 1.5
  b: 0.75

fetcher:
  timeout_seconds: 30
  allow_private: false

expiry:
  ttl_days: 30
  max_chunks: 10000
  interval_seconds: 3600
`

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "doc-db.yaml")
	if err := os.WriteFile(yamlPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return yamlPath
}

// log セクションを省略した既存 doc-db.yaml でも起動できる（後方互換・CFG-03 の例外）。
func TestLoadFrom_LogSectionOmitted_DefaultsApplied(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no HOME")
	}
	cfg, err := LoadFrom(writeYAML(t, baseYAMLWithoutLog))
	if err != nil {
		t.Fatalf("LoadFrom: %v (log セクション省略で失敗してはならない)", err)
	}
	wantPath := filepath.Join(home, ".doc-db", "doc-db.log")
	if cfg.Log.Path != wantPath {
		t.Errorf("Log.Path = %q, want %q", cfg.Log.Path, wantPath)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "info")
	}
}

// log セクションを明示指定した場合はその値が使われ、チルダは展開される。
func TestLoadFrom_LogSectionExplicit_Respected(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no HOME")
	}
	yaml := baseYAMLWithoutLog + "\nlog:\n  path: \"~/custom/doc-db.log\"\n  level: \"debug\"\n"
	cfg, err := LoadFrom(writeYAML(t, yaml))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	wantPath := filepath.Join(home, "custom", "doc-db.log")
	if cfg.Log.Path != wantPath {
		t.Errorf("Log.Path = %q, want %q", cfg.Log.Path, wantPath)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "debug")
	}
}

// "stdout" / "stderr" はチルダ展開されず特殊値のまま保持される。
func TestLoadFrom_LogPath_StdoutNotExpanded(t *testing.T) {
	yaml := baseYAMLWithoutLog + "\nlog:\n  path: \"stdout\"\n  level: \"info\"\n"
	cfg, err := LoadFrom(writeYAML(t, yaml))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Log.Path != "stdout" {
		t.Errorf("Log.Path = %q, want %q", cfg.Log.Path, "stdout")
	}
}

// 不正な log.level は fail-fast で拒否される（CFG-03）。
func TestLoadFrom_LogLevel_Invalid_FailsFast(t *testing.T) {
	yaml := baseYAMLWithoutLog + "\nlog:\n  path: \"stdout\"\n  level: \"verbose\"\n"
	_, err := LoadFrom(writeYAML(t, yaml))
	if err == nil {
		t.Fatal("不正な log.level で LoadFrom が成功してはならない")
	}
}
