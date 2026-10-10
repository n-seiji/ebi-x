package codex

import "testing"

func TestSplitPlaybookNote(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		wantRest    string
		wantNote    *PlaybookNote
		wantInvalid bool
	}{
		{
			name:     "no section",
			text:     "回答です。",
			wantRest: "回答です。",
		},
		{
			name:     "note",
			text:     "回答です。\n\n## playbook メモ追記\n- 対象: slides\n- 内容: 納品は3週後から2週後に変わった\n\n## チャンネルメモリ追記\n用語",
			wantRest: "回答です。\n\n## チャンネルメモリ追記\n用語",
			wantNote: &PlaybookNote{Target: "slides", Body: "納品は3週後から2週後に変わった"},
		},
		{
			name:     "full-width colon and backquoted name",
			text:     "## playbook メモ追記\n- 対象：`slides`\n- 内容：締切は金曜",
			wantNote: &PlaybookNote{Target: "slides", Body: "締切は金曜"},
		},
		{
			name:        "missing body",
			text:        "回答です。\n## playbook メモ追記\n- 対象: slides",
			wantRest:    "回答です。",
			wantInvalid: true,
		},
		{
			name:        "repeated section",
			text:        "回答です。\n## playbook メモ追記\n- 対象: a\n- 内容: x\n## playbook メモ追記\n- 対象: b\n- 内容: y",
			wantRest:    "回答です。",
			wantInvalid: true,
		},
		{
			name:     "heading inside a code block is prose",
			text:     "```\n## playbook メモ追記\n- 対象: a\n```",
			wantRest: "```\n## playbook メモ追記\n- 対象: a\n```",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest, note, invalid := SplitPlaybookNote(tt.text)
			if rest != tt.wantRest || invalid != tt.wantInvalid {
				t.Errorf("SplitPlaybookNote() rest = %q, invalid = %v; want %q, %v", rest, invalid, tt.wantRest, tt.wantInvalid)
			}
			if (note == nil) != (tt.wantNote == nil) || (note != nil && *note != *tt.wantNote) {
				t.Errorf("SplitPlaybookNote() note = %#v, want %#v", note, tt.wantNote)
			}
		})
	}
}
