package slackbot

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func messageShortcut(user, channel, timestamp, threadTS string) *slack.InteractionCallback {
	callback := &slack.InteractionCallback{Type: slack.InteractionTypeMessageAction, CallbackID: requestShortcutID, TriggerID: "T1"}
	callback.User.ID = user
	callback.Channel.ID = channel
	callback.Message.Timestamp = timestamp
	callback.Message.ThreadTimestamp = threadTS
	callback.Message.Text = "ビルドが落ちています"
	return callback
}

func requestSubmission(user string, target requestTarget, text string) *slack.InteractionCallback {
	metadata, _ := json.Marshal(target)
	callback := &slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission}
	callback.User.ID = user
	callback.View.CallbackID = requestModalID
	callback.View.PrivateMetadata = string(metadata)
	callback.View.State = &slack.ViewState{Values: map[string]map[string]slack.BlockAction{
		requestBlockID: {requestActionID: {Value: text}},
	}}
	return callback
}

func TestRequestShortcutOpensModal(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, successfulTurnRunner())

	bot.HandleInteraction(context.Background(), messageShortcut("U1", "C1", "300.2", "300.1"))

	if len(api.calls) != 1 || api.calls[0].kind != "modal:T1" {
		t.Fatalf("calls = %#v, want one modal", api.calls)
	}
	metadata, quoted, _ := strings.Cut(api.calls[0].text, "\n")
	var target requestTarget
	if err := json.Unmarshal([]byte(metadata), &target); err != nil {
		t.Fatal(err)
	}
	if want := (requestTarget{Channel: "C1", MessageTS: "300.2", ThreadTS: "300.1"}); target != want {
		t.Fatalf("target = %#v, want %#v", target, want)
	}
	if quoted != "ビルドが落ちています" {
		t.Fatalf("quoted = %q", quoted)
	}
}

func TestRequestShortcutRefusesWithoutPermission(t *testing.T) {
	for name, callback := range map[string]*slack.InteractionCallback{
		"unallowed user":    messageShortcut("U9", "C1", "300.2", ""),
		"unallowed channel": messageShortcut("U1", "C9", "300.2", ""),
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeSlack{}
			bot := newTestBot(t, &fakeStore{claim: true}, api, successfulTurnRunner())

			bot.HandleInteraction(context.Background(), callback)

			if len(api.calls) != 1 || !strings.HasPrefix(api.calls[0].kind, "ephemeral:") || api.calls[0].text != shortcutForbidden {
				t.Fatalf("calls = %#v, want only a refusal the user alone sees", api.calls)
			}
		})
	}
}

func TestRequestSubmissionStartsWorkInMessageThread(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{threadMessages: []ThreadMessage{{AuthorID: "U2", Timestamp: "300.2", Text: "ビルドが落ちています"}}}
	runner := successfulTurnRunner()
	bot := newTestBot(t, store, api, runner)

	bot.HandleInteraction(context.Background(), requestSubmission("U1", requestTarget{Channel: "C1", MessageTS: "300.2"}, "原因を調べて\n直して"))

	if len(api.posts) == 0 || api.posts[0].threadTS != "300.2" || !strings.Contains(api.posts[0].text, "<@U1> からの依頼") ||
		!strings.Contains(api.posts[0].text, "> 原因を調べて\n> 直して") {
		t.Fatalf("posts = %#v, want the request quoted in the message's thread", api.posts)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if got := store.threadKeys; !reflect.DeepEqual(got, []string{"v5:C1:300.2"}) {
		t.Fatalf("thread keys = %v, want the message's thread", got)
	}
	if !strings.Contains(runner.prompts[0], "ビルドが落ちています") || !strings.Contains(runner.prompts[0], "<message_text>\n原因を調べて\n直して\n</message_text>") {
		t.Fatalf("prompt = %q, want the thread and the request", runner.prompts[0])
	}
}

func TestRequestSubmissionRechecksPermission(t *testing.T) {
	api := &fakeSlack{}
	runner := successfulTurnRunner()
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)

	bot.HandleInteraction(context.Background(), requestSubmission("U9", requestTarget{Channel: "C1", MessageTS: "300.2"}, "調べて"))
	bot.HandleInteraction(context.Background(), requestSubmission("U1", requestTarget{Channel: "C1", MessageTS: "300.2"}, "  "))

	if runner.calls != 0 || len(api.calls) != 0 {
		t.Fatalf("runner calls = %d, slack calls = %#v, want nothing", runner.calls, api.calls)
	}
}
