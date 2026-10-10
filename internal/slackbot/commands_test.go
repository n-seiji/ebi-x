package slackbot

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAsideMessagesAreIgnored(t *testing.T) {
	now := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
	for _, text := range []string{"(aside) あとで見ます", "（独り言）これは違うかも", "!aside 停止"} {
		t.Run(text, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			runner := &fakeRunner{}
			bot := newTestBot(t, store, api, runner)
			configureActiveSubscription(bot, store, now)

			bot.HandleMessage(context.Background(), messageReply("U2", "200.2", text))
			bot.HandleMessage(context.Background(), directMessage("U1", "500.1", "", text))
			event := mention()
			event.Text = "<@UBOT> " + text
			bot.HandleMention(context.Background(), event)

			if runner.calls != 0 || store.claimCalls != 0 || len(api.calls) != 0 {
				t.Fatalf("runner calls = %d, claims = %d, Slack calls = %v; want the aside ignored", runner.calls, store.claimCalls, api.calls)
			}
		})
	}
}

func TestMuteStopsPlainRepliesUntilMention(t *testing.T) {
	now := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := successfulTurnRunner()
	bot := newTestBot(t, store, api, runner)
	configureActiveSubscription(bot, store, now)

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "ミュート"))

	if _, ok := store.GetSubscription("C1:100.1"); ok {
		t.Fatal("subscription remains after mute")
	}
	if len(api.postTexts) != 1 || api.postTexts[0] != mutedMessage {
		t.Fatalf("posts = %q, want %q", api.postTexts, mutedMessage)
	}

	bot.HandleMessage(context.Background(), messageReply("U2", "300.3", "続けて"))
	if runner.calls != 0 {
		t.Fatalf("runner calls = %d, want plain replies ignored while muted", runner.calls)
	}
}

func TestUnmuteMentionSubscribesMarkedThread(t *testing.T) {
	tests := []struct {
		name   string
		marked bool
		want   string
	}{
		{name: "marked", marked: true, want: unmutedMessage},
		{name: "unmarked", marked: false, want: unmuteFailedMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
			store := &fakeStore{claim: true}
			api := &fakeSlack{hasReaction: test.marked}
			runner := &fakeRunner{}
			bot := newTestBot(t, store, api, runner)
			bot.config.ThreadSubscriptionReaction = "thread-subete"
			bot.config.ThreadSubscriptionTTL = time.Hour
			bot.now = func() time.Time { return now }
			event := mention()
			event.ThreadTimeStamp = "100.1"
			event.TimeStamp = "200.2"
			event.Text = "<@UBOT> ミュート解除"

			bot.HandleMention(context.Background(), event)

			if runner.calls != 0 {
				t.Fatalf("runner calls = %d, want no turn for unmute", runner.calls)
			}
			if len(api.postTexts) != 1 || api.postTexts[0] != test.want {
				t.Fatalf("posts = %q, want %q", api.postTexts, test.want)
			}
		})
	}
}

func TestMuteInDirectMessageExplainsAside(t *testing.T) {
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)

	bot.HandleMessage(context.Background(), directMessage("U1", "500.1", "", "mute"))

	if runner.calls != 0 || len(api.postTexts) != 1 || !strings.Contains(api.postTexts[0], "(aside)") {
		t.Fatalf("posts = %q, runner calls = %d; want the DM note only", api.postTexts, runner.calls)
	}
}

func TestCommandsRequireTheWholeMessage(t *testing.T) {
	for _, text := range []string{"ミュートの仕方を教えて", "mute the alerts"} {
		if isMuteCommand(text) {
			t.Errorf("isMuteCommand(%q) = true", text)
		}
	}
	if !isMuteCommand(" Mute! ") || !isUnmuteCommand("ミュート解除。") {
		t.Error("commands are not normalized like the stop command")
	}
	if isAside("これは (aside) ではない") {
		t.Error("aside must start the message")
	}
}
