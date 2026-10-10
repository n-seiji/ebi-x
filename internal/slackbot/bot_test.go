package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/memory"
	"github.com/n-seiji/ebi-x/internal/slackfmt"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/n-seiji/ebi-x/internal/workspace"
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
	followUps           map[string]state.FollowUp
}

func (s *fakeStore) GetFollowUp(threadKey string) (state.FollowUp, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	followUp, ok := s.followUps[threadKey]
	return followUp, ok
}

func (s *fakeStore) SetFollowUp(threadKey string, followUp state.FollowUp) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.followUps == nil {
		s.followUps = make(map[string]state.FollowUp)
	}
	s.followUps[threadKey] = followUp
	return nil
}

func (s *fakeStore) DeleteFollowUp(threadKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.followUps[threadKey]
	delete(s.followUps, threadKey)
	return ok, nil
}

func (s *fakeStore) TakeDueFollowUps(now time.Time) ([]state.FollowUp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []state.FollowUp
	for key, followUp := range s.followUps {
		if !followUp.DueAt.After(now) {
			due = append(due, followUp)
			delete(s.followUps, key)
		}
	}
	return due, nil
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
	uploads           []fakeUpload
	uploadErrs        []error
	posts             []fakePost
}

type fakePost struct {
	channel  string
	threadTS string
	text     string
}

type fakeUpload struct {
	channel  string
	threadTS string
	filename string
	content  string
}

func (s *fakeSlack) UploadFile(_ context.Context, channel, threadTS, filename string, size int64, content io.Reader) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return errors.New("size does not match content")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slackCall{kind: "upload", text: filename})
	if len(s.uploadErrs) > 0 {
		err := s.uploadErrs[0]
		s.uploadErrs = s.uploadErrs[1:]
		if err != nil {
			return err
		}
	}
	s.uploads = append(s.uploads, fakeUpload{channel: channel, threadTS: threadTS, filename: filename, content: string(data)})
	return nil
}

func (s *fakeSlack) PostMessage(_ context.Context, channel, threadTS, text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slackCall{kind: "post", text: text})
	s.postTexts = append(s.postTexts, text)
	s.posts = append(s.posts, fakePost{channel: channel, threadTS: threadTS, text: text})
	var err error
	if len(s.postErrs) > 0 {
		err = s.postErrs[0]
		s.postErrs = s.postErrs[1:]
	}
	if err == nil && threadTS == "" {
		// Top-level posts get distinct timestamps so threads can be told apart.
		return fmt.Sprintf("top-%d", len(s.posts)), nil
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
	denied    [][]string
	prompts   []string
	onRun     func(call int)
}

func (r *fakeRunner) Run(_ context.Context, threadID, sandbox, cwd string, roots, denied []string, prompt string, callback func(string) error, onActivity func(codex.Activity)) (*codex.TurnResult, error) {
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
	r.denied = append(r.denied, denied)
	r.prompts = append(r.prompts, prompt)
	if callback != nil {
		if err := callback("codex-thread"); err != nil {
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
		AllowedUserIDs:        []string{"U1"},
		AllowedChannelIDs:     []string{"C1"},
		SharedWriteChannelIDs: []string{"C1"},
		WorkspaceDir:          "/repo/workspace",
		MemoryDir:             filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:          time.Minute,
		BotUserID:             "UBOT",
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
	bot.allowedUsers = makeSet([]string{"U1", "U2", "U3"})
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
		Messages:  []string{"Done."},
	}}}}
	bot := newTestBot(t, store, api, runner)
	bot.config.AllowWorkflows = true
	bot.allowedWorkflows = makeSet([]string{"Wf0BSM19MCDT"})
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
		{name: "workflow not in allowlist", enabled: true, workflowID: "Wf0OTHER123", channel: "C1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			bot := newTestBot(t, store, api, &fakeRunner{})
			bot.config.AllowWorkflows = test.enabled
			bot.allowedWorkflows = makeSet([]string{"Wf0BSM19MCDT"})
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
			runner := successfulTurnRunner()
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
	runner := successfulTurnRunner()
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
		successfulTurnRunner().responses,
		successfulTurnRunner().responses...,
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
			runner := successfulTurnRunner()
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
			runner := successfulTurnRunner()
			bot := newTestBot(t, store, api, runner)
			bot.config.ThreadSubscriptionReaction = "thread-subete"
			bot.config.ThreadSubscriptionTTL = 48 * time.Hour
			var waits []time.Duration
			bot.sleep = func(ctx context.Context, duration time.Duration) error {
				if duration == statusRefreshDelay {
					<-ctx.Done()
					return ctx.Err()
				}
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
	runner := successfulTurnRunner()
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
	runner := successfulTurnRunner()
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
			runner := successfulTurnRunner()
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

func TestWebAPIPostMessageSendsMarkdownBlock(t *testing.T) {
	const text = "#### 見出し\n\n**太字** と [ラベル](https://example.test/a.ts)"
	var form url.Values
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/chat.postMessage" {
			t.Errorf("request path = %q, want /chat.postMessage", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		form = r.Form
		return postMessageResponse(r, `{"ok":true,"ts":"100.2"}`), nil
	})}

	api := &webAPI{client: slack.New("token", slack.OptionAPIURL("https://slack.test/"), slack.OptionHTTPClient(httpClient))}
	timestamp, err := api.PostMessage(context.Background(), "C1", "100.1", text)
	if err != nil {
		t.Fatalf("PostMessage() error = %v, want nil", err)
	}
	if timestamp != "100.2" {
		t.Fatalf("PostMessage() timestamp = %q, want 100.2", timestamp)
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(form.Get("blocks")), &blocks); err != nil {
		t.Fatalf("blocks field %q is not JSON: %v", form.Get("blocks"), err)
	}
	if len(blocks) != 1 || blocks[0].Type != "markdown" {
		t.Fatalf("blocks = %+v, want a single markdown block", blocks)
	}
	if blocks[0].Text != text {
		t.Fatalf("markdown block text = %q, want the Markdown unchanged", blocks[0].Text)
	}
	// Slack shows the text field in notification previews only, so it carries
	// the same answer without the Markdown syntax.
	if want := slackfmt.PlainText(text); form.Get("text") != want {
		t.Fatalf("text field = %q, want the plain-text fallback %q", form.Get("text"), want)
	}
	if form.Get("thread_ts") != "100.1" {
		t.Fatalf("thread_ts = %q, want 100.1", form.Get("thread_ts"))
	}
}

