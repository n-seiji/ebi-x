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

func newApprovalBot(t *testing.T, runner *fakeRunner) (*Bot, *fakeSlack, *fakeStore, *state.Store) {
	t.Helper()
	approvals, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newTestBot(t, store, api, runner)
	bot.config.ApprovalChannelID = "COWNER"
	bot.config.Approvals = approvals
	bot.approvers = makeSet([]string{"UOWNER", "UOWNER2"})
	return bot, api, store, approvals
}

// approvalCommand mentions the bot in the approval channel, in threadTS when
// it is set, and returns the bot's replies there.
func approvalCommand(bot *Bot, api *fakeSlack, user, threadTS, text string) string {
	api.mu.Lock()
	start := len(api.posts)
	api.mu.Unlock()
	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{
		User:            user,
		Channel:         "COWNER",
		TimeStamp:       "900.1",
		ThreadTimeStamp: threadTS,
		Text:            "<@UBOT> " + text,
	})
	api.mu.Lock()
	defer api.mu.Unlock()
	want := threadTS
	if want == "" {
		want = "900.1"
	}
	var replies []string
	for _, post := range api.posts[start:] {
		if post.channel == "COWNER" && post.threadTS == want {
			replies = append(replies, post.text)
		}
	}
	return strings.Join(replies, "|")
}

func approvalRequests(api *fakeSlack) []fakePost {
	var requests []fakePost
	for _, post := range api.posts {
		if post.channel == "COWNER" && post.threadTS == "" {
			requests = append(requests, post)
		}
	}
	return requests
}

func TestUnknownUserMentionRequestsApprovalOnce(t *testing.T) {
	bot, api, store, approvals := newApprovalBot(t, &fakeRunner{})
	event := mention()
	event.User = "U2"

	bot.HandleMention(context.Background(), event)
	bot.HandleMention(context.Background(), event)

	if store.claimCalls != 0 {
		t.Fatalf("ClaimEvent called %d times, want 0", store.claimCalls)
	}
	requests := approvalRequests(api)
	if len(requests) != 1 || !strings.Contains(requests[0].text, "ユーザー <@U2>") || !strings.Contains(requests[0].text, "<#C1>") {
		t.Fatalf("approval requests = %+v, want one for U2 from C1", requests)
	}
	approval, _ := approvals.GetApproval(state.ApprovalUser, "U2")
	if approval.Status != state.Pending || approval.RequestMessageTS != "top-1" || approval.RequestThreadTS != "100.1" {
		t.Fatalf("approval = %+v, want pending with request message and origin thread", approval)
	}
	var origin []string
	for _, post := range api.posts {
		if post.channel == "C1" {
			origin = append(origin, post.text)
		}
	}
	if got := strings.Join(origin, "|"); got != approvalQueuedMessage+"|"+approvalQueuedMessage {
		t.Fatalf("origin replies = %q, want approval requested twice", got)
	}
}

func TestApprovalStartsTheWaitingMention(t *testing.T) {
	runner := successfulTurnRunner()
	bot, api, _, _ := newApprovalBot(t, runner)
	bot.now = func() time.Time { return time.Unix(100, 0).Add(time.Hour) }
	event := mention()
	event.User = "U2"
	event.ThreadTimeStamp = "90.1"
	bot.HandleMention(context.Background(), event)

	approvalCommand(bot, api, "UOWNER", "top-1", "許可")

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want the waiting mention started", runner.calls)
	}
	if len(runner.prompts) != 1 || !strings.Contains(runner.prompts[0], "do it") {
		t.Fatalf("runner prompts = %q, want the waiting request", runner.prompts)
	}
	var notice string
	for _, post := range api.posts {
		if post.channel == "C1" && post.threadTS == "90.1" && strings.Contains(post.text, "許可されました") {
			notice = post.text
		}
	}
	if !strings.Contains(notice, "依頼を始めます") {
		t.Fatalf("notice = %q, want the request to start", notice)
	}
}

