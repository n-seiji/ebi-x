package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/memory"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

type fakeStore struct {
	mu                  sync.Mutex
	claim               bool
	claimErr            error
	claimCalls          int
	current             state.State
	transitions         [][2]state.State
	threadIDs           map[string]string
	threadKeys          []string
	subscriptions       map[string]state.Subscription
	subscriptionCalls   []subscriptionCall
	subscriptionDeletes []string
	subscriptionErr     error
}

type subscriptionCall struct {
	threadKey string
	startedAt time.Time
	expiresAt time.Time
}

func (s *fakeStore) ClaimEvent(string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	if s.claimErr == nil && s.claim {
		s.current = state.Received
	}
	return s.claim, s.claimErr
}

func (s *fakeStore) Transition(_ string, from, to state.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != from {
		return errors.New("unexpected source state")
	}
	s.current = to
	s.transitions = append(s.transitions, [2]state.State{from, to})
	return nil
}

func (s *fakeStore) GetThread(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threadKeys = append(s.threadKeys, key)
	threadID, ok := s.threadIDs[key]
	return threadID, ok
}

func (s *fakeStore) SetThread(key, threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threadIDs == nil {
		s.threadIDs = make(map[string]string)
	}
	s.threadIDs[key] = threadID
	return nil
}

func (s *fakeStore) GetSubscription(key string) (state.Subscription, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subscription, ok := s.subscriptions[key]
	return subscription, ok
}

func (s *fakeStore) SetSubscription(key string, startedAt, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscriptionCalls = append(s.subscriptionCalls, subscriptionCall{
		threadKey: key,
		startedAt: startedAt,
		expiresAt: expiresAt,
	})
	if s.subscriptionErr != nil {
		return s.subscriptionErr
	}
	if s.subscriptions == nil {
		s.subscriptions = make(map[string]state.Subscription)
	}
	s.subscriptions[key] = state.Subscription{StartedAt: startedAt, ExpiresAt: expiresAt}
	return nil
}

func (s *fakeStore) DeleteSubscriptionIfExpired(key string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subscription, ok := s.subscriptions[key]
	if !ok || subscription.ExpiresAt.After(now) {
		return false, nil
	}
	s.subscriptionDeletes = append(s.subscriptionDeletes, key)
	delete(s.subscriptions, key)
	return true, nil
}

type slackCall struct {
	kind string
	text string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type fakeSlack struct {
	mu                sync.Mutex
	calls             []slackCall
	postErrs          []error
	postTexts         []string
	threadMessages    []ThreadMessage
	threadErr         error
	threadCalls       int
	threadLatest      string
	hasReaction       bool
	reactionErr       error
	reactionErrs      []error
	reactionCalls     int
	reactionChannel   string
	reactionTimestamp string
	reactionName      string
}

func (s *fakeSlack) PostMessage(_ context.Context, _, _, text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slackCall{kind: "post", text: text})
	s.postTexts = append(s.postTexts, text)
	var err error
	if len(s.postErrs) > 0 {
		err = s.postErrs[0]
		s.postErrs = s.postErrs[1:]
	}
	return "reply-ts", err
}

func (s *fakeSlack) GetThreadMessages(_ context.Context, _, _, latest string) ([]ThreadMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threadCalls++
	s.threadLatest = latest
	return append([]ThreadMessage(nil), s.threadMessages...), s.threadErr
}

func (s *fakeSlack) SetStatus(_ context.Context, _, _, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slackCall{kind: "status", text: status})
	return nil
}

func (s *fakeSlack) AddReaction(_ context.Context, _, _, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slackCall{kind: "add:" + name})
	return nil
}

func (s *fakeSlack) RemoveReaction(_ context.Context, _, _, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slackCall{kind: "remove:" + name})
	return nil
}

func (s *fakeSlack) HasReaction(_ context.Context, channel, timestamp, reaction string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reactionCalls++
	s.reactionChannel = channel
	s.reactionTimestamp = timestamp
	s.reactionName = reaction
	if len(s.reactionErrs) > 0 {
		err := s.reactionErrs[0]
		s.reactionErrs = s.reactionErrs[1:]
		return s.hasReaction, err
	}
	return s.hasReaction, s.reactionErr
}

type runnerResponse struct {
	result *codex.TurnResult
	err    error
}

type fakeRunner struct {
	mu        sync.Mutex
	responses []runnerResponse
	calls     int
	threadIDs []string
	sandboxes []string
	cwds      []string
	roots     [][]string
	prompts   []string
	onRun     func(call int)
}

func (r *fakeRunner) Run(_ context.Context, threadID, sandbox, cwd string, roots []string, prompt string, callback func(string) error) (*codex.TurnResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.onRun != nil {
		r.onRun(r.calls)
	}
	r.threadIDs = append(r.threadIDs, threadID)
	r.sandboxes = append(r.sandboxes, sandbox)
	r.cwds = append(r.cwds, cwd)
	r.roots = append(r.roots, roots)
	r.prompts = append(r.prompts, prompt)
	if callback != nil {
		if err := callback("plan-thread"); err != nil {
			return nil, err
		}
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response.result, response.err
}

func newTestBot(t *testing.T, store *fakeStore, api *fakeSlack, runner *fakeRunner) *Bot {
	t.Helper()
	return New(api, store, runner, Config{
		AllowedUserIDs: []string{"U1"},
		WorkspaceDir:   "/repo/workspace",
		MemoryDir:      filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:   time.Minute,
		BotUserID:      "UBOT",
	}, nil)
}

func mention() *slackevents.AppMentionEvent {
	return &slackevents.AppMentionEvent{
		User:      "U1",
		Channel:   "C1",
		TimeStamp: "100.1",
		Text:      "<@UBOT> do it",
	}
}

func messageReply(user, timestamp, text string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{
		Type:            "message",
		User:            user,
		Text:            text,
		ThreadTimeStamp: "100.1",
		TimeStamp:       timestamp,
		Channel:         "C1",
		ChannelType:     "channel",
	}
}

func configureActiveSubscription(bot *Bot, store *fakeStore, now time.Time) {
	bot.allowedChannels = makeSet([]string{"C1"})
	bot.now = func() time.Time { return now }
	if store.subscriptions == nil {
		store.subscriptions = make(map[string]state.Subscription)
	}
	store.subscriptions["C1:100.1"] = state.Subscription{
		StartedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(time.Hour),
	}
}

func TestAllowlistRejectsUserAndChannel(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Bot)
		event     *slackevents.AppMentionEvent
	}{
		{
			name: "user",
			event: &slackevents.AppMentionEvent{
				User: "U2", Channel: "C1", TimeStamp: "1", Text: "<@UBOT> work",
			},
		},
		{
			name: "channel",
			configure: func(bot *Bot) {
				bot.allowedChannels = makeSet([]string{"C2"})
			},
			event: mention(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			bot := newTestBot(t, store, api, &fakeRunner{})
			if test.configure != nil {
				test.configure(bot)
			}
			bot.HandleMention(context.Background(), test.event)
			if store.claimCalls != 0 {
				t.Fatalf("ClaimEvent called %d times, want 0", store.claimCalls)
			}
			if got := strings.Join(api.postTexts, "|"); got != "403 forbidden. @seiji に確認してください。" {
				t.Fatalf("posts = %q, want forbidden response", got)
			}
		})
	}
}

