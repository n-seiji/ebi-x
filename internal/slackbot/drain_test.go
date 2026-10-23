package slackbot

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

// gatedRunner holds each turn until release is closed.
type gatedRunner struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	prompts []string
}

func (r *gatedRunner) Run(_ context.Context, _, _, _ string, _, _ []string, text string, callback func(string) error, _ func(codex.Activity)) (*codex.TurnResult, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, text)
	first := len(r.prompts) == 1
	r.mu.Unlock()
	if callback != nil {
		if err := callback("codex-thread"); err != nil {
			return nil, err
		}
	}
	if first {
		close(r.started)
	}
	<-r.release
	return &codex.TurnResult{Completed: true, Messages: []string{"Done."}}, nil
}

func (r *gatedRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.prompts)
}

func newDrainBot(t *testing.T, store *fakeStore, api *fakeSlack, runner Runner, parallel int) *Bot {
	t.Helper()
	return New(api, store, runner, Config{
		AllowedUserIDs:    []string{"U1"},
		AllowedChannelIDs: []string{"C1"},
		WorkspaceDir:      "/repo/workspace",
		MemoryDir:         t.TempDir(),
		CodexTimeout:      time.Minute,
		MaxParallelWork:   parallel,
		BotUserID:         "UBOT",
	}, nil)
}

func otherThreadMention() *slackevents.AppMentionEvent {
	return &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "200.1", Text: "<@UBOT> next"}
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestDrainDefersNewRequestsUntilRunningWorkEnds(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &gatedRunner{started: make(chan struct{}), release: make(chan struct{})}
	bot := newDrainBot(t, store, api, runner, 3)

	var wg sync.WaitGroup
	wg.Go(func() { bot.HandleMention(context.Background(), mention()) })
	<-runner.started

	drained := bot.Drain()
	bot.HandleMention(context.Background(), otherThreadMention())

	if closed(drained) {
		t.Fatal("drain finished while a turn was still running")
	}
	if got := bot.ActiveRequests(); got != 1 {
		t.Fatalf("ActiveRequests() = %d, want the running turn only", got)
	}
	if runner.calls() != 1 {
		t.Fatalf("runner calls = %d, want only the running turn", runner.calls())
	}
	want := []state.Deferred{{Source: "mention", AuthorID: "U1", Channel: "C1", Timestamp: "200.1", ThreadTS: "200.1", Message: "next"}}
	got := store.deferred
	for i := range got {
		got[i].DeferredAt = time.Time{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("deferred = %#v, want %#v", got, want)
	}
	if !slices.Contains(api.postTexts, deferredMessage) {
		t.Fatalf("posts = %q, want the deferred notice", api.postTexts)
	}

	close(runner.release)
	wg.Wait()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish after the running turn ended")
	}
	if !slices.Contains(api.postTexts, "Done.") {
		t.Fatalf("posts = %q, want the running turn's result", api.postTexts)
	}
	if got := bot.ActiveRequests(); got != 0 {
		t.Fatalf("ActiveRequests() = %d after the drain, want 0", got)
	}
}

func TestDrainDefersQueuedWork(t *testing.T) {
	store := &looseStore{threads: make(map[string]string)}
	api := &fakeSlack{}
	runner := &gatedRunner{started: make(chan struct{}), release: make(chan struct{})}
	bot := New(api, store, runner, Config{
		AllowedUserIDs:    []string{"U1"},
		AllowedChannelIDs: []string{"C1"},
		WorkspaceDir:      "/repo/workspace",
		MemoryDir:         t.TempDir(),
		CodexTimeout:      time.Minute,
		MaxParallelWork:   1,
		BotUserID:         "UBOT",
	}, nil)

	var wg sync.WaitGroup
	wg.Go(func() { bot.HandleMention(context.Background(), mention()) })
	<-runner.started
	queued := make(chan struct{})
	go func() {
		defer close(queued)
		bot.HandleMention(context.Background(), otherThreadMention())
	}()
	waitFor(t, func() bool { return slices.Contains(statusTexts(api), queuedStatus) })

	drained := bot.Drain()
	select {
	case <-queued:
	case <-time.After(5 * time.Second):
		t.Fatal("queued request was not deferred when the drain began")
	}
	if runner.calls() != 1 {
		t.Fatalf("runner calls = %d, want the queued request not started", runner.calls())
	}
	if !slices.Contains(api.postTexts, deferredMessage) {
		t.Fatalf("posts = %q, want the deferred notice", api.postTexts)
	}
	close(runner.release)
	wg.Wait()
	<-drained
}

func TestRunDeferredResumesRequestsInOrder(t *testing.T) {
	store := &fakeStore{claim: true, deferred: []state.Deferred{
		{Source: "mention", AuthorID: "U1", Channel: "C1", Timestamp: "100.1", ThreadTS: "100.1", Message: "first"},
		{Source: "unknown", AuthorID: "U1", Channel: "C1", Timestamp: "100.2", ThreadTS: "100.1", Message: "dropped"},
	}}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{Completed: true, Messages: []string{"Done."}}}}}
	bot := newTestBot(t, store, api, runner)

	var wg sync.WaitGroup
	bot.runDeferred(context.Background(), &wg)
	wg.Wait()

	if runner.calls != 1 || !strings.Contains(runner.prompts[0], "first") {
		t.Fatalf("runner calls = %d, prompts = %q; want the deferred request run once", runner.calls, runner.prompts)
	}
	if len(store.deferred) != 0 {
		t.Fatalf("deferred = %#v, want taken", store.deferred)
	}
	if !slices.Contains(api.postTexts, "Done.") {
		t.Fatalf("posts = %q, want the result", api.postTexts)
	}
}

func TestStopCancelsDeferredRequests(t *testing.T) {
	store := &fakeStore{claim: true, deferred: []state.Deferred{
		{Source: "mention", AuthorID: "U1", Channel: "C1", Timestamp: "100.2", ThreadTS: "100.1", Message: "later"},
		{Source: "mention", AuthorID: "U1", Channel: "C1", Timestamp: "300.1", ThreadTS: "300.1", Message: "other thread"},
	}}
	api := &fakeSlack{}
	bot := newTestBot(t, store, api, &fakeRunner{})

	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{
		User: "U1", Channel: "C1", TimeStamp: "100.3", ThreadTimeStamp: "100.1", Text: "<@UBOT> 停止",
	})

	if len(store.deferred) != 1 || store.deferred[0].ThreadTS != "300.1" {
		t.Fatalf("deferred = %#v, want only the other thread's request", store.deferred)
	}
	if !slices.Equal(api.postTexts, []string{deferredStoppedMessage}) {
		t.Fatalf("posts = %q, want the deferred-stopped message", api.postTexts)
	}
}