func TestApprovalWaitsForEveryMissingSubject(t *testing.T) {
	runner := successfulTurnRunner()
	bot, api, _, _ := newApprovalBot(t, runner)
	bot.now = func() time.Time { return time.Unix(100, 0).Add(time.Hour) }
	event := mention()
	event.User = "U2"
	event.Channel = "C2"
	bot.HandleMention(context.Background(), event)

	approvalCommand(bot, api, "UOWNER", "top-1", "許可")
	if runner.calls != 0 {
		t.Fatalf("runner calls = %d after one approval, want 0", runner.calls)
	}
	approvalCommand(bot, api, "UOWNER", "top-2", "許可")
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d after both approvals, want 1", runner.calls)
	}
	var notices []string
	for _, post := range api.posts {
		if post.channel == "C2" && strings.Contains(post.text, "許可されました") {
			notices = append(notices, post.text)
		}
	}
	if len(notices) != 2 || !strings.Contains(notices[0], "残りの許可を待っています") || !strings.Contains(notices[1], "依頼を始めます") {
		t.Fatalf("notices = %q, want wait then start", notices)
	}
}

func TestApprovalDoesNotStartOldOrEmptyMentions(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		now       time.Time
		wantReply string
	}{
		{name: "older than a day", text: "<@UBOT> do it", now: time.Unix(100, 0).Add(25 * time.Hour), wantReply: approvalQueuedMessage},
		{name: "empty mention", text: "<@UBOT>", now: time.Unix(100, 0), wantReply: approvalRequestedMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := successfulTurnRunner()
			bot, api, _, _ := newApprovalBot(t, runner)
			bot.now = func() time.Time { return time.Unix(100, 0) }
			event := mention()
			event.User = "U2"
			event.Text = test.text
			bot.HandleMention(context.Background(), event)
			if got := api.posts[len(api.posts)-1].text; got != test.wantReply {
				t.Fatalf("reply = %q, want %q", got, test.wantReply)
			}
			bot.now = func() time.Time { return test.now }

			approvalCommand(bot, api, "UOWNER", "top-1", "許可")

			if runner.calls != 0 {
				t.Fatalf("runner calls = %d, want 0", runner.calls)
			}
			for _, post := range api.posts {
				if post.channel == "C1" && strings.Contains(post.text, "許可されました") && !strings.Contains(post.text, "もう一度 mention") {
					t.Fatalf("notice = %q, want a request to mention again", post.text)
				}
			}
		})
	}
}

func TestUnknownUserAndChannelRequestBothApprovals(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
	event := mention()
	event.User = "U2"
	event.Channel = "C2"

	bot.HandleMention(context.Background(), event)

	if got := len(approvalRequests(api)); got != 2 {
		t.Fatalf("approval requests = %d, want 2", got)
	}
	if approvals.ApprovalStatus(state.ApprovalUser, "U2") != state.Pending ||
		approvals.ApprovalStatus(state.ApprovalChannel, "C2") != state.Pending {
		t.Fatal("want user and channel pending")
	}
}

func TestApprovalIsNotRequestedForDeniedOrUnapprovableSubjects(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Bot, *state.Store)
		channel   string
	}{
		{name: "denied user", channel: "C1", configure: func(_ *Bot, approvals *state.Store) {
			decide(t, approvals, state.ApprovalUser, "U2", false)
		}},
		{name: "DM", channel: "D1"},
		{name: "approvals disabled", channel: "C1", configure: func(bot *Bot, _ *state.Store) {
			bot.config.ApprovalChannelID = ""
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bot, api, store, approvals := newApprovalBot(t, &fakeRunner{})
			if test.configure != nil {
				test.configure(bot, approvals)
			}
			event := mention()
			event.User = "U2"
			event.Channel = test.channel

			bot.HandleMention(context.Background(), event)

			if store.claimCalls != 0 || len(approvalRequests(api)) != 0 {
				t.Fatalf("claims = %d, approval requests = %d, want 0", store.claimCalls, len(approvalRequests(api)))
			}
			if got := strings.Join(api.postTexts, "|"); got != "403 forbidden. @seiji に確認してください。" {
				t.Fatalf("posts = %q, want forbidden response", got)
			}
		})
	}
}