func TestAllowedWorkflowMentionIsHandled(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"## 方針\nDone.\n## 作業指示\nNONE"},
	}}}}
	bot := newTestBot(t, store, api, runner)
	bot.config.AllowWorkflows = true
	event := mention()
	event.User = "UWORKFLOW"
	event.BotID = "BWORKFLOW"

	bot.handleMention(context.Background(), event, "Wf0BSM19MCDT")

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestBotMentionWithoutWorkflowIDIsForbidden(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newTestBot(t, store, api, &fakeRunner{})
	bot.config.AllowWorkflows = true
	event := mention()
	event.User = "UOTHERBOT"
	event.BotID = "BOTHER"

	bot.handleMention(context.Background(), event, "")

	if store.claimCalls != 0 {
		t.Fatalf("ClaimEvent called %d times, want 0", store.claimCalls)
	}
	if got := strings.Join(api.postTexts, "|"); got != "403 forbidden. @seiji に確認してください。" {
		t.Fatalf("posts = %q, want forbidden response", got)
	}
}

func TestUnauthorizedBareMentionIsForbidden(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newTestBot(t, store, api, &fakeRunner{})
	event := mention()
	event.User = "UDENIED"
	event.Text = "<@UBOT>"

	bot.HandleMention(context.Background(), event)

	if got := strings.Join(api.postTexts, "|"); got != "403 forbidden. @seiji に確認してください。" {
		t.Fatalf("posts = %q, want forbidden response", got)
	}
}

func TestWorkflowAuthorizationBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		workflowID string
		channel    string
	}{
		{name: "disabled", workflowID: "Wf0BSM19MCDT", channel: "C1"},
		{name: "wrong prefix", enabled: true, workflowID: "Fx0BSM19MCDT", channel: "C1"},
		{name: "invalid characters", enabled: true, workflowID: "WfBAD-id", channel: "C1"},
		{name: "disallowed channel", enabled: true, workflowID: "Wf0BSM19MCDT", channel: "C2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			bot := newTestBot(t, store, api, &fakeRunner{})
			bot.config.AllowWorkflows = test.enabled
			bot.allowedChannels = makeSet([]string{"C1"})
			event := mention()
			event.User = "UWORKFLOW"
			event.BotID = "BWORKFLOW"
			event.Channel = test.channel

			bot.handleMention(context.Background(), event, test.workflowID)

			if store.claimCalls != 0 {
				t.Fatalf("ClaimEvent called %d times, want 0", store.claimCalls)
			}
			if got := strings.Join(api.postTexts, "|"); got != "403 forbidden. @seiji に確認してください。" {
				t.Fatalf("posts = %q, want forbidden response", got)
			}
		})
	}
}

func TestForbiddenResponseMentionsConfiguredAdmin(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newTestBot(t, store, api, &fakeRunner{})
	bot.config.AdminUserID = "UADMIN"
	event := mention()
	event.User = "UDENIED"

	bot.HandleMention(context.Background(), event)

	if got := strings.Join(api.postTexts, "|"); got != "403 forbidden. <@UADMIN> に確認してください。" {
		t.Fatalf("posts = %q, want configured admin mention", got)
	}
}

func TestWorkflowIDFromPayload(t *testing.T) {
	payload := json.RawMessage(`{"event":{"type":"app_mention","workflow_id":"Wf0BSM19MCDT"}}`)
	if got := workflowIDFromPayload(payload); got != "Wf0BSM19MCDT" {
		t.Fatalf("workflowIDFromPayload() = %q, want Wf0BSM19MCDT", got)
	}
}

func TestDuplicateEventIsSkipped(t *testing.T) {
	store := &fakeStore{claim: false}
	runner := &fakeRunner{}
	api := &fakeSlack{}
	newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())
	if runner.calls != 0 || len(api.calls) != 0 {
		t.Fatalf("duplicate event caused runner=%d Slack calls=%d", runner.calls, len(api.calls))
	}
}

func TestDuplicateEventWithEnabledSubscriptionSkipsMarkerLookup(t *testing.T) {
	store := &fakeStore{claim: false}
	runner := &fakeRunner{}
	api := &fakeSlack{hasReaction: true}
	bot := newTestBot(t, store, api, runner)
	bot.config.ThreadSubscriptionReaction = "thread-subete"
	bot.config.ThreadSubscriptionTTL = 48 * time.Hour

	bot.HandleMention(context.Background(), mention())

	if api.reactionCalls != 0 || len(store.subscriptionCalls) != 0 {
		t.Fatalf("duplicate event caused reaction lookups=%d subscription saves=%d, want none", api.reactionCalls, len(store.subscriptionCalls))
	}
	if runner.calls != 0 {
		t.Fatalf("duplicate event caused runner=%d, want 0", runner.calls)
	}
}

func TestMarkedMentionStartsThreadSubscription(t *testing.T) {
	now := time.Date(2026, time.August, 27, 10, 30, 0, 0, time.UTC)
	tests := []struct {
		name       string
		event      *slackevents.AppMentionEvent
		wantThread string
	}{
		{name: "root mention", event: mention(), wantThread: "100.1"},
		{
			name: "thread parent",
			event: &slackevents.AppMentionEvent{
				User: "U1", Channel: "C1", TimeStamp: "200.2", ThreadTimeStamp: "100.1", Text: "<@UBOT> do it",
			},
			wantThread: "100.1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{hasReaction: true}
			runner := successfulPlanRunner()
			bot := newTestBot(t, store, api, runner)
			bot.config.ThreadSubscriptionReaction = "thread-subete"
			bot.config.ThreadSubscriptionTTL = 48 * time.Hour
			bot.now = func() time.Time { return now }

			bot.HandleMention(context.Background(), test.event)

			want := state.Subscription{StartedAt: now, ExpiresAt: now.Add(48 * time.Hour)}
			if got, ok := store.GetSubscription("C1:" + test.wantThread); !ok || got != want {
				t.Fatalf("subscription = (%+v, %v), want (%+v, true)", got, ok, want)
			}
			if api.reactionCalls != 1 || api.reactionChannel != "C1" || api.reactionTimestamp != test.wantThread || api.reactionName != "thread-subete" {
				t.Fatalf("reaction lookup = %d calls with (%q, %q, %q), want one parent lookup", api.reactionCalls, api.reactionChannel, api.reactionTimestamp, api.reactionName)
			}
			if runner.calls != 1 {
				t.Fatalf("runner calls = %d, want normal mention turn", runner.calls)
			}
		})
	}
}

func TestUnmarkedMentionDoesNotStartThreadSubscription(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := successfulPlanRunner()
	bot := newTestBot(t, store, api, runner)
	bot.config.ThreadSubscriptionReaction = "thread-subete"
	bot.config.ThreadSubscriptionTTL = 48 * time.Hour

	bot.HandleMention(context.Background(), mention())

	if len(store.subscriptionCalls) != 0 {
		t.Fatalf("subscription saves = %v, want none", store.subscriptionCalls)
	}
	if api.reactionCalls != 1 || runner.calls != 1 {
		t.Fatalf("reaction lookups = %d, runner calls = %d, want 1 each", api.reactionCalls, runner.calls)
	}
}

