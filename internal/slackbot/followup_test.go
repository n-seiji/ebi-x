package slackbot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

var followUpNow = time.Date(2026, 10, 9, 7, 0, 0, 0, time.UTC)

func followUpRunner(messages ...string) *fakeRunner {
	runner := &fakeRunner{}
	for _, message := range messages {
		runner.responses = append(runner.responses, runnerResponse{result: &codex.TurnResult{
			Completed: true,
			Messages:  []string{message},
		}})
	}
	return runner
}

func newFollowUpBot(t *testing.T, store *fakeStore, api *fakeSlack, runner *fakeRunner) *Bot {
	t.Helper()
	bot := newTestBot(t, store, api, runner)
	bot.now = func() time.Time { return followUpNow }
	return bot
}

func TestTurnSchedulesFollowUp(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := followUpRunner("CIを待っています。\n\n## フォローアップ\n- いつ: 30m\n- やること: CIの結果を確認して報告する")
	bot := newFollowUpBot(t, store, api, runner)

	bot.HandleMention(context.Background(), mention())

	followUp, ok := store.GetFollowUp("C1:100.1")
	if !ok {
		t.Fatal("follow-up was not scheduled")
	}
	want := state.FollowUp{
		Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Task: "CIの結果を確認して報告する",
		DueAt: followUpNow.Add(30 * time.Minute), Chain: 1, CreatedAt: followUpNow,
	}
	if followUp != want {
		t.Fatalf("follow-up = %#v, want %#v", followUp, want)
	}
	post := api.postTexts[len(api.postTexts)-1]
	if strings.Contains(post, "## フォローアップ") || !strings.Contains(post, "ごろにフォローアップします: CIの結果を確認して報告する") {
		t.Fatalf("result post = %q, want the section replaced by a notice", post)
	}
}

func TestRequestReplacesOrCancelsPendingFollowUp(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := followUpRunner("了解です。")
	bot := newFollowUpBot(t, store, api, runner)
	store.threadIDs = map[string]string{"v5:C1:100.1": "codex-thread"}
	store.followUps = map[string]state.FollowUp{"C1:100.1": {
		Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Task: "CIを確認する", DueAt: followUpNow.Add(time.Hour), Chain: 1,
	}}

	bot.HandleMention(context.Background(), mentionAt("100.2", "100.1"))

	if !strings.Contains(runner.prompts[0], "予定中のフォローアップがあります") || !strings.Contains(runner.prompts[0], "CIを確認する") {
		t.Fatalf("resume prompt does not mention the pending follow-up: %q", runner.prompts[0])
	}
	if _, ok := store.GetFollowUp("C1:100.1"); ok {
		t.Fatal("pending follow-up survived an answer that did not set one")
	}
	if post := api.postTexts[len(api.postTexts)-1]; !strings.Contains(post, "は取り消しました") {
		t.Fatalf("result post = %q, want a cancellation notice", post)
	}
}

func TestFireFollowUpResumesSessionAndReports(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := followUpRunner("CIは成功しました。\n\n## フォローアップ\n- いつ: 1h\n- やること: デプロイを確認する")
	bot := newFollowUpBot(t, store, api, runner)
	store.threadIDs = map[string]string{"v5:C1:100.1": "codex-thread"}

	bot.fireFollowUp(context.Background(), state.FollowUp{
		Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Task: "CIを確認する", DueAt: followUpNow, Chain: 2,
	})

	if len(api.postTexts) != 2 || !strings.HasPrefix(api.postTexts[0], "⏰ 予定していたフォローアップを始めます: CIを確認する") {
		t.Fatalf("posts = %q, want an announcement and the result", api.postTexts)
	}
	if runner.threadIDs[0] != "codex-thread" || !strings.Contains(runner.prompts[0], "予定した作業: CIを確認する") {
		t.Fatalf("turn = thread %q prompt %q, want the session resumed with the task", runner.threadIDs[0], runner.prompts[0])
	}
	if strings.Contains(runner.prompts[0], "<slack_message>") {
		t.Fatal("follow-up prompt presents the task as a Slack message")
	}
	next, ok := store.GetFollowUp("C1:100.1")
	if !ok || next.Chain != 3 || next.Task != "デプロイを確認する" {
		t.Fatalf("next follow-up = %#v, %v; want chain 3", next, ok)
	}
	if !strings.Contains(api.postTexts[1], "CIは成功しました。") {
		t.Fatalf("result post = %q", api.postTexts[1])
	}
}

func TestFollowUpChainLimit(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := followUpRunner("まだです。\n\n## フォローアップ\n- いつ: 1h\n- やること: また確認する")
	bot := newFollowUpBot(t, store, api, runner)
	store.threadIDs = map[string]string{"v5:C1:100.1": "codex-thread"}

	bot.fireFollowUp(context.Background(), state.FollowUp{
		Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Task: "確認する", DueAt: followUpNow, Chain: codex.MaxFollowUpChain,
	})

	if _, ok := store.GetFollowUp("C1:100.1"); ok {
		t.Fatal("follow-up scheduled past the chain limit")
	}
	if post := api.postTexts[len(api.postTexts)-1]; !strings.Contains(post, "上限") {
		t.Fatalf("result post = %q, want a chain-limit notice", post)
	}
}

func TestFireFollowUpDropsWhenAccessWasRemoved(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newFollowUpBot(t, store, api, runner)

	bot.fireFollowUp(context.Background(), state.FollowUp{
		Channel: "C1", ThreadTS: "100.1", AuthorID: "U9", Task: "確認する", DueAt: followUpNow,
	})

	if runner.calls != 0 || len(api.postTexts) != 0 {
		t.Fatalf("runner calls = %d, posts = %q; want nothing", runner.calls, api.postTexts)
	}
}

func TestStopCancelsScheduledFollowUp(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})
	store.followUps = map[string]state.FollowUp{"C1:100.1": {Channel: "C1", ThreadTS: "100.1", DueAt: followUpNow.Add(time.Hour)}}

	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{
		User: "U1", Channel: "C1", TimeStamp: "100.2", ThreadTimeStamp: "100.1", Text: "<@UBOT> 停止",
	})

	if _, ok := store.GetFollowUp("C1:100.1"); ok {
		t.Fatal("follow-up survived the stop command")
	}
	if len(api.postTexts) != 1 || api.postTexts[0] != followUpStoppedMessage {
		t.Fatalf("posts = %q, want the follow-up cancellation", api.postTexts)
	}
}

func TestRunFollowUpsStartsDueOnes(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := followUpRunner("確認しました。")
	bot := newFollowUpBot(t, store, api, runner)
	store.threadIDs = map[string]string{"v5:C1:100.1": "codex-thread"}
	store.followUps = map[string]state.FollowUp{
		"C1:100.1": {Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Task: "確認する", DueAt: followUpNow},
		"C1:200.1": {Channel: "C1", ThreadTS: "200.1", AuthorID: "U1", Task: "後で", DueAt: followUpNow.Add(time.Hour)},
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
	bot.runFollowUps(acceptCtx, context.Background(), &wg)
	wg.Wait()

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want only the due follow-up", runner.calls)
	}
	if _, ok := store.GetFollowUp("C1:200.1"); !ok {
		t.Fatal("follow-up that is not due yet was removed")
	}
}
