package slackbot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
)

func approvalPosts(api *fakeSlack) []string {
	var alerts []string
	for _, post := range api.posts {
		if post.channel == "CAPPROVAL" {
			alerts = append(alerts, post.text)
		}
	}
	return alerts
}

func newFailureAlertBot(t *testing.T, api *fakeSlack, now *time.Time) *Bot {
	t.Helper()
	bot := newTestBot(t, &fakeStore{claim: true}, api, &fakeRunner{})
	bot.config.ApprovalChannelID = "CAPPROVAL"
	bot.now = func() time.Time { return *now }
	return bot
}

func TestRepeatedWorkFailuresAlertAdminOnce(t *testing.T) {
	api := &fakeSlack{}
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)
	bot := newFailureAlertBot(t, api, &now)
	err := errors.New("codex exec: argument of 140000 bytes exceeds 131072; reduce the Codex configuration\nmore detail")

	for i := 0; i < repeatedFailureCount-1; i++ {
		bot.noteWorkFailure(context.Background(), err)
		now = now.Add(10 * time.Minute)
	}
	if alerts := approvalPosts(api); len(alerts) != 0 {
		t.Fatalf("alerts before the threshold = %q", alerts)
	}
	bot.noteWorkFailure(context.Background(), err)
	alerts := approvalPosts(api)
	if len(alerts) != 1 {
		t.Fatalf("alerts = %q, want one", alerts)
	}
	for _, want := range []string{"直近60分で3件失敗", "`codex exec: argument of 140000 bytes exceeds 131072; reduce the Codex configuration`"} {
		if !strings.Contains(alerts[0], want) {
			t.Errorf("alert does not contain %q:\n%s", want, alerts[0])
		}
	}

	// Further failures within the alert interval do not repeat it.
	now = now.Add(10 * time.Minute)
	bot.noteWorkFailure(context.Background(), err)
	if alerts := approvalPosts(api); len(alerts) != 1 {
		t.Fatalf("alerts = %q, want still one", alerts)
	}
}

func TestSpreadOutWorkFailuresDoNotAlert(t *testing.T) {
	api := &fakeSlack{}
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)
	bot := newFailureAlertBot(t, api, &now)

	for i := 0; i < 5; i++ {
		bot.noteWorkFailure(context.Background(), errors.New("turn failed"))
		now = now.Add(40 * time.Minute)
	}
	if alerts := approvalPosts(api); len(alerts) != 0 {
		t.Fatalf("alerts = %q, want none for failures over an hour apart", alerts)
	}
}

func TestWorkFailuresWithoutApprovalChannelDoNotAlert(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, &fakeRunner{})
	for i := 0; i < repeatedFailureCount; i++ {
		bot.noteWorkFailure(context.Background(), errors.New("turn failed"))
	}
	if len(api.posts) != 0 {
		t.Fatalf("posts = %+v, want none", api.posts)
	}
}

func TestFailedMentionsAlertAdminAfterRepeating(t *testing.T) {
	api := &fakeSlack{}
	var responses []runnerResponse
	for i := 0; i < repeatedFailureCount; i++ {
		responses = append(responses, runnerResponse{err: errors.New("codex exec: argument too long")})
	}
	runner := &fakeRunner{responses: responses}
	bot := newTestBot(t, &fakeStore{claim: true}, api, runner)
	bot.config.ApprovalChannelID = "CAPPROVAL"
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)
	bot.now = func() time.Time { return now }

	for i := 0; i < repeatedFailureCount; i++ {
		event := mention()
		event.TimeStamp = fmt.Sprintf("10%d.1", i)
		bot.HandleMention(context.Background(), event)
	}

	alerts := approvalPosts(api)
	if len(alerts) != 1 || !strings.Contains(alerts[0], "codex exec: argument too long") {
		t.Fatalf("alerts = %q, want one naming the error", alerts)
	}
	if got := strings.Count(strings.Join(api.postTexts, "\n"), workFailureMessage); got != repeatedFailureCount {
		t.Errorf("failure replies = %d, want %d", got, repeatedFailureCount)
	}
}

func TestSignInFailuresKeepTheirOwnAlert(t *testing.T) {
	api := &fakeSlack{}
	var responses []runnerResponse
	for i := 0; i < repeatedFailureCount; i++ {
		responses = append(responses, runnerResponse{err: fmt.Errorf("401: %w", codex.ErrNotSignedIn)})
	}
	bot := newTestBot(t, &fakeStore{claim: true}, api, &fakeRunner{responses: responses})
	bot.config.ApprovalChannelID = "CAPPROVAL"
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)
	bot.now = func() time.Time { return now }

	for i := 0; i < repeatedFailureCount; i++ {
		event := mention()
		event.TimeStamp = fmt.Sprintf("10%d.1", i)
		bot.HandleMention(context.Background(), event)
	}

	alerts := approvalPosts(api)
	if len(alerts) != 1 || alerts[0] != authAdminAlert {
		t.Fatalf("alerts = %q, want only the sign-in alert", alerts)
	}
}