func TestFailedApprovalPostIsRolledBack(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
	api.postErrs = []error{fmt.Errorf("not_in_channel")}
	event := mention()
	event.User = "U2"

	bot.HandleMention(context.Background(), event)

	if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != "" {
		t.Fatalf("status = %q, want no record", got)
	}
	if got := api.posts[len(api.posts)-1].text; got != "403 forbidden. @seiji に確認してください。" {
		t.Fatalf("last post = %q, want forbidden response", got)
	}
}

func TestApproveInRequestThreadAllowsUser(t *testing.T) {
	runner := successfulTurnRunner()
	bot, api, _, approvals := newApprovalBot(t, runner)
	event := mention()
	event.User = "U2"
	bot.HandleMention(context.Background(), event)

	reply := approvalCommand(bot, api, "UOWNER", "top-1", "許可")

	if !strings.Contains(reply, "✅ ユーザー <@U2>: <@UOWNER> が許可しました") {
		t.Fatalf("reply = %q, want approval by UOWNER", reply)
	}
	approval, _ := approvals.GetApproval(state.ApprovalUser, "U2")
	if approval.Status != state.Approved || approval.DecidedBy != "UOWNER" || approval.DecidedAt.IsZero() {
		t.Fatalf("approval = %+v, want approved by UOWNER", approval)
	}
	notified := false
	for _, post := range api.posts {
		if post.channel == "C1" && post.threadTS == "100.1" && strings.Contains(post.text, "許可されました") {
			notified = true
		}
	}
	if !notified {
		t.Fatalf("posts = %+v, want approval notice in the origin thread", api.posts)
	}

	bot.HandleMention(context.Background(), event)
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestDenyInRequestThreadDoesNotNotifyOrigin(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
	event := mention()
	event.User = "U2"
	bot.HandleMention(context.Background(), event)

	reply := approvalCommand(bot, api, "UOWNER", "top-1", "拒否")

	if !strings.Contains(reply, "🚫 ユーザー <@U2>: <@UOWNER> が拒否しました") {
		t.Fatalf("reply = %q, want denial", reply)
	}
	if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != state.Denied {
		t.Fatalf("status = %q, want denied", got)
	}
	for _, post := range api.posts {
		if post.channel == "C1" && strings.Contains(post.text, "許可されました") {
			t.Fatalf("denial notified origin thread: %+v", post)
		}
	}
}
func TestFirstDecisionWins(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
	event := mention()
	event.User = "U2"
	bot.HandleMention(context.Background(), event)

	approvalCommand(bot, api, "UOWNER", "top-1", "許可")
	reply := approvalCommand(bot, api, "UOWNER2", "top-1", "拒否")

	if !strings.Contains(reply, "すでに<@UOWNER> が許可しました") {
		t.Fatalf("reply = %q, want already decided", reply)
	}
	if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != state.Approved {
		t.Fatalf("status = %q, want approved", got)
	}
}

func TestApprovalChannelAcceptsOnlyDecisionsInRequestThreads(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		threadTS string
		text     string
		want     string
	}{
		{name: "non-approver", user: "U1", threadTS: "top-1", text: "許可", want: notApproverMessage},
		{name: "other word", user: "UOWNER", threadTS: "top-1", text: "OK", want: approvalUsageMessage},
		{name: "extra words", user: "UOWNER", threadTS: "top-1", text: "許可 お願いします", want: approvalUsageMessage},
		{name: "not a request thread", user: "UOWNER", threadTS: "555.5", text: "許可", want: approvalUsageMessage},
		{name: "top level", user: "UOWNER", text: "許可", want: approvalUsageMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{}
			bot, api, store, approvals := newApprovalBot(t, runner)
			bot.allowedChannels = makeSet([]string{"C1", "COWNER"})
			event := mention()
			event.User = "U2"
			bot.HandleMention(context.Background(), event)

			reply := approvalCommand(bot, api, test.user, test.threadTS, test.text)

			if reply != test.want {
				t.Fatalf("reply = %q, want %q", reply, test.want)
			}
			if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != state.Pending {
				t.Fatalf("status = %q, want pending", got)
			}
			if store.claimCalls != 0 || runner.calls != 0 || len(approvalRequests(api)) != 1 {
				t.Fatalf("claims = %d, runner calls = %d, requests = %d; want only the original request", store.claimCalls, runner.calls, len(approvalRequests(api)))
			}
		})
	}
}

