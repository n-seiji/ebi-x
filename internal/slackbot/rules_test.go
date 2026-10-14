package slackbot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadActionRules(t *testing.T) {
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules.md")
	if err := os.WriteFile(rules, []byte("- push は確認せずに行う\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large.md")
	if err := os.WriteFile(large, []byte(strings.Repeat("a", maxActionRulesBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		path string
		want string
	}{
		"unset":     {path: "", want: ""},
		"missing":   {path: filepath.Join(dir, "none.md"), want: ""},
		"present":   {path: rules, want: "- push は確認せずに行う\n"},
		"too large": {path: large, want: ""},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := readActionRules(test.path); got != test.want {
				t.Fatalf("readActionRules() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNewThreadPromptCarriesOperatorRules(t *testing.T) {
	rules := filepath.Join(t.TempDir(), "rules.md")
	if err := os.WriteFile(rules, []byte("- PR の作成は確認せずに行う"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := successfulTurnRunner()
	bot := newTestBot(t, &fakeStore{claim: true}, &fakeSlack{}, runner)
	bot.config.ActionRulesFile = rules

	bot.HandleMention(context.Background(), mention())

	if len(runner.prompts) != 1 || !strings.Contains(runner.prompts[0], "<action_rules>\n- PR の作成は確認せずに行う\n</action_rules>") {
		t.Fatalf("prompts = %q, want the operator's rules", runner.prompts)
	}
}
