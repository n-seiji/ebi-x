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
	"github.com/slack-go/slack"
)

type fakeBlockMessage struct {
	channel   string
	timestamp string
	fallback  string
	blocks    []slack.Block
}

func (s *fakeSlack) PostBlocks(_ context.Context, channel, fallback string, blocks []slack.Block) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blockPostErr != nil {
		return "", s.blockPostErr
	}
	s.blockPosts = append(s.blockPosts, fakeBlockMessage{channel: channel, fallback: fallback, blocks: blocks})
	return fmt.Sprintf("approval-%d", len(s.blockPosts)), nil
}

func (s *fakeSlack) UpdateBlocks(_ context.Context, channel, timestamp, fallback string, blocks []slack.Block) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockUpdates = append(s.blockUpdates, fakeBlockMessage{channel: channel, timestamp: timestamp, fallback: fallback, blocks: blocks})
	return nil
}

func (s *fakeSlack) PostEphemeral(_ context.Context, _, user, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ephemerals = append(s.ephemerals, user+":"+text)
	return nil
}

func (s *fakeSlack) IsChannelMember(_ context.Context, _, user string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.members[user], s.memberErr
}

func newApprovalBot(t *testing.T, runner *fakeRunner) (*Bot, *fakeSlack, *fakeStore, *state.Store) {
	t.Helper()
	approvals, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	store := &fakeStore{claim: true}
	api := &fakeSlack{members: map[string]bool{"UOWNER": true, "UOWNER2": true}}
	bot := newTestBot(t, store, api, runner)
	bot.config.ApprovalChannelID = "GOWNER"
	bot.config.Approvals = approvals
	return bot, api, store, approvals
}

func approvalClick(user, actionID, value string) slack.InteractionCallback {
	var callback slack.InteractionCallback
	callback.Type = slack.InteractionTypeBlockActions
	callback.User.ID = user
	callback.Channel.ID = "GOWNER"
	callback.Container.MessageTs = "approval-1"
	callback.ActionCallback.BlockActions = []*slack.BlockAction{{ActionID: actionID, Value: value}}
	return callback
}

func hasActions(blocks []slack.Block) bool {
	for _, block := range blocks {
		if block.BlockType() == slack.MBTAction {
			return true
		}
	}
	return false
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
	if len(api.blockPosts) != 1 {
		t.Fatalf("approval requests = %d, want 1", len(api.blockPosts))
	}
	request := api.blockPosts[0]
	if request.channel != "GOWNER" || !hasActions(request.blocks) || !strings.Contains(request.fallback, "<@U2>") {
		t.Fatalf("approval request = %+v, want buttons for U2 in GOWNER", request)
	}
	if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != state.Pending {
		t.Fatalf("status = %q, want pending", got)
	}
	if got := strings.Join(api.postTexts, "|"); got != approvalRequestedMessage+"|"+approvalRequestedMessage {
		t.Fatalf("posts = %q, want approval requested twice", got)
	}
}

func TestUnknownUserAndChannelRequestBothApprovals(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
	event := mention()
	event.User = "U2"
	event.Channel = "G2"

	bot.HandleMention(context.Background(), event)

	if len(api.blockPosts) != 2 {
		t.Fatalf("approval requests = %d, want 2", len(api.blockPosts))
	}
	if approvals.ApprovalStatus(state.ApprovalUser, "U2") != state.Pending ||
		approvals.ApprovalStatus(state.ApprovalChannel, "G2") != state.Pending {
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
			mustRequest(t, approvals, state.ApprovalUser, "U2")
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

			if store.claimCalls != 0 || len(api.blockPosts) != 0 {
				t.Fatalf("claims = %d, approval requests = %d, want 0", store.claimCalls, len(api.blockPosts))
			}
			if got := strings.Join(api.postTexts, "|"); got != "403 forbidden. @seiji に確認してください。" {
				t.Fatalf("posts = %q, want forbidden response", got)
			}
		})
	}
}

func TestFailedApprovalPostIsRolledBack(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
	api.blockPostErr = fmt.Errorf("not_in_channel")
	event := mention()
	event.User = "U2"

	bot.HandleMention(context.Background(), event)

	if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != "" {
		t.Fatalf("status = %q, want no record", got)
	}
	if got := strings.Join(api.postTexts, "|"); got != "403 forbidden. @seiji に確認してください。" {
		t.Fatalf("posts = %q, want forbidden response", got)
	}
}