func TestWorkflowMentionInApprovalChannelIsIgnored(t *testing.T) {
	bot, api, store, _ := newApprovalBot(t, &fakeRunner{})
	bot.config.AllowWorkflows = true
	event := mention()
	event.Channel = "COWNER"
	event.User = "UWORKFLOW"
	event.BotID = "BWORKFLOW"

	bot.handleMention(context.Background(), event, "Wf0NEW")

	if store.claimCalls != 0 || len(api.posts) != 0 {
		t.Fatalf("claims = %d, posts = %+v; want nothing", store.claimCalls, api.posts)
	}
}
func TestApprovedUserAndChannelAreHandled(t *testing.T) {
	runner := successfulTurnRunner()
	bot, api, _, _ := newApprovalBot(t, runner)
	event := mention()
	event.User = "U2"
	event.Channel = "C2"
	bot.HandleMention(context.Background(), event)
	approvalCommand(bot, api, "UOWNER", "top-1", "許可")
	approvalCommand(bot, api, "UOWNER", "top-2", "許可")

	bot.HandleMention(context.Background(), event)

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}
func TestWorkflowApproval(t *testing.T) {
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"## 方針\nDone.\n## 作業指示\nNONE"},
	}}}}
	bot, api, _, approvals := newApprovalBot(t, runner)
	bot.config.AllowWorkflows = true
	event := mention()
	event.User = "UWORKFLOW"
	event.BotID = "BWORKFLOW"

	bot.handleMention(context.Background(), event, "Wf0NEW")
	if approvals.ApprovalStatus(state.ApprovalWorkflow, "Wf0NEW") != state.Pending {
		t.Fatal("want workflow pending")
	}
	approvalCommand(bot, api, "UOWNER", "top-1", "許可")
	bot.handleMention(context.Background(), event, "Wf0NEW")

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestDisabledWorkflowsAreNotOfferedForApproval(t *testing.T) {
	bot, api, _, _ := newApprovalBot(t, &fakeRunner{})
	event := mention()
	event.User = "UWORKFLOW"
	event.BotID = "BWORKFLOW"

	bot.handleMention(context.Background(), event, "Wf0NEW")

	if got := len(approvalRequests(api)); got != 0 {
		t.Fatalf("approval requests = %d, want 0", got)
	}
}

