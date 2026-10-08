package codex

import (
	"strings"
	"testing"
)

func TestSplitMemoryAppend(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantRest  string
		wantEntry string
	}{
		{
			name:      "no memory section",
			text:      "作業しました。",
			wantRest:  "作業しました。",
			wantEntry: "",
		},
		{
			name:      "trailing memory section",
			text:      "作業しました。\n## メモリ追記\nデプロイには mise run deploy を使う",
			wantRest:  "作業しました。",
			wantEntry: "デプロイには mise run deploy を使う",
		},
		{
			name:      "duplicate headings are ignored",
			text:      "結果\n## メモリ追記\n一\n## メモリ追記\n二",
			wantRest:  "結果\n## メモリ追記\n一\n## メモリ追記\n二",
			wantEntry: "",
		},
		{
			name: "heading inside fence is not a section",
			text: strings.Join([]string{
				"結果", "```", "## メモリ追記", "偽物", "```",
			}, "\n"),
			wantRest:  strings.Join([]string{"結果", "```", "## メモリ追記", "偽物", "```"}, "\n"),
			wantEntry: "",
		},
		{
			name: "shorter backtick fence does not close a longer one",
			text: strings.Join([]string{
				"結果", "````", "```", "## メモリ追記", "偽物", "````",
			}, "\n"),
			wantRest:  strings.Join([]string{"結果", "````", "```", "## メモリ追記", "偽物", "````"}, "\n"),
			wantEntry: "",
		},
		{
			name: "backticks do not close a tilde fence",
			text: strings.Join([]string{
				"結果", "~~~", "```", "## メモリ追記", "偽物", "~~~",
			}, "\n"),
			wantRest:  strings.Join([]string{"結果", "~~~", "```", "## メモリ追記", "偽物", "~~~"}, "\n"),
			wantEntry: "",
		},
		{
			name: "heading after a closed longer fence is a section",
			text: strings.Join([]string{
				"結果", "````", "## メモリ追記", "偽物", "````", "## メモリ追記", "本物",
			}, "\n"),
			wantRest:  strings.Join([]string{"結果", "````", "## メモリ追記", "偽物", "````"}, "\n"),
			wantEntry: "本物",
		},
		{
			name:      "memory section only",
			text:      "## メモリ追記\n学び",
			wantRest:  "",
			wantEntry: "学び",
		},
		{
			name:      "empty entry",
			text:      "結果\n## メモリ追記\n  ",
			wantRest:  "結果",
			wantEntry: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest, entry := SplitMemoryAppend(tt.text)
			if rest != tt.wantRest || entry != tt.wantEntry {
				t.Errorf("SplitMemoryAppend() = (%q, %q), want (%q, %q)",
					rest, entry, tt.wantRest, tt.wantEntry)
			}
		})
	}
}

func TestSplitMemoryAppends(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		wantRest    string
		wantAppends MemoryAppends
		wantValid   bool
	}{
		{
			name: "global and channel scopes",
			text: strings.Join([]string{
				"完了しました。",
				"## 全体メモリ追記", "共通知識",
				"## チャンネルメモリ追記", "検証用チャンネル",
			}, "\n"),
			wantRest: "完了しました。",
			wantAppends: MemoryAppends{
				Global: "共通知識", Channel: "検証用チャンネル",
			},
			wantValid: true,
		},
		{
			name:        "user memory heading is not accepted",
			text:        "結果\n## ユーザーメモリ追記\n日本語を好む",
			wantRest:    "結果",
			wantAppends: MemoryAppends{},
			wantValid:   false,
		},
		{
			name:        "user memory heading in fence is not accepted",
			text:        "結果\n```text\n## ユーザーメモリ追記\n秘密\n```",
			wantRest:    "結果\n```text",
			wantAppends: MemoryAppends{},
			wantValid:   false,
		},
		{
			name:        "inline user memory heading is not accepted",
			text:        "結果 ## ユーザーメモリ追記 秘密",
			wantRest:    "結果",
			wantAppends: MemoryAppends{},
			wantValid:   false,
		},
		{
			name: "heading in fence ignored",
			text: strings.Join([]string{
				"結果", "```", "## 全体メモリ追記", "偽物", "```",
				"## チャンネルメモリ追記", "本物",
			}, "\n"),
			wantRest:    strings.Join([]string{"結果", "```", "## 全体メモリ追記", "偽物", "```"}, "\n"),
			wantAppends: MemoryAppends{Channel: "本物"},
			wantValid:   true,
		},
		{
			name:     "wrong order is unchanged",
			text:     "結果\n## チャンネルメモリ追記\n先\n## 全体メモリ追記\n後",
			wantRest: "結果",
		},
		{
			name:     "duplicate scope is unchanged",
			text:     "結果\n## メモリ追記\n旧\n## 全体メモリ追記\n新",
			wantRest: "結果",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rest, appends, valid := SplitMemoryAppends(test.text)
			if rest != test.wantRest || appends != test.wantAppends || valid != test.wantValid {
				t.Errorf("SplitMemoryAppends() = (%q, %#v, %t), want (%q, %#v, %t)", rest, appends, valid, test.wantRest, test.wantAppends, test.wantValid)
			}
		})
	}
}