func TestRepeatedMarkedMentionRenewsThreadSubscription(t *testing.T) {
	firstNow := time.Date(2026, time.August, 27, 10, 30, 0, 0, time.UTC)
	secondNow := firstNow.Add(12 * time.Hour)
	currentNow := firstNow
	store := &fakeStore{claim: true}
	api := &fakeSlack{hasReaction: true}
	runner := &fakeRunner{responses: append(
		successfulPlanRunner().responses,
		successfulPlanRunner().responses...,
	)}
	bot := newTestBot(t, store, api, runner)
	bot.config.ThreadSubscriptionReaction = "thread-subete"
	bot.config.ThreadSubscriptionTTL = 48 * time.Hour
	bot.now = func() time.Time { return currentNow }

	bot.HandleMention(context.Background(), mention())
	currentNow = secondNow
	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{
		User: "U1", Channel: "C1", TimeStamp: "200.2", ThreadTimeStamp: "100.1", Text: "<@UBOT> again",
	})

	want := state.Subscription{StartedAt: secondNow, ExpiresAt: secondNow.Add(48 * time.Hour)}
	if got, ok := store.GetSubscription("C1:100.1"); !ok || got != want {
		t.Fatalf("renewed subscription = (%+v, %v), want (%+v, true)", got, ok, want)
	}
	if len(store.subscriptionCalls) != 2 || api.reactionCalls != 2 || runner.calls != 2 {
		t.Fatalf("subscription saves = %d, reaction lookups = %d, runner calls = %d, want 2 each", len(store.subscriptionCalls), api.reactionCalls, runner.calls)
	}
}

func TestSubscriptionStartFailuresDoNotBlockMentionTurn(t *testing.T) {
	tests := []struct {
		name     string
		api      *fakeSlack
		storeErr error
	}{
		{name: "reaction lookup", api: &fakeSlack{reactionErr: errors.New("Slack unavailable")}},
		{name: "subscription save", api: &fakeSlack{hasReaction: true}, storeErr: errors.New("disk unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true, subscriptionErr: test.storeErr}
			runner := successfulPlanRunner()
			bot := newTestBot(t, store, test.api, runner)
			bot.config.ThreadSubscriptionReaction = "thread-subete"
			bot.config.ThreadSubscriptionTTL = 48 * time.Hour

			bot.HandleMention(context.Background(), mention())

			if runner.calls != 1 || store.current != state.Done {
				t.Fatalf("runner calls = %d, state = %q, want normal completed mention turn", runner.calls, store.current)
			}
		})
	}
}

func TestSubscriptionMarkerLookupRetriesTransientSlackErrors(t *testing.T) {
	tests := []struct {
		name       string
		firstError error
		wantWait   time.Duration
	}{
		{
			name:       "rate limit honors Retry-After",
			firstError: &slack.RateLimitedError{RetryAfter: 3 * time.Second},
			wantWait:   3 * time.Second,
		},
		{
			name:       "server error retries immediately",
			firstError: slack.StatusCodeError{Code: http.StatusServiceUnavailable, Status: http.StatusText(http.StatusServiceUnavailable)},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{hasReaction: true, reactionErrs: []error{test.firstError, nil}}
			runner := successfulPlanRunner()
			bot := newTestBot(t, store, api, runner)
			bot.config.ThreadSubscriptionReaction = "thread-subete"
			bot.config.ThreadSubscriptionTTL = 48 * time.Hour
			var waits []time.Duration
			bot.sleep = func(_ context.Context, duration time.Duration) error {
				waits = append(waits, duration)
				return nil
			}

			bot.HandleMention(context.Background(), mention())

			if api.reactionCalls != 2 {
				t.Fatalf("reaction lookup calls = %d, want 2", api.reactionCalls)
			}
			if len(store.subscriptionCalls) != 1 {
				t.Fatalf("subscription saves = %d, want 1 after successful retry", len(store.subscriptionCalls))
			}
			var wantWaits []time.Duration
			if test.wantWait != 0 {
				wantWaits = []time.Duration{test.wantWait}
			}
			if !reflect.DeepEqual(waits, wantWaits) {
				t.Fatalf("retry waits = %v, want %v", waits, wantWaits)
			}
			if runner.calls != 1 || store.current != state.Done {
				t.Fatalf("runner calls = %d, state = %q, want completed mention", runner.calls, store.current)
			}
		})
	}
}

func TestSubscriptionMarkerLookupSecondFailureSkipsSubscriptionAndContinuesMention(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{hasReaction: true, reactionErrs: []error{
		slack.StatusCodeError{Code: http.StatusBadGateway, Status: http.StatusText(http.StatusBadGateway)},
		slack.StatusCodeError{Code: http.StatusServiceUnavailable, Status: http.StatusText(http.StatusServiceUnavailable)},
	}}
	runner := successfulPlanRunner()
	bot := newTestBot(t, store, api, runner)
	bot.config.ThreadSubscriptionReaction = "thread-subete"
	bot.config.ThreadSubscriptionTTL = 48 * time.Hour

	bot.HandleMention(context.Background(), mention())

	if api.reactionCalls != 2 {
		t.Fatalf("reaction lookup calls = %d, want one retry", api.reactionCalls)
	}
	if len(store.subscriptionCalls) != 0 {
		t.Fatalf("subscription saves = %v, want none after final lookup failure", store.subscriptionCalls)
	}
	if runner.calls != 1 || store.current != state.Done {
		t.Fatalf("runner calls = %d, state = %q, want fail-open completed mention", runner.calls, store.current)
	}
}

func TestSubscriptionMarkerLookupDoesNotRetryPermanent4xx(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{hasReaction: true, reactionErrs: []error{
		slack.StatusCodeError{Code: http.StatusBadRequest, Status: http.StatusText(http.StatusBadRequest)},
	}}
	runner := successfulPlanRunner()
	bot := newTestBot(t, store, api, runner)
	bot.config.ThreadSubscriptionReaction = "thread-subete"
	bot.config.ThreadSubscriptionTTL = 48 * time.Hour

	bot.HandleMention(context.Background(), mention())

	if api.reactionCalls != 1 {
		t.Fatalf("reaction lookup calls = %d, want no retry", api.reactionCalls)
	}
	if len(store.subscriptionCalls) != 0 {
		t.Fatalf("subscription saves = %v, want none after permanent lookup failure", store.subscriptionCalls)
	}
	if runner.calls != 1 || store.current != state.Done {
		t.Fatalf("runner calls = %d, state = %q, want fail-open completed mention", runner.calls, store.current)
	}
}

func TestSubscriptionMarkerLookupRequiresEnabledAuthorizedMention(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Bot)
		event     *slackevents.AppMentionEvent
		wantRuns  int
	}{
		{name: "disabled", event: mention(), wantRuns: 1},
		{
			name: "unauthorized",
			configure: func(bot *Bot) {
				bot.config.ThreadSubscriptionReaction = "thread-subete"
				bot.config.ThreadSubscriptionTTL = 48 * time.Hour
			},
			event: func() *slackevents.AppMentionEvent {
				event := mention()
				event.User = "UDENIED"
				return event
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{hasReaction: true}
			runner := successfulPlanRunner()
			bot := newTestBot(t, store, api, runner)
			if test.configure != nil {
				test.configure(bot)
			}

			bot.HandleMention(context.Background(), test.event)

			if api.reactionCalls != 0 || len(store.subscriptionCalls) != 0 {
				t.Fatalf("reaction lookups = %d, subscription saves = %d, want none", api.reactionCalls, len(store.subscriptionCalls))
			}
			if runner.calls != test.wantRuns {
				t.Fatalf("runner calls = %d, want %d", runner.calls, test.wantRuns)
			}
		})
	}
}