func TestWebAPIPostMessageRetriesWithoutBlocksWhenSlackRejectsThem(t *testing.T) {
	var forms []url.Values
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		forms = append(forms, r.Form)
		if len(forms) == 1 {
			return postMessageResponse(r, `{"ok":false,"error":"invalid_blocks"}`), nil
		}
		return postMessageResponse(r, `{"ok":true,"ts":"100.2"}`), nil
	})}

	api := &webAPI{client: slack.New("token", slack.OptionAPIURL("https://slack.test/"), slack.OptionHTTPClient(httpClient))}
	timestamp, err := api.PostMessage(context.Background(), "C1", "100.1", "# 見出し")
	if err != nil {
		t.Fatalf("PostMessage() error = %v, want the retry to succeed", err)
	}
	if timestamp != "100.2" {
		t.Fatalf("PostMessage() timestamp = %q, want 100.2", timestamp)
	}
	if len(forms) != 2 {
		t.Fatalf("requests = %d, want 2", len(forms))
	}
	if forms[1].Get("blocks") != "" {
		t.Fatalf("retry sent blocks = %q, want none", forms[1].Get("blocks"))
	}
	if forms[1].Get("text") != "見出し" {
		t.Fatalf("retry text = %q, want the plain-text fallback", forms[1].Get("text"))
	}
}

func TestWebAPIPostMessageKeepsOtherErrors(t *testing.T) {
	var requests int
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return postMessageResponse(r, `{"ok":false,"error":"channel_not_found"}`), nil
	})}

	api := &webAPI{client: slack.New("token", slack.OptionAPIURL("https://slack.test/"), slack.OptionHTTPClient(httpClient))}
	if _, err := api.PostMessage(context.Background(), "C1", "100.1", "hello"); err == nil {
		t.Fatal("PostMessage() error = nil, want channel_not_found")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1: only a rejected block payload is retried", requests)
	}
}

func postMessageResponse(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

func TestAllowedActiveMessageReplyRunsTurnInSharedThreadSession(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := successfulTurnRunner()
	bot := newTestBot(t, store, api, runner)
	configureActiveSubscription(bot, store, now)

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "please continue"))

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if got := store.threadKeys; !reflect.DeepEqual(got, []string{"v5:C1:100.1"}) {
		t.Fatalf("thread keys = %v, want v3 channel/thread session", got)
	}
	if len(runner.prompts) != 1 {
		t.Fatalf("runner prompts = %d, want 1", len(runner.prompts))
	}
	for _, want := range []string{
		"<authenticated_slack_author_id>\nU2\n</authenticated_slack_author_id>",
		"<message_text>\nplease continue\n</message_text>",
		"## 添付ファイル",
		"## チャンネルメモリ追記",
	} {
		if !strings.Contains(runner.prompts[0], want) {
			t.Errorf("message prompt does not contain %q", want)
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

func TestAllowedActiveHumanThreadBroadcastRunsTurn(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	runner := successfulTurnRunner()
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
		successfulTurnRunner().responses,
		successfulTurnRunner().responses...,
	)}
	bot := newTestBot(t, store, &fakeSlack{}, runner)
	configureActiveSubscription(bot, store, now)

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "first reply"))
	bot.HandleMessage(context.Background(), messageReply("U3", "300.3", "second reply"))

	if got := store.threadKeys; !reflect.DeepEqual(got, []string{"v5:C1:100.1", "v5:C1:100.1"}) {
		t.Fatalf("thread keys = %v, want one shared Slack thread key", got)
	}
	if got := runner.threadIDs; !reflect.DeepEqual(got, []string{"", "codex-thread"}) {
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

	bot.HandleMessage(context.Background(), messageReply("U1", "200.2", "expired"))

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
	runner := successfulTurnRunner()
	bot := New(&fakeSlack{}, store, runner, Config{
		AllowedUserIDs:    []string{"U2"},
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

func successfulTurnRunner() *fakeRunner {
	return &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"Done."},
	}}}}
}

func TestTurnSlackOutputsSanitizeForbiddenUserMemory(t *testing.T) {
	tests := []struct {
		name     string
		turnText string
		want     string
	}{
		{
			name:     "normal answer",
			turnText: "No work needed.\n## ユーザーメモリ追記\nprivate turn memory",
			want:     "No work needed.",
		},
		{
			name:     "unstructured answer",
			turnText: "unstructured answer ``` ## ユーザーメモリ追記 private turn memory",
			want:     "unstructured answer ```",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			runner := &fakeRunner{responses: []runnerResponse{
				{result: &codex.TurnResult{
					Completed: true,
					Messages:  []string{test.turnText},
				}},
				{result: &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}},
			}}

			newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())

			posts := strings.Join(api.postTexts, "|")
			if !strings.Contains(posts, test.want) {
				t.Fatalf("posts = %q, want preserved output %q", posts, test.want)
			}
			for _, forbidden := range []string{"## ユーザーメモリ追記", "private turn memory"} {
				if strings.Contains(posts, forbidden) {
					t.Fatalf("posts = %q, must not expose %q", posts, forbidden)
				}
			}
		})
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

func TestSessionsAreSharedBySlackThread(t *testing.T) {
	store := &fakeStore{claim: true}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{"Done."}}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{"Done."}}},
	}}
	bot := newTestBot(t, store, &fakeSlack{}, runner)
	bot.allowedUsers["U2"] = struct{}{}
	bot.HandleMention(context.Background(), mention())
	bot.HandleMention(context.Background(), &slackevents.AppMentionEvent{
		User: "U2", Channel: "C1", TimeStamp: "200.2", ThreadTimeStamp: "100.1", Text: "<@UBOT> do it",
	})

	want := []string{"v5:C1:100.1", "v5:C1:100.1"}
	if !reflect.DeepEqual(store.threadKeys, want) {
		t.Fatalf("thread keys = %v, want %v", store.threadKeys, want)
	}
	if len(store.threadIDs) != 1 {
		t.Fatalf("stored thread keys = %v, want one shared session", store.threadIDs)
	}
	if got := runner.threadIDs; !reflect.DeepEqual(got, []string{"", "codex-thread"}) {
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
		Messages:  []string{"Done."},
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
	for _, want := range []string{"<slack_thread>", "root context", "important detail", "<message_text>\ncurrent request"} {
		if !strings.Contains(got, want) {
			t.Errorf("turn prompt does not contain %q", want)
		}
	}
	if strings.Count(got, "<@UBOT> current request") != 0 {
		t.Error("current mention was duplicated into Slack thread context")
	}
}