func TestApprovedUserAndChannelAreHandled(t *testing.T) {
	runner := successfulPlanRunner()
	bot, _, _, approvals := newApprovalBot(t, runner)
	for _, subject := range []approvalSubject{{state.ApprovalUser, "U2"}, {state.ApprovalChannel, "G2"}} {
		mustRequest(t, approvals, subject.kind, subject.id)
		if _, _, err := approvals.DecideApproval(subject.kind, subject.id, true, "UOWNER", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	event := mention()
	event.User = "U2"
	event.Channel = "G2"

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
	if len(api.blockPosts) != 1 || approvals.ApprovalStatus(state.ApprovalWorkflow, "Wf0NEW") != state.Pending {
		t.Fatalf("approval requests = %d, approvals = %+v, want workflow pending", len(api.blockPosts), approvals.ListApprovals())
	}
	bot.HandleApprovalAction(context.Background(), approvalClick("UOWNER", approveActionID, "workflow:Wf0NEW"))
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

	if len(api.blockPosts) != 0 {
		t.Fatalf("approval requests = %d, want 0", len(api.blockPosts))
	}
}

func TestApprovalButtonDecidesOnce(t *testing.T) {
	bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
	mustRequest(t, approvals, state.ApprovalUser, "U2")

	bot.HandleApprovalAction(context.Background(), approvalClick("UOWNER", approveActionID, "user:U2"))
	bot.HandleApprovalAction(context.Background(), approvalClick("UOWNER2", denyActionID, "user:U2"))

	approval, _ := approvals.GetApproval(state.ApprovalUser, "U2")
	if approval.Status != state.Approved || approval.DecidedBy != "UOWNER" || approval.DecidedAt.IsZero() {
		t.Fatalf("approval = %+v, want approved by UOWNER", approval)
	}
	if len(api.blockUpdates) != 2 || hasActions(api.blockUpdates[0].blocks) || api.blockUpdates[0].timestamp != "approval-1" {
		t.Fatalf("updates = %+v, want buttons removed from approval-1", api.blockUpdates)
	}
	if len(api.ephemerals) != 1 || !strings.Contains(api.ephemerals[0], "UOWNER2:すでに<@UOWNER> が許可しました") {
		t.Fatalf("ephemerals = %q, want already decided notice", api.ephemerals)
	}
}

func TestApprovalButtonRejectsNonMembersAndOtherChannels(t *testing.T) {
	tests := []struct {
		name      string
		click     slack.InteractionCallback
		memberErr error
		ephemeral bool
	}{
		{name: "non-member", click: approvalClick("U2", approveActionID, "user:U2"), ephemeral: true},
		{name: "membership lookup error", click: approvalClick("UOWNER", approveActionID, "user:U2"), memberErr: fmt.Errorf("missing_scope"), ephemeral: true},
		{name: "other channel", click: func() slack.InteractionCallback {
			click := approvalClick("UOWNER", approveActionID, "user:U2")
			click.Channel.ID = "C1"
			return click
		}()},
		{name: "malformed value", click: approvalClick("UOWNER", approveActionID, "user:<!channel>")},
		{name: "unknown action", click: approvalClick("UOWNER", "other", "user:U2")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bot, api, _, approvals := newApprovalBot(t, &fakeRunner{})
			api.memberErr = test.memberErr
			mustRequest(t, approvals, state.ApprovalUser, "U2")

			bot.HandleApprovalAction(context.Background(), test.click)

			if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != state.Pending {
				t.Fatalf("status = %q, want pending", got)
			}
			if got := len(api.ephemerals) == 1; got != test.ephemeral {
				t.Fatalf("ephemerals = %q, want notice %v", api.ephemerals, test.ephemeral)
			}
		})
	}
}

func TestConcurrentApprovalsAreAllKept(t *testing.T) {
	bot, _, _, approvals := newApprovalBot(t, &fakeRunner{})
	const count = 32
	for i := range count {
		mustRequest(t, approvals, state.ApprovalUser, fmt.Sprintf("U%d", i))
	}

	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			bot.HandleApprovalAction(context.Background(), approvalClick("UOWNER", approveActionID, fmt.Sprintf("user:U%d", i)))
		})
	}
	wg.Wait()

	for i := range count {
		if got := approvals.ApprovalStatus(state.ApprovalUser, fmt.Sprintf("U%d", i)); got != state.Approved {
			t.Fatalf("U%d status = %q, want approved", i, got)
		}
	}
}

func TestApprovalCommands(t *testing.T) {
	bot, api, store, approvals := newApprovalBot(t, &fakeRunner{})
	mustRequest(t, approvals, state.ApprovalUser, "U2")
	if _, _, err := approvals.DecideApproval(state.ApprovalUser, "U2", true, "UOWNER", time.Now()); err != nil {
		t.Fatal(err)
	}
	command := func(user, text string) string {
		api.postTexts = nil
		event := mention()
		event.User = user
		event.Channel = "GOWNER"
		event.Text = "<@UBOT> " + text
		bot.HandleMention(context.Background(), event)
		return strings.Join(api.postTexts, "|")
	}

	list := command("UOWNER", "許可一覧")
	for _, want := range []string{"ユーザー <@U1>", "チャンネル <#C1>", "✅ ユーザー <@U2> — <@UOWNER> が許可しました"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list = %q, want containing %q", list, want)
		}
	}
	if got := command("UOWNER", "許可取消 <@U1>"); !strings.Contains(got, ".env の固定の許可") {
		t.Fatalf("revoke fixed = %q, want refusal", got)
	}
	if got := command("U3", "許可取消 U2"); got != notApproverMessage {
		t.Fatalf("revoke by non-member = %q, want %q", got, notApproverMessage)
	}
	if got := command("UOWNER", "許可取消 <@U2|someone>"); !strings.Contains(got, "記録を削除しました") {
		t.Fatalf("revoke = %q, want deleted", got)
	}
	if got := approvals.ApprovalStatus(state.ApprovalUser, "U2"); got != "" {
		t.Fatalf("status after revoke = %q, want none", got)
	}
	if got := command("UOWNER", "許可取消 U2"); !strings.Contains(got, "記録はありません") {
		t.Fatalf("second revoke = %q, want not found", got)
	}
	if store.claimCalls != 0 {
		t.Fatalf("ClaimEvent called %d times by commands, want 0", store.claimCalls)
	}
}

func TestApprovedUserMayReplyInSubscribedThread(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	runner := successfulPlanRunner()
	bot, _, store, approvals := newApprovalBot(t, runner)
	configureActiveSubscription(bot, store, now)
	bot.allowedUsers = makeSet([]string{"U1"})
	mustRequest(t, approvals, state.ApprovalUser, "U9")
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

func mustRequest(t *testing.T, approvals *state.Store, kind state.ApprovalKind, id string) {
	t.Helper()
	created, err := approvals.RequestApproval(kind, id, "U1", "C1", time.Now())
	if err != nil || !created {
		t.Fatalf("RequestApproval(%s, %s) = %v, %v; want created", kind, id, created, err)
	}
}