func TestWebAPIHasReactionUsesReactionsGet(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/reactions.get" {
			t.Errorf("request path = %q, want /reactions.get", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if r.Form.Get("channel") != "C1" || r.Form.Get("timestamp") != "100.1" {
			t.Errorf("request item = (%q, %q), want (C1, 100.1)", r.Form.Get("channel"), r.Form.Get("timestamp"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"type":"message","message":{"reactions":[{"name":"eyes"},{"name":"thread-subete"}]}}`)),
			Request:    r,
		}, nil
	})}

	api := &webAPI{client: slack.New("token", slack.OptionAPIURL("https://slack.test/"), slack.OptionHTTPClient(httpClient))}
	marked, err := api.HasReaction(context.Background(), "C1", "100.1", "thread-subete")
	if err != nil {
		t.Fatalf("HasReaction() error = %v, want nil", err)
	}
	if !marked {
		t.Fatal("HasReaction() = false, want true")
	}
	marked, err = api.HasReaction(context.Background(), "C1", "100.1", "missing")
	if err != nil {
		t.Fatalf("HasReaction() for absent reaction error = %v, want nil", err)
	}
	if marked {
		t.Fatal("HasReaction() for absent reaction = true, want false")
	}
}

func TestAllowedActiveMessageReplyRunsPlanningInSharedThreadSession(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := successfulPlanRunner()
	bot := newTestBot(t, store, api, runner)
	configureActiveSubscription(bot, store, now)

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "please continue"))

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if got := store.threadKeys; !reflect.DeepEqual(got, []string{"v3:C1:100.1"}) {
		t.Fatalf("thread keys = %v, want v3 channel/thread session", got)
	}
	if len(runner.prompts) != 1 {
		t.Fatalf("runner prompts = %d, want 1", len(runner.prompts))
	}
	for _, want := range []string{
		"<authenticated_slack_author_id>\nU2\n</authenticated_slack_author_id>",
		"<message_text>\nplease continue\n</message_text>",
		"## 方針",
		"## 作業指示",
	} {
		if !strings.Contains(runner.prompts[0], want) {
			t.Errorf("message plan prompt does not contain %q", want)
		}
	}
	if api.reactionCalls != 0 {
		t.Fatalf("message reply reaction lookups = %d, want 0", api.reactionCalls)
	}
	if len(store.subscriptionCalls) != 0 {
		t.Fatalf("message reply renewed subscription: %v", store.subscriptionCalls)
	}
	wantSubscription := state.Subscription{StartedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	if got, ok := store.GetSubscription("C1:100.1"); !ok || got != wantSubscription {
		t.Fatalf("subscription after reply = (%+v, %v), want unchanged (%+v, true)", got, ok, wantSubscription)
	}
}

func TestAllowedActiveHumanThreadBroadcastRunsPlanning(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	runner := successfulPlanRunner()
	bot := newTestBot(t, store, &fakeSlack{}, runner)
	configureActiveSubscription(bot, store, now)
	event := messageReply("U2", "200.2", "broadcast follow up")
	event.SubType = slack.MsgSubTypeThreadBroadcast

	bot.HandleMessage(context.Background(), event)

	if runner.calls != 1 {
		t.Fatalf("thread broadcast runner calls = %d, want 1", runner.calls)
	}
	if store.claimCalls != 1 {
		t.Fatalf("thread broadcast claim calls = %d, want 1", store.claimCalls)
	}
}

func TestMessageRepliesFromMultipleAuthorsShareOneSession(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	runner := &fakeRunner{responses: append(
		successfulPlanRunner().responses,
		successfulPlanRunner().responses...,
	)}
	bot := newTestBot(t, store, &fakeSlack{}, runner)
	configureActiveSubscription(bot, store, now)

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "first reply"))
	bot.HandleMessage(context.Background(), messageReply("U3", "300.3", "second reply"))

	if got := store.threadKeys; !reflect.DeepEqual(got, []string{"v3:C1:100.1", "v3:C1:100.1"}) {
		t.Fatalf("thread keys = %v, want one shared Slack thread key", got)
	}
	if got := runner.threadIDs; !reflect.DeepEqual(got, []string{"", "plan-thread"}) {
		t.Fatalf("runner thread IDs = %v, want second author to resume first author's session", got)
	}
	if len(runner.prompts) != 2 ||
		!strings.Contains(runner.prompts[0], "<authenticated_slack_author_id>\nU2\n") ||
		!strings.Contains(runner.prompts[1], "<authenticated_slack_author_id>\nU3\n") {
		t.Fatalf("prompts do not preserve authenticated authors: %q", runner.prompts)
	}
}

func TestMessageReplyCheapFiltersSkipProcessing(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Bot, *fakeStore)
		event     *slackevents.MessageEvent
	}{
		{
			name: "disallowed channel",
			configure: func(_ *Bot, store *fakeStore) {
				store.subscriptions["C2:100.1"] = state.Subscription{ExpiresAt: time.Now().Add(time.Hour)}
			},
			event: func() *slackevents.MessageEvent {
				event := messageReply("U2", "200.2", "ignored")
				event.Channel = "C2"
				return event
			}(),
		},
		{name: "root message", event: func() *slackevents.MessageEvent {
			event := messageReply("U2", "200.2", "ignored")
			event.ThreadTimeStamp = ""
			return event
		}()},
		{
			name: "bot subtype",
			event: func() *slackevents.MessageEvent {
				event := messageReply("UBOT2", "200.2", "ignored")
				event.SubType = "bot_message"
				event.BotID = "B2"
				return event
			}(),
		},
		{name: "edit", event: func() *slackevents.MessageEvent {
			event := messageReply("U2", "200.2", "edited")
			event.SubType = "message_changed"
			event.Message = &slack.Msg{Edited: &slack.Edited{User: "U2", Timestamp: "300.3"}}
			return event
		}()},
		{name: "delete", event: func() *slackevents.MessageEvent {
			event := messageReply("U2", "200.2", "deleted")
			event.SubType = "message_deleted"
			event.DeletedTimeStamp = "200.2"
			return event
		}()},
		{name: "contains ebi mention", event: messageReply("U2", "200.2", "please <@UBOT> continue")},
		{name: "private channel", event: func() *slackevents.MessageEvent {
			event := messageReply("U2", "200.2", "ignored")
			event.ChannelType = "group"
			return event
		}()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true, subscriptions: map[string]state.Subscription{
				"C1:100.1": {ExpiresAt: time.Now().Add(time.Hour)},
			}}
			api := &fakeSlack{hasReaction: true}
			runner := &fakeRunner{}
			bot := newTestBot(t, store, api, runner)
			bot.allowedChannels = makeSet([]string{"C1"})
			if test.configure != nil {
				test.configure(bot, store)
			}

			bot.HandleMessage(context.Background(), test.event)

			if store.claimCalls != 0 || runner.calls != 0 || len(api.calls) != 0 || api.threadCalls != 0 || api.reactionCalls != 0 {
				t.Fatalf("ignored message caused claim=%d runner=%d Slack calls=%d thread reads=%d reaction lookups=%d",
					store.claimCalls, runner.calls, len(api.calls), api.threadCalls, api.reactionCalls)
			}
		})
	}
}

func TestMessageReplyWithoutSubscriptionIsIgnored(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{hasReaction: true}
	runner := &fakeRunner{}
	bot := newTestBot(t, store, api, runner)
	bot.allowedChannels = makeSet([]string{"C1"})

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "not subscribed"))

	if store.claimCalls != 0 || runner.calls != 0 || len(api.calls) != 0 || api.threadCalls != 0 || api.reactionCalls != 0 {
		t.Fatalf("unsubscribed message caused claim=%d runner=%d Slack calls=%d thread reads=%d reaction lookups=%d",
			store.claimCalls, runner.calls, len(api.calls), api.threadCalls, api.reactionCalls)
	}
}

func TestExpiredMessageReplySubscriptionIsDeletedAndIgnored(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true, subscriptions: map[string]state.Subscription{
		"C1:100.1": {StartedAt: now.Add(-2 * time.Hour), ExpiresAt: now},
	}}
	api := &fakeSlack{hasReaction: true}
	runner := &fakeRunner{}
	bot := newTestBot(t, store, api, runner)
	bot.allowedChannels = makeSet([]string{"C1"})
	bot.now = func() time.Time { return now }

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "expired"))

	if !reflect.DeepEqual(store.subscriptionDeletes, []string{"C1:100.1"}) {
		t.Fatalf("subscription deletes = %v, want expired thread", store.subscriptionDeletes)
	}
	if _, ok := store.GetSubscription("C1:100.1"); ok {
		t.Fatal("expired subscription still exists")
	}
	if store.claimCalls != 0 || runner.calls != 0 || len(api.calls) != 0 || api.threadCalls != 0 || api.reactionCalls != 0 {
		t.Fatalf("expired message caused claim=%d runner=%d Slack calls=%d thread reads=%d reaction lookups=%d",
			store.claimCalls, runner.calls, len(api.calls), api.threadCalls, api.reactionCalls)
	}
}

type renewalInterleavingStore struct {
	*state.Store
	expiryCheckStarted chan struct{}
	continueCheck      chan struct{}
}

func (s *renewalInterleavingStore) DeleteSubscriptionIfExpired(key string, now time.Time) (bool, error) {
	close(s.expiryCheckStarted)
	<-s.continueCheck
	return s.Store.DeleteSubscriptionIfExpired(key, now)
}

func TestMessageReplyExpiryCheckDoesNotDeleteConcurrentRenewal(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	stateStore, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}
	const subscriptionKey = "C1:100.1"
	if err := stateStore.SetSubscription(subscriptionKey, now.Add(-2*time.Hour), now); err != nil {
		t.Fatalf("SetSubscription() expired setup error = %v, want nil", err)
	}
	store := &renewalInterleavingStore{
		Store:              stateStore,
		expiryCheckStarted: make(chan struct{}),
		continueCheck:      make(chan struct{}),
	}
	defer func() {
		select {
		case <-store.continueCheck:
		default:
			close(store.continueCheck)
		}
	}()
	runner := successfulPlanRunner()
	bot := New(&fakeSlack{}, store, runner, Config{
		AllowedChannelIDs: []string{"C1"},
		WorkspaceDir:      "/repo/workspace",
		MemoryDir:         filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:      time.Minute,
		BotUserID:         "UBOT",
	}, nil)
	bot.now = func() time.Time { return now }

	done := make(chan struct{})
	go func() {
		defer close(done)
		bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "renewed reply"))
	}()
	select {
	case <-store.expiryCheckStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("message handler did not enter atomic expiry check")
	}
	renewed := state.Subscription{StartedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := stateStore.SetSubscription(subscriptionKey, renewed.StartedAt, renewed.ExpiresAt); err != nil {
		t.Fatalf("SetSubscription() renewal error = %v, want nil", err)
	}
	close(store.continueCheck)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("message handler did not finish after concurrent renewal")
	}

	if got, ok := stateStore.GetSubscription(subscriptionKey); !ok || got != renewed {
		t.Fatalf("subscription after interleaved renewal = (%+v, %v), want (%+v, true)", got, ok, renewed)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls after renewal = %d, want 1 for now-active subscription", runner.calls)
	}
}

func TestDuplicateSubscribedMessageReplyIsSkippedWithoutSlackCalls(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: false}
	api := &fakeSlack{hasReaction: true}
	runner := &fakeRunner{}
	bot := newTestBot(t, store, api, runner)
	configureActiveSubscription(bot, store, now)

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "duplicate"))

	if store.claimCalls != 1 || runner.calls != 0 || len(api.calls) != 0 || api.threadCalls != 0 || api.reactionCalls != 0 {
		t.Fatalf("duplicate message caused claim=%d runner=%d Slack calls=%d thread reads=%d reaction lookups=%d",
			store.claimCalls, runner.calls, len(api.calls), api.threadCalls, api.reactionCalls)
	}
}

func successfulPlanRunner() *fakeRunner {
	return &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"## 方針\nDone.\n## 作業指示\nNONE"},
	}}}}
}

func TestFailClosedTransitionsToDoneWithCheckmark(t *testing.T) {
	tests := []struct {
		name   string
		result *codex.TurnResult
		want   string
	}{
		{
			name:   "parse failure",
			result: &codex.TurnResult{Completed: true, Messages: []string{"unstructured plan"}},
			want:   "unstructured plan",
		},
		{
			name:   "empty messages",
			result: &codex.TurnResult{Completed: true},
			want:   failClosedMessage,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			runner := &fakeRunner{responses: []runnerResponse{{result: test.result}}}
			newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())
			if store.current != state.Done {
				t.Fatalf("state = %q, want done", store.current)
			}
			assertTransitions(t, store.transitions, [][2]state.State{
				{state.Received, state.Planning},
				{state.Planning, state.Done},
			})
			if !strings.Contains(strings.Join(api.postTexts, ""), test.want) {
				t.Errorf("posts %q do not contain %q", api.postTexts, test.want)
			}
			assertFinalReactionOrder(t, api.calls, "white_check_mark")
		})
	}
}

func TestPlanSlackOutputsSanitizeForbiddenUserMemory(t *testing.T) {
	tests := []struct {
		name     string
		planText string
		want     string
	}{
		{
			name:     "successful NONE policy",
			planText: "## 方針\nNo work needed.\n## 作業指示\nNONE\n## ユーザーメモリ追記\nprivate plan memory",
			want:     "No work needed.",
		},
		{
			name:     "parse fail-closed",
			planText: "unstructured plan ``` ## ユーザーメモリ追記 private plan memory",
			want:     "unstructured plan ```",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			runner := &fakeRunner{responses: []runnerResponse{
				{result: &codex.TurnResult{
					Completed: true,
					Messages:  []string{test.planText},
				}},
				{result: &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}},
			}}

			newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())

			posts := strings.Join(api.postTexts, "|")
			if !strings.Contains(posts, test.want) {
				t.Fatalf("posts = %q, want preserved output %q", posts, test.want)
			}
			for _, forbidden := range []string{"## ユーザーメモリ追記", "private plan memory"} {
				if strings.Contains(posts, forbidden) {
					t.Fatalf("posts = %q, must not expose %q", posts, forbidden)
				}
			}
		})
	}
}

