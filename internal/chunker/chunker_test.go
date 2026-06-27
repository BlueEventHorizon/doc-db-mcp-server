package chunker

import (
	"strings"
	"testing"
)

// -----------------------------------------------------------------------
// 基本: H1〜H6 各レベルでの分割
// -----------------------------------------------------------------------

func TestSplit_H1Boundary(t *testing.T) {
	c := New(1500)
	md := "# A\n\nbody A\n\n# B\n\nbody B\n"

	chunks, err := c.Split("doc.md", md)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2", len(chunks))
	}

	if chunks[0].HeadingPath != "# A" {
		t.Errorf("chunk[0] heading = %q, want %q", chunks[0].HeadingPath, "# A")
	}
	if chunks[1].HeadingPath != "# B" {
		t.Errorf("chunk[1] heading = %q, want %q", chunks[1].HeadingPath, "# B")
	}
	if !strings.Contains(chunks[0].Text, "body A") {
		t.Errorf("chunk[0] text missing body A: %q", chunks[0].Text)
	}
	if chunks[0].ChunkIndex != 0 || chunks[1].ChunkIndex != 1 {
		t.Errorf("indices = %d, %d", chunks[0].ChunkIndex, chunks[1].ChunkIndex)
	}
	if chunks[0].Path != "doc.md" {
		t.Errorf("path = %q", chunks[0].Path)
	}
}

func TestSplit_NestedHeadingsAllLevels(t *testing.T) {
	c := New(1500)
	md := "" +
		"# A\n" +
		"## B\n" +
		"### C\n" +
		"#### D\n" +
		"##### E\n" +
		"###### F\n" +
		"text\n"

	chunks, err := c.Split("p", md)
	if err != nil {
		t.Fatal(err)
	}
	// 各見出し行が独立セクションを開始 → 6 セクション
	if len(chunks) != 6 {
		t.Fatalf("len(chunks) = %d, want 6", len(chunks))
	}

	// 最下層は完全な階層パスを持つ
	last := chunks[len(chunks)-1]
	want := "# A > ## B > ### C > #### D > ##### E > ###### F"
	if last.HeadingPath != want {
		t.Errorf("deepest heading_path = %q\nwant %q", last.HeadingPath, want)
	}
}

// -----------------------------------------------------------------------
// CHK-02: 見出しなし文書 → 1 チャンク
// -----------------------------------------------------------------------

func TestSplit_NoHeading_SingleChunk(t *testing.T) {
	c := New(1500)
	md := "no heading here.\nsecond line.\n"

	chunks, err := c.Split("p", md)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}
	if chunks[0].HeadingPath != "" {
		t.Errorf("heading_path = %q, want empty", chunks[0].HeadingPath)
	}
	if !strings.Contains(chunks[0].Text, "no heading") {
		t.Errorf("text missing: %q", chunks[0].Text)
	}
}

// -----------------------------------------------------------------------
// CHK-03: MaxChunkSize 超過時の再分割
// -----------------------------------------------------------------------

func TestSplit_MaxChunkSize_Splits(t *testing.T) {
	c := New(10) // 小さい上限でテスト
	body := strings.Repeat("a", 35)
	md := "# H\n" + body + "\n"

	chunks, err := c.Split("p", md)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want >=2 (size exceeds limit)", len(chunks))
	}
	// 全 chunk が maxSize 以下
	for i, ch := range chunks {
		if runeLen := len([]rune(ch.Text)); runeLen > 10 {
			t.Errorf("chunk[%d] rune len = %d, exceeds maxSize 10", i, runeLen)
		}
		if ch.HeadingPath != "# H" {
			t.Errorf("chunk[%d] heading_path = %q, want %q", i, ch.HeadingPath, "# H")
		}
	}
}

func TestSplit_MaxChunkSize_DefaultFallback(t *testing.T) {
	// 0 or negative → defaultMaxChunkSize (1500)
	c := New(0)
	if c.effectiveMaxChunkSize() != defaultMaxChunkSize {
		t.Errorf("effective = %d, want %d", c.effectiveMaxChunkSize(), defaultMaxChunkSize)
	}
	c2 := New(-5)
	if c2.effectiveMaxChunkSize() != defaultMaxChunkSize {
		t.Errorf("effective(-5) = %d, want %d", c2.effectiveMaxChunkSize(), defaultMaxChunkSize)
	}
}

// -----------------------------------------------------------------------
// 見出しスタックの巻き戻し
// -----------------------------------------------------------------------

func TestSplit_HeadingStack_PopsOnSameOrShallower(t *testing.T) {
	c := New(1500)
	md := "" +
		"# A\n" +
		"text A\n" +
		"## A1\n" +
		"text A1\n" +
		"# B\n" + // H1 戻り: スタックから A, A1 が両方落ちる
		"text B\n"

	chunks, err := c.Split("p", md)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("len = %d, want 3", len(chunks))
	}
	want := []string{"# A", "# A > ## A1", "# B"}
	for i, w := range want {
		if chunks[i].HeadingPath != w {
			t.Errorf("chunk[%d] heading_path = %q, want %q", i, chunks[i].HeadingPath, w)
		}
	}
}

// -----------------------------------------------------------------------
// parseHeading の境界条件
// -----------------------------------------------------------------------

func TestParseHeading_Cases(t *testing.T) {
	cases := []struct {
		in    string
		level int
		text  string
	}{
		{"# H", 1, "H"},
		{"## H", 2, "H"},
		{"###### H6", 6, "H6"},
		{"####### H7", 0, ""}, // H7 以上は見出しではない
		{"#H", 0, ""},         // # の直後にスペース/タブが必要
		{"#\tH", 1, "H"},      // タブも OK
		{"plain", 0, ""},
		{"", 0, ""},
		{"#", 1, ""}, // # のみ（行末）
	}
	for _, tc := range cases {
		gotLvl, gotTxt := parseHeading(tc.in)
		if gotLvl != tc.level || gotTxt != tc.text {
			t.Errorf("parseHeading(%q) = (%d, %q), want (%d, %q)",
				tc.in, gotLvl, gotTxt, tc.level, tc.text)
		}
	}
}