func TestExistingSessionSkipsSlackThreadFetch(t *testing.T) {
	store := &fakeStore{
		claim:     true,
		threadIDs: map[string]string{"v5:C1:100.1": "existing-thread"},
	}
	api := &fakeSlack{threadErr: errors.New("must not be called")}
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"Done."},
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

	stop := bot.startStatus(context.Background(), "C1", "100.1", workingStatus).Stop
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

func TestTurnPromptInjectsMemoryContent(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		Completed: true,
		Messages:  []string{"No work needed."},
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
			t.Errorf("turn prompt does not contain %q", want)
		}
	}
	if strings.Contains(prompt, "user_memory") {
		t.Error("turn prompt must omit user memory")
	}
	if strings.Contains(prompt, "MEMORY.md") {
		t.Error("turn prompt should inject memory content, not the file path")
	}
}

func TestWorkMemoryAppendIsWrittenByBot(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
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
		{result: &codex.TurnResult{Completed: false, Err: "failed"}},
	}}
	newTestBot(t, store, api, runner).HandleMention(context.Background(), mention())
	if store.current != state.Interrupted {
		t.Fatalf("state = %q, want interrupted", store.current)
	}
	assertFinalReactionOrder(t, api.calls, "x")
}

// looseStore accepts any transition so concurrent events do not interfere.
type looseStore struct {
	mu      sync.Mutex
	threads map[string]string
}

func (s *looseStore) ClaimEvent(string) (bool, error) { return true, nil }

func (s *looseStore) GetFollowUp(string) (state.FollowUp, bool)            { return state.FollowUp{}, false }
func (s *looseStore) SetFollowUp(string, state.FollowUp) error             { return nil }
func (s *looseStore) DeleteFollowUp(string) (bool, error)                  { return false, nil }
func (s *looseStore) TakeDueFollowUps(time.Time) ([]state.FollowUp, error) { return nil, nil }

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

func TestPostSplitsLongMessage(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{}, api, &fakeRunner{})
	text := strings.Repeat("界", maxSlackMessageRunes+1)
	if err := bot.post(context.Background(), "C1", "1", text); err != nil {
		t.Fatalf("post() error = %v", err)
	}
	if len(api.postTexts) != 2 {
		t.Fatalf("post attempts = %d, want 2", len(api.postTexts))
	}
	if len([]rune(api.postTexts[0])) != maxSlackMessageRunes || api.postTexts[1] != "界" {
		t.Fatalf("post chunks have rune lengths %d and %d", len([]rune(api.postTexts[0])), len([]rune(api.postTexts[1])))
	}
}

func TestPostKeepsMarkdownStructureAcrossChunks(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{}, api, &fakeRunner{})
	heading := "#### " + strings.Repeat("見", 100)
	text := strings.TrimSuffix(strings.Repeat(heading+"\n", 100), "\n")
	if err := bot.post(context.Background(), "C1", "1", text); err != nil {
		t.Fatalf("post() error = %v", err)
	}
	if len(api.postTexts) < 2 {
		t.Fatalf("post attempts = %d, want at least 2", len(api.postTexts))
	}
	for i, chunk := range api.postTexts {
		for _, line := range strings.Split(chunk, "\n") {
			if line != heading {
				t.Fatalf("chunk %d cut a heading: %q", i, line)
			}
		}
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
			Completed: true, Messages: []string{"Done."},
		}})
		// Each new thread starts a session with the catalog read now.
		bot.HandleMention(context.Background(), mentionAt(fmt.Sprintf("%d.1", 100+len(runner.prompts)), ""))
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

// parallelRunner holds each execution open until release is closed,
// recording how many sessions run at once.
type parallelRunner struct {
	mu          sync.Mutex
	running     int
	maxRunning  int
	workCwds    []string
	workRoots   [][]string
	workPrompts []string
	started     chan struct{}
	release     chan struct{}
}

func newParallelRunner() *parallelRunner {
	return &parallelRunner{started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (r *parallelRunner) Run(ctx context.Context, _, _, cwd string, roots, _ []string, prompt string, callback func(string) error, onActivity func(codex.Activity)) (*codex.TurnResult, error) {
	if callback != nil {
		if err := callback("codex-thread"); err != nil {
			return nil, err
		}
	}

	r.mu.Lock()
	r.running++
	r.maxRunning = max(r.maxRunning, r.running)
	r.workCwds = append(r.workCwds, cwd)
	r.workRoots = append(r.workRoots, roots)
	r.workPrompts = append(r.workPrompts, prompt)
	r.mu.Unlock()
	r.started <- struct{}{}
	defer func() {
		r.mu.Lock()
		r.running--
		r.mu.Unlock()
	}()
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}, nil
}

func (r *parallelRunner) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("work turn did not start")
	}
}

func (r *parallelRunner) assertNotStarted(t *testing.T) {
	t.Helper()
	select {
	case <-r.started:
		t.Fatal("work turn started while it should be waiting")
	case <-time.After(200 * time.Millisecond):
	}
}

func newParallelBot(t *testing.T, api *fakeSlack, runner Runner, maxParallel int) *Bot {
	t.Helper()
	return New(api, &looseStore{threads: map[string]string{}}, runner, Config{
		AllowedUserIDs:        []string{"U1"},
		AllowedChannelIDs:     []string{"C1"},
		SharedWriteChannelIDs: []string{"C1"},
		WorkspaceDir:          "/repo/workspace",
		MemoryDir:             filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:          time.Minute,
		BotUserID:             "UBOT",
		MaxParallelWork:       maxParallel,
	}, nil)
}