func TestNormalPlanSanitizesForbiddenUserMemoryBeforeWork(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{
			"## 方針\nImplement safely.\n## 作業指示\nMake the change. ## ユーザーメモリ追記 private plan memory",
		}}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}},
	}}

	newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())

	if runner.calls != 2 {
		t.Fatalf("runner calls = %d, want plan and work", runner.calls)
	}
	workPrompt := runner.prompts[1]
	if !strings.Contains(workPrompt, "Make the change.") {
		t.Fatalf("work prompt = %q, want preserved instruction", workPrompt)
	}
	for _, forbidden := range []string{"## ユーザーメモリ追記", "private plan memory"} {
		if strings.Contains(workPrompt, forbidden) || strings.Contains(strings.Join(api.postTexts, "|"), forbidden) {
			t.Fatalf("normal plan leaked %q into work or Slack output", forbidden)
		}
	}
}

func TestWorkSlackOutputSanitizesForbiddenUserMemoryAnywhere(t *testing.T) {
	tests := []struct {
		name     string
		workText string
	}{
		{
			name:     "inside fence",
			workText: "Work completed.\n```text\n## ユーザーメモリ追記\nprivate work memory\n```",
		},
		{
			name:     "inline",
			workText: "Work completed. ## ユーザーメモリ追記 private work memory",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			runner := &fakeRunner{responses: []runnerResponse{
				{result: &codex.TurnResult{Completed: true, Messages: []string{
					"## 方針\nDo work.\n## 作業指示\nMake a change.",
				}}},
				{result: &codex.TurnResult{Completed: true, Messages: []string{test.workText}}},
			}}

			newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())

			posts := strings.Join(api.postTexts, "|")
			if !strings.Contains(posts, "Work completed.") {
				t.Fatalf("posts = %q, want preserved normal work output", posts)
			}
			for _, forbidden := range []string{"## ユーザーメモリ追記", "private work memory"} {
				if strings.Contains(posts, forbidden) {
					t.Fatalf("posts = %q, must not expose %q", posts, forbidden)
				}
			}
		})
	}
}

