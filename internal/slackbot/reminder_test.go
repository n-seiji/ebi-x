package slackbot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

const reminderAnswer = "明日の10時にお知らせします。\n\n## リマインダー\n- いつ: 2026-10-10T10:00:00+09:00\n- 内容: 見積もりを送る"

func TestTurnSetsReminder(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, followUpRunner(reminderAnswer))

	bot.HandleMention(context.Background(), mention())

	want := state.Reminder{
		ID: "C1:100.1:100.1", Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Text: "見積もりを送る",
		DueAt: time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC), CreatedAt: followUpNow,
	}
	got, ok := store.reminders[want.ID]
	if !ok || got.Text != want.Text || !got.DueAt.Equal(want.DueAt) || got.AuthorID != "U1" || got.ThreadTS != "100.1" {
		t.Fatalf("reminders = %#v, want %#v", store.reminders, want)
	}
	post := api.postTexts[len(api.postTexts)-1]
	if strings.Contains(post, "## リマインダー") || !strings.Contains(post, "にこのスレッドでリマインドします: 見積もりを送る") {
		t.Fatalf("result post = %q, want the section replaced by a notice", post)
	}
}

func TestReminderRejectsBadTimeLimitAndUnattendedTurns(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, followUpRunner("## リマインダー\n- いつ: 明日\n- 内容: 送る", reminderAnswer, reminderAnswer))

	bot.HandleMention(context.Background(), mention())
	if len(store.reminders) != 0 || !strings.Contains(api.postTexts[len(api.postTexts)-1], "「明日」を読み取れないか範囲外") {
		t.Fatalf("reminders = %#v, post = %q; want the time rejected", store.reminders, api.postTexts[len(api.postTexts)-1])
	}

	for i := range maxRemindersPerUser {
		_ = store.AddReminder(state.Reminder{ID: fmt.Sprint(i), Channel: "C1", ThreadTS: "1.1", AuthorID: "U1", DueAt: followUpNow.Add(time.Hour)})
	}
	bot.HandleMention(context.Background(), mentionAt("200.1", ""))
	if len(store.reminders) != maxRemindersPerUser || !strings.Contains(api.postTexts[len(api.postTexts)-1], "20件まで") {
		t.Fatalf("post = %q, want the limit notice", api.postTexts[len(api.postTexts)-1])
	}

	store.reminders = nil
	store.threadIDs = map[string]string{"v5:C1:100.1": "codex-thread"}
	bot.fireFollowUp(context.Background(), state.FollowUp{Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Task: "確認する", DueAt: followUpNow, Chain: 1})
	if len(store.reminders) != 0 || !strings.Contains(api.postTexts[len(api.postTexts)-1], reminderSourceNotice) {
		t.Fatalf("reminders = %#v, post = %q; want none from a follow-up", store.reminders, api.postTexts[len(api.postTexts)-1])
	}
}

func TestReminderCancelRequestAndStopCommand(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, followUpRunner("取り消しました。\n\n## リマインダー\n- いつ: 停止"))
	pending := func() {
		store.reminders = map[string]state.Reminder{
			"a": {ID: "a", Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", DueAt: followUpNow.Add(time.Hour)},
			"b": {ID: "b", Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", DueAt: followUpNow.Add(2 * time.Hour)},
			"c": {ID: "c", Channel: "C1", ThreadTS: "300.1", AuthorID: "U1", DueAt: followUpNow.Add(time.Hour)},
		}
	}

	pending()
	bot.HandleMention(context.Background(), mention())
	if _, ok := store.reminders["c"]; len(store.reminders) != 1 || !ok || !strings.Contains(api.postTexts[len(api.postTexts)-1], "リマインダー（2件）を取り消しました") {
		t.Fatalf("reminders = %#v, post = %q; want this thread's cancelled", store.reminders, api.postTexts[len(api.postTexts)-1])
	}

	pending()
	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "100.3", ThreadTimeStamp: "100.1", Text: "<@UBOT> 停止"})
	if len(store.reminders) != 1 || api.postTexts[len(api.postTexts)-1] != reminderStoppedMsg {
		t.Fatalf("reminders = %#v, posts = %q; want the stop command to cancel them", store.reminders, api.postTexts)
	}
}

func TestRunRemindersPostsDueOnesOnce(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newFollowUpBot(t, store, api, runner)
	store.reminders = map[string]state.Reminder{
		"due":   {ID: "due", Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Text: "見積もりを送る", DueAt: followUpNow},
		"late":  {ID: "late", Channel: "C1", ThreadTS: "200.1", AuthorID: "U1", Text: "会議の準備", DueAt: followUpNow.Add(-time.Hour)},
		"later": {ID: "later", Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Text: "後で", DueAt: followUpNow.Add(time.Hour)},
		"gone":  {ID: "gone", Channel: "C1", ThreadTS: "100.1", AuthorID: "U9", Text: "権限なし", DueAt: followUpNow},
	}
	ctx, cancel := context.WithCancel(context.Background())
	bot.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}

	bot.runReminders(ctx)

	if runner.calls != 0 {
		t.Fatalf("runner calls = %d, want reminders to start no work", runner.calls)
	}
	byThread := make(map[string]string)
	for _, post := range api.posts {
		byThread[post.threadTS] = post.text
	}
	if len(api.posts) != 2 || byThread["100.1"] != "<@U1> 🔔 リマインダー: 見積もりを送る" {
		t.Fatalf("posts = %#v, want the due reminder in its thread", api.posts)
	}
	if late := byThread["200.1"]; !strings.HasPrefix(late, "<@U1> 🔔 リマインダー: 会議の準備") || !strings.Contains(late, "遅れました") {
		t.Fatalf("late post = %q, want it marked late", late)
	}
	if _, ok := store.reminders["later"]; len(store.reminders) != 1 || !ok {
		t.Fatalf("reminders = %#v, want only the later one left", store.reminders)
	}
}
