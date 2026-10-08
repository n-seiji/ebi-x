package codex

import (
	"strings"
	"testing"
)

func TestParsePlan(t *testing.T) {
	tests := []struct {
		name            string
		text            string
		wantPolicy      string
		wantInstruction string
		wantErr         bool
	}{
		{
			name:            "valid",
			text:            "preamble\n## 方針\n慎重に進める\n## 作業指示\nテストを追加する",
			wantPolicy:      "慎重に進める",
			wantInstruction: "テストを追加する",
		},
		{
			name:    "policy only",
			text:    "## 方針\n慎重に進める",
			wantErr: true,
		},
		{
			name:    "instruction only",
			text:    "## 作業指示\nテストを追加する",
			wantErr: true,
		},
		{
			name:    "wrong order",
			text:    "## 作業指示\n作業\n## 方針\n方針",
			wantErr: true,
		},
		{
			name:    "duplicate policy",
			text:    "## 方針\n一\n## 方針\n二\n## 作業指示\n作業",
			wantErr: true,
		},
		{
			name:    "duplicate instruction",
			text:    "## 方針\n方針\n## 作業指示\n一\n## 作業指示\n二",
			wantErr: true,
		},
		{
			name:    "empty policy",
			text:    "## 方針\n\n## 作業指示\n作業",
			wantErr: true,
		},
		{
			name:    "empty instruction",
			text:    "## 方針\n方針\n## 作業指示\n  ",
			wantErr: true,
		},
		{
			name: "heading-like text in closed fences",
			text: strings.Join([]string{
				"```markdown", "## 方針", "偽物", "```",
				"## 方針", "本物", "## 作業指示", "作業",
				"~~~", "## 作業指示", "偽物", "~~~",
			}, "\n"),
			wantPolicy:      "本物",
			wantInstruction: "作業\n~~~\n## 作業指示\n偽物\n~~~",
		},
		{
			name: "heading-like text in unclosed fence",
			text: strings.Join([]string{
				"## 方針", "本物", "## 作業指示", "作業",
				"```", "## 方針", "偽物",
			}, "\n"),
			wantPolicy:      "本物",
			wantInstruction: "作業\n```\n## 方針\n偽物",
		},
		{
			name: "shorter fence does not close a longer fence",
			text: strings.Join([]string{
				"## 方針", "本物", "## 作業指示", "作業",
				"````", "```", "## 方針", "偽物", "````",
			}, "\n"),
			wantPolicy:      "本物",
			wantInstruction: "作業\n````\n```\n## 方針\n偽物\n````",
		},
		{
			name:            "heading not at line start",
			text:            "文章中の ## 方針 は偽物\n## 方針\n本物\n## 作業指示\n作業",
			wantPolicy:      "本物",
			wantInstruction: "作業",
		},
		{
			name:            "surrounding heading whitespace",
			text:            "  ## 方針  \n方針\n\t## 作業指示 \t\n作業",
			wantPolicy:      "方針",
			wantInstruction: "作業",
		},
		{
			name:            "NONE alone",
			text:            "## 方針\n何もしない\n## 作業指示\nNONE",
			wantPolicy:      "何もしない",
			wantInstruction: "",
		},
		{
			name:            "NONE with commentary is ordinary instruction",
			text:            "## 方針\n確認する\n## 作業指示\nNONE\nただし後で確認",
			wantPolicy:      "確認する",
			wantInstruction: "NONE\nただし後で確認",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, instruction, err := ParsePlan(tt.text)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParsePlan() = (%q, %q, nil), want error", policy, instruction)
				}
				if policy != "" || instruction != "" {
					t.Errorf("ParsePlan() on error = (%q, %q), want empty values", policy, instruction)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePlan() error = %v", err)
			}
			if policy != tt.wantPolicy || instruction != tt.wantInstruction {
				t.Errorf("ParsePlan() = (%q, %q), want (%q, %q)",
					policy, instruction, tt.wantPolicy, tt.wantInstruction)
			}
		})
	}
}

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
			wantRest:  "本文",
			wantValid: false,
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