func TestNoneTransitionsToDoneWithCheckmark(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"## 方針\nNo work needed.\n## 作業指示\nNONE"},
	}}}}
	newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())
	assertTransitions(t, store.transitions, [][2]state.State{
		{state.Received, state.Planning},
		{state.Planning, state.PlanPosted},
		{state.PlanPosted, state.Done},
	})
	assertStatusSequence(t, api.calls, []string{planningStatus, ""})
	assertFinalReactionOrder(t, api.calls, "white_check_mark")
}

func TestPlanWorkDone(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{
			Completed: true,
			Messages:  []string{"## 方針\nImplement safely.\n## 作業指示\nChange the file."},
		}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}},
	}}
	runner.onRun = func(call int) {
		if call == 2 && len(api.postTexts) != 0 {
			t.Fatalf("posts before work started = %q, want none", api.postTexts)
		}
	}
	newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())
	assertTransitions(t, store.transitions, [][2]state.State{
		{state.Received, state.Planning},
		{state.Planning, state.PlanPosted},
		{state.PlanPosted, state.Working},
		{state.Working, state.Done},
	})
	if runner.calls != 2 {
		t.Fatalf("runner calls = %d, want 2", runner.calls)
	}
	if got := strings.Join(api.postTexts, "|"); got != "Work completed." {
		t.Fatalf("posts = %q", got)
	}
	if runner.roots[0] != nil || runner.roots[1] != nil {
		t.Fatalf("writable roots = %v, want no writable roots for either turn", runner.roots)
	}
	assertStatusSequence(t, api.calls, []string{planningStatus, workingStatus, ""})
	assertFinalReactionOrder(t, api.calls, "white_check_mark")
}

func TestPlanSessionsAreSharedBySlackThread(t *testing.T) {
	store := &fakeStore{claim: true}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{"## 方針\nDone.\n## 作業指示\nNONE"}}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{"## 方針\nDone.\n## 作業指示\nNONE"}}},
	}}
	bot := newTestBot(t, store, &fakeSlack{}, runner)
	bot.allowedUsers["U2"] = struct{}{}
	bot.HandleMention(context.Background(), mention())
	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{
		User: "U2", Channel: "C1", TimeStamp: "200.2", ThreadTimeStamp: "100.1", Text: "<@UBOT> do it",
	})

	want := []string{"v3:C1:100.1", "v3:C1:100.1"}
	if !reflect.DeepEqual(store.threadKeys, want) {
		t.Fatalf("thread keys = %v, want %v", store.threadKeys, want)
	}
	if len(store.threadIDs) != 1 {
		t.Fatalf("stored thread keys = %v, want one shared session", store.threadIDs)
	}
	if got := runner.threadIDs; !reflect.DeepEqual(got, []string{"", "plan-thread"}) {
		t.Fatalf("runner thread IDs = %v, want second user to resume shared session", got)
	}
}

func TestFirstThreadMentionReceivesEarlierSlackMessages(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{threadMessages: []ThreadMessage{
		{AuthorID: "U2", Timestamp: "100.1", Text: "root context"},
		{AuthorID: "U3", Timestamp: "200.2", Text: "important detail"},
		{AuthorID: "U1", Timestamp: "300.3", Text: "<@UBOT> current request"},
	}}
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"## 方針\nDone.\n## 作業指示\nNONE"},
	}}}}
	event := mention()
	event.TimeStamp = "300.3"
	event.ThreadTimeStamp = "100.1"
	event.Text = "<@UBOT> current request"

	newTestBot(t, store, api, runner).HandleMention(context.Background(), event)

	if api.threadCalls != 1 || api.threadLatest != "300.3" {
		t.Fatalf("thread fetch calls = %d, latest = %q, want one call through current mention", api.threadCalls, api.threadLatest)
	}
	if len(runner.prompts) != 1 {
		t.Fatalf("runner prompts = %d, want 1", len(runner.prompts))
	}
	got := runner.prompts[0]
	for _, want := range []string{"<slack_thread>", "root context", "important detail", "<user_message>\ncurrent request"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan prompt does not contain %q", want)
		}
	}
	if strings.Count(got, "<@UBOT> current request") != 0 {
		t.Error("current mention was duplicated into Slack thread context")
	}
}

func TestExistingPlanSessionSkipsSlackThreadFetch(t *testing.T) {
	store := &fakeStore{
		claim:     true,
		threadIDs: map[string]string{"v3:C1:100.1": "existing-thread"},
	}
	api := &fakeSlack{threadErr: errors.New("must not be called")}
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"## 方針\nDone.\n## 作業指示\nNONE"},
	}}}}
	event := mention()
	event.TimeStamp = "200.2"
	event.ThreadTimeStamp = "100.1"

	newTestBot(t, store, api, runner).HandleMention(context.Background(), event)

	if api.threadCalls != 0 {
		t.Fatalf("thread fetch calls = %d, want 0", api.threadCalls)
	}
	if len(runner.threadIDs) != 1 || runner.threadIDs[0] != "existing-thread" {
		t.Fatalf("runner thread IDs = %v, want existing session", runner.threadIDs)
	}
	if strings.Contains(runner.prompts[0], "<slack_thread>") {
		t.Error("existing session prompt unexpectedly contains Slack thread context")
	}
}

func TestThreadFetchFailureDoesNotStartCodex(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{threadErr: errors.New("Slack unavailable")}
	runner := &fakeRunner{}
	event := mention()
	event.TimeStamp = "200.2"
	event.ThreadTimeStamp = "100.1"

	newTestBot(t, store, api, runner).HandleMention(context.Background(), event)

	if runner.calls != 0 {
		t.Fatalf("runner calls = %d, want 0", runner.calls)
	}
	if store.current != state.Failed {
		t.Fatalf("state = %q, want failed", store.current)
	}
	if !strings.Contains(strings.Join(api.postTexts, "|"), threadFailureMessage) {
		t.Fatalf("posts = %q, want thread failure message", api.postTexts)
	}
	assertFinalReactionOrder(t, api.calls, "x")
}

