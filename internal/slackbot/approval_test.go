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
	if got := strings.Join(origin, "|"); got != approvalRequestedMessage+"|"+approvalRequestedMessage {
		t.Fatalf("origin replies = %q, want approval requested twice", got)
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
		t.Fatalf("approvals = %+v, want user and channel pending", approvals.ListApprovals())
	}
}

func TestApprovalIsNotRequestedForDeniedOrUnapprovableSubjects(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Bot, *state.Store)
		channel   string
	}{
		{name: "denied user", channel: "C1", configure: func(_ *Bot, approvals *state.Store) {
			if _, _, err := approvals.DecideApproval(state.ApprovalUser, "U2", false, "UOWNER", time.Now()); err != nil {
				t.Fatal(err)
			}
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
	runner := successfulPlanRunner()
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

func TestDenyByIDIsNotedInRequestThread(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
	event := mention()
	event.User = "U2"
	bot.HandleMention(context.Background(), event)

	reply := approvalCommand(bot, api, "UOWNER", "", "拒否 <@U2>")

	if !strings.Contains(reply, "🚫 ユーザー <@U2>: <@UOWNER> が拒否しました") {
		t.Fatalf("reply = %q, want denial", reply)
	}
	if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != state.Denied {
		t.Fatalf("status = %q, want denied", got)
	}
	noted := false
	for _, post := range api.posts {
		if post.channel == "COWNER" && post.threadTS == "top-1" && strings.Contains(post.text, "拒否しました") {
			noted = true
		}
		if post.channel == "C1" && strings.Contains(post.text, "許可されました") {
			t.Fatalf("denial notified origin thread: %+v", post)
		}
	}
	if !noted {
		t.Fatalf("posts = %+v, want denial noted in the request thread", api.posts)
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

func TestApprovalCommandRejections(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		threadTS string
		text     string
		want     string
	}{
		{name: "non-approver", user: "U1", threadTS: "top-1", text: "許可", want: notApproverMessage},
		{name: "not a request thread", user: "UOWNER", threadTS: "555.5", text: "許可", want: approvalThreadMissing},
		{name: "top level without ID", user: "UOWNER", text: "許可", want: approvalThreadMissing},
		{name: "unreadable ID", user: "UOWNER", text: "許可 everyone", want: "ID として読み取れませんでした"},
		{name: "fixed allowlist", user: "UOWNER", text: "拒否 U1", want: ".env の固定の許可"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
			event := mention()
			event.User = "U2"
			bot.HandleMention(context.Background(), event)

			reply := approvalCommand(bot, api, test.user, test.threadTS, test.text)

			if !strings.Contains(reply, test.want) {
				t.Fatalf("reply = %q, want containing %q", reply, test.want)
			}
			if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != state.Pending {
				t.Fatalf("status = %q, want pending", got)
			}
		})
	}
}

func TestApprovedUserAndChannelAreHandled(t *testing.T) {
	runner := successfulPlanRunner()
	bot, api, _, _ := newApprovalBot(t, runner)
	approvalCommand(bot, api, "UOWNER", "", "許可 U2")
	approvalCommand(bot, api, "UOWNER", "", "許可 <#C2|general>")
	event := mention()
	event.User = "U2"
	event.Channel = "C2"

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
		t.Fatalf("approvals = %+v, want workflow pending", approvals.ListApprovals())
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
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			approvalCommand(bot, api, "UOWNER", "", fmt.Sprintf("許可 UX%d", i))
		})
	}
	wg.Wait()

	for i := range count {
		if got := approvals.ApprovalStatus(state.ApprovalUser, fmt.Sprintf("UX%d", i)); got != state.Approved {
			t.Fatalf("UX%d status = %q, want approved", i, got)
		}
	}
}

func TestListAndRevokeCommands(t *testing.T) {
	bot, api, store, approvals := newApprovalBot(t, &fakeRunner{})
	approvalCommand(bot, api, "UOWNER", "", "許可 U2")

	list := approvalCommand(bot, api, "UOWNER", "", "許可一覧")
	for _, want := range []string{"ユーザー <@U1>", "チャンネル <#C1>", "✅ ユーザー <@U2> — <@UOWNER> が許可しました"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list = %q, want containing %q", list, want)
		}
	}
	if got := approvalCommand(bot, api, "UOWNER", "", "許可取消 <@U1>"); !strings.Contains(got, ".env の固定の許可") {
		t.Fatalf("revoke fixed = %q, want refusal", got)
	}
	if got := approvalCommand(bot, api, "U3", "", "許可取消 U2"); got != notApproverMessage {
		t.Fatalf("revoke by non-approver = %q, want %q", got, notApproverMessage)
	}
	if got := approvalCommand(bot, api, "UOWNER", "", "許可取消 <@U2|someone>"); !strings.Contains(got, "記録を削除しました") {
		t.Fatalf("revoke = %q, want deleted", got)
	}
	if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != "" {
		t.Fatalf("status after revoke = %q, want none", got)
	}
	if got := approvalCommand(bot, api, "UOWNER", "", "許可取消 U2"); !strings.Contains(got, "記録はありません") {
		t.Fatalf("second revoke = %q, want not found", got)
	}
	if store.claimCalls != 0 {
		t.Fatalf("ClaimEvent called %d times by commands, want 0", store.claimCalls)
	}
}

func TestOtherTextInApprovalChannelIsANormalMention(t *testing.T) {
	runner := successfulPlanRunner()
	bot, api, _, _ := newApprovalBot(t, runner)
	bot.allowedChannels = makeSet([]string{"C1", "COWNER"})

	approvalCommand(bot, api, "U1", "", "許可について教えて")

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestApprovedUserMayReplyInSubscribedThread(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	runner := successfulPlanRunner()
	bot, _, store, approvals := newApprovalBot(t, runner)
	configureActiveSubscription(bot, store, now)
	bot.allowedUsers = makeSet([]string{"U1"})
	if _, _, err := approvals.DecideApproval(state.ApprovalUser, "U9", true, "UOWNER", now); err != nil {
		t.Fatal(err)
	}

	bot.HandleMessage(context.Background(), messageReply("U9", "200.2", "please continue"))

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestNormalizeSlackID(t *testing.T) {
	for input, want := range map[string]string{
		"U123":            "U123",
		"<@U123>":         "U123",
		"<@U123|someone>": "U123",
		"<#C123|general>": "C123",
		"Wf0ABC":          "Wf0ABC",
	} {
		if got := normalizeSlackID(input); got != want {
			t.Errorf("normalizeSlackID(%q) = %q, want %q", input, got, want)
		}
	}
}