func mentionAt(timestamp, threadTS string) *slackevents.AppMentionEvent {
	return &slackevents.AppMentionEvent{
		User: "U1", Channel: "C1", TimeStamp: timestamp, ThreadTimeStamp: threadTS, Text: "<@UBOT> do it",
	}
}

func handleAsync(bot *Bot, ctx context.Context, events ...*slackevents.AppMentionEvent) *sync.WaitGroup {
	var wg sync.WaitGroup
	for _, event := range events {
		wg.Go(func() { bot.HandleMention(ctx, event) })
	}
	return &wg
}

func TestWorkTurnsForDifferentThreadsRunInParallel(t *testing.T) {
	runner := newParallelRunner()
	bot := newParallelBot(t, &fakeSlack{}, runner, 2)
	wg := handleAsync(bot, context.Background(), mentionAt("100.1", ""), mentionAt("200.2", ""))
	defer wg.Wait()
	defer close(runner.release)

	runner.waitStarted(t)
	runner.waitStarted(t)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.maxRunning != 2 {
		t.Fatalf("max concurrent work turns = %d, want 2", runner.maxRunning)
	}
}

func TestWorkTurnsBeyondLimitWaitWithQueuedStatus(t *testing.T) {
	runner := newParallelRunner()
	api := &fakeSlack{}
	bot := newParallelBot(t, api, runner, 1)
	ctx := context.Background()
	first := handleAsync(bot, ctx, mentionAt("100.1", ""))
	runner.waitStarted(t)

	second := handleAsync(bot, ctx, mentionAt("200.2", ""))
	runner.assertNotStarted(t)
	api.mu.Lock()
	queued := false
	for _, call := range api.calls {
		queued = queued || (call.kind == "status" && call.text == queuedStatus)
	}
	api.mu.Unlock()
	if !queued {
		t.Error("waiting work turn did not show the queued status")
	}

	close(runner.release)
	runner.waitStarted(t)
	first.Wait()
	second.Wait()
	if runner.maxRunning != 1 {
		t.Fatalf("max concurrent work turns = %d, want 1", runner.maxRunning)
	}
}

func TestWorkTurnsInOneThreadRunInOrder(t *testing.T) {
	runner := newParallelRunner()
	bot := newParallelBot(t, &fakeSlack{}, runner, 2)
	ctx := context.Background()
	first := handleAsync(bot, ctx, mentionAt("100.1", ""))
	runner.waitStarted(t)

	second := handleAsync(bot, ctx, mentionAt("100.2", "100.1"))
	runner.assertNotStarted(t)
	close(runner.release)
	runner.waitStarted(t)
	first.Wait()
	second.Wait()
	if runner.maxRunning != 1 {
		t.Fatalf("max concurrent work turns in one thread = %d, want 1", runner.maxRunning)
	}
}

func TestCancelledWaitForWorkFailsWithoutRunningWork(t *testing.T) {
	runner := newParallelRunner()
	api := &fakeSlack{}
	store := &fakeStore{claim: true}
	bot := New(api, store, runner, Config{
		AllowedUserIDs:        []string{"U1"},
		AllowedChannelIDs:     []string{"C1"},
		SharedWriteChannelIDs: []string{"C1"},
		WorkspaceDir:          "/repo/workspace",
		MemoryDir:             filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:          time.Minute,
		BotUserID:             "UBOT",
		MaxParallelWork:       1,
	}, nil)
	// Occupy the only work slot.
	bot.workSlots <- struct{}{}
	defer func() { <-bot.workSlots }()

	ctx, cancel := context.WithCancel(context.Background())
	done := handleAsync(bot, ctx, mention())
	runner.assertNotStarted(t)
	cancel()
	done.Wait()

	assertTransitions(t, store.transitions, [][2]state.State{
		{state.Received, state.Failed},
	})
	if len(api.postTexts) == 0 || api.postTexts[len(api.postTexts)-1] != workStartFailureMessage {
		t.Fatalf("posts = %q, want work start failure", api.postTexts)
	}
}

type fakeWorkspaces struct {
	mu        sync.Mutex
	threadIDs []string
	acquired  []string
	creates   []bool
	released  int
	err       error
	// lazy makes the thread's checkout exist only after an Acquire that
	// creates it, like workspace.Manager.
	lazy   bool
	cloned map[string]bool
}

func (w *fakeWorkspaces) ThreadDir(threadID string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.threadIDs = append(w.threadIDs, threadID)
	return "/home/workspace/" + threadID, nil
}

func (w *fakeWorkspaces) OtherThreadPaths(threadID string) ([]string, error) {
	return []string{"/home/workspace/OTHER-" + threadID}, nil
}

func (w *fakeWorkspaces) Acquire(_ context.Context, threadID string, createCheckouts bool) (*workspace.Lease, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	w.acquired = append(w.acquired, threadID)
	w.creates = append(w.creates, createCheckouts)
	release := func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.released++
	}
	if w.lazy && !createCheckouts && !w.cloned[threadID] {
		lease := workspace.NewLease("/home/workspace/"+threadID, []string{"/shared/plain"}, nil, release)
		lease.PendingRepos = []string{"/src/app"}
		return lease, nil
	}
	if w.cloned == nil {
		w.cloned = make(map[string]bool)
	}
	w.cloned[threadID] = true
	checkoutPath := "/home/checkouts/" + threadID + "/app"
	lease := workspace.NewLease(
		"/home/workspace/"+threadID,
		[]string{"/shared/plain", checkoutPath, checkoutPath + ".gitdir"},
		[]workspace.Checkout{{Repo: "/src/app", Path: checkoutPath, Branch: "ebi-x/" + threadID}},
		release,
	)
	return lease, nil
}

