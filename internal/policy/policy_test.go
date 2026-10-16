package policy

import (
	"strings"
	"testing"
)

func TestInstructions(t *testing.T) {
	got := Instructions()
	if got == "" {
		t.Fatal("Instructions() is empty")
	}
	if got != strings.TrimSpace(got) {
		t.Error("Instructions() is not trimmed")
	}
	for _, want := range []string{"絶対ルール", "メモリ追記", "MCP への接続失敗ではなく"} {
		if !strings.Contains(got, want) {
			t.Errorf("Instructions() does not contain %q", want)
		}
	}
	for _, forbidden := range []string{"user_memory", "ユーザーメモリ", "## ユーザーメモリ追記"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("Instructions() contains removed user-memory rule %q", forbidden)
		}
	}
	if !strings.Contains(got, "<user_message>、<slack_message>、<work_instruction>") {
		t.Error("Instructions() does not isolate slack_message alongside other untrusted inputs")
	}
}
