package slackbot

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

func directMessage(user, timestamp, threadTS, text string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{
		Type:            "message",
		User:            user,
		Text:            text,
		TimeStamp:       timestamp,
		ThreadTimeStamp: threadTS,
		Channel:         "D1",
		ChannelType:     "im",
	}
}

func TestDirectMessageStartsThreadWithoutChannelAllowlist(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := successfulTurnRunner()
	bot := newTestBot(t, store, api, runner)

	bot.HandleMessage(context.Background(), directMessage("U1", "500.1", "", "調べて"))

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if got := store.threadKeys; !reflect.DeepEqual(got, []string{"v5:D1:500.1"}) {
		t.Fatalf("thread keys = %v, want the message to start its own thread", got)
	}
	if api.threadCalls != 0 {
		t.Fatalf("thread reads = %d, want 0 for a top-level message", api.threadCalls)
	}
	if len(api.posts) == 0 || api.posts[len(api.posts)-1].threadTS != "500.1" {
		t.Fatalf("posts = %#v, want the result in the message's thread", api.posts)
	}
	if !strings.Contains(runner.prompts[0], "<message_text>\n調べて\n</message_text>") {
		t.Fatalf("prompt = %q, want the message text", runner.prompts[0])
	}
}

func TestDirectMessageReplyContinuesThread(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{threadMessages: []ThreadMessage{{AuthorID: "U1", Timestamp: "500.1", Text: "最初"}}}
	runner := successfulTurnRunner()
	bot := newTestBot(t, store, api, runner)

	bot.HandleMessage(context.Background(), directMessage("U1", "600.1", "500.1", "<@UBOT> 続けて"))

	if got := store.threadKeys; !reflect.DeepEqual(got, []string{"v5:D1:500.1"}) {
		t.Fatalf("thread keys = %v, want the existing thread", got)
	}
	if api.threadCalls != 1 {
		t.Fatalf("thread reads = %d, want the thread history for a new session", api.threadCalls)
	}
	if !strings.Contains(runner.prompts[0], "<message_text>\n続けて\n</message_text>") {
		t.Fatalf("prompt = %q, want the mention stripped", runner.prompts[0])
	}
}

func TestDirectMessageFromUnallowedUserIsForbidden(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newTestBot(t, store, api, runner)

	bot.HandleMessage(context.Background(), directMessage("U9", "500.1", "", "調べて"))

	if runner.calls != 0 {
		t.Fatalf("runner calls = %d, want 0", runner.calls)
	}
	if got := strings.Join(api.postTexts, "|"); got != "403 forbidden. @seiji に確認してください。" {
		t.Fatalf("posts = %q, want forbidden response", got)
	}
}

func TestDirectMessageFromUnallowedUserRequestsApproval(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})

	bot.HandleMessage(context.Background(), directMessage("U9", "500.1", "", "調べて"))

	if status := approvals.ApprovalStatus(state.ApprovalUser, "U9"); status != state.Pending {
		t.Fatalf("approval status = %q, want pending", status)
	}
	requests := approvalRequests(api)
	if len(requests) != 1 || !strings.Contains(requests[0].text, "DM で依頼しました") {
		t.Fatalf("approval requests = %#v, want one naming the DM", requests)
	}
}

func TestDirectMessageFiltersSkipProcessing(t *testing.T) {
	tests := map[string]func(*slackevents.MessageEvent){
		"bot user":     func(e *slackevents.MessageEvent) { e.User = "UBOT" },
		"bot message":  func(e *slackevents.MessageEvent) { e.BotID = "B1" },
		"subtype":      func(e *slackevents.MessageEvent) { e.SubType = "message_changed" },
		"blank":        func(e *slackevents.MessageEvent) { e.Text = "<@UBOT>  " },
		"not a DM":     func(e *slackevents.MessageEvent) { e.Channel = "C1" },
		"deleted":      func(e *slackevents.MessageEvent) { e.DeletedTimeStamp = "1.0" },
		"missing user": func(e *slackevents.MessageEvent) { e.User = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			runner := &fakeRunner{}
			bot := newTestBot(t, store, api, runner)
			event := directMessage("U1", "500.1", "", "調べて")
			mutate(event)

			bot.HandleMessage(context.Background(), event)

			if runner.calls != 0 || store.claimCalls != 0 || len(api.calls) != 0 {
				t.Fatalf("runner calls = %d, claims = %d, Slack calls = %v; want none", runner.calls, store.claimCalls, api.calls)
			}
		})
	}
}

func TestDirectMessageStopCommand(t *testing.T) {
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)

	bot.HandleMessage(context.Background(), directMessage("U1", "600.1", "500.1", "停止"))

	if runner.calls != 0 {
		t.Fatalf("runner calls = %d, want 0", runner.calls)
	}
	if got := strings.Join(api.postTexts, "|"); got != nothingToStopMessage {
		t.Fatalf("posts = %q, want %q", got, nothingToStopMessage)
	}
}

func TestAppHomeOpenedSetsSuggestedPrompts(t *testing.T) {
	tests := []struct {
		name  string
		event *slackevents.AppHomeOpenedEvent
		want  []slackCall
	}{
		{
			name:  "messages tab",
			event: &slackevents.AppHomeOpenedEvent{User: "U1", Channel: "D1", Tab: "messages"},
			want:  []slackCall{{kind: "prompts:D1", text: "3"}},
		},
		{name: "unallowed user", event: &slackevents.AppHomeOpenedEvent{User: "U9", Channel: "D1", Tab: "messages"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeSlack{}
			bot := newTestBot(t, &fakeStore{claim: true}, api, &fakeRunner{})

			bot.HandleAppHomeOpened(context.Background(), test.event)

			if !reflect.DeepEqual(api.calls, test.want) {
				t.Fatalf("Slack calls = %v, want %v", api.calls, test.want)
			}
		})
	}
}

func TestFollowUpRunsInDirectMessage(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := followUpRunner("確認しました。")
	bot := newFollowUpBot(t, store, api, runner)

	bot.fireFollowUp(context.Background(), state.FollowUp{
		Channel: "D1", ThreadTS: "500.1", AuthorID: "U1", Task: "確認する", DueAt: followUpNow,
	})

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want the follow-up to run in the DM", runner.calls)
	}
}
