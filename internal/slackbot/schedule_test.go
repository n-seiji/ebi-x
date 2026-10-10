package slackbot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

const scheduleAnswer = "毎朝確認します。\n\n## 定期実行\n- いつ: 平日 9:00\n- やること: main の CI の失敗を確認して報告する"

func TestTurnSetsSchedule(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, followUpRunner(scheduleAnswer))

	bot.HandleMention(context.Background(), mention())

	spec, _ := codex.ParseScheduleSpec("平日 09:00")
	want := state.Schedule{
		Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Spec: "平日 09:00", Task: "main の CI の失敗を確認して報告する",
		NextAt: spec.Next(followUpNow.In(time.Local)), CreatedAt: followUpNow,
	}
	if got := store.schedules["C1:100.1"]; got != want {
		t.Fatalf("schedule = %#v, want %#v", got, want)
	}
	post := api.postTexts[len(api.postTexts)-1]
	if strings.Contains(post, "## 定期実行") || !strings.Contains(post, "🔁 定期実行を設定しました: 平日 09:00 に「main の CI の失敗を確認して報告する」") {
		t.Fatalf("result post = %q, want the section replaced by a notice", post)
	}
}

func TestUnattendedTurnCannotSetSchedule(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, followUpRunner(scheduleAnswer))
	store.threadIDs = map[string]string{"v5:C1:100.1": "codex-thread"}

	bot.fireFollowUp(context.Background(), state.FollowUp{Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Task: "確認する", DueAt: followUpNow, Chain: 1})

	if len(store.schedules) != 0 {
		t.Fatalf("schedules = %#v, want none from a follow-up", store.schedules)
	}
	if post := api.postTexts[len(api.postTexts)-1]; !strings.Contains(post, scheduleSourceNotice) {
		t.Fatalf("result post = %q, want the refusal", post)
	}
}

func TestScheduleRejectsBadSpecAndLimit(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, followUpRunner("## 定期実行\n- いつ: 毎時\n- やること: 確認する", scheduleAnswer))

	bot.HandleMention(context.Background(), mention())
	if len(store.schedules) != 0 || !strings.Contains(api.postTexts[len(api.postTexts)-1], "「毎時」を読み取れなかった") {
		t.Fatalf("schedules = %#v, post = %q; want the spec rejected", store.schedules, api.postTexts[len(api.postTexts)-1])
	}

	for i := range maxSchedulesPerUser {
		_ = store.SetSchedule(fmt.Sprintf("C1:%d.1", i), state.Schedule{Channel: "C1", ThreadTS: fmt.Sprintf("%d.1", i), AuthorID: "U1", Spec: "毎日 09:00"})
	}
	bot.HandleMention(context.Background(), mentionAt("200.1", ""))
	if _, ok := store.schedules["C1:200.1"]; ok || !strings.Contains(api.postTexts[len(api.postTexts)-1], "5件まで") {
		t.Fatalf("post = %q, want the limit notice", api.postTexts[len(api.postTexts)-1])
	}
}

func TestScheduleStopRequestAndCommand(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, followUpRunner("止めました。\n\n## 定期実行\n- いつ: 停止"))
	store.schedules = map[string]state.Schedule{"C1:100.1": {Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Spec: "平日 09:00"}}

	bot.HandleMention(context.Background(), mention())
	if _, ok := store.schedules["C1:100.1"]; ok || !strings.Contains(api.postTexts[len(api.postTexts)-1], "🔁 定期実行（平日 09:00）を止めました。") {
		t.Fatalf("schedules = %#v, post = %q; want it stopped", store.schedules, api.postTexts[len(api.postTexts)-1])
	}

	store.schedules = map[string]state.Schedule{"C1:100.1": {Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Spec: "平日 09:00"}}
	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "100.3", ThreadTimeStamp: "100.1", Text: "<@UBOT> 停止"})
	if _, ok := store.schedules["C1:100.1"]; ok || api.postTexts[len(api.postTexts)-1] != scheduleStoppedMsg {
		t.Fatalf("schedules = %#v, posts = %q; want the stop command to stop it", store.schedules, api.postTexts)
	}
}

func TestFireScheduleStartsNewThread(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{topTimestamp: "900.1"}
	runner := followUpRunner("CI は成功しています。")
	bot := newFollowUpBot(t, store, api, runner)

	bot.fireSchedule(context.Background(), state.Schedule{Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Spec: "平日 09:00", Task: "CI を確認する"})

	if len(api.posts) != 2 || api.posts[0].threadTS != "" || !strings.HasPrefix(api.posts[0].text, "🔁 定期実行（平日 09:00）: CI を確認する") ||
		!strings.Contains(api.posts[0].text, "https://slack.test/archives/C1/p100.1") {
		t.Fatalf("posts = %#v, want a new top-level post linking the original thread", api.posts)
	}
	if api.posts[1].threadTS != "900.1" || !strings.Contains(api.posts[1].text, "CI は成功しています。") {
		t.Fatalf("result = %#v, want it in the new thread", api.posts[1])
	}
	if runner.threadIDs[0] != "" || !strings.Contains(runner.prompts[0], "定期実行として設定した作業です") ||
		!strings.Contains(runner.prompts[0], "<authenticated_slack_author_id>\nU1\n") {
		t.Fatalf("turn = thread %q prompt %q, want a fresh session for the author", runner.threadIDs[0], runner.prompts[0])
	}
}

func TestFireScheduleSkipsWhenAccessWasRemoved(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newFollowUpBot(t, store, api, runner)

	bot.fireSchedule(context.Background(), state.Schedule{Channel: "C1", ThreadTS: "100.1", AuthorID: "U9", Spec: "平日 09:00", Task: "確認する"})

	if runner.calls != 0 || len(api.postTexts) != 0 {
		t.Fatalf("runner calls = %d, posts = %q; want nothing", runner.calls, api.postTexts)
	}
}

func TestRunSchedulesStartsDueOnesAndAdvances(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{topTimestamp: "900.1"}
	runner := followUpRunner("確認しました。")
	bot := newFollowUpBot(t, store, api, runner)
	store.schedules = map[string]state.Schedule{
		"C1:100.1": {Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Spec: "毎日 09:00", Task: "確認する", NextAt: followUpNow},
		"C1:200.1": {Channel: "C1", ThreadTS: "200.1", AuthorID: "U1", Spec: "毎日 09:00", Task: "後で", NextAt: followUpNow.Add(time.Hour)},
	}
	acceptCtx, stopAccepting := context.WithCancel(context.Background())
	bot.sleep = func(ctx context.Context, duration time.Duration) error {
		if duration == followUpPollInterval {
			stopAccepting()
			return context.Canceled
		}
		return sleepContext(ctx, duration)
	}

	var wg sync.WaitGroup
	bot.runSchedules(acceptCtx, context.Background(), &wg)
	wg.Wait()

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want only the due schedule", runner.calls)
	}
	if got := store.schedules["C1:100.1"]; !got.NextAt.After(followUpNow) || got.Runs != 1 {
		t.Fatalf("schedule = %#v, want it advanced", got)
	}
}
