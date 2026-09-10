// Package slackbot connects Slack mentions to Codex planning and work turns.
package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/memory"
	"github.com/n-seiji/ebi-x/internal/playbook"
	"github.com/n-seiji/ebi-x/internal/prompt"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

const (
	maxSlackMessageRunes  = 3900
	maxThreadContextRunes = 12000
	claimFailureMessage   = "受付に失敗しました。お手数ですが、もう一度 mention してください。"
	failClosedMessage     = "作業指示を確定できなかったため、作業は開始していません。指示を明確にして、もう一度 mention してください。"
	planFailureMessage    = "⚠️ 方針の検討または投稿に失敗しました。もう一度 mention してください。"
	threadFailureMessage  = "⚠️ スレッドの読み込みに失敗したため、作業は開始していません。もう一度 mention してください。"
	workFailureMessage    = "⚠️ 作業が完了したことを確認できませんでした。状況を確認し、新しい mention で依頼し直してください。"
	forbiddenMessage      = "403 forbidden. %s に確認してください。"
	planningStatus        = "が方針を考えています…"
	workingStatus         = "が作業を進めています…"
	statusRefreshDelay    = 80 * time.Second
)

// SlackAPI is the subset of Slack Web API used by Bot.
type SlackAPI interface {
	PostMessage(ctx context.Context, channel, threadTS, text string) (string, error)
	GetThreadMessages(ctx context.Context, channel, threadTS, latest string) ([]ThreadMessage, error)
	SetStatus(ctx context.Context, channel, threadTS, status string) error
	HasReaction(ctx context.Context, channel, timestamp, reaction string) (bool, error)
	AddReaction(ctx context.Context, channel, timestamp, name string) error
	RemoveReaction(ctx context.Context, channel, timestamp, name string) error
}

// ThreadMessage is the Slack thread data supplied to a first planning turn.
type ThreadMessage struct {
	AuthorID  string
	Timestamp string
	Text      string
}

// Store is the persistent state used by Bot.
type Store interface {
	ClaimEvent(eventKey string) (bool, error)
	Transition(eventKey string, from, to state.State) error
	GetThread(threadKey string) (string, bool)
	SetThread(threadKey, threadID string) error
	GetSubscription(threadKey string) (state.Subscription, bool)
	SetSubscription(threadKey string, startedAt, expiresAt time.Time) error
	DeleteSubscriptionIfExpired(threadKey string, now time.Time) (bool, error)
}

// Runner executes one Codex turn.
type Runner interface {
	Run(ctx context.Context, threadID, sandbox, cwd string, writableRoots []string, text string, onThreadStarted func(string) error) (*codex.TurnResult, error)
}

// Config contains paths, allowlists, and timeout settings needed by Bot.
type Config struct {
	AllowedUserIDs             []string
	AllowedChannelIDs          []string
	AllowWorkflows             bool
	AdminUserID                string
	WorkspaceDir               string
	MemoryDir                  string
	PlaybooksDir               string
	CodexTimeout               time.Duration
	ThreadSubscriptionReaction string
	ThreadSubscriptionTTL      time.Duration
	BotUserID                  string
	// WritableRoots are extra directories the work turn may write to.
	WritableRoots []string
}

// Bot handles Slack mentions.
type Bot struct {
	api       SlackAPI
	store     Store
	runner    Runner
	config    Config
	playbooks []playbook.Playbook

	allowedUsers    map[string]struct{}
	allowedChannels map[string]struct{}
	workMu          sync.Mutex
	memoryMu        sync.RWMutex
	threadMu        sync.Mutex
	threadLocks     map[string]*sync.Mutex
	now             func() time.Time
	sleep           func(context.Context, time.Duration) error
}

type triggerSource uint8

const (
	mentionTrigger triggerSource = iota
	messageTrigger
)

type processingTrigger struct {
	source      triggerSource
	authorID    string
	channel     string
	timestamp   string
	threadTS    string
	message     string
	threadReply bool
}