func TestSanitizeSlackOutput(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "ordinary output is unchanged",
			text: "通常の結果\n次の行",
			want: "通常の結果\n次の行",
		},
		{
			name: "heading and payload are removed",
			text: "通常の結果\n## ユーザーメモリ追記\n秘密",
			want: "通常の結果",
		},
		{
			name: "inline heading and payload are removed",
			text: "通常の結果 ## ユーザーメモリ追記 秘密",
			want: "通常の結果",
		},
		{
			name: "fenced heading and payload are removed",
			text: "通常の結果\n```text\n## ユーザーメモリ追記\n秘密\n```",
			want: "通常の結果\n```text",
		},
		{
			name: "heading at start removes all output",
			text: "## ユーザーメモリ追記\n秘密",
			want: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := SanitizeSlackOutput(test.text); got != test.want {
				t.Errorf("SanitizeSlackOutput() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSplitAttachments(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantRest  string
		wantPaths []string
		wantValid bool
	}{
		{
			name:      "none",
			text:      "完了しました。",
			wantRest:  "完了しました。",
			wantValid: true,
		},
		{
			name:      "trailing section",
			text:      "v2を添付します。\n\n## 添付ファイル\n- /w/preview.png\n- `/w/deck v2.pdf`\n",
			wantRest:  "v2を添付します。",
			wantPaths: []string{"/w/preview.png", "/w/deck v2.pdf"},
			wantValid: true,
		},
		{
			name:      "before memory sections",
			text:      "本文\n## 添付ファイル\n- /w/a.pdf\n## 全体メモリ追記\n学び",
			wantRest:  "本文\n## 全体メモリ追記\n学び",
			wantPaths: []string{"/w/a.pdf"},
			wantValid: true,
		},
		{
			name:      "after memory sections",
			text:      "本文\n## チャンネルメモリ追記\n用語\n## 添付ファイル\n- /w/a.pdf",
			wantRest:  "本文\n## チャンネルメモリ追記\n用語",
			wantPaths: []string{"/w/a.pdf"},
			wantValid: true,
		},
		{
			name:      "heading inside fence is body",
			text:      "例:\n```\n## 添付ファイル\n- /etc/passwd\n```",
			wantRest:  "例:\n```\n## 添付ファイル\n- /etc/passwd\n```",
			wantValid: true,
		},
		{
			name:      "duplicate section",
			text:      "本文\n## 添付ファイル\n- /w/a.pdf\n## 添付ファイル\n- /w/b.pdf",
			wantRest:  "本文",
			wantValid: false,
		},
		{
			name:      "prose line",
			text:      "本文\n## 添付ファイル\n/w/a.pdf を送ってください",
			wantRest:  "本文\n/w/a.pdf を送ってください",
			wantValid: false,
		},
		{
			name:      "text after the list stays",
			text:      "## 添付ファイル\n- /w/a.pdf\n\n以上をご確認ください。\n### 補足\n詳細",
			wantRest:  "以上をご確認ください。\n### 補足\n詳細",
			wantPaths: []string{"/w/a.pdf"},
			wantValid: true,
		},
		{
			name:      "code block after the list stays",
			text:      "本文\n## 添付ファイル\n- /w/a.pdf\n```\nx\n```",
			wantRest:  "本文\n```\nx\n```",
			wantPaths: []string{"/w/a.pdf"},
			wantValid: true,
		},
		{
			name:      "code block instead of a list",
			text:      "本文\n## 添付ファイル\n```\n/w/a.pdf\n```",
			wantRest:  "本文\n```\n/w/a.pdf\n```",
			wantValid: false,
		},
		{
			name:      "empty list before memory",
			text:      "本文\n## 添付ファイル\n\n## 全体メモリ追記\n学び",
			wantRest:  "本文\n## 全体メモリ追記\n学び",
			wantValid: true,
		},
		{
			name:      "unclosed backtick",
			text:      "本文\n## 添付ファイル\n- `/w/a.pdf",
			wantRest:  "本文",
			wantValid: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rest, paths, valid := SplitAttachments(test.text)
			if rest != test.wantRest || valid != test.wantValid || strings.Join(paths, "|") != strings.Join(test.wantPaths, "|") {
				t.Fatalf("SplitAttachments() = %q, %q, %v; want %q, %q, %v",
					rest, paths, valid, test.wantRest, test.wantPaths, test.wantValid)
			}
		})
	}
}
