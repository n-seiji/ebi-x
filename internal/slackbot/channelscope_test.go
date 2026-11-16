package slackbot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
			if !reflect.DeepEqual(runner.withoutSlack, []bool{true}) {
				t.Fatalf("withoutSlack = %v, want Slack MCP servers off", runner.withoutSlack)
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
	if scope := bot.channelScope(context.Background(), "CAPPROVAL"); scope != (channelScope{}) {
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

	// A failed lookup is not remembered, so the channel opens again once
	// Slack answers.
	api.channelInfoErr = errors.New("ratelimited")
	now = now.Add(channelScopeTTL + time.Second)
	bot.channelScope(context.Background(), "C1")
	api.channelInfoErr = nil
	bot.channelScope(context.Background(), "C1")
	if api.channelInfoCalls != 4 {
		t.Fatalf("ChannelInfo calls = %d, want a failed lookup retried", api.channelInfoCalls)
	}
}

func TestClosedChannelHistoryIsSaved(t *testing.T) {
	api := &fakeSlack{
		channelInfos: map[string]ChannelInfo{"C1": {External: true}},
		history: []HistoryMessage{
			{ThreadMessage: ThreadMessage{AuthorID: "U2", Timestamp: "1793300000.000100", Text: "請求書の件\n2行目"}, ReplyCount: 1},
			{ThreadMessage: ThreadMessage{AuthorID: "U3", Timestamp: "1793200000.000100", Text: "おはようございます"}},
		},
		threadMessages: []ThreadMessage{
			{AuthorID: "U2", Timestamp: "1793300000.000100", Text: "請求書の件"},
			{AuthorID: "U1", Timestamp: "1793300100.000100", Text: "確認します"},
		},
	}
	runner := completedRunner()
	bot, dir := newFilesTestBot(t, api, runner)

	bot.HandleMention(context.Background(), mention())

	path := filepath.Join(dir, channelHistoryDir, channelHistoryFile)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	for _, want := range []string{"<@U2> (ts 1793300000.000100): 請求書の件\n  2行目", "  - [", "<@U1> (ts 1793300100.000100): 確認します", "おはようございます"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("history = %q, want %q", content, want)
		}
	}
	if len(runner.prompts) != 1 || !strings.Contains(runner.prompts[0], path) {
		t.Fatalf("prompt = %q, want the history path", runner.prompts)
	}

	// A fresh copy is reused rather than read from Slack again.
	lease := workspace.NewLease(dir, nil, nil, func() {})
	lease.Area = workspace.Closed("C1")
	scope := bot.channelScope(context.Background(), "C1")
	bot.channelHistoryNote(context.Background(), lease, "C1", scope)
	if api.historyCalls != 1 {
		t.Fatalf("history calls = %d, want the saved copy reused", api.historyCalls)
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

func TestHistoryIsNotSavedInTheOpenArea(t *testing.T) {
	api := &fakeSlack{channelInfos: map[string]ChannelInfo{"C1": {Private: true}}}
	bot := newTestBot(t, &fakeStore{claim: true}, api, completedRunner())
	dir := t.TempDir()
	lease := workspace.NewLease(dir, nil, nil, func() {})
	scope := bot.channelScope(context.Background(), "C1")

	note := bot.channelHistoryNote(context.Background(), lease, "C1", scope)

	if api.historyCalls != 0 || !strings.Contains(note, "取得できませんでした") {
		t.Fatalf("history calls = %d, note = %q, want nothing saved for an open-area thread", api.historyCalls, note)
	}
	if _, err := os.Stat(filepath.Join(dir, channelHistoryDir)); !os.IsNotExist(err) {
		t.Fatalf("history directory stat = %v, want none", err)
	}
}
