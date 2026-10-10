package slackbot

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

func TestCommandLabelKeepsProgramAndSubcommandOnly(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{command: "go test ./...", want: "go test"},
		{command: "bash -lc 'go test ./...'", want: "go test"},
		{command: `/bin/zsh -lc "cd /repo && git commit -m 'fix'"`, want: "git commit"},
		{command: "/usr/bin/rg -n TODO", want: "rg"},
		{command: `curl -H "Authorization: Bearer secret" https://x`, want: "curl"},
		{command: "pnpm build slides images --draft", want: "pnpm build"},
		{command: "cd /repo", want: "cd"},
		{command: "", want: ""},
	}
	for _, test := range tests {
		if got := commandLabel(test.command); got != test.want {
			t.Errorf("commandLabel(%q) = %q, want %q", test.command, got, test.want)
		}
	}
}

func TestProgressStatus(t *testing.T) {
	p := &progress{}
	steps := []struct {
		activity codex.Activity
		want     string
	}{
		{
			activity: codex.Activity{Kind: codex.ActivityPlan, Total: 3, Current: "原因を調べる"},
			want:     "が「原因を調べる」を進めています… 0/3 完了",
		},
		{
			activity: codex.Activity{Kind: codex.ActivityCommand, Command: "bash -lc 'go test ./...'"},
			want:     "がコマンドを実行しています（go test）… 0/3 完了",
		},
		{
			activity: codex.Activity{Kind: codex.ActivityFileChange, Paths: []string{"/w/internal/a.go", "/w/b.go"}},
			want:     "がファイルを編集しています（a.go ほか1件）… 0/3 完了",
		},
		{
			activity: codex.Activity{Kind: codex.ActivityPlan, Done: 3, Total: 3},
			want:     "がファイルを編集しています（a.go ほか1件）… 3/3 完了",
		},
	}
	for i, step := range steps {
		if got := p.observe(step.activity); got != step.want {
			t.Errorf("step %d status = %q, want %q", i, got, step.want)
		}
	}

	search := (&progress{}).observe(codex.Activity{Kind: codex.ActivityWebSearch, Query: strings.Repeat("あ", 50)})
	if want := "がWebを検索しています（" + strings.Repeat("あ", maxStatusDetailRunes-1) + "…）…"; search != want {
		t.Errorf("long search status = %q, want %q", search, want)
	}
}

func TestStatusKeeperShowsUpdatesPromptly(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{}, api, &fakeRunner{})
	keeper := bot.startStatus(context.Background(), "C1", "100.1", workingStatus)
	keeper.Update("がコマンドを実行しています（go test）…")
	waitFor(t, func() bool {
		return slices.Contains(statusTexts(api), "がコマンドを実行しています（go test）…")
	})
	keeper.Stop()

	got := statusTexts(api)
	if got[0] != workingStatus {
		t.Fatalf("first status = %q, want %q", got[0], workingStatus)
	}
}

// blockingRunner reports activity and then runs until its context ends.
type blockingRunner struct {
	started chan struct{}
}

func (r *blockingRunner) Run(ctx context.Context, _, _, _ string, _, _ []string, _ string, callback func(string) error, onActivity func(codex.Activity)) (*codex.TurnResult, error) {
	if callback != nil {
		if err := callback("codex-thread"); err != nil {
			return nil, err
		}
	}
	if onActivity != nil {
		onActivity(codex.Activity{Kind: codex.ActivityCommand, Command: "go test ./..."})
	}
	close(r.started)
	<-ctx.Done()
	return nil, fmt.Errorf("codex exec: %w", ctx.Err())
}

func TestStopCommandCancelsRunningWork(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &blockingRunner{started: make(chan struct{})}
	bot := New(api, store, runner, Config{
		AllowedUserIDs:    []string{"U1"},
		AllowedChannelIDs: []string{"C1"},
		WorkspaceDir:      "/repo/workspace",
		MemoryDir:         t.TempDir(),
		CodexTimeout:      time.Minute,
		BotUserID:         "UBOT",
	}, nil)

	var wg sync.WaitGroup
	wg.Go(func() { bot.HandleMention(context.Background(), mention()) })
	<-runner.started
	waitFor(t, func() bool {
		return slices.Contains(statusTexts(api), "がコマンドを実行しています（go test）…")
	})

	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{
		User: "U1", Channel: "C1", TimeStamp: "100.2", ThreadTimeStamp: "100.1", Text: "<@UBOT> 停止",
	})
	wg.Wait()

	if !slices.Contains(api.postTexts, stoppedMessage) {
		t.Fatalf("posts = %q, want stopped message", api.postTexts)
	}
	if store.current != state.Interrupted {
		t.Fatalf("state = %s, want interrupted", store.current)
	}
	if !hasCall(api, "add:ok_hand") {
		t.Fatalf("calls = %#v, want ok_hand on the stop command", api.calls)
	}
	if len(bot.running) != 0 {
		t.Fatalf("running = %#v, want empty after the request ended", bot.running)
	}
}

func TestStopCommandWithNothingRunning(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newTestBot(t, store, api, runner)

	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{
		User: "U1", Channel: "C1", TimeStamp: "100.2", ThreadTimeStamp: "100.1", Text: "<@UBOT> stop!",
	})

	if runner.calls != 0 || store.claimCalls != 0 {
		t.Fatalf("runner calls = %d, claims = %d, want none", runner.calls, store.claimCalls)
	}
	if !slices.Equal(api.postTexts, []string{nothingToStopMessage}) {
		t.Fatalf("posts = %q, want nothing-to-stop message", api.postTexts)
	}
}

func TestIsStopCommand(t *testing.T) {
	for _, message := range []string{"停止", " 止めて。", "Stop", "キャンセルして！"} {
		if !isStopCommand(message) {
			t.Errorf("isStopCommand(%q) = false, want true", message)
		}
	}
	for _, message := range []string{"停止の仕方を教えて", "do not stop", ""} {
		if isStopCommand(message) {
			t.Errorf("isStopCommand(%q) = true, want false", message)
		}
	}
}

func statusTexts(api *fakeSlack) []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	var texts []string
	for _, call := range api.calls {
		if call.kind == "status" {
			texts = append(texts, call.text)
		}
	}
	return texts
}

func hasCall(api *fakeSlack, kind string) bool {
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls {
		if call.kind == kind {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStopReplyInSubscribedThread(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newTestBot(t, store, api, runner)
	configureActiveSubscription(bot, store, time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	cancelled, untrack := bot.trackRequest(context.Background(), "C1:100.1", "C1:100.1")
	defer untrack()

	bot.HandleMessage(context.Background(), messageReply("U1", "100.3", "止めて"))

	if context.Cause(cancelled) != errStoppedByUser {
		t.Fatalf("cause = %v, want stopped by user", context.Cause(cancelled))
	}
	if runner.calls != 0 || store.claimCalls != 0 {
		t.Fatalf("runner calls = %d, claims = %d, want the reply not to start work", runner.calls, store.claimCalls)
	}
}
