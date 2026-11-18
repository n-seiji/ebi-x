package slackbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
)

func bareThreadMention() *slackevents.AppMentionEvent {
	event := mention()
	event.Text = "<@UBOT> "
	event.ThreadTimeStamp = "100.1"
	event.TimeStamp = "100.5"
	return event
}

func TestBareMentionInThreadAnswersTheThread(t *testing.T) {
	runner := successfulTurnRunner()
	api := &fakeSlack{threadMessages: []ThreadMessage{
		{Timestamp: "100.1", AuthorID: "U1", Text: "Amazon で返品したらポイントは戻る？"},
		{Timestamp: "100.5", AuthorID: "U1", Text: "<@UBOT> "},
	}}
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)

	bot.HandleMention(context.Background(), bareThreadMention())

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	for _, want := range []string{bareThreadMentionRequest, "Amazon で返品したらポイントは戻る？"} {
		if !strings.Contains(runner.prompts[0], want) {
			t.Errorf("prompt does not contain %q:\n%s", want, runner.prompts[0])
		}
	}
}

func TestBareMentionOutsideThreadStillAsksNothing(t *testing.T) {
	runner := &fakeRunner{}
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)
	event := mention()
	event.Text = "<@UBOT>"

	bot.HandleMention(context.Background(), event)

	if runner.calls != 0 || len(api.posts) != 0 {
		t.Fatalf("runner calls = %d, posts = %+v, want nothing", runner.calls, api.posts)
	}
}

func TestBareThreadMentionStartsOnceApproved(t *testing.T) {
	runner := successfulTurnRunner()
	bot, api, _, _ := newApprovalBot(t, runner)
	bot.now = func() time.Time { return time.Unix(100, 0) }
	event := bareThreadMention()
	event.User = "U2"

	bot.HandleMention(context.Background(), event)
	approvalCommand(bot, api, "UOWNER", "top-1", "許可")

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if !strings.Contains(runner.prompts[0], bareThreadMentionRequest) {
		t.Errorf("prompt does not carry the thread request:\n%s", runner.prompts[0])
	}
	for _, post := range api.posts {
		if post.channel == "C1" && strings.Contains(post.text, "もう一度 mention") {
			t.Fatalf("post = %q, want the request to start without another mention", post.text)
		}
	}
}