func TestWorkUsesThreadWorkspaceAndCheckouts(t *testing.T) {
	runner := newParallelRunner()
	close(runner.release)
	workspaces := &fakeWorkspaces{}
	playbooksDir := t.TempDir()
	bot := New(&fakeSlack{}, &fakeStore{claim: true}, runner, Config{
		AllowedUserIDs:        []string{"U1"},
		AllowedChannelIDs:     []string{"C1"},
		SharedWriteChannelIDs: []string{"C1"},
		WorkspaceDir:          "/repo/workspace",
		MemoryDir:             filepath.Join(t.TempDir(), "memory"),
		PlaybooksDir:          playbooksDir,
		CodexTimeout:          time.Minute,
		BotUserID:             "UBOT",
		WritableRoots:         []string{"/src/app", "/shared/plain"},
		Workspaces:            workspaces,
	}, nil)

	bot.HandleMention(context.Background(), mention())

	if want := []string{"/home/workspace/C1-100.1"}; !reflect.DeepEqual(runner.workCwds, want) {
		t.Errorf("work cwd = %v, want %v", runner.workCwds, want)
	}
	wantRoots := []string{"/shared/plain", "/home/checkouts/C1-100.1/app", "/home/checkouts/C1-100.1/app.gitdir", playbooksDir}
	if len(runner.workRoots) != 1 || !reflect.DeepEqual(runner.workRoots[0], wantRoots) {
		t.Errorf("work roots = %v, want %v", runner.workRoots, wantRoots)
	}
	if !strings.Contains(runner.workPrompts[0], "/src/app → /home/checkouts/C1-100.1/app") {
		t.Errorf("work prompt does not list the thread checkout:\n%s", runner.workPrompts[0])
	}
	if workspaces.released != 1 {
		t.Errorf("lease released %d times, want 1", workspaces.released)
	}
}

func TestWorkspacePreparationFailureDoesNotStartWork(t *testing.T) {
	runner := newParallelRunner()
	api := &fakeSlack{}
	store := &fakeStore{claim: true}
	bot := New(api, store, runner, Config{
		AllowedUserIDs:        []string{"U1"},
		AllowedChannelIDs:     []string{"C1"},
		SharedWriteChannelIDs: []string{"C1"},
		WorkspaceDir:          "/repo/workspace",
		MemoryDir:             filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:          time.Minute,
		BotUserID:             "UBOT",
		Workspaces:            &fakeWorkspaces{err: errors.New("git clone failed")},
	}, nil)

	bot.HandleMention(context.Background(), mention())

	if len(runner.workCwds) != 0 {
		t.Fatal("work turn ran without a prepared workspace")
	}
	assertTransitions(t, store.transitions, [][2]state.State{
		{state.Received, state.Failed},
	})
}

func TestUnauthorizedSubscribedMessageReplyIsIgnored(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := successfulTurnRunner()
	bot := newTestBot(t, store, api, runner)
	configureActiveSubscription(bot, store, now)

	bot.HandleMessage(context.Background(), messageReply("UOUTSIDER", "200.2", "run this for me"))

	if store.claimCalls != 0 || runner.calls != 0 || len(api.postTexts) != 0 {
		t.Fatalf("unauthorized reply caused claim=%d runner=%d posts=%q", store.claimCalls, runner.calls, api.postTexts)
	}
}

func TestMentionWithoutChannelAllowlistIsForbidden(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := successfulTurnRunner()
	bot := newTestBot(t, store, api, runner)
	bot.allowedChannels = nil
	event := mention()
	event.Channel = "D1"

	bot.HandleMention(context.Background(), event)

	if store.claimCalls != 0 || runner.calls != 0 {
		t.Fatalf("mention without channel allowlist caused claim=%d runner=%d", store.claimCalls, runner.calls)
	}
}

func TestChannelWithoutSharedWritesCannotChangePlaybooksOrGlobalMemory(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{
			Completed: true,
			Messages: []string{strings.Join([]string{
				"Work completed.",
				"## 全体メモリ追記", "全チャンネルに効く指示",
				"## チャンネルメモリ追記", "このチャンネルの用語",
			}, "\n")},
		}},
	}}
	playbooksDir := t.TempDir()
	bot := New(api, store, runner, Config{
		AllowedUserIDs:    []string{"U1"},
		AllowedChannelIDs: []string{"C1"},
		WorkspaceDir:      "/repo/workspace",
		MemoryDir:         filepath.Join(t.TempDir(), "memory"),
		PlaybooksDir:      playbooksDir,
		CodexTimeout:      time.Minute,
		BotUserID:         "UBOT",
	}, nil)

	bot.HandleMention(context.Background(), mention())

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if slices.Contains(runner.roots[0], playbooksDir) {
		t.Errorf("work roots = %v, must not include playbooks", runner.roots[0])
	}
	if strings.Contains(runner.prompts[0], "## 全体メモリ追記") {
		t.Error("work prompt offers global memory to a channel without shared writes")
	}
	if _, err := os.Stat(filepath.Join(bot.config.MemoryDir, "MEMORY.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("global memory stat error = %v, want not exist", err)
	}
	data, err := os.ReadFile(filepath.Join(bot.config.MemoryDir, "channels", "C1", "MEMORY.md"))
	if err != nil || !strings.Contains(string(data), "このチャンネルの用語") {
		t.Errorf("channel memory = %q, %v; want channel entry", data, err)
	}
}

func TestTurnsDenyOtherThreadPaths(t *testing.T) {
	store := &fakeStore{claim: true}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}},
	}}
	bot := newTestBot(t, store, &fakeSlack{}, runner)
	bot.config.Workspaces = &fakeWorkspaces{}

	bot.HandleMention(context.Background(), mention())

	want := []string{"/home/workspace/OTHER-C1-100.1"}
	if len(runner.denied) != 1 || !reflect.DeepEqual(runner.denied[0], want) {
		t.Fatalf("denied paths = %v, want %v for the single turn", runner.denied, want)
	}
}

func TestNeutralizeBroadcasts(t *testing.T) {
	got := neutralizeBroadcasts("hi <!channel> <!here|here> <!EVERYONE> <!subteam^S123|@team> <@U1> <!date^1|x>")
	want := "hi @channel @here @EVERYONE @subteam^S123 <@U1> <!date^1|x>"
	if got != want {
		t.Fatalf("neutralizeBroadcasts() = %q, want %q", got, want)
	}
}

func TestWebAPIPostMessageNeutralizesBroadcasts(t *testing.T) {
	var form url.Values
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		form = r.Form
		return postMessageResponse(r, `{"ok":true,"ts":"100.2"}`), nil
	})}
	api := &webAPI{client: slack.New("token", slack.OptionAPIURL("https://slack.test/"), slack.OptionHTTPClient(httpClient))}

	if _, err := api.PostMessage(context.Background(), "C1", "100.1", "完了 <!channel>"); err != nil {
		t.Fatalf("PostMessage() error = %v, want nil", err)
	}
	for _, field := range []string{"blocks", "text"} {
		if strings.Contains(form.Get(field), "<!channel>") {
			t.Fatalf("%s field = %q, must not contain a broadcast mention", field, form.Get(field))
		}
	}
}