func TestFormatThreadContextCapsRunesAndKeepsEnds(t *testing.T) {
	got := formatThreadContext([]ThreadMessage{{
		AuthorID: "U1", Timestamp: "100.1", Text: strings.Repeat("前", maxThreadContextRunes) + "末尾",
	}}, "200.2")
	if len([]rune(got)) != maxThreadContextRunes {
		t.Fatalf("context runes = %d, want %d", len([]rune(got)), maxThreadContextRunes)
	}
	for _, want := range []string{"[100.1 / U1]", "中間を省略", "末尾"} {
		if !strings.Contains(got, want) {
			t.Errorf("thread context does not contain %q", want)
		}
	}
}

func TestKeepStatusRefreshesUntilStopped(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{}, api, &fakeRunner{})
	sleepCalls := make(chan time.Duration)
	releaseSleep := make(chan struct{})
	bot.sleep = func(ctx context.Context, duration time.Duration) error {
		select {
		case sleepCalls <- duration:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-releaseSleep:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	stop := bot.keepStatus(context.Background(), "C1", "100.1", workingStatus)
	if got := receiveDuration(t, sleepCalls); got != statusRefreshDelay {
		t.Fatalf("first status delay = %v, want %v", got, statusRefreshDelay)
	}
	releaseSleep <- struct{}{}
	if got := receiveDuration(t, sleepCalls); got != statusRefreshDelay {
		t.Fatalf("second status delay = %v, want %v", got, statusRefreshDelay)
	}
	stop()

	assertStatusSequence(t, api.calls, []string{workingStatus, workingStatus})
}

func TestWorkTurnReceivesWritableRootsAndPlanTurnDoesNot(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{
			Completed: true,
			Messages:  []string{"## 方針\nDo work.\n## 作業指示\nMake a change."},
		}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}},
	}}
	roots := []string{"/extra/one", "/extra/two"}
	playbooksDir := t.TempDir()
	bot := New(api, store, runner, Config{
		AllowedUserIDs: []string{"U1"},
		WorkspaceDir:   "/repo/workspace",
		MemoryDir:      filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:   time.Minute,
		BotUserID:      "UBOT",
		WritableRoots:  roots,
		PlaybooksDir:   playbooksDir,
	}, nil)
	// New must copy the slice, so later mutation by the caller is not observed.
	roots[0] = "/mutated"

	bot.HandleMention(context.Background(), mention())
	if runner.calls != 2 {
		t.Fatalf("runner calls = %d, want 2", runner.calls)
	}
	if runner.roots[0] != nil {
		t.Errorf("plan turn writable roots = %v, want nil", runner.roots[0])
	}
	if got := runner.cwds[0]; got != "/repo/workspace" {
		t.Errorf("plan turn cwd = %q, want isolated workspace", got)
	}
	if got := runner.sandboxes[0]; got != "read-only-network" {
		t.Errorf("plan turn sandbox = %q, want read-only-network", got)
	}
	want := []string{"/extra/one", "/extra/two", playbooksDir}
	if !reflect.DeepEqual(runner.roots[1], want) {
		t.Errorf("work turn writable roots = %v, want %v", runner.roots[1], want)
	}
	if got := runner.cwds[1]; got != "/repo/workspace" {
		t.Errorf("work turn cwd = %q, want isolated workspace", got)
	}
}

func TestPlanPromptInjectsMemoryContent(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"## 方針\nNo work needed.\n## 作業指示\nNONE"},
	}}}}
	bot := newTestBot(t, store, api, runner)
	entries := []struct {
		scope memory.Scope
		text  string
	}{
		{scope: memory.ScopeGlobal, text: "全体の学び"},
		{scope: memory.ScopeChannel, text: "チャンネルの慣習"},
	}
	for _, entry := range entries {
		if _, err := memory.AppendScoped(bot.config.MemoryDir, entry.scope, "C1", entry.text); err != nil {
			t.Fatalf("append %s memory: %v", entry.scope, err)
		}
	}
	bot.HandleMention(context.Background(), mention())
	if len(runner.prompts) != 1 {
		t.Fatalf("runner prompts = %d, want 1", len(runner.prompts))
	}
	prompt := runner.prompts[0]
	for _, want := range []string{
		"<global_memory>", "全体の学び", "</global_memory>",
		"<channel_memory>", "チャンネルの慣習", "</channel_memory>",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("plan prompt does not contain %q", want)
		}
	}
	if strings.Contains(prompt, "user_memory") {
		t.Error("plan prompt must omit user memory")
	}
	if strings.Contains(prompt, "MEMORY.md") {
		t.Error("plan prompt should inject memory content, not the file path")
	}
}

func TestWorkMemoryAppendIsWrittenByBot(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{
			Completed: true,
			Messages:  []string{"## 方針\nDo work.\n## 作業指示\nMake a change."},
		}},
		{result: &codex.TurnResult{
			Completed: true,
			Messages: []string{strings.Join([]string{
				"Work completed.",
				"## 全体メモリ追記", "ビルドは mise run build を使う",
				"## チャンネルメモリ追記", "動作確認用チャンネル",
			}, "\n")},
		}},
	}}
	bot := newTestBot(t, store, api, runner)
	bot.HandleMention(context.Background(), mention())
	if store.current != state.Done {
		t.Fatalf("state = %q, want done", store.current)
	}
	files := []struct {
		path string
		want string
	}{
		{path: filepath.Join(bot.config.MemoryDir, "MEMORY.md"), want: "ビルドは mise run build を使う"},
		{path: filepath.Join(bot.config.MemoryDir, "channels", "C1", "MEMORY.md"), want: "動作確認用チャンネル"},
	}
	for _, file := range files {
		data, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatalf("read memory %s: %v", file.path, err)
		}
		if !strings.Contains(string(data), file.want) {
			t.Fatalf("memory file %s = %q, want %q", file.path, data, file.want)
		}
	}
	posts := strings.Join(api.postTexts, "|")
	if !strings.Contains(posts, "Work completed.") || !strings.Contains(posts, "全体・チャンネルメモリを更新しました") {
		t.Fatalf("posts = %q, want work result and memory notification", posts)
	}
	for _, private := range []string{"ビルドは mise run build を使う", "動作確認用チャンネル"} {
		if strings.Contains(posts, private) {
			t.Fatalf("posts = %q, should not expose memory content %q", posts, private)
		}
	}
}

func TestUserMemoryOutputIsNotAccepted(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{
			Completed: true,
			Messages:  []string{"## 方針\nDo work.\n## 作業指示\nMake a change."},
		}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{
			"Work completed.\n## ユーザーメモリ追記\nprivate memory",
		}}},
	}}
	bot := newTestBot(t, store, api, runner)
	bot.HandleMention(context.Background(), mention())

	posts := strings.Join(api.postTexts, "|")
	if !strings.Contains(posts, "Work completed.") || strings.Contains(posts, "メモリを更新しました") || strings.Contains(posts, "## ユーザーメモリ追記") || strings.Contains(posts, "private memory") {
		t.Fatalf("posts = %q, want work result", posts)
	}
	for _, path := range []string{
		filepath.Join(bot.config.MemoryDir, "MEMORY.md"),
		filepath.Join(bot.config.MemoryDir, "users", "U1", "MEMORY.md"),
		filepath.Join(bot.config.MemoryDir, "channels", "C1", "MEMORY.md"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("user memory output should not be written to %s, stat error = %v", path, err)
		}
	}
}

func TestWorkFailureIsInterrupted(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{
			Completed: true,
			Messages:  []string{"## 方針\nDo work.\n## 作業指示\nMake a change."},
		}},
		{result: &codex.TurnResult{Completed: false, Err: "failed"}},
	}}
	newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())
	if store.current != state.Interrupted {
		t.Fatalf("state = %q, want interrupted", store.current)
	}
	assertFinalReactionOrder(t, api.calls, "x")
}