// New constructs a Bot.
func New(api SlackAPI, store Store, runner Runner, config Config, playbooks []playbook.Playbook) *Bot {
	config.WritableRoots = append([]string(nil), config.WritableRoots...)
	if config.PlaybooksDir != "" {
		config.WritableRoots = append(config.WritableRoots, config.PlaybooksDir)
	}
	b := &Bot{
		api:             api,
		store:           store,
		runner:          runner,
		config:          config,
		playbooks:       append([]playbook.Playbook(nil), playbooks...),
		allowedUsers:    makeSet(config.AllowedUserIDs),
		allowedChannels: makeSet(config.AllowedChannelIDs),
		threadLocks:     make(map[string]*sync.Mutex),
		now:             time.Now,
		sleep:           sleepContext,
	}
	return b
}

// HandleMention filters and processes one already-acknowledged app mention.
func (b *Bot) HandleMention(ctx context.Context, event *slackevents.AppMentionEvent) {
	b.handleMention(ctx, event, "")
}

func (b *Bot) handleMention(ctx context.Context, event *slackevents.AppMentionEvent, workflowID string) {
	if event == nil || event.Edited != nil || event.User == b.config.BotUserID {
		return
	}
	if event.BotID == "" {
		if _, ok := b.allowedUsers[event.User]; !ok {
			log.Printf("slackbot: rejecting user %q", event.User)
			b.forbidden(ctx, event)
			return
		}
	} else if !b.config.AllowWorkflows || !validWorkflowID(workflowID) {
		log.Printf("slackbot: rejecting bot %q with workflow %q", event.BotID, workflowID)
		b.forbidden(ctx, event)
		return
	}
	if len(b.allowedChannels) > 0 {
		if _, ok := b.allowedChannels[event.Channel]; !ok {
			log.Printf("slackbot: rejecting channel %q", event.Channel)
			b.forbidden(ctx, event)
			return
		}
	}
	threadTS := event.ThreadTimeStamp
	if threadTS == "" {
		threadTS = event.TimeStamp
	}

	message := stripBotMention(event.Text, b.config.BotUserID)
	if message == "" {
		return
	}
	b.processTrigger(ctx, processingTrigger{
		source:      mentionTrigger,
		authorID:    event.User,
		channel:     event.Channel,
		timestamp:   event.TimeStamp,
		threadTS:    threadTS,
		message:     message,
		threadReply: event.ThreadTimeStamp != "",
	})
}

// HandleMessage filters and processes one already-acknowledged ordinary
// public-channel message event from an active subscribed thread.
func (b *Bot) HandleMessage(ctx context.Context, event *slackevents.MessageEvent) {
	if event == nil ||
		event.ChannelType != "channel" ||
		event.ThreadTimeStamp == "" ||
		event.User == "" ||
		event.User == b.config.BotUserID ||
		event.BotID != "" ||
		(event.SubType != "" && event.SubType != slack.MsgSubTypeThreadBroadcast) ||
		event.IsEdited() ||
		event.DeletedTimeStamp != "" ||
		strings.TrimSpace(event.Text) == "" {
		return
	}
	if _, ok := b.allowedChannels[event.Channel]; !ok {
		return
	}
	if b.config.BotUserID != "" && strings.Contains(event.Text, "<@"+b.config.BotUserID+">") {
		return
	}

	subscriptionKey := event.Channel + ":" + event.ThreadTimeStamp
	now := b.now()
	deleted, err := b.store.DeleteSubscriptionIfExpired(subscriptionKey, now)
	if err != nil {
		log.Printf("slackbot: delete expired subscription %q: %v", subscriptionKey, err)
		return
	}
	if deleted {
		return
	}
	subscription, ok := b.store.GetSubscription(subscriptionKey)
	if !ok || !subscription.ExpiresAt.After(now) {
		return
	}

	b.processTrigger(ctx, processingTrigger{
		source:      messageTrigger,
		authorID:    event.User,
		channel:     event.Channel,
		timestamp:   event.TimeStamp,
		threadTS:    event.ThreadTimeStamp,
		message:     event.Text,
		threadReply: true,
	})
}