// dirWorkspaces gives each thread real directories so attachments can be
// validated against the filesystem.
type dirWorkspaces struct {
	base string
}

func (w dirWorkspaces) ThreadDir(threadID string) (string, error) {
	dir := filepath.Join(w.base, "workspace", threadID)
	return dir, os.MkdirAll(dir, 0o700)
}

func (w dirWorkspaces) OtherThreadPaths(string) ([]string, error) { return nil, nil }

func (w dirWorkspaces) Acquire(_ context.Context, threadID string, _ bool) (*workspace.Lease, error) {
	dir, err := w.ThreadDir(threadID)
	if err != nil {
		return nil, err
	}
	checkout := filepath.Join(w.base, "checkouts", threadID, "slides")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return nil, err
	}
	return workspace.NewLease(dir, []string{checkout, checkout + ".gitdir"},
		[]workspace.Checkout{{Repo: "/src/slides", Path: checkout, Branch: "ebi-x/" + threadID}}, func() {}), nil
}

func newAttachmentBot(t *testing.T, api *fakeSlack, store *fakeStore, workResult string) string {
	t.Helper()
	base := t.TempDir()
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{strings.ReplaceAll(workResult, "$BASE", base)}}},
	}}
	bot := New(api, store, runner, Config{
		AllowedUserIDs:    []string{"U1"},
		AllowedChannelIDs: []string{"C1"},
		WorkspaceDir:      "/unused",
		MemoryDir:         filepath.Join(base, "memory"),
		CodexTimeout:      time.Minute,
		BotUserID:         "UBOT",
		Workspaces:        dirWorkspaces{base: base},
	}, nil)
	for path, content := range map[string]string{
		"workspace/C1-100.1/preview.png":            "png-data",
		"checkouts/C1-100.1/slides/out/deck.pdf":    "pdf-data",
		"workspace/C1-200.1/other-thread-draft.pdf": "other",
	} {
		full := filepath.Join(base, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bot.HandleMention(context.Background(), mention())
	return base
}

func TestWorkAttachmentsAreUploadedToThread(t *testing.T) {
	api := &fakeSlack{}
	store := &fakeStore{claim: true}
	newAttachmentBot(t, api, store,
		"v1のプレビューを添付します。\n\n## 添付ファイル\n- preview.png\n- $BASE/checkouts/C1-100.1/slides/out/deck.pdf\n\n## 全体メモリ追記\n学び")

	want := []fakeUpload{
		{channel: "C1", threadTS: "100.1", filename: "preview.png", content: "png-data"},
		{channel: "C1", threadTS: "100.1", filename: "deck.pdf", content: "pdf-data"},
	}
	if !reflect.DeepEqual(api.uploads, want) {
		t.Fatalf("uploads = %+v, want %+v", api.uploads, want)
	}
	if len(api.postTexts) != 1 || !strings.HasPrefix(api.postTexts[0], "v1のプレビューを添付します。") ||
		strings.Contains(api.postTexts[0], "添付ファイル") || strings.Contains(api.postTexts[0], "⚠️") {
		t.Fatalf("posts = %q, want the body without the attachment section or a warning", api.postTexts)
	}
	// Files are uploaded before the result so a failure can be reported in it.
	var order []string
	for _, call := range api.calls {
		if call.kind == "upload" || call.kind == "post" {
			order = append(order, call.kind)
		}
	}
	if strings.Join(order, ",") != "upload,upload,post" {
		t.Fatalf("call order = %v, want uploads before the result post", order)
	}
	assertTransitions(t, store.transitions, [][2]state.State{
		{state.Received, state.Working},
		{state.Working, state.Done},
	})
	assertFinalReactionOrder(t, api.calls, "white_check_mark")
}

func TestFailedAttachmentIsReportedAndFileKept(t *testing.T) {
	api := &fakeSlack{uploadErrs: []error{errors.New("upload refused")}}
	store := &fakeStore{claim: true}
	base := newAttachmentBot(t, api, store, "プレビューを添付します。\n## 添付ファイル\n- preview.png")

	if len(api.uploads) != 0 {
		t.Fatalf("uploads = %+v, want none", api.uploads)
	}
	if len(api.postTexts) != 1 || !strings.Contains(api.postTexts[0], "添付できませんでした") ||
		!strings.Contains(api.postTexts[0], "preview.png") || !strings.Contains(api.postTexts[0], "添付を再送して") {
		t.Fatalf("posts = %q, want a failure notice naming the file", api.postTexts)
	}
	if _, err := os.Stat(filepath.Join(base, "workspace", "C1-100.1", "preview.png")); err != nil {
		t.Fatalf("generated file was not kept: %v", err)
	}
	assertTransitions(t, store.transitions, [][2]state.State{
		{state.Received, state.Working},
		{state.Working, state.Interrupted},
	})
	assertFinalReactionOrder(t, api.calls, "x")
}

func TestAttachmentFromAnotherThreadIsNotUploaded(t *testing.T) {
	api := &fakeSlack{}
	store := &fakeStore{claim: true}
	newAttachmentBot(t, api, store,
		"添付します。\n## 添付ファイル\n- preview.png\n- ../C1-200.1/other-thread-draft.pdf\n- $BASE/workspace/C1-200.1/other-thread-draft.pdf")

	if len(api.uploads) != 1 || api.uploads[0].filename != "preview.png" {
		t.Fatalf("uploads = %+v, want only this thread's preview", api.uploads)
	}
	if !strings.Contains(api.postTexts[0], "作業領域外") {
		t.Fatalf("post = %q, want the rejected file reported", api.postTexts[0])
	}
	assertFinalReactionOrder(t, api.calls, "x")
}

func TestMalformedAttachmentSectionUploadsNothing(t *testing.T) {
	api := &fakeSlack{}
	store := &fakeStore{claim: true}
	newAttachmentBot(t, api, store, "添付します。\n## 添付ファイル\npreview.png を送ってください")

	if len(api.uploads) != 0 {
		t.Fatalf("uploads = %+v, want none", api.uploads)
	}
	if len(api.postTexts) != 1 || !strings.Contains(api.postTexts[0], "添付ファイルの指定を読み取れなかった") ||
		strings.Contains(api.postTexts[0], "## 添付ファイル") {
		t.Fatalf("posts = %q, want the notice without the attachment heading", api.postTexts)
	}
	assertFinalReactionOrder(t, api.calls, "x")
}

func TestWebAPIUploadFileUsesExternalUpload(t *testing.T) {
	var paths []string
	var uploaded string
	var completeForm url.Values
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		var body string
		switch r.URL.Path {
		case "/files.getUploadURLExternal":
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("filename") != "deck.pdf" || r.Form.Get("length") != "8" {
				t.Errorf("getUploadURLExternal form = %v", r.Form)
			}
			body = `{"ok":true,"upload_url":"https://files.slack.test/upload/1","file_id":"F1"}`
		case "/upload/1":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			uploaded = string(data)
			body = "OK"
		case "/files.completeUploadExternal":
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			completeForm = r.Form
			body = `{"ok":true,"files":[{"id":"F1","title":"deck.pdf"}]}`
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}

	api := &webAPI{client: slack.New("token", slack.OptionAPIURL("https://slack.test/"), slack.OptionHTTPClient(httpClient))}
	if err := api.UploadFile(context.Background(), "C1", "100.1", "deck.pdf", 8, strings.NewReader("pdf-data")); err != nil {
		t.Fatalf("UploadFile() error = %v", err)
	}
	if strings.Join(paths, ",") != "/files.getUploadURLExternal,/upload/1,/files.completeUploadExternal" {
		t.Fatalf("requests = %v", paths)
	}
	if !strings.Contains(uploaded, "pdf-data") {
		t.Fatalf("uploaded body = %q, want the file content", uploaded)
	}
	if completeForm.Get("channel_id") != "C1" || completeForm.Get("thread_ts") != "100.1" {
		t.Fatalf("completeUploadExternal form = %v, want the thread", completeForm)
	}
}

func TestSharedWorkspaceFilesAreNotAttached(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{"添付します。\n## 添付ファイル\n- report.pdf"}}},
	}}
	New(api, &fakeStore{claim: true}, runner, Config{
		AllowedUserIDs:    []string{"U1"},
		AllowedChannelIDs: []string{"C1"},
		WorkspaceDir:      dir,
		MemoryDir:         filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:      time.Minute,
		BotUserID:         "UBOT",
	}, nil).HandleMention(context.Background(), mention())

	if len(api.uploads) != 0 {
		t.Fatalf("uploads = %+v, want none from a directory every thread shares", api.uploads)
	}
	if !strings.Contains(api.postTexts[0], "作業領域外") {
		t.Fatalf("post = %q, want the rejection reported", api.postTexts[0])
	}
}

