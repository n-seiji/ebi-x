package slackbot

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/n-seiji/ebi-x/internal/workspace"
)

func completedRunner() *fakeRunner {
	return &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}},
	}}
}

func TestClosedChannelTurnsAreKeptApart(t *testing.T) {
	tests := []struct {
		name string
		info ChannelInfo
		err  error
		kind string
	}{
		{name: "private", info: ChannelInfo{Private: true}, kind: "プライベートチャンネル"},
		{name: "slack connect", info: ChannelInfo{External: true}, kind: "外部の組織と共有しているチャンネル"},
		{name: "lookup failure", err: errors.New("missing_scope"), kind: "種類を確認できなかったチャンネル"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeSlack{channelInfos: map[string]ChannelInfo{"C1": test.info}, channelInfoErr: test.err}
			runner := completedRunner()
			bot := newTestBot(t, &fakeStore{claim: true}, api, runner)
			workspaces := &fakeWorkspaces{}
			bot.config.Workspaces = workspaces

			bot.HandleMention(context.Background(), mention())

			if !reflect.DeepEqual(workspaces.areas, []workspace.Area{workspace.Closed("C1")}) {
				t.Fatalf("areas = %v, want the channel's own closed area", workspaces.areas)
			}
			if !reflect.DeepEqual(runner.denied, [][]string{{"/home/workspace", "/home/checkouts", "/shared/plain"}}) {
				t.Fatalf("denied = %v, want the open threads' files hidden", runner.denied)
			}
			if !reflect.DeepEqual(runner.access, []codex.Access{{WithoutSlack: true}}) {
				t.Fatalf("access = %+v, want Slack MCP servers off", runner.access)
			}
			if len(runner.prompts) != 1 || !strings.Contains(runner.prompts[0], test.kind) || !strings.Contains(runner.prompts[0], "Slack の検索・読み取りツールは使えません") {
				t.Fatalf("prompt = %q, want the closed channel note naming %q", runner.prompts, test.kind)
			}
		})
	}
}

func TestDirectMessagesKeepFilesApartButKeepSlackSearch(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, completedRunner())
	scope := bot.channelScope(context.Background(), "D1")
	if scope.area != workspace.Closed("D1") || scope.slackLimited {
		t.Fatalf("DM scope = %+v, want its own area with Slack search", scope)
	}
	if api.channelInfoCalls != 0 {
		t.Fatalf("ChannelInfo calls = %d, want none for a DM", api.channelInfoCalls)
	}
}

func TestApprovalChannelStaysOpen(t *testing.T) {
	api := &fakeSlack{channelInfos: map[string]ChannelInfo{"CAPPROVAL": {Private: true}}}
	bot := newTestBot(t, &fakeStore{claim: true}, api, completedRunner())
	bot.config.ApprovalChannelID = "CAPPROVAL"
	if scope := bot.channelScope(context.Background(), "CAPPROVAL"); !reflect.DeepEqual(scope, channelScope{}) {
		t.Fatalf("approval channel scope = %+v, want open", scope)
	}
}

func TestChannelScopeIsCachedForAWhile(t *testing.T) {
	api := &fakeSlack{channelInfos: map[string]ChannelInfo{"C1": {Private: true}}}
	bot := newTestBot(t, &fakeStore{claim: true}, api, completedRunner())
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	bot.now = func() time.Time { return now }

	bot.channelScope(context.Background(), "C1")
	bot.channelScope(context.Background(), "C1")
	if api.channelInfoCalls != 1 {
		t.Fatalf("ChannelInfo calls = %d, want 1 within the cache time", api.channelInfoCalls)
	}
	now = now.Add(channelScopeTTL + time.Second)
	bot.channelScope(context.Background(), "C1")
	if api.channelInfoCalls != 2 {
		t.Fatalf("ChannelInfo calls = %d, want a new lookup after the cache time", api.channelInfoCalls)
	}

	api.channelInfoErr = errors.New("ratelimited")
	now = now.Add(channelScopeTTL + time.Second)
	bot.channelScope(context.Background(), "C1")
	api.channelInfoErr = nil
	bot.channelScope(context.Background(), "C1")
	if api.channelInfoCalls != 4 {
		t.Fatalf("ChannelInfo calls = %d, want a failed lookup retried", api.channelInfoCalls)
	}
}

func TestSubscribedPrivateChannelRepliesAreHandled(t *testing.T) {
	store := &fakeStore{claim: true, subscriptions: map[string]state.Subscription{"C1:100.1": {ExpiresAt: time.Now().Add(time.Hour)}}}
	runner := completedRunner()
	api := &fakeSlack{channelInfos: map[string]ChannelInfo{"C1": {Private: true}}}
	bot := newTestBot(t, store, api, runner)
	event := messageReply("U1", "100.2", "続きをお願いします")
	event.ChannelType = "group"

	bot.HandleMessage(context.Background(), event)

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want the private channel reply handled", runner.calls)
	}
}
