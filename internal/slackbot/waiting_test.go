package slackbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
)

func waitingRunner() *fakeRunner {
	return &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"手順を確認しました。\n\n## 回答待ち\n- 本番に適用してよいですか？"},
	}}}}
}

func TestWaitingTurnNotifiesRequester(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newTestBot(t, store, api, waitingRunner())

	bot.HandleMention(context.Background(), mention())

	want := "手順を確認しました。\n\n🙋 <@U1> 回答待ちです。このスレッドで ebi-x に mention して答えてください。\n- 本番に適用してよいですか？"
	if len(api.postTexts) != 1 || api.postTexts[0] != want {
		t.Fatalf("posts = %q, want %q", api.postTexts, want)
	}
	assertTransitions(t, store.transitions, [][2]state.State{
		{state.Received, state.Working},
		{state.Working, state.Done},
	})
	assertFinalReactionOrder(t, api.calls, waitingReaction)
	for _, call := range api.calls {
		if call.kind == "add:"+finishedReaction {
			t.Fatalf("reaction calls = %v, want no ✅ while waiting", api.calls)
		}
	}
}

func TestWaitingNoticeInSubscribedThreadAsksForAPlainReply(t *testing.T) {
	now := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newTestBot(t, store, api, waitingRunner())
	configureActiveSubscription(bot, store, now)

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "適用して"))

	if len(api.postTexts) != 1 || !strings.Contains(api.postTexts[0], "🙋 <@U2> 回答待ちです。このスレッドに返信してください。") {
		t.Fatalf("posts = %q, want a plain-reply hint for the subscribed thread", api.postTexts)
	}
}

func TestWaitingNoticeInDirectMessageAsksForAPlainReply(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, waitingRunner())

	bot.HandleMessage(context.Background(), directMessage("U1", "500.1", "", "適用して"))

	if len(api.postTexts) != 1 || !strings.Contains(api.postTexts[0], "回答待ちです。このスレッドに返信してください。") {
		t.Fatalf("posts = %q, want a plain-reply hint in the DM", api.postTexts)
	}
}