// blockingInstruction marks the work prompt that gatedRunner holds open.
const blockingInstruction = "BLOCKWORK"

// looseStore accepts any transition so concurrent events do not interfere.
type looseStore struct {
	mu      sync.Mutex
	threads map[string]string
}

func (s *looseStore) ClaimEvent(string) (bool, error) { return true, nil }

func (s *looseStore) Transition(string, state.State, state.State) error { return nil }

func (s *looseStore) GetThread(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.threads[key]
	return id, ok
}

func (s *looseStore) SetThread(key, threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[key] = threadID
	return nil
}

func (s *looseStore) GetSubscription(string) (state.Subscription, bool) {
	return state.Subscription{}, false
}

func (s *looseStore) SetSubscription(string, time.Time, time.Time) error { return nil }

func (s *looseStore) DeleteSubscriptionIfExpired(string, time.Time) (bool, error) {
	return false, nil
}

// gatedRunner holds the work turn open until release is closed so a test can
// observe what other turns may run concurrently.
type gatedRunner struct {
	workStarted chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (r *gatedRunner) Run(_ context.Context, _, _, _ string, _ []string, prompt string, callback func(string) error) (*codex.TurnResult, error) {
	if callback != nil {
		if err := callback("plan-thread"); err != nil {
			return nil, err
		}
	}
	if strings.Contains(prompt, blockingInstruction) {
		close(r.workStarted)
		<-r.release
		return &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}, nil
	}
	if strings.Contains(prompt, "second") {
		return &codex.TurnResult{
			Completed: true,
			Messages:  []string{"## 方針\nNothing to do.\n## 作業指示\nNONE"},
		}, nil
	}
	return &codex.TurnResult{
		Completed: true,
		Messages:  []string{"## 方針\nWork on it.\n## 作業指示\n" + blockingInstruction},
	}, nil
}

func (r *gatedRunner) releaseWork() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func TestPlanTurnRunsWhileWorkTurnIsBlocked(t *testing.T) {
	runner := &gatedRunner{
		workStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}
	bot := New(&fakeSlack{}, &looseStore{threads: map[string]string{}}, runner, Config{
		AllowedUserIDs: []string{"U1"},
		WorkspaceDir:   "/repo/workspace",
		MemoryDir:      filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:   time.Minute,
		BotUserID:      "UBOT",
	}, nil)

	ctx := context.Background()
	workDone := make(chan struct{})
	go func() {
		defer close(workDone)
		bot.HandleMention(ctx, mention())
	}()
	defer func() {
		runner.releaseWork()
		<-workDone
	}()

	select {
	case <-runner.workStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("work turn did not start")
	}

	planDone := make(chan struct{})
	go func() {
		defer close(planDone)
		bot.HandleMention(ctx, &slackevents.AppMentionEvent{
			User: "U1", Channel: "C1", TimeStamp: "200.2", Text: "<@UBOT> second",
		})
	}()
	select {
	case <-planDone:
	case <-time.After(5 * time.Second):
		t.Fatal("plan turn did not finish while a work turn was blocked")
	}
}

func TestSplitMessageUsesRuneBoundaries(t *testing.T) {
	text := strings.Repeat("a", 3899) + "日" + "本"
	chunks := splitMessage(text, maxSlackMessageRunes)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if len([]rune(chunks[0])) != 3900 || chunks[0][len(chunks[0])-3:] != "日" {
		t.Fatalf("first chunk has %d runes and suffix %q", len([]rune(chunks[0])), chunks[0][len(chunks[0])-3:])
	}
	if chunks[1] != "本" || strings.Join(chunks, "") != text {
		t.Fatalf("split did not preserve UTF-8 text")
	}
}

func TestPostSplitsLongMessage(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{}, api, &fakeRunner{})
	text := strings.Repeat("界", 3901)
	if err := bot.post(context.Background(), "C1", "1", text); err != nil {
		t.Fatalf("post() error = %v", err)
	}
	if len(api.postTexts) != 2 {
		t.Fatalf("post attempts = %d, want 2", len(api.postTexts))
	}
	if len([]rune(api.postTexts[0])) != 3900 || api.postTexts[1] != "界" {
		t.Fatalf("post chunks have rune lengths %d and %d", len([]rune(api.postTexts[0])), len([]rune(api.postTexts[1])))
	}
}

func TestPostRetriesRateLimitAfterDelay(t *testing.T) {
	api := &fakeSlack{postErrs: []error{&slack.RateLimitedError{RetryAfter: 3 * time.Second}, nil}}
	bot := newTestBot(t, &fakeStore{}, api, &fakeRunner{})
	var waited time.Duration
	bot.sleep = func(_ context.Context, duration time.Duration) error {
		waited = duration
		return nil
	}
	if err := bot.post(context.Background(), "C1", "1", "hello"); err != nil {
		t.Fatalf("post() error = %v", err)
	}
	if waited != 3*time.Second {
		t.Fatalf("waited = %v, want 3s", waited)
	}
	if len(api.postTexts) != 2 || api.postTexts[0] != "hello" || api.postTexts[1] != "hello" {
		t.Fatalf("post attempts = %q", api.postTexts)
	}
}

func assertTransitions(t *testing.T, got, want [][2]state.State) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transition %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func assertFinalReactionOrder(t *testing.T, calls []slackCall, final string) {
	t.Helper()
	addIndex, removeIndex := -1, -1
	for i, call := range calls {
		if call.kind == "add:"+final {
			addIndex = i
		}
		if call.kind == "remove:eyes" {
			removeIndex = i
		}
	}
	if addIndex < 0 || removeIndex < 0 || addIndex >= removeIndex {
		t.Fatalf("reaction calls = %v, want add:%s before remove:eyes", calls, final)
	}
}

func assertStatusSequence(t *testing.T, calls []slackCall, want []string) {
	t.Helper()
	var got []string
	for _, call := range calls {
		if call.kind == "status" {
			got = append(got, call.text)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status sequence = %q, want %q", got, want)
	}
}

func receiveDuration(t *testing.T, calls <-chan time.Duration) time.Duration {
	t.Helper()
	select {
	case duration := <-calls:
		return duration
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for status refresh")
		return 0
	}
}

func TestPlaybooksReloadBetweenRequests(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newTestBot(t, store, api, runner)
	bot.config.PlaybooksDir = t.TempDir()
	path := filepath.Join(bot.config.PlaybooksDir, "example.md")
	for _, description := range []string{"first-version", "updated-version", ""} {
		if description == "" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("---\nname: example\ndescription: "+description+"\n---\nBody"), 0600); err != nil {
			t.Fatal(err)
		}
		runner.responses = append(runner.responses, runnerResponse{result: &codex.TurnResult{
			Completed: true, Messages: []string{"## 方針\nDone.\n## 作業指示\nNONE"},
		}})
		bot.HandleMention(context.Background(), mention())
		got := runner.prompts[len(runner.prompts)-1]
		if description == "" {
			if strings.Contains(got, "name: example") {
				t.Fatal("removed playbook remains in prompt")
			}
		} else if !strings.Contains(got, description) {
			t.Fatalf("prompt missing current description %q", description)
		}
		if description != "first-version" && strings.Contains(got, "first-version") {
			t.Fatal("stale playbook remains in prompt")
		}
	}
}
