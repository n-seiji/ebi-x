package slackbot

import (
	"context"
	"errors"
	"testing"

	"github.com/slack-go/slack/slackevents"
)

func TestIsAliveQuestion(t *testing.T) {
	for _, text := range []string{"生きてる？", "生きてる?", "いきてる", "生きてますか？", "起きてる〜？", "Alive?", "ping"} {
		if !isAliveQuestion(text) {
			t.Errorf("isAliveQuestion(%q) = false", text)
		}
	}
	for _, text := range []string{"", "生きてるか確認するスクリプトを書いて", "pingを打つ処理を直して", "do it"} {
		if isAliveQuestion(text) {
			t.Errorf("isAliveQuestion(%q) = true", text)
		}
	}
}

func aliveMention(user, channel string) *slackevents.AppMentionEvent {
	return &slackevents.AppMentionEvent{User: user, Channel: channel, TimeStamp: "100.1", Text: "<@UBOT> 生きてる？"}
}

func TestAliveQuestionRepliesWithoutTurn(t *testing.T) {
	for name, tc := range map[string]struct {
		usage UsageReader
		want  string
	}{
		"codex up":   {&fakeUsage{limits: usageLimits(10, 10)}, aliveReply},
		"codex down": {&fakeUsage{err: errors.New("not logged in")}, aliveCodexDownReply},
		"no usage":   {nil, aliveNoCodexReply},
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeSlack{}
			runner := &fakeRunner{}
			bot := newTestBot(t, &fakeStore{claim: true}, api, runner)
			bot.config.Usage = tc.usage

			bot.HandleMention(context.Background(), aliveMention("U1", "C1"))

			if runner.calls != 0 {
				t.Fatalf("runner calls = %d", runner.calls)
			}
			if len(api.posts) != 1 || api.posts[0].text != tc.want || api.posts[0].threadTS != "100.1" {
				t.Fatalf("posts = %+v", api.posts)
			}
		})
	}
}

func TestAliveQuestionAnswersAnyChannelAndUser(t *testing.T) {
	bot, api, _, _ := newApprovalBot(t, &fakeRunner{})
	bot.config.Usage = &fakeUsage{limits: usageLimits(10, 10)}

	// Neither the user nor the channel is approved, and no approval is asked.
	bot.HandleMention(context.Background(), aliveMention("USTRANGER", "COTHER"))
	// The approval channel answers it too.
	bot.HandleMention(context.Background(), aliveMention("USTRANGER", "COWNER"))

	if len(api.posts) != 2 {
		t.Fatalf("posts = %+v", api.posts)
	}
	for _, post := range api.posts {
		if post.text != aliveReply {
			t.Fatalf("posts = %+v", api.posts)
		}
	}
}

func TestAliveQuestionInDirectMessage(t *testing.T) {
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)

	bot.HandleMessage(context.Background(), &slackevents.MessageEvent{
		Type: "message", ChannelType: "im", User: "U1", Channel: "D1", TimeStamp: "200.1", Text: "生きてる？",
	})

	if runner.calls != 0 || len(api.posts) != 1 || api.posts[0].text != aliveNoCodexReply || api.posts[0].threadTS != "200.1" {
		t.Fatalf("runner calls = %d, posts = %+v", runner.calls, api.posts)
	}
}