func TestUploadRetriesOnlyRateLimits(t *testing.T) {
	tests := []struct {
		name        string
		errs        []error
		wantUploads int
		wantCalls   int
	}{
		{name: "rate limited", errs: []error{&slack.RateLimitedError{RetryAfter: time.Millisecond}}, wantUploads: 1, wantCalls: 2},
		{name: "server error", errs: []error{slack.StatusCodeError{Code: 502, Status: "502 Bad Gateway"}}, wantUploads: 0, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeSlack{uploadErrs: test.errs}
			newAttachmentBot(t, api, &fakeStore{claim: true}, "添付します。\n## 添付ファイル\n- preview.png")
			calls := 0
			for _, call := range api.calls {
				if call.kind == "upload" {
					calls++
				}
			}
			if len(api.uploads) != test.wantUploads || calls != test.wantCalls {
				t.Fatalf("uploads = %d, calls = %d; want %d and %d", len(api.uploads), calls, test.wantUploads, test.wantCalls)
			}
		})
	}
}

func TestResumedSessionReceivesOnlyTheRequest(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{"確認したいことがあります。"}}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{"Work completed."}}},
	}}
	bot := newTestBot(t, store, api, runner)
	bot.config.PlaybooksDir = t.TempDir()
	path := filepath.Join(bot.config.PlaybooksDir, "example.md")
	if err := os.WriteFile(path, []byte("---\nname: example\ndescription: first-version\n---\nBody"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.AppendScoped(bot.config.MemoryDir, memory.ScopeChannel, "C1", "channel-fact"); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		bot.HandleMention(context.Background(), mentionAt(fmt.Sprintf("100.%d", i+1), "100.1"))
		if runner.calls != i+1 {
			t.Fatalf("runner calls = %d, want exactly one per request", runner.calls)
		}
		if runner.sandboxes[i] != "workspace-write" {
			t.Fatalf("sandbox = %q", runner.sandboxes[i])
		}
	}
	if !reflect.DeepEqual(runner.threadIDs, []string{"", "codex-thread"}) {
		t.Fatalf("session IDs = %v, want same session resumed", runner.threadIDs)
	}
	for _, want := range []string{path, "first-version", "channel-fact", "1800字以内"} {
		if !strings.Contains(runner.prompts[0], want) {
			t.Errorf("first prompt missing %q", want)
		}
	}
	for _, repeated := range []string{path, "channel-fact", "1800字以内", "<slack_thread>"} {
		if strings.Contains(runner.prompts[1], repeated) {
			t.Errorf("resumed prompt repeats %q", repeated)
		}
	}
	if !strings.Contains(runner.prompts[1], "<message_text>\ndo it\n</message_text>") {
		t.Error("resumed prompt lacks the request")
	}
	if !reflect.DeepEqual(api.postTexts, []string{"確認したいことがあります。", "Work completed."}) {
		t.Fatalf("posts = %q", api.postTexts)
	}
	assertTransitions(t, store.transitions, [][2]state.State{
		{state.Received, state.Working}, {state.Working, state.Done},
		{state.Received, state.Working}, {state.Working, state.Done},
	})
}

