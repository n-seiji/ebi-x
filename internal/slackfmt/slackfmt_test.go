package slackfmt

import (
	"strings"
	"testing"
)

func TestSplitKeepsShortTextWhole(t *testing.T) {
	text := "# 方針\n\n- 一つ目\n- 二つ目"
	chunks := Split(text, 8000)
	if len(chunks) != 1 || chunks[0] != text {
		t.Fatalf("Split() = %q, want the text unchanged in one chunk", chunks)
	}
}

func TestSplitCutsOnLineBoundaries(t *testing.T) {
	line := strings.Repeat("あ", 90)
	text := strings.Join([]string{line, line, line}, "\n")
	chunks := Split(text, 200)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0] != line+"\n"+line {
		t.Fatalf("first chunk = %q, want two whole lines", chunks[0])
	}
	if chunks[1] != line {
		t.Fatalf("second chunk = %q, want the remaining line", chunks[1])
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) > 200 {
			t.Fatalf("chunk has %d runes, want at most 200", len([]rune(chunk)))
		}
	}
}

func TestSplitReopensFencedCodeBlock(t *testing.T) {
	body := strings.Repeat("x", 60)
	text := "```go\n" + body + "\n" + body + "\n" + body + "\n```"
	chunks := Split(text, 140)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want at least 2", len(chunks))
	}
	for i, chunk := range chunks {
		if !strings.HasPrefix(chunk, "```go\n") {
			t.Errorf("chunk %d does not open a fence: %q", i, chunk)
		}
		if !strings.HasSuffix(chunk, "\n```") {
			t.Errorf("chunk %d does not close its fence: %q", i, chunk)
		}
		if len([]rune(chunk)) > 140 {
			t.Errorf("chunk %d has %d runes, want at most 140", i, len([]rune(chunk)))
		}
	}
	var rebuilt strings.Builder
	for _, chunk := range chunks {
		for _, line := range strings.Split(chunk, "\n") {
			if line == "```go" || line == "```" {
				continue
			}
			rebuilt.WriteString(line + "\n")
		}
	}
	if want := body + "\n" + body + "\n" + body + "\n"; rebuilt.String() != want {
		t.Fatalf("fenced content = %q, want %q", rebuilt.String(), want)
	}
}

func TestSplitStopsTrackingFenceAfterItCloses(t *testing.T) {
	text := "```\ncode\n```\n" + strings.Repeat("prose\n", 40)
	for _, chunk := range Split(text, 100)[1:] {
		if strings.HasPrefix(chunk, "```") {
			t.Fatalf("chunk after the closed fence reopens it: %q", chunk)
		}
	}
}

func TestSplitCutsLineLongerThanLimitAtRuneBoundaries(t *testing.T) {
	text := strings.Repeat("界", 250)
	chunks := Split(text, 100)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	if len([]rune(chunks[0])) != 100 || len([]rune(chunks[2])) != 50 {
		t.Fatalf("chunk rune lengths = %d and %d, want 100 and 50",
			len([]rune(chunks[0])), len([]rune(chunks[2])))
	}
	if strings.Join(chunks, "") != text {
		t.Fatal("split did not preserve UTF-8 text")
	}
}

func TestSplitFallsBackToRuneChunksBelowMinimumLimit(t *testing.T) {
	text := "```go\n" + strings.Repeat("x", 40) + "\n```"
	chunks := Split(text, 10)
	for _, chunk := range chunks {
		if len([]rune(chunk)) > 10 {
			t.Fatalf("chunk has %d runes, want at most 10", len([]rune(chunk)))
		}
	}
	if strings.Join(chunks, "") != text {
		t.Fatal("split did not preserve the text")
	}
}

func TestSplitReturnsOneChunkForEmptyText(t *testing.T) {
	if chunks := Split("", 8000); len(chunks) != 1 || chunks[0] != "" {
		t.Fatalf("Split(\"\") = %q, want one empty chunk", chunks)
	}
}

func TestPlainTextRemovesMarkdownSyntax(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "heading",
			text: "#### API定義の根拠",
			want: "API定義の根拠",
		},
		{
			name: "link keeps its target",
			text: "根拠は [perk-review.ts](https://example.test/a.ts) です。",
			want: "根拠は perk-review.ts (https://example.test/a.ts) です。",
		},
		{
			name: "emphasis and code marks",
			text: "**altops** でも `approve` は同じです",
			want: "altops でも approve は同じです",
		},
		{
			name: "fence lines drop but code stays",
			text: "```go\nfmt.Println()\n```",
			want: "fmt.Println()",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := PlainText(test.text); got != test.want {
				t.Fatalf("PlainText() = %q, want %q", got, test.want)
			}
		})
	}
}
