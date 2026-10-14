package slackbot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
)

func newPlaybookNoteBot(t *testing.T, responses ...string) (*Bot, *fakeSlack, *fakeRunner) {
	t.Helper()
	api := &fakeSlack{}
	runner := &fakeRunner{}
	for _, response := range responses {
		runner.responses = append(runner.responses, runnerResponse{result: &codex.TurnResult{
			Completed: true, Messages: []string{response},
		}})
	}
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)
	bot.config.PlaybooksDir = t.TempDir()
	path := filepath.Join(bot.config.PlaybooksDir, "slides.md")
	if err := os.WriteFile(path, []byte("---\nname: slides\ndescription: スライドを作る\n---\n初回の納品は3週後"), 0o600); err != nil {
		t.Fatal(err)
	}
	edited := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, edited, edited); err != nil {
		t.Fatal(err)
	}
	bot.now = func() time.Time { return edited.Add(24 * time.Hour) }
	return bot, api, runner
}

func TestPlaybookNoteReachesTheNextThread(t *testing.T) {
	bot, api, runner := newPlaybookNoteBot(t,
		"作りました。\n\n## playbook メモ追記\n- 対象: slides\n- 内容: 初回の納品は2週後に変わった",
		"次の作業です。",
	)
	bot.HandleMention(context.Background(), mentionAt("100.1", ""))
	posts := strings.Join(api.postTexts, "|")
	if !strings.Contains(posts, "📝 playbook「slides」のメモを更新しました: 初回の納品は2週後に変わった") || strings.Contains(posts, "## playbook メモ追記") {
		t.Fatalf("posts = %q, want the result and the note notice", posts)
	}

	bot.HandleMention(context.Background(), mentionAt("200.1", ""))
	got := runner.prompts[len(runner.prompts)-1]
	if !strings.Contains(got, "<playbook_notes>\n- 2026-10-02 初回の納品は2週後に変わった\n  </playbook_notes>") {
		t.Fatalf("next thread's prompt lacks the note:\n%s", got)
	}
}

func TestPlaybookNoteForUnknownPlaybook(t *testing.T) {
	bot, api, _ := newPlaybookNoteBot(t, "作りました。\n## playbook メモ追記\n- 対象: deploy\n- 内容: x")
	bot.HandleMention(context.Background(), mentionAt("100.1", ""))
	if posts := strings.Join(api.postTexts, "|"); !strings.Contains(posts, "playbook「deploy」が見つからない") {
		t.Fatalf("posts = %q, want a missing playbook notice", posts)
	}
	if _, err := os.Stat(filepath.Join(bot.config.MemoryDir, "playbooks")); !os.IsNotExist(err) {
		t.Errorf("notes directory exists after an unknown target: %v", err)
	}
}

func TestPlaybookNoteNeedsSharedWrites(t *testing.T) {
	bot, api, _ := newPlaybookNoteBot(t, "作りました。\n## playbook メモ追記\n- 対象: slides\n- 内容: x")
	bot.sharedWriteChannels = map[string]struct{}{}
	bot.HandleMention(context.Background(), mentionAt("100.1", ""))
	posts := strings.Join(api.postTexts, "|")
	if strings.Contains(posts, "メモを更新しました") || strings.Contains(posts, "## playbook メモ追記") {
		t.Fatalf("posts = %q, want no note saved", posts)
	}
	if _, err := os.Stat(filepath.Join(bot.config.MemoryDir, "playbooks", "slides.md")); !os.IsNotExist(err) {
		t.Errorf("note saved from a channel without shared writes: %v", err)
	}
}

func TestMalformedPlaybookNote(t *testing.T) {
	bot, api, _ := newPlaybookNoteBot(t, "作りました。\n## playbook メモ追記\n- 対象: slides")
	bot.HandleMention(context.Background(), mentionAt("100.1", ""))
	if posts := strings.Join(api.postTexts, "|"); !strings.Contains(posts, playbookNoteInvalidNotice) || !strings.Contains(posts, "作りました。") {
		t.Fatalf("posts = %q, want the result and the malformed notice", posts)
	}
}