func TestConcurrentApprovalsAreAllKept(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
	const count = 32
	for i := range count {
		id := fmt.Sprintf("UX%d", i)
		if _, err := approvals.RequestApproval(state.ApprovalUser, id, id, "C1", "100.1", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := approvals.SetApprovalRequestMessage(state.ApprovalUser, id, fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			approvalCommand(bot, api, "UOWNER", fmt.Sprintf("req-%d", i), "許可")
		})
	}
	wg.Wait()

	for i := range count {
		if got := approvals.ApprovalStatus(state.ApprovalUser, fmt.Sprintf("UX%d", i)); got != state.Approved {
			t.Fatalf("UX%d status = %q, want approved", i, got)
		}
	}
}
func TestApprovedUserMayReplyInSubscribedThread(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	runner := successfulTurnRunner()
	bot, _, store, approvals := newApprovalBot(t, runner)
	configureActiveSubscription(bot, store, now)
	bot.allowedUsers = makeSet([]string{"U1"})
	decide(t, approvals, state.ApprovalUser, "U9", true)

	bot.HandleMessage(context.Background(), messageReply("U9", "200.2", "please continue"))

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func decide(t *testing.T, approvals *state.Store, kind state.ApprovalKind, id string, approve bool) {
	t.Helper()
	if _, err := approvals.RequestApproval(kind, id, "U1", "C1", "100.1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, decided, err := approvals.DecideApproval(kind, id, approve, "UOWNER", time.Now()); err != nil || !decided {
		t.Fatalf("DecideApproval(%s, %s) = %v, %v; want decided", kind, id, decided, err)
	}
}

func TestApproverJoinsAndApprovesPublicChannel(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})

	reply := approvalCommand(bot, api, "UOWNER", "", "参加 <#C9|times-shion>")

	if !strings.Contains(reply, "✅ チャンネル <#C9> に参加し、<@UOWNER> が許可しました") {
		t.Fatalf("reply = %q, want joined and approved", reply)
	}
	if len(api.joined) != 1 || api.joined[0] != "C9" {
		t.Fatalf("joined = %v, want C9", api.joined)
	}
	if got := approvals.ApprovalStatus(state.ApprovalChannel, "C9"); got != state.Approved {
		t.Fatalf("status = %q, want approved", got)
	}
	announced := false
	for _, post := range api.posts {
		if post.channel == "C9" && strings.Contains(post.text, "ebi-x が参加しました") {
			announced = true
		}
	}
	if !announced {
		t.Fatalf("posts = %+v, want a note in the joined channel", api.posts)
	}
}

func TestJoinStartsTheMentionWaitingForTheChannel(t *testing.T) {
	runner := successfulTurnRunner()
	bot, api, _, _ := newApprovalBot(t, runner)
	bot.now = func() time.Time { return time.Unix(100, 0).Add(time.Hour) }
	event := mention()
	event.Channel = "C2"
	bot.HandleMention(context.Background(), event)

	approvalCommand(bot, api, "UOWNER", "", "参加 <#C2>")

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want the waiting mention started", runner.calls)
	}
}

func TestJoinIsRefusedWhenNotPossible(t *testing.T) {
	tests := []struct {
		name      string
		user      string
		configure func(*fakeSlack, *state.Store)
		want      string
	}{
		{name: "non-approver", user: "U1", want: notApproverMessage},
		{name: "private channel", user: "UOWNER", configure: func(api *fakeSlack, _ *state.Store) {
			api.joinErr = fmt.Errorf("method_not_supported_for_channel_type")
		}, want: "プライベートチャンネルのため"},
		{name: "denied channel", user: "UOWNER", configure: func(_ *fakeSlack, approvals *state.Store) {
			if _, err := approvals.RequestApproval(state.ApprovalChannel, "C9", "U1", "C9", "1.1", time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, _, err := approvals.DecideApproval(state.ApprovalChannel, "C9", false, "UOWNER", time.Now()); err != nil {
				t.Fatal(err)
			}
		}, want: "拒否済みのため参加しません"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
			if test.configure != nil {
				test.configure(api, approvals)
			}

			reply := approvalCommand(bot, api, test.user, "", "参加 <#C9>")

			if !strings.Contains(reply, test.want) {
				t.Fatalf("reply = %q, want %q", reply, test.want)
			}
			if got := approvals.ApprovalStatus(state.ApprovalChannel, "C9"); got == state.Approved {
				t.Fatal("channel approved, want not")
			}
			if test.name != "private channel" && len(api.joined) != 0 {
				t.Fatalf("joined = %v, want none", api.joined)
			}
		})
	}
}