func (b *Bot) processTrigger(ctx context.Context, trigger processingTrigger) {
	channel := trigger.channel
	timestamp := trigger.timestamp
	threadTS := trigger.threadTS

	eventKey := channel + ":" + timestamp
	// v3 prevents sessions created before thread sharing from being resumed.
	threadKey := "v3:" + channel + ":" + threadTS

	claimed, err := b.store.ClaimEvent(eventKey)
	if err != nil {
		log.Printf("slackbot: claim %q: %v", eventKey, err)
		if postErr := b.post(ctx, channel, threadTS, claimFailureMessage); postErr != nil {
			log.Printf("slackbot: post claim failure: %v", postErr)
		}
		return
	}
	if !claimed {
		return
	}
	if trigger.source == mentionTrigger {
		b.startThreadSubscription(ctx, channel, threadTS)
	}
	b.addReaction(ctx, channel, timestamp, "eyes")

	if err := b.store.Transition(eventKey, state.Received, state.Planning); err != nil {
		log.Printf("slackbot: start planning %q: %v", eventKey, err)
		if postErr := b.post(ctx, channel, threadTS, planFailureMessage); postErr != nil {
			log.Printf("slackbot: post planning transition failure %q: %v", eventKey, postErr)
		}
		b.finalReaction(ctx, channel, timestamp, false)
		return
	}
	b.setStatus(ctx, channel, threadTS, planningStatus)
	defer b.clearStatus(ctx, channel, threadTS)

	lock := b.threadLock(threadKey)
	lock.Lock()
	threadID, hasThread := b.store.GetThread(threadKey)
	var slackThread string
	if !hasThread && trigger.threadReply {
		threadMessages, err := b.api.GetThreadMessages(ctx, channel, threadTS, timestamp)
		if err != nil {
			lock.Unlock()
			log.Printf("slackbot: read thread context %q: %v", eventKey, err)
			b.fail(ctx, eventKey, state.Planning, state.Failed, channel, threadTS, timestamp, threadFailureMessage)
			return
		}
		slackThread = formatThreadContext(threadMessages, timestamp)
	}
	b.memoryMu.RLock()
	memoryContext, memErr := memory.ReadContext(b.config.MemoryDir, channel)
	b.memoryMu.RUnlock()
	if memErr != nil {
		log.Printf("slackbot: read memory: %v", memErr)
	}
	// Read a fresh, local snapshot so concurrent turns never mutate the catalog.
	currentPlaybooks := b.playbooks
	if b.config.PlaybooksDir != "" {
		var err error
		currentPlaybooks, err = playbook.List(b.config.PlaybooksDir)
		if err != nil {
			log.Printf("slackbot: reload playbooks: %v", err)
			currentPlaybooks = nil
		}
	}
	var planPrompt string
	if trigger.source == messageTrigger {
		planPrompt = prompt.BuildMessagePlanPrompt(memoryContext, currentPlaybooks, slackThread, trigger.authorID, trigger.message)
	} else {
		planPrompt = prompt.BuildPlanPrompt(memoryContext, currentPlaybooks, slackThread, trigger.message)
	}
	planResult, runErr := b.runTurn(ctx, threadID, "read-only-network", b.config.WorkspaceDir, nil, planPrompt, func(id string) error {
		if err := b.store.SetThread(threadKey, id); err != nil {
			return fmt.Errorf("persist plan thread: %w", err)
		}
		return nil
	})
	lock.Unlock()

	if runErr != nil || planResult == nil || !planResult.Completed {
		if runErr != nil {
			log.Printf("slackbot: plan turn %q: %v", eventKey, runErr)
		} else if planResult != nil {
			log.Printf("slackbot: plan turn %q incomplete: %s", eventKey, planResult.Err)
		}
		b.fail(ctx, eventKey, state.Planning, state.Failed, channel, threadTS, timestamp, planFailureMessage)
		return
	}
	if len(planResult.Messages) == 0 {
		b.finishFailClosed(ctx, eventKey, channel, threadTS, timestamp, failClosedMessage)
		return
	}

	planText := codex.SanitizeSlackOutput(planResult.Messages[len(planResult.Messages)-1])
	policy, instruction, err := codex.ParsePlan(planText)
	if err != nil {
		log.Printf("slackbot: parse plan %q: %v", eventKey, err)
		b.finishFailClosed(ctx, eventKey, channel, threadTS, timestamp, planText+"\n\n"+failClosedMessage)
		return
	}
	if err := b.store.Transition(eventKey, state.Planning, state.PlanPosted); err != nil {
		log.Printf("slackbot: persist posted plan %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Planning, state.Failed, channel, threadTS, timestamp, planFailureMessage)
		return
	}
	if instruction == "" {
		if err := b.post(ctx, channel, threadTS, policy); err != nil {
			log.Printf("slackbot: post policy %q: %v", eventKey, err)
			b.fail(ctx, eventKey, state.PlanPosted, state.Failed, channel, threadTS, timestamp, planFailureMessage)
			return
		}
		if err := b.store.Transition(eventKey, state.PlanPosted, state.Done); err != nil {
			log.Printf("slackbot: finish NONE %q: %v", eventKey, err)
			b.fail(ctx, eventKey, state.PlanPosted, state.Failed, channel, threadTS, timestamp, planFailureMessage)
			return
		}
		b.finalReaction(ctx, channel, timestamp, true)
		return
	}
	if err := b.store.Transition(eventKey, state.PlanPosted, state.Working); err != nil {
		log.Printf("slackbot: start work %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.PlanPosted, state.Failed, channel, threadTS, timestamp, planFailureMessage)
		return
	}
	// A work plan is intentionally not posted: Slack would clear the progress
	// status when processing that reply. Refresh the status until work completes.
	stopWorkingStatus := b.keepStatus(ctx, channel, threadTS, workingStatus)
	defer stopWorkingStatus()

	// workMu serializes work turns and the memory append that follows them.
	// memoryMu is only held around the memory access itself, so a plan turn can
	// read memory while a work turn runs; when both are needed the order is
	// always workMu then memoryMu. The memory directory is intentionally not a
	// writable root: the agent proposes memory entries through the output
	// contract and the bot writes them.
	b.workMu.Lock()
	b.memoryMu.RLock()
	workMemoryContext, memErr := memory.ReadContext(b.config.MemoryDir, channel)
	b.memoryMu.RUnlock()
	if memErr != nil {
		log.Printf("slackbot: refresh memory before work: %v", memErr)
	}
	workPrompt := prompt.BuildWorkPrompt(instruction, workMemoryContext)
	workResult, workErr := b.runTurn(ctx, "", "workspace-write", b.config.WorkspaceDir, b.config.WritableRoots, workPrompt, nil)
	var resultText string
	var memoryAppends codex.MemoryAppends
	if workErr == nil && workResult != nil && workResult.Completed && len(workResult.Messages) > 0 {
		var memoryOutputValid bool
		resultText, memoryAppends, memoryOutputValid = codex.SplitMemoryAppends(workResult.Messages[len(workResult.Messages)-1])
		resultText = codex.SanitizeSlackOutput(resultText)
		if !memoryOutputValid {
			log.Printf("slackbot: ignore malformed scoped memory output %q", eventKey)
		}
	}
	var updatedMemoryScopes []string
	if memoryAppends != (codex.MemoryAppends{}) {
		targets := []struct {
			scope memory.Scope
			label string
			entry string
		}{
			{scope: memory.ScopeGlobal, label: "全体", entry: memoryAppends.Global},
			{scope: memory.ScopeChannel, label: "チャンネル", entry: memoryAppends.Channel},
		}
		b.memoryMu.Lock()
		for _, target := range targets {
			if target.entry == "" {
				continue
			}
			written, err := memory.AppendScoped(
				b.config.MemoryDir, target.scope, channel, target.entry,
			)
			if err != nil {
				log.Printf("slackbot: append %s memory %q: %v", target.scope, eventKey, err)
				continue
			}
			if written != "" {
				updatedMemoryScopes = append(updatedMemoryScopes, target.label)
			}
		}
		b.memoryMu.Unlock()
	}
	b.workMu.Unlock()

	if workErr != nil || workResult == nil || !workResult.Completed || len(workResult.Messages) == 0 {
		if workErr != nil {
			log.Printf("slackbot: work turn %q: %v", eventKey, workErr)
		}
		b.fail(ctx, eventKey, state.Working, state.Interrupted, channel, threadTS, timestamp, workFailureMessage)
		return
	}
	if resultText == "" {
		resultText = "作業が完了しました。"
	}
	if len(updatedMemoryScopes) > 0 {
		resultText += "\n\n📝 " + strings.Join(updatedMemoryScopes, "・") + "メモリを更新しました。"
	}
	if err := b.post(ctx, channel, threadTS, resultText); err != nil {
		log.Printf("slackbot: post work result %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Working, state.Interrupted, channel, threadTS, timestamp, workFailureMessage)
		return
	}
	if err := b.store.Transition(eventKey, state.Working, state.Done); err != nil {
		log.Printf("slackbot: finish work %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Working, state.Interrupted, channel, threadTS, timestamp, workFailureMessage)
		return
	}
	b.finalReaction(ctx, channel, timestamp, true)
}

func (b *Bot) forbidden(ctx context.Context, event *slackevents.AppMentionEvent) {
	contact := "@seiji"
	if b.config.AdminUserID != "" {
		contact = "<@" + b.config.AdminUserID + ">"
	}
	threadTS := event.ThreadTimeStamp
	if threadTS == "" {
		threadTS = event.TimeStamp
	}
	if err := b.post(ctx, event.Channel, threadTS, fmt.Sprintf(forbiddenMessage, contact)); err != nil {
		log.Printf("slackbot: post forbidden response: %v", err)
	}
}

func validWorkflowID(id string) bool {
	if len(id) <= 2 || !strings.HasPrefix(id, "Wf") {
		return false
	}
	for _, char := range id[2:] {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func workflowIDFromPayload(payload json.RawMessage) string {
	var envelope struct {
		Event struct {
			WorkflowID string `json:"workflow_id"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ""
	}
	return envelope.Event.WorkflowID
}

func (b *Bot) startThreadSubscription(ctx context.Context, channel, threadTS string) {
	marker := b.config.ThreadSubscriptionReaction
	if marker == "" {
		return
	}
	var marked bool
	err := b.retrySlack(ctx, func() error {
		var err error
		marked, err = b.api.HasReaction(ctx, channel, threadTS, marker)
		return err
	})
	if err != nil {
		log.Printf("slackbot: read subscription marker %q: %v", channel+":"+threadTS, err)
		return
	}
	if !marked {
		return
	}
	now := b.now()
	if err := b.store.SetSubscription(channel+":"+threadTS, now, now.Add(b.config.ThreadSubscriptionTTL)); err != nil {
		log.Printf("slackbot: save subscription %q: %v", channel+":"+threadTS, err)
	}
}

func (b *Bot) runTurn(ctx context.Context, threadID, sandbox, cwd string, roots []string, text string, callback func(string) error) (*codex.TurnResult, error) {
	turnCtx, cancel := context.WithTimeout(ctx, b.config.CodexTimeout)
	defer cancel()
	return b.runner.Run(turnCtx, threadID, sandbox, cwd, roots, text, callback)
}

func (b *Bot) finishFailClosed(ctx context.Context, eventKey, channel, threadTS, timestamp, text string) {
	if err := b.post(ctx, channel, threadTS, text); err != nil {
		log.Printf("slackbot: post fail-closed plan %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Planning, state.Failed, channel, threadTS, timestamp, planFailureMessage)
		return
	}
	if err := b.store.Transition(eventKey, state.Planning, state.Done); err != nil {
		log.Printf("slackbot: finish fail-closed %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Planning, state.Failed, channel, threadTS, timestamp, planFailureMessage)
		return
	}
	b.finalReaction(ctx, channel, timestamp, true)
}

func (b *Bot) fail(ctx context.Context, eventKey string, from, to state.State, channel, threadTS, timestamp, message string) {
	if err := b.store.Transition(eventKey, from, to); err != nil {
		log.Printf("slackbot: transition %q to %s: %v", eventKey, to, err)
	}
	if err := b.post(ctx, channel, threadTS, message); err != nil {
		log.Printf("slackbot: post failure %q: %v", eventKey, err)
	}
	b.finalReaction(ctx, channel, timestamp, false)
}

// All done outcomes, including fail-closed and NONE, receive ✅. Only failed
// and interrupted outcomes receive ❌.
func (b *Bot) finalReaction(ctx context.Context, channel, timestamp string, success bool) {
	name := "x"
	if success {
		name = "white_check_mark"
	}
	// Add the terminal reaction first so a transient API failure cannot leave
	// the message with no status reaction.
	b.addReaction(ctx, channel, timestamp, name)
	if err := b.api.RemoveReaction(ctx, channel, timestamp, "eyes"); err != nil {
		log.Printf("slackbot: remove eyes reaction: %v", err)
	}
}

func (b *Bot) addReaction(ctx context.Context, channel, timestamp, name string) {
	if err := b.api.AddReaction(ctx, channel, timestamp, name); err != nil {
		log.Printf("slackbot: add %s reaction: %v", name, err)
	}
}

func (b *Bot) setStatus(ctx context.Context, channel, threadTS, status string) {
	if err := b.api.SetStatus(ctx, channel, threadTS, status); err != nil {
		log.Printf("slackbot: set thread status %q: %v", status, err)
	}
}

func (b *Bot) keepStatus(ctx context.Context, channel, threadTS, status string) func() {
	statusCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	b.setStatus(ctx, channel, threadTS, status)
	go func() {
		defer close(done)
		for {
			if err := b.sleep(statusCtx, statusRefreshDelay); err != nil {
				return
			}
			b.setStatus(statusCtx, channel, threadTS, status)
		}
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}
}

func (b *Bot) clearStatus(ctx context.Context, channel, threadTS string) {
	// Shutdown cancellation should not leave a stale loading indicator behind.
	clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	b.setStatus(clearCtx, channel, threadTS, "")
}

func (b *Bot) post(ctx context.Context, channel, threadTS, text string) error {
	for _, chunk := range splitMessage(text, maxSlackMessageRunes) {
		if err := b.retrySlack(ctx, func() error {
			_, err := b.api.PostMessage(ctx, channel, threadTS, chunk)
			return err
		}); err != nil {
			return fmt.Errorf("post Slack message: %w", err)
		}
	}
	return nil
}

func (b *Bot) retrySlack(ctx context.Context, operation func() error) error {
	err := operation()
	if err == nil || !retryable(err) {
		return err
	}
	if rateLimited, ok := errors.AsType[*slack.RateLimitedError](err); ok {
		if err := b.sleep(ctx, rateLimited.RetryAfter); err != nil {
			return fmt.Errorf("wait for Slack retry: %w", err)
		}
	}
	return operation()
}

func (b *Bot) threadLock(key string) *sync.Mutex {
	b.threadMu.Lock()
	defer b.threadMu.Unlock()
	lock := b.threadLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		b.threadLocks[key] = lock
	}
	return lock
}

func splitMessage(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}
	chunks := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > 0 {
		n := min(len(runes), limit)
		chunks = append(chunks, string(runes[:n]))
		runes = runes[n:]
	}
	return chunks
}

func formatThreadContext(messages []ThreadMessage, currentTimestamp string) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.Timestamp == currentTimestamp || strings.TrimSpace(message.Text) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s / %s]\n%s", message.Timestamp, message.AuthorID, message.Text))
	}
	return truncateThreadContext(strings.Join(parts, "\n\n"), maxThreadContextRunes)
}

func truncateThreadContext(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	const omission = "\n\n... 長いスレッドの中間を省略 ...\n\n"
	omissionRunes := []rune(omission)
	available := limit - len(omissionRunes)
	if available <= 0 {
		return string(runes[:limit])
	}
	head := available / 3
	tail := available - head
	return string(runes[:head]) + omission + string(runes[len(runes)-tail:])
}

func retryable(err error) bool {
	if _, ok := errors.AsType[*slack.RateLimitedError](err); ok {
		return true
	}
	var status interface{ HTTPStatusCode() int }
	return errors.As(err, &status) && (status.HTTPStatusCode() == 429 || status.HTTPStatusCode() >= 500)
}

func stripBotMention(text, botUserID string) string {
	if botUserID != "" {
		text = strings.ReplaceAll(text, "<@"+botUserID+">", "")
	}
	return strings.TrimSpace(text)
}

func makeSet(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type webAPI struct {
	client *slack.Client
}

func (w *webAPI) PostMessage(ctx context.Context, channel, threadTS, text string) (string, error) {
	_, timestamp, err := w.client.PostMessageContext(ctx, channel, slack.MsgOptionText(text, false), slack.MsgOptionTS(threadTS))
	return timestamp, err
}

func (w *webAPI) GetThreadMessages(ctx context.Context, channel, threadTS, latest string) ([]ThreadMessage, error) {
	var result []ThreadMessage
	cursor := ""
	for {
		messages, _, nextCursor, err := w.client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadTS,
			Latest:    latest,
			Inclusive: true,
			Limit:     200,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("get Slack thread replies: %w", err)
		}
		for _, message := range messages {
			result = append(result, ThreadMessage{
				AuthorID:  message.User,
				Timestamp: message.Timestamp,
				Text:      message.Text,
			})
		}
		if nextCursor == "" || nextCursor == cursor {
			return result, nil
		}
		cursor = nextCursor
	}
}

func (w *webAPI) SetStatus(ctx context.Context, channel, threadTS, status string) error {
	return w.client.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: channel,
		ThreadTS:  threadTS,
		Status:    status,
	})
}

func (w *webAPI) HasReaction(ctx context.Context, channel, timestamp, reaction string) (bool, error) {
	reactions, err := w.client.GetReactionsContext(ctx, slack.ItemRef{Channel: channel, Timestamp: timestamp}, slack.GetReactionsParameters{})
	if err != nil {
		return false, err
	}
	for _, itemReaction := range reactions {
		if itemReaction.Name == reaction {
			return true, nil
		}
	}
	return false, nil
}

func (w *webAPI) AddReaction(ctx context.Context, channel, timestamp, name string) error {
	return w.client.AddReactionContext(ctx, name, slack.ItemRef{Channel: channel, Timestamp: timestamp})
}

func (w *webAPI) RemoveReaction(ctx context.Context, channel, timestamp, name string) error {
	return w.client.RemoveReactionContext(ctx, name, slack.ItemRef{Channel: channel, Timestamp: timestamp})
}

// RunSocketMode connects a Bot to Slack Socket Mode. It acknowledges every
// envelope before dispatching app mentions and ordinary messages separately.
func RunSocketMode(acceptCtx, turnCtx context.Context, botToken, appToken string, bot *Bot, wg *sync.WaitGroup) error {
	client := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	auth, err := client.AuthTestContext(acceptCtx)
	if err != nil {
		return fmt.Errorf("authenticate Slack bot: %w", err)
	}
	bot.api = &webAPI{client: client}
	bot.config.BotUserID = auth.UserID
	socketClient := socketmode.New(client)
	runErr := make(chan error, 1)
	go func() {
		runErr <- socketClient.RunContext(acceptCtx)
	}()

	for {
		select {
		case <-acceptCtx.Done():
			return nil
		case err := <-runErr:
			if acceptCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("run Slack Socket Mode: %w", err)
		case event, ok := <-socketClient.Events:
			if !ok {
				return errors.New("Slack Socket Mode event channel closed")
			}
			if event.Request != nil {
				socketClient.Ack(*event.Request)
			}
			if event.Type != socketmode.EventTypeEventsAPI {
				continue
			}
			apiEvent, ok := event.Data.(slackevents.EventsAPIEvent)
			if !ok {
				continue
			}
			switch innerEvent := apiEvent.InnerEvent.Data.(type) {
			case *slackevents.AppMentionEvent:
				workflowID := ""
				if event.Request != nil {
					workflowID = workflowIDFromPayload(event.Request.Payload)
				}
				wg.Go(func() {
					bot.handleMention(turnCtx, innerEvent, workflowID)
				})
			case *slackevents.MessageEvent:
				wg.Go(func() {
					bot.HandleMessage(turnCtx, innerEvent)
				})
			}
		}
	}
}