func newLazyCheckoutBot(t *testing.T, api *fakeSlack, runner *fakeRunner, workspaces *fakeWorkspaces) *Bot {
	t.Helper()
	return New(api, &fakeStore{claim: true}, runner, Config{
		AllowedUserIDs:    []string{"U1"},
		AllowedChannelIDs: []string{"C1"},
		WorkspaceDir:      "/repo/workspace",
		MemoryDir:         filepath.Join(t.TempDir(), "memory"),
		CodexTimeout:      time.Minute,
		BotUserID:         "UBOT",
		Workspaces:        workspaces,
	}, nil)
}

func TestQuestionDoesNotCreateCheckouts(t *testing.T) {
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{"回答です。"}}},
	}}
	workspaces := &fakeWorkspaces{lazy: true}
	bot := newLazyCheckoutBot(t, api, runner, workspaces)

	bot.HandleMention(context.Background(), mention())

	if !reflect.DeepEqual(workspaces.creates, []bool{false}) {
		t.Fatalf("Acquire createCheckouts = %v, want [false]", workspaces.creates)
	}
	if !reflect.DeepEqual(runner.roots, [][]string{{"/shared/plain"}}) {
		t.Errorf("roots = %v, want only the shared root", runner.roots)
	}
	if !strings.Contains(runner.prompts[0], "- /src/app") || !strings.Contains(runner.prompts[0], codex.CheckoutRequestHeading) {
		t.Errorf("prompt does not offer a checkout of the pending repository:\n%s", runner.prompts[0])
	}
	if !reflect.DeepEqual(api.postTexts, []string{"回答です。"}) {
		t.Fatalf("posts = %q", api.postTexts)
	}
	if workspaces.released != 1 {
		t.Errorf("released = %d, want 1", workspaces.released)
	}
}

func TestCheckoutRequestCreatesCheckoutsAndContinuesSession(t *testing.T) {
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{codex.CheckoutRequestHeading}}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{"変更しました。"}}},
		{result: &codex.TurnResult{Completed: true, Messages: []string{"続きも対応しました。"}}},
	}}
	workspaces := &fakeWorkspaces{lazy: true}
	bot := newLazyCheckoutBot(t, api, runner, workspaces)

	bot.HandleMention(context.Background(), mention())

	if !reflect.DeepEqual(workspaces.creates, []bool{false, true}) {
		t.Fatalf("Acquire createCheckouts = %v, want [false true]", workspaces.creates)
	}
	if !reflect.DeepEqual(runner.threadIDs, []string{"", "codex-thread"}) {
		t.Fatalf("session IDs = %v, want the requesting session continued", runner.threadIDs)
	}
	checkout := "/home/checkouts/C1-100.1/app"
	if want := []string{"/shared/plain", checkout, checkout + ".gitdir"}; !reflect.DeepEqual(runner.roots[1], want) {
		t.Errorf("continued roots = %v, want %v", runner.roots[1], want)
	}
	if !strings.Contains(runner.prompts[1], "/src/app → "+checkout) {
		t.Errorf("continuation prompt does not list the checkout:\n%s", runner.prompts[1])
	}
	if !reflect.DeepEqual(api.postTexts, []string{"変更しました。"}) {
		t.Fatalf("posts = %q, want only the continued answer", api.postTexts)
	}

	// Later requests reuse the checkout without asking again.
	bot.HandleMention(context.Background(), mentionAt("100.2", "100.1"))
	if !reflect.DeepEqual(workspaces.creates, []bool{false, true, false}) {
		t.Fatalf("Acquire createCheckouts = %v", workspaces.creates)
	}
	if !strings.Contains(runner.prompts[2], "/src/app → "+checkout) || strings.Contains(runner.prompts[2], codex.CheckoutRequestHeading) {
		t.Errorf("resumed prompt does not describe the existing checkout:\n%s", runner.prompts[2])
	}
	if workspaces.released != 3 {
		t.Errorf("released = %d, want 3", workspaces.released)
	}
}

func TestCheckoutRequestPreparationFailureIsInterrupted(t *testing.T) {
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		{result: &codex.TurnResult{Completed: true, Messages: []string{codex.CheckoutRequestHeading}}},
	}}
	workspaces := &fakeWorkspaces{lazy: true}
	store := &fakeStore{claim: true}
	bot := newLazyCheckoutBot(t, api, runner, workspaces)
	bot.store = store
	runner.onRun = func(int) {
		workspaces.mu.Lock()
		workspaces.err = errors.New("clone failed")
		workspaces.mu.Unlock()
	}

	bot.HandleMention(context.Background(), mention())

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if !reflect.DeepEqual(api.postTexts, []string{workFailureMessage}) {
		t.Fatalf("posts = %q", api.postTexts)
	}
	assertTransitions(t, store.transitions, [][2]state.State{
		{state.Received, state.Working}, {state.Working, state.Interrupted},
	})
	if workspaces.released != 1 {
		t.Errorf("released = %d, want 1", workspaces.released)
	}
}

func TestIncompleteSingleTurnIsInterruptedAndNotRetried(t *testing.T) {
	for _, result := range []*codex.TurnResult{nil, {Completed: false, Err: "failed"}, {Completed: true}} {
		store, err := state.NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{responses: []runnerResponse{{result: result}}}
		api := &fakeSlack{}
		bot := New(api, store, runner, Config{AllowedUserIDs: []string{"U1"}, AllowedChannelIDs: []string{"C1"}, WorkspaceDir: "/repo/workspace", CodexTimeout: time.Minute}, nil)
		bot.HandleMention(context.Background(), mention())
		bot.HandleMention(context.Background(), mention())
		if runner.calls != 1 {
			t.Fatalf("incomplete execution retried: calls=%d", runner.calls)
		}
		if !strings.Contains(strings.Join(api.postTexts, ""), workFailureMessage) {
			t.Fatalf("posts = %q", api.postTexts)
		}
		assertFinalReactionOrder(t, api.calls, "x")
	}
}

func TestUnreadablePlaybookCatalogDoesNotStartExecution(t *testing.T) {
	store := &fakeStore{claim: true}
	runner := &fakeRunner{}
	bot := newTestBot(t, store, &fakeSlack{}, runner)
	bot.config.PlaybooksDir = filepath.Join(t.TempDir(), "missing")
	bot.HandleMention(context.Background(), mention())
	if runner.calls != 0 || store.current != state.Failed {
		t.Fatalf("runner calls=%d, state=%s; want retryable failure before execution", runner.calls, store.current)
	}
}
