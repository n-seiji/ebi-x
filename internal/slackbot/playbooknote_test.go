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
	// Playbook notes are only taken from the approval channel.
	bot.config.ApprovalChannelID = "C1"
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
	bot.config.ApprovalChannelID = ""
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

func playbookNote(text string) string {
	return "作りました。\n## playbook メモ追記\n- 対象: slides\n- 内容: " + text
}

func lastThreadPost(api *fakeSlack, threadTS string) string {
	for i := len(api.posts) - 1; i >= 0; i-- {
		if api.posts[i].threadTS == threadTS {
			return api.posts[i].text
		}
	}
	return ""
}

func TestPlaybookNotesFoldOnceTheyGather(t *testing.T) {
	bot, api, runner := newPlaybookNoteBot(t, playbookNote("一つ目"), playbookNote("二つ目"), playbookNote("三つ目"), "納品の目安を2週後に更新しました。", "次の作業です。")
	path := filepath.Join(bot.config.PlaybooksDir, "slides.md")
	runner.onRun = func(call int) {
		if call != 4 {
			return
		}
		if err := os.WriteFile(path, []byte("---\nname: slides\ndescription: スライドを作る\n---\n初回の納品は2週後"), 0o600); err != nil {
			t.Error(err)
		}
		folded := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(path, folded, folded); err != nil {
			t.Error(err)
		}
	}
	for i, ts := range []string{"100.1", "200.1", "300.1"} {
		bot.HandleMention(context.Background(), mentionAt(ts, ""))
		if folded := strings.Contains(lastThreadPost(api, ts), "本文に取り込みました"); folded != (i == 2) {
			t.Fatalf("note %d: folded = %v, posts %q", i+1, folded, api.postTexts)
		}
	}
	if runner.calls != 4 || !strings.Contains(runner.prompts[3], "依頼者への確認は不要") || !strings.Contains(runner.prompts[3], "三つ目") {
		t.Fatalf("fold turn not run: calls %d, prompts %q", runner.calls, runner.prompts)
	}
	if runner.threadIDs[3] != "codex-thread" {
		t.Errorf("fold ran in session %q, want the note turn's session", runner.threadIDs[3])
	}
	var admin []string
	for _, post := range api.posts {
		if post.channel == "C1" && post.threadTS == "" {
			admin = append(admin, post.text)
		}
	}
	if len(admin) != 1 || !strings.Contains(admin[0], "playbook「slides」にメモ3件を取り込みました") || !strings.Contains(admin[0], "納品の目安を2週後に更新しました。") {
		t.Fatalf("admin posts = %q, want one fold record", admin)
	}

	bot.HandleMention(context.Background(), mentionAt("400.1", ""))
	if got := runner.prompts[len(runner.prompts)-1]; strings.Contains(got, "<playbook_notes>") {
		t.Errorf("folded notes still shown:\n%s", got)
	}
}

func TestPlaybookFoldThatChangesNothingIsNotRecorded(t *testing.T) {
	bot, api, _ := newPlaybookNoteBot(t, playbookNote("一つ目"), playbookNote("二つ目"), playbookNote("三つ目"), "一時的な事情だけなので本文は変えませんでした。")
	for _, ts := range []string{"100.1", "200.1", "300.1"} {
		bot.HandleMention(context.Background(), mentionAt(ts, ""))
	}
	if last := lastThreadPost(api, "300.1"); !strings.Contains(last, "本文は変わりませんでした: 一時的な事情だけなので本文は変えませんでした。") {
		t.Fatalf("last post = %q, want the skipped notice", last)
	}
	for _, post := range api.posts {
		if post.channel == "C1" && post.threadTS == "" {
			t.Fatalf("admin post %q for a fold that changed nothing", post.text)
		}
	}
}
