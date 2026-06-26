// package chunker は Markdown を見出し境界でチャンク分割する。
package chunker

import (
	"bufio"
	"fmt"
	"strings"
)

// Chunk は分割された 1 チャンクを表す。
type Chunk struct {
	// Path はドキュメントのパス（upsert 時に渡されるファイルパスまたは URL）。
	Path string
	// HeadingPath は見出し階層パス。例: "# A > ## B > ### C"
	HeadingPath string
	// Text はチャンクのテキスト本文（見出し行を含む）。
	Text string
	// ChunkIndex はドキュメント内のチャンクの 0-based インデックス。
	ChunkIndex int
}

// Chunker は Markdown テキストを見出し境界でチャンク分割する。
type Chunker struct {
	// MaxChunkSize はチャンクあたりの最大文字数（Rune 単位）。
	// 0 以下の場合はデフォルト値 (1500) を使用する。
	MaxChunkSize int
}

// defaultMaxChunkSize は最大チャンクサイズのフォールバック値（CHK-03 確定値）。
// 通常は config.ChunkerConfig.MaxChunkSize から渡される。
const defaultMaxChunkSize = 1500

// New は maxChunkSize を指定して Chunker を生成する（DES-001 §9.1）。
// maxChunkSize <= 0 の場合は defaultMaxChunkSize を使用する。
func New(maxChunkSize int) *Chunker {
	if maxChunkSize <= 0 {
		maxChunkSize = defaultMaxChunkSize
	}
	return &Chunker{MaxChunkSize: maxChunkSize}
}

// effectiveMaxChunkSize は実効最大チャンクサイズを返す。
func (c *Chunker) effectiveMaxChunkSize() int {
	if c.MaxChunkSize <= 0 {
		return defaultMaxChunkSize
	}
	return c.MaxChunkSize
}

// Split は Markdown テキストを見出し境界でチャンク分割し、[]Chunk を返す。
//
// ルール:
//   - H1〜H6 の ATX 形式見出し行（# Heading）を境界として分割する。
//   - 各チャンクに path と見出し階層パス（"# A > ## B > ### C" 形式）を付与する。
//   - 見出しなしの文書は全体を 1 チャンクとして扱う（CHK-02）。
//   - MaxChunkSize を超えるチャンクはさらに均等分割する（CHK-03 / §9）。
func (c *Chunker) Split(path, content string) ([]Chunk, error) {
	maxSize := c.effectiveMaxChunkSize()

	// 見出し境界でセクションに分割する
	sections, err := splitIntoSections(content)
	if err != nil {
		return nil, fmt.Errorf("chunker.Split: %w", err)
	}

	// MaxChunkSize を超えるセクションをさらに分割する
	var chunks []Chunk
	idx := 0
	for _, sec := range sections {
		subChunks := splitBySize(sec.headingPath, sec.text, maxSize)
		for _, sub := range subChunks {
			chunks = append(chunks, Chunk{
				Path:        path,
				HeadingPath: sub.headingPath,
				Text:        sub.text,
				ChunkIndex:  idx,
			})
			idx++
		}
	}

	return chunks, nil
}

// headingEntry は見出し階層スタックの 1 エントリ。
type headingEntry struct {
	level int
	text  string
}

// section は見出し境界で分割された中間表現。
type section struct {
	headingPath string
	text        string
}

// splitIntoSections は Markdown テキストを H1〜H6 見出し境界でセクションに分割する。
// 見出しなし文書は全体を 1 セクションとして返す（headingPath は空文字列）。
// bufio.Scanner のデフォルト上限（64 KB/行）を超える行があるとエラーを返す（DES-001 §10）。
func splitIntoSections(content string) ([]section, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))

	var headingStack []headingEntry
	var currentLines []string
	var sections []section

	// 現在のバッファを section として確定する。
	// headingStack は flush 時点の状態を使う。
	flushSection := func() {
		text := strings.TrimRight(strings.Join(currentLines, "\n"), "\n")
		if text != "" {
			sections = append(sections, section{
				headingPath: buildHeadingPath(headingStack),
				text:        text,
			})
		}
		currentLines = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		level, headingText := parseHeading(line)
		if level > 0 {
			// 見出し行を検出: 現在のバッファを flush してから新しいセクションを開始
			if len(currentLines) > 0 {
				flushSection()
			}
			// 見出しスタックを更新: 同じレベル以上のエントリを削除してから追加
			newStack := make([]headingEntry, 0, len(headingStack)+1)
			for _, h := range headingStack {
				if h.level < level {
					newStack = append(newStack, h)
				}
			}
			headingStack = append(newStack, headingEntry{level: level, text: headingText})
			// 新しいセクションの先頭行として見出し行を追加
			currentLines = []string{line}
		} else {
			currentLines = append(currentLines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("chunker.splitIntoSections: %w", err)
	}

	// 最後のバッファを flush
	if len(currentLines) > 0 {
		flushSection()
	}

	// 見出しなし文書（sections が空の場合）: 全体を 1 チャンクとして返す（CHK-02）
	if len(sections) == 0 {
		sections = append(sections, section{
			headingPath: "",
			text:        strings.TrimRight(content, "\n"),
		})
	}

	return sections, nil
}

// buildHeadingPath は見出しスタックから "# A > ## B > ### C" 形式のパスを生成する。
func buildHeadingPath(stack []headingEntry) string {
	if len(stack) == 0 {
		return ""
	}
	parts := make([]string, len(stack))
	for i, h := range stack {
		prefix := strings.Repeat("#", h.level)
		parts[i] = fmt.Sprintf("%s %s", prefix, h.text)
	}
	return strings.Join(parts, " > ")
}

// parseHeading は Markdown の ATX 形式見出し行を解析し、レベル(1〜6)と見出しテキストを返す。
// 見出し行でない場合は level=0, text="" を返す。
// Setext 形式（=== / ---）はサポートしない。
func parseHeading(line string) (level int, text string) {
	if !strings.HasPrefix(line, "#") {
		return 0, ""
	}
	// # の数をカウント
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i > 6 {
		// H7 以上は見出しとして扱わない
		return 0, ""
	}
	// # の直後はスペース（U+0020）・タブ（U+0009）・行末でなければならない（CommonMark ATX 仕様）
	if i < len(line) && line[i] != ' ' && line[i] != '\t' {
		return 0, ""
	}
	headingText := strings.TrimSpace(line[i:])
	return i, headingText
}

// subChunk は MaxChunkSize 分割後の中間表現。
type subChunk struct {
	headingPath string
	text        string
}

// splitBySize はテキストが maxSize（Rune 単位）を超える場合に均等分割する（CHK-03）。
// 分割後の各チャンクに同じ headingPath を付与する。
func splitBySize(headingPath, text string, maxSize int) []subChunk {
	runes := []rune(text)
	total := len(runes)

	if total <= maxSize {
		return []subChunk{{headingPath: headingPath, text: text}}
	}

	// 必要な分割数を計算し、均等なサイズで分割する
	numParts := (total + maxSize - 1) / maxSize
	partSize := (total + numParts - 1) / numParts

	var result []subChunk
	for start := 0; start < total; start += partSize {
		end := start + partSize
		if end > total {
			end = total
		}
		result = append(result, subChunk{
			headingPath: headingPath,
			text:        string(runes[start:end]),
		})
	}
	return result
}
