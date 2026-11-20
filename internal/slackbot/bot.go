package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/n-seiji/ebi-x/internal/attachment"
	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/memory"
	"github.com/n-seiji/ebi-x/internal/playbook"
	"github.com/n-seiji/ebi-x/internal/prompt"
	"github.com/n-seiji/ebi-x/internal/slackfmt"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/n-seiji/ebi-x/internal/workspace"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

const (
	// Slack renders at most 12,000 characters in one markdown block. The
	// margin covers characters that count as more than one and the fence
	// lines Split repeats across a cut, and keeps one reply readable.
	maxSlackMessageRunes    = 8000
	maxThreadContextRunes   = 12000
	claimFailureMessage     = "受付に失敗しました。お手数ですが、もう一度 mention してください。"
	threadFailureMessage    = "⚠️ スレッドの読み込みに失敗したため、作業は開始していません。もう一度 mention してください。"
	workStartFailureMessage = "⚠️ 作業を開始できなかったため、作業は行っていません。もう一度 mention してください。"
	workFailureMessage      = "⚠️ 作業が完了したことを確認できませんでした。状況を確認し、新しい mention で依頼し直してください。"
	forbiddenMessage        = "🔒 %sには ebi-x を使う権限がありません。使いたい場合は %s に許可を頼んでください。"
	attachmentFailureNotice = "⚠️ 次のファイルを添付できませんでした。生成済みのファイルは作業領域に残しています。再送する場合は「添付を再送して」と依頼してください。"
	attachmentOutputNotice  = "⚠️ 添付ファイルの指定を読み取れなかったため、ファイルは添付していません。再送する場合は「添付を再送して」と依頼してください。"
	workingStatus           = "が作業を進めています…"
	queuedStatus            = "が作業の順番を待っています…"
	stoppedMessage          = "⏹️ 依頼により作業を停止しました。途中までの変更が作業領域に残っている場合があります。続ける場合は、新しい mention で依頼してください。"
	nothingToStopMessage    = "停止する作業はありません。"
	statusRefreshDelay      = 80 * time.Second
	statusMinInterval       = 3 * time.Second
)

var errStoppedByUser = errors.New("stopped by user")

type SlackAPI interface {
	PostMessage(ctx context.Context, channel, threadTS, text string) (string, error)
	GetThreadMessages(ctx context.Context, channel, threadTS, latest string) ([]ThreadMessage, error)
	GetMessage(ctx context.Context, channel, threadTS, timestamp string) (ThreadMessage, error)
	ChannelInfo(ctx context.Context, channel string) (ChannelInfo, error)
	DownloadFile(ctx context.Context, url string, w io.Writer) error
	SetStatus(ctx context.Context, channel, threadTS, status string) error
	HasReaction(ctx context.Context, channel, timestamp, reaction string) (bool, error)
	ReactionUsers(ctx context.Context, channel, timestamp, reaction string) ([]string, error)
	ChannelMembers(ctx context.Context, channel string) ([]string, error)
	UserGroupMembers(ctx context.Context, group string) ([]string, error)
	People(ctx context.Context, users []string) ([]string, error)
	AddReaction(ctx context.Context, channel, timestamp, name string) error
	RemoveReaction(ctx context.Context, channel, timestamp, name string) error
	UploadFile(ctx context.Context, channel, threadTS, filename string, size int64, content io.Reader) error
	SetSuggestedPrompts(ctx context.Context, channel string, prompts []suggestedPrompt) error
	PublishHome(ctx context.Context, userID string, sections []string) error
	Permalink(ctx context.Context, channel, timestamp string) (string, error)
	OpenRequestModal(ctx context.Context, triggerID, metadata, quoted string) error
	PostEphemeral(ctx context.Context, channel, user, text string) error
}

type ThreadMessage struct {
	AuthorID  string
	Timestamp string
	Text      string
	Files     []SlackFile
}

type SlackFile struct {
	Name     string
	Mimetype string
	Size     int
	// Reading it needs files:read.
	URL string
}

type Store interface {
	ClaimEvent(eventKey string) (bool, error)
	Transition(eventKey string, from, to state.State) error
	GetThread(threadKey string) (string, bool)
	SetThread(threadKey, threadID string) error
	GetSubscription(threadKey string) (state.Subscription, bool)
	SetSubscription(threadKey string, startedAt, expiresAt time.Time) error
	DeleteSubscriptionIfExpired(threadKey string, now time.Time) (bool, error)
	GetFollowUp(threadKey string) (state.FollowUp, bool)
	SetFollowUp(threadKey string, followUp state.FollowUp) error
	DeleteFollowUp(threadKey string) (state.FollowUp, bool, error)
	TakeDueFollowUps(now time.Time) ([]state.FollowUp, error)
	FollowUps() []state.FollowUp
	SetSchedule(threadKey string, schedule state.Schedule) error
	DeleteSchedule(threadKey string) (state.Schedule, bool, error)
	TakeDueSchedules(now time.Time, next func(state.Schedule) time.Time) ([]state.Schedule, error)
	Schedules() []state.Schedule
	AddReminder(reminder state.Reminder) error
	DeleteReminders(match func(state.Reminder) bool) ([]state.Reminder, error)
	TakeDueReminders(now time.Time) ([]state.Reminder, error)
	Reminders() []state.Reminder
	PullWatches() []state.PullWatch
	GetPullWatch(key string) (state.PullWatch, bool)
	SetPullWatch(watch state.PullWatch) error
	DeletePullWatches(match func(state.PullWatch) bool) ([]state.PullWatch, error)
	DoneWatches() []state.DoneWatch
	SetDoneWatch(watch state.DoneWatch) error
	UpdateDoneWatch(key string, update func(*state.DoneWatch) bool) (state.DoneWatch, bool, error)
	DeleteDoneWatches(match func(state.DoneWatch) bool) ([]state.DoneWatch, error)
	Usage() state.Usage
	SetUsage(usage state.Usage) error
	AddDeferred(deferred state.Deferred) error
	TakeDeferred() ([]state.Deferred, error)
	DeleteDeferred(match func(state.Deferred) bool) (int, error)
}

type Runner interface {
	Run(ctx context.Context, threadID, sandbox, cwd string, writableRoots, deniedPaths []string, access codex.Access, text string, onThreadStarted func(string) error, onActivity func(codex.Activity)) (*codex.TurnResult, error)
}

type Workspaces interface {
	Acquire(ctx context.Context, threadID string, area workspace.Area, createCheckouts bool) (*workspace.Lease, error)
	Denied(area workspace.Area) ([]string, error)
}

type Config struct {
	AllowedUserIDs             []string
	AllowedChannelIDs          []string
	ApprovalChannelID          string
	ApproverUserIDs            []string
	Approvals                  Approvals
	AllowWorkflows             bool
	AllowedWorkflowIDs         []string
	AdminUserID                string
	EBIXHome                   string
	WorkspaceDir               string
	MemoryDir                  string
	PlaybooksDir               string
	ChannelRulesFile           string
	ActionRulesFile            string
	CodexTimeout               time.Duration
	ThreadSubscriptionReaction string
	ThreadSubscriptionTTL      time.Duration
	BotUserID                  string
	WritableRoots              []string
	MaxParallelWork            int
	GitHub                     GitHub
	// Its own reviews and comments do not wake a watch.
	GitHubLogin string
	// A turn could otherwise watch any pull request the token can read and
	// relay it into its channel.
	GitHubRepos []string
	Workspaces  Workspaces
	Usage       UsageReader
}

type Bot struct {
	api       SlackAPI
	store     Store
	runner    Runner
	config    Config
	playbooks []playbook.Playbook

	allowedUsers     map[string]struct{}
	allowedChannels  map[string]struct{}
	allowedWorkflows map[string]struct{}
	approvers        map[string]struct{}
	workSlots        chan struct{}
	memoryMu         sync.RWMutex
	threadMu         sync.Mutex
	threadLocks      map[string]*sync.Mutex
	workLocks        map[string]*sync.Mutex
	runningMu        sync.Mutex
	running          map[string]map[string]runningRequest
	usageMu          sync.Mutex
	adminAlerts      map[string]time.Time
	failureMu        sync.Mutex
	recentFailures   []time.Time
	drain            *drainer
	scopeMu          sync.Mutex
	scopes           map[string]cachedScope
	now              func() time.Time
	sleep            func(context.Context, time.Duration) error
}

type triggerSource uint8

const (
	mentionTrigger triggerSource = iota
	messageTrigger
	followUpTrigger
	pullWatchTrigger
	scheduleTrigger
)

type processingTrigger struct {
	source      triggerSource
	authorID    string
	channel     string
	timestamp   string
	threadTS    string
	message     string
	threadReply bool
	followUp    *state.FollowUp
}

func New(api SlackAPI, store Store, runner Runner, config Config, playbooks []playbook.Playbook) *Bot {
	config.WritableRoots = append([]string(nil), config.WritableRoots...)
	if config.Workspaces == nil {
		config.Workspaces = sharedWorkspaces{dir: config.WorkspaceDir, roots: config.WritableRoots}
	}
	b := &Bot{
		api:              api,
		store:            store,
		runner:           runner,
		config:           config,
		playbooks:        append([]playbook.Playbook(nil), playbooks...),
		allowedUsers:     makeSet(config.AllowedUserIDs),
		allowedChannels:  makeSet(config.AllowedChannelIDs),
		allowedWorkflows: makeSet(config.AllowedWorkflowIDs),
		approvers:        makeSet(config.ApproverUserIDs),
		workSlots:        make(chan struct{}, max(config.MaxParallelWork, 1)),
		drain:            newDrainer(),
		threadLocks:      make(map[string]*sync.Mutex),
		workLocks:        make(map[string]*sync.Mutex),
		running:          make(map[string]map[string]runningRequest),
		now:              time.Now,
		sleep:            sleepContext,
	}
	return b
}

func (b *Bot) HandleMention(ctx context.Context, event *slackevents.AppMentionEvent) {
	b.handleMention(ctx, event, "")
}

func (b *Bot) handleMention(ctx context.Context, event *slackevents.AppMentionEvent, workflowID string) {
	if event == nil || event.Edited != nil || event.User == b.config.BotUserID {
		return
	}
	if event.BotID == "" && isAliveQuestion(stripBotMention(event.Text, b.config.BotUserID)) {
		b.answerAliveQuestion(ctx, event.Channel, mentionThreadTS(event))
		return
	}
	// In the approval channel, request threads and the approval words decide
	// requests; everything else is ordinary work that may also change
	// playbooks and global memory.
	if b.approvalsEnabled() && event.Channel == b.config.ApprovalChannelID {
		if event.BotID != "" {
			return
		}
		if b.isApprovalCommand(event) {
			b.handleApprovalCommand(ctx, event)
			return
		}
	}
	var missing []approvalSubject
	if event.BotID == "" {
		if !b.userAllowed(event.User) {
			missing = append(missing, approvalSubject{kind: state.ApprovalUser, id: event.User})
		}
	} else if !b.config.AllowWorkflows || !validWorkflowID(workflowID) {
		log.Printf("slackbot: rejecting bot %q with workflow %q", event.BotID, workflowID)
		b.forbidden(ctx, event, []approvalSubject{{kind: state.ApprovalWorkflow, id: workflowID}})
		return
	} else if !b.workflowAllowed(workflowID) {
		missing = append(missing, approvalSubject{kind: state.ApprovalWorkflow, id: workflowID})
	}
	if !b.channelAllowed(event.Channel) {
		missing = append(missing, approvalSubject{kind: state.ApprovalChannel, id: event.Channel})
	}
	if len(missing) > 0 {
		log.Printf("slackbot: rejecting mention by %q (bot %q, workflow %q) in %q", event.User, event.BotID, workflowID, event.Channel)
		b.rejectUnapproved(ctx, event, missing)
		return
	}
	threadTS := event.ThreadTimeStamp
	if threadTS == "" {
		threadTS = event.TimeStamp
	}

	message := stripBotMention(event.Text, b.config.BotUserID)
	if message == "" {
		return
	}
	if isStopCommand(message) {
		b.stopThread(ctx, event.Channel, threadTS, event.TimeStamp)
		return
	}
	// Reminding people to react is the bot's own job, not the agent's, so
	// these commands never start a turn. Workflows cannot start a watch,
	// since it mentions people on its own for days.
	if isDoneWatchStopCommand(message) {
		b.stopDoneWatchCommand(ctx, event.Channel, threadTS)
		return
	}
	if days, ok := parseDoneWatchCommand(event.Text, b.config.BotUserID); ok && event.BotID == "" {
		b.startDoneWatch(ctx, event.Channel, threadTS, event.TimeStamp, event.Text, event.User, days)
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

func (b *Bot) HandleMessage(ctx context.Context, event *slackevents.MessageEvent) {
	if event != nil && event.ChannelType == "im" {
		b.handleDirectMessage(ctx, event)
		return
	}
	if event == nil ||
		(event.ChannelType != "channel" && event.ChannelType != "group") ||
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
	if !b.channelAllowed(event.Channel) {
		return
	}
	// A subscription lets allowed users talk without mentioning the bot; it
	// does not extend the bot to everyone who can reply in the thread.
	if !b.userAllowed(event.User) {
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

	if isStopCommand(event.Text) {
		b.stopThread(ctx, event.Channel, event.ThreadTimeStamp, event.TimeStamp)
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
	// While draining for a restart, a request that has not started is saved
	// for the next process instead of keeping this one alive.
	finish, ok := b.beginRequest()
	if !ok {
		b.deferRequest(ctx, trigger)
		return
	}
	defer finish()
	// Slack calls keep ctx, so a stop never keeps the bot from reporting.
	stopCtx, untrack := b.trackRequest(ctx, threadRef(channel, threadTS), eventKey, trigger.authorID)
	defer untrack()
	failure := func(message string) string {
		if errors.Is(context.Cause(stopCtx), errStoppedByUser) {
			return stoppedMessage
		}
		return message
	}
	if trigger.source == mentionTrigger {
		b.startThreadSubscription(ctx, channel, threadTS)
	}
	b.addReaction(ctx, channel, timestamp, "eyes")

	workspaceID, err := workspace.ThreadID(channel, threadTS)
	if err != nil {
		log.Printf("slackbot: prepare thread workspace %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Received, state.Failed, channel, threadTS, timestamp, workStartFailureMessage)
		return
	}

	// Hold the lock through execution and delivery so replies see the preceding
	// result and never resume a session concurrently. The version prefix keeps
	// sessions with the retired planning contract from resuming.
	threadKey := "v5:" + channel + ":" + threadTS
	lock := b.keyedLock(b.threadLocks, threadKey)
	lock.Lock()
	defer lock.Unlock()
	if err := stopCtx.Err(); err != nil {
		b.fail(ctx, eventKey, state.Received, state.Failed, channel, threadTS, timestamp, failure(workStartFailureMessage))
		return
	}
	defer b.clearStatus(ctx, channel, threadTS)
	threadID, hasThread := b.store.GetThread(threadKey)
	var slackThread string
	var threadMessages []ThreadMessage
	if !hasThread && trigger.threadReply {
		var err error
		threadMessages, err = b.api.GetThreadMessages(ctx, channel, threadTS, timestamp)
		if err != nil {
			log.Printf("slackbot: read thread context %q: %v", eventKey, err)
			b.fail(ctx, eventKey, state.Received, state.Failed, channel, threadTS, timestamp, threadFailureMessage)
			return
		}
		slackThread = formatThreadContext(threadMessages, timestamp)
	}
	files := b.sharedFiles(ctx, trigger, threadMessages)
	scope := b.channelScope(ctx, channel)
	output, started, workErr := b.work(ctx, stopCtx, eventKey, channel, threadTS, threadKey, workspaceID, threadID, slackThread, files, scope, trigger)
	if !started && errors.Is(workErr, errDraining) && stopCtx.Err() == nil {
		b.deferRequest(ctx, trigger)
		return
	}
	if !started {
		log.Printf("slackbot: start work %q: %v", eventKey, workErr)
		b.noteWorkFailure(ctx, workErr)
		b.fail(ctx, eventKey, state.Received, state.Failed, channel, threadTS, timestamp, failure(workStartFailureMessage))
		return
	}
	if workErr != nil {
		log.Printf("slackbot: work turn %q: %v", eventKey, workErr)
		message := workFailureMessage
		switch {
		case errors.Is(workErr, codex.ErrUsageLimit):
			message = b.usageLimitFailure(ctx)
		case errors.Is(workErr, codex.ErrNotSignedIn):
			message = b.authFailure(ctx)
		default:
			// The usage limit and sign-in have their own alerts.
			b.noteWorkFailure(ctx, workErr)
		}
		b.fail(ctx, eventKey, state.Working, state.Interrupted, channel, threadTS, timestamp, failure(message))
		return
	}
	resultText := output.text
	if resultText == "" {
		resultText = "作業が完了しました。"
	}
	if output.memoryUpdated {
		resultText += "\n\n📝 全体メモリを更新しました。"
	}
	if output.playbookNoteNotice != "" {
		resultText += "\n\n" + output.playbookNoteNotice
	}
	if output.playbookFold != nil {
		resultText += "\n" + output.playbookFold.notice()
	}
	if notice := b.scheduleFollowUp(trigger, output); notice != "" {
		resultText += "\n\n" + notice
	}
	if notice := b.applySchedule(trigger, output); notice != "" {
		resultText += "\n\n" + notice
	}
	if notice := b.applyReminder(trigger, output); notice != "" {
		resultText += "\n\n" + notice
	}
	if notice := b.watchPulls(ctx, trigger, output.pullURLs); notice != "" {
		resultText += "\n\n" + notice
	}
	// The deliverable that did not reach Slack must not look successful. The
	// files stay in the work area for a resend request.
	final := state.Done
	if notice := output.attachmentNotice(); notice != "" {
		resultText += "\n\n" + notice
		final = state.Interrupted
	}
	if output.waiting {
		resultText += "\n\n" + b.waitingNotice(trigger.authorID, channel, threadTS, output.questions)
	}
	if err := b.post(ctx, channel, threadTS, resultText); err != nil {
		log.Printf("slackbot: post work result %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Working, state.Interrupted, channel, threadTS, timestamp, workFailureMessage)
		return
	}
	b.recordPlaybookFold(ctx, output.playbookFold, channel, threadTS)
	if output.userSettingsNotice != "" {
		if err := b.api.PostEphemeral(ctx, channel, trigger.authorID, output.userSettingsNotice); err != nil {
			log.Printf("slackbot: post user settings notice %q: %v", eventKey, err)
		}
	}
	if err := b.store.Transition(eventKey, state.Working, final); err != nil {
		log.Printf("slackbot: finish work %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Working, state.Interrupted, channel, threadTS, timestamp, workFailureMessage)
		return
	}
	reaction := finishedReaction
	switch {
	case final != state.Done:
		reaction = failedReaction
	case output.waiting:
		reaction = waitingReaction
	}
	b.finalReaction(ctx, channel, timestamp, reaction)
}

type workOutput struct {
	text                    string
	memoryUpdated           bool
	userSettingsNotice      string
	attachmentOutputInvalid bool
	failedAttachments       []attachment.Rejection
	followUp                *codex.FollowUpRequest
	followUpInvalid         bool
	schedule                *codex.ScheduleRequest
	scheduleInvalid         bool
	reminder                *codex.ReminderRequest
	reminderInvalid         bool
	waiting                 bool
	questions               []string
	pullURLs                []string
	playbookFold            *playbookFold
	playbookNoteNotice      string
}

// Without it the posted result would read as a complete delivery when it was
// not.
func (o workOutput) attachmentNotice() string {
	if o.attachmentOutputInvalid {
		return attachmentOutputNotice
	}
	if len(o.failedAttachments) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(attachmentFailureNotice)
	for _, failed := range o.failedAttachments {
		fmt.Fprintf(&builder, "\n- `%s`: %s", failed.Path, failed.Reason)
	}
	return builder.String()
}

// started false means nothing was run and the event may be retried.
func (b *Bot) work(ctx, stopCtx context.Context, eventKey, channel, threadTS, threadKey, workspaceID, threadID, slackThread string, files []ThreadMessage, scope channelScope, trigger processingTrigger) (output workOutput, started bool, err error) {
	// Work still waiting for its turn when a restart begins is deferred
	// rather than started, so the restart only waits for running turns.
	if b.draining() {
		return workOutput{}, false, errDraining
	}
	rule, err := b.channelRule(channel)
	if err != nil {
		return workOutput{}, false, err
	}
	scope.rule = rule
	waitCtx, stopWaiting := b.untilDrain(stopCtx)
	release, err := b.acquireWork(waitCtx, threadKey, channel, threadTS)
	if err != nil {
		cause := context.Cause(waitCtx)
		stopWaiting()
		if errors.Is(cause, errDraining) {
			return workOutput{}, false, errDraining
		}
		return workOutput{}, false, err
	}
	stopWaiting()
	defer release()

	// Checkouts are cloned only when the turn asks for them, so a request
	// that only reads code or answers a question does not create any.
	lease, err := b.config.Workspaces.Acquire(stopCtx, workspaceID, scope.area, false)
	if err != nil {
		return workOutput{}, false, fmt.Errorf("prepare workspace: %w", err)
	}
	defer func() { lease.Release() }()
	scope.hidden = rule.limitLease(lease)
	sharedWritable := b.sharedWritable(channel)
	trigger.message += b.saveSharedFiles(ctx, lease, files)
	trigger.message += scope.note()

	// A new session receives the memory, playbook catalog, and rules once;
	// a resumed session already holds them, so only the request is sent.
	var turnPrompt string
	settings := b.readUserSettings(trigger.authorID)
	if threadID == "" {
		currentPlaybooks, err := b.currentPlaybooks()
		if err != nil {
			return workOutput{}, false, fmt.Errorf("reload playbooks: %w", err)
		}
		// memoryMu is only held around the memory access itself, so other
		// turns can read memory while this one runs. The memory directory is
		// intentionally not a writable root: the agent proposes memory
		// entries through the output contract and the bot writes them.
		b.memoryMu.RLock()
		memoryContext, memErr := memory.ReadContext(b.config.MemoryDir)
		b.memoryMu.RUnlock()
		if memErr != nil {
			log.Printf("slackbot: read memory: %v", memErr)
		}
		turnPrompt = prompt.BuildTurnPrompt(memoryContext, currentPlaybooks, readActionRules(b.config.ActionRulesFile), slackThread, trigger.authorID, trigger.message, settings, lease.Checkouts, lease.PendingRepos, sharedWritable, b.pullWatchEnabled())
	} else if trigger.source == pullWatchTrigger {
		turnPrompt = prompt.BuildPullWatchPrompt(trigger.message, lease.Checkouts, lease.PendingRepos)
	} else if trigger.followUp != nil {
		turnPrompt = prompt.BuildFollowUpPrompt(trigger.followUp.Task, trigger.followUp.DueAt, b.now(), trigger.followUp.Chain, lease.Checkouts, lease.PendingRepos)
	} else {
		turnPrompt = prompt.BuildResumePrompt(trigger.authorID, trigger.message, settings, lease.Checkouts, lease.PendingRepos, b.pendingFollowUp(threadRef(channel, threadTS)))
	}

	if err := b.store.Transition(eventKey, state.Received, state.Working); err != nil {
		return workOutput{}, false, err
	}
	status := b.startStatus(ctx, channel, threadTS, workingStatus)
	defer status.Stop()
	tracker := &progress{}
	onActivity := func(activity codex.Activity) {
		status.Update(tracker.observe(activity))
	}

	sessionID := threadID
	persistThread := func(id string) error {
		sessionID = id
		if err := b.store.SetThread(threadKey, id); err != nil {
			return fmt.Errorf("persist thread: %w", err)
		}
		return nil
	}
	finalText, err := b.runWorkTurn(stopCtx, threadID, lease, scope, sharedWritable, turnPrompt, persistThread, onActivity)
	if err != nil {
		return workOutput{}, true, err
	}
	finalText, checkoutsRequested := codex.SplitCheckoutRequest(finalText)
	if checkoutsRequested && len(lease.PendingRepos) > 0 {
		if sessionID == "" {
			return workOutput{}, true, errors.New("checkout request without a session to continue")
		}
		// Release is idempotent, so the deferred call is a no-op for this
		// lease if acquiring the next one fails.
		lease.Release()
		next, err := b.config.Workspaces.Acquire(stopCtx, workspaceID, scope.area, true)
		if err != nil {
			return workOutput{}, true, fmt.Errorf("prepare requested checkouts: %w", err)
		}
		lease = next
		scope.hidden = rule.limitLease(lease)
		finalText, err = b.runWorkTurn(stopCtx, sessionID, lease, scope, sharedWritable, prompt.BuildCheckoutsReadyPrompt(lease.Checkouts), persistThread, onActivity)
		if err != nil {
			return workOutput{}, true, err
		}
		finalText, _ = codex.SplitCheckoutRequest(finalText)
	}

	resultText, attachmentPaths, attachmentOutputValid := codex.SplitAttachments(finalText)
	if !attachmentOutputValid {
		log.Printf("slackbot: ignore malformed attachment output %q", eventKey)
		output.attachmentOutputInvalid = true
	}
	// The follow-up section is taken out before the memory sections, which
	// run to the end of the response.
	resultText, output.followUp, output.followUpInvalid = codex.SplitFollowUp(resultText)
	if output.followUpInvalid {
		log.Printf("slackbot: ignore malformed follow-up output %q", eventKey)
	}
	resultText, output.schedule, output.scheduleInvalid = codex.SplitSchedule(resultText)
	if output.scheduleInvalid {
		log.Printf("slackbot: ignore malformed schedule output %q", eventKey)
	}
	resultText, output.reminder, output.reminderInvalid = codex.SplitReminder(resultText)
	if output.reminderInvalid {
		log.Printf("slackbot: ignore malformed reminder output %q", eventKey)
	}
	resultText, output.questions, output.waiting = codex.SplitQuestions(resultText)
	resultText, output.pullURLs = codex.SplitPullWatches(resultText)
	resultText, userSettings, userSettingsPresent, userSettingsInvalid := codex.SplitUserSettings(resultText)
	if userSettingsInvalid {
		log.Printf("slackbot: ignore malformed user settings output %q", eventKey)
	} else if userSettingsPresent {
		output.userSettingsNotice = b.saveUserSettings(eventKey, trigger, userSettings)
	}
	resultText, playbookNote, playbookNoteInvalid := codex.SplitPlaybookNote(resultText)
	resultText, memoryAppends, memoryOutputValid := codex.SplitMemoryAppends(resultText)
	output.text = codex.SanitizeSlackOutput(resultText)
	if sharedWritable {
		if notice := b.channelRulesNotice(); notice != "" {
			output.text += "\n\n" + notice
		}
	}
	if !memoryOutputValid {
		log.Printf("slackbot: ignore malformed scoped memory output %q", eventKey)
	}
	if memoryAppends.Global != "" && !sharedWritable {
		log.Printf("slackbot: ignore global memory proposal from channel without shared writes %q", eventKey)
		memoryAppends.Global = ""
	}
	if memoryAppends.Global != "" {
		b.memoryMu.Lock()
		written, err := memory.Append(b.config.MemoryDir, memoryAppends.Global)
		b.memoryMu.Unlock()
		if err != nil {
			log.Printf("slackbot: append global memory %q: %v", eventKey, err)
		} else if written != "" {
			output.memoryUpdated = true
		}
	}
	var foldTarget *playbook.Playbook
	output.playbookNoteNotice, foldTarget = b.savePlaybookNote(eventKey, playbookNote, playbookNoteInvalid, sharedWritable)
	if foldTarget != nil && sessionID != "" {
		output.playbookFold = b.foldPlaybookNotes(stopCtx, sessionID, lease, scope, *foldTarget, onActivity)
	}
	// Upload while the thread's work lock and lease are held, so a later
	// turn in the same thread cannot replace the files mid-upload.
	if len(attachmentPaths) > 0 {
		output.failedAttachments = b.uploadAttachments(ctx, eventKey, channel, threadTS, lease, attachmentPaths)
	}
	return output, true, nil
}

func (b *Bot) runWorkTurn(ctx context.Context, threadID string, lease *workspace.Lease, scope channelScope, sharedWritable bool, text string, onThreadStarted func(string) error, onActivity func(codex.Activity)) (string, error) {
	roots := append([]string(nil), lease.WritableRoots...)
	if b.config.PlaybooksDir != "" && sharedWritable {
		roots = append(roots, b.config.PlaybooksDir)
	}
	denied, err := b.config.Workspaces.Denied(lease.Area)
	if err != nil {
		// Running without the list would expose closed channels' files.
		return "", fmt.Errorf("list paths hidden from the turn: %w", err)
	}
	denied = append(denied, scope.hidden...)
	// Hidden, not just read-only, elsewhere: the rules name channels and
	// hold instructions meant for one.
	if rulesDir := b.channelRulesDir(); rulesDir != "" {
		if sharedWritable {
			roots = append(roots, rulesDir)
		} else {
			denied = append(denied, rulesDir)
		}
	}
	sandbox := "workspace-write"
	if scope.rule.ReadOnly {
		sandbox = "read-only"
	}
	access := scope.rule.access(scope.slackLimited)
	result, err := b.runTurn(ctx, threadID, sandbox, lease.Dir, roots, denied, access, text, onThreadStarted, onActivity)
	if err != nil {
		return "", err
	}
	if result == nil || !result.Completed || len(result.Messages) == 0 {
		if result != nil && result.Err != "" {
			if codex.IsUsageLimitMessage(result.Err) {
				return "", fmt.Errorf("work turn incomplete: %s: %w", result.Err, codex.ErrUsageLimit)
			}
			if codex.IsNotSignedInMessage(result.Err) {
				return "", fmt.Errorf("work turn incomplete: %s: %w", result.Err, codex.ErrNotSignedIn)
			}
			return "", fmt.Errorf("work turn incomplete: %s", result.Err)
		}
		return "", errors.New("work turn incomplete")
	}
	return result.Messages[len(result.Messages)-1], nil
}

// Shared writable roots and the playbooks directory are not attachable: they
// belong to other threads too.
func (b *Bot) uploadAttachments(ctx context.Context, eventKey, channel, threadTS string, lease *workspace.Lease, paths []string) []attachment.Rejection {
	files, failed := attachment.Resolve(lease.ThreadAreas(), lease.Dir, paths)
	for _, rejection := range failed {
		log.Printf("slackbot: reject attachment %q for %q: %s", rejection.Path, eventKey, rejection.Reason)
	}
	for _, file := range files {
		upload := func() error {
			content, err := file.Open()
			if err != nil {
				return err
			}
			defer content.Close()
			return b.api.UploadFile(ctx, channel, threadTS, file.Name(), file.Size(), content)
		}
		// Only a rate limit proves Slack did not take the file. Retrying any
		// other failure could share the file twice.
		err := upload()
		if rateLimited, ok := errors.AsType[*slack.RateLimitedError](err); ok {
			if err = b.sleep(ctx, rateLimited.RetryAfter); err == nil {
				err = upload()
			}
		}
		if err != nil {
			log.Printf("slackbot: upload attachment %q for %q: %v", file.Path, eventKey, err)
			failed = append(failed, attachment.Rejection{Path: file.Path, Reason: "Slackへのアップロードに失敗しました"})
		}
	}
	return failed
}

func (b *Bot) acquireWork(ctx context.Context, threadKey, channel, threadTS string) (func(), error) {
	lock := b.keyedLock(b.workLocks, threadKey)
	release := func() {
		<-b.workSlots
		lock.Unlock()
	}
	if lock.TryLock() {
		select {
		case b.workSlots <- struct{}{}:
			return release, nil
		default:
			lock.Unlock()
		}
	}

	defer b.startStatus(ctx, channel, threadTS, queuedStatus).Stop()
	lock.Lock()
	// Prefer cancellation over a slot that frees up at the same moment, so a
	// shutdown does not start queued work.
	if err := ctx.Err(); err != nil {
		lock.Unlock()
		return nil, fmt.Errorf("wait for work slot: %w", err)
	}
	select {
	case b.workSlots <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		lock.Unlock()
		return nil, fmt.Errorf("wait for work slot: %w", ctx.Err())
	}
}

type sharedWorkspaces struct {
	dir   string
	roots []string
}

func (w sharedWorkspaces) Acquire(context.Context, string, workspace.Area, bool) (*workspace.Lease, error) {
	lease := workspace.NewLease(w.dir, w.roots, nil, func() {})
	lease.Shared = true
	return lease, nil
}

func (w sharedWorkspaces) Denied(workspace.Area) ([]string, error) { return nil, nil }

// Names who lacks access so the author asks the admin rather than retrying.
func (b *Bot) forbidden(ctx context.Context, event *slackevents.AppMentionEvent, missing []approvalSubject) {
	contact := "@seiji"
	if b.config.AdminUserID != "" {
		contact = "<@" + b.config.AdminUserID + ">"
	}
	threadTS := event.ThreadTimeStamp
	if threadTS == "" {
		threadTS = event.TimeStamp
	}
	if err := b.post(ctx, event.Channel, threadTS, fmt.Sprintf(forbiddenMessage, missingSubjects(missing), contact)); err != nil {
		log.Printf("slackbot: post forbidden response: %v", err)
	}
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

func (b *Bot) runTurn(ctx context.Context, threadID, sandbox, cwd string, roots, denied []string, access codex.Access, text string, callback func(string) error, onActivity func(codex.Activity)) (*codex.TurnResult, error) {
	turnCtx, cancel := context.WithTimeout(ctx, b.config.CodexTimeout)
	defer cancel()
	return b.runner.Run(turnCtx, threadID, sandbox, cwd, roots, denied, access, text, callback, onActivity)
}

func (b *Bot) fail(ctx context.Context, eventKey string, from, to state.State, channel, threadTS, timestamp, message string) {
	if err := b.store.Transition(eventKey, from, to); err != nil {
		log.Printf("slackbot: transition %q to %s: %v", eventKey, to, err)
	}
	if err := b.post(ctx, channel, threadTS, message); err != nil {
		log.Printf("slackbot: post failure %q: %v", eventKey, err)
	}
	b.finalReaction(ctx, channel, timestamp, failedReaction)
}

const (
	finishedReaction = "white_check_mark"
	waitingReaction  = "raising_hand"
	failedReaction   = "x"
)

func (b *Bot) finalReaction(ctx context.Context, channel, timestamp, name string) {
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

func (b *Bot) clearStatus(ctx context.Context, channel, threadTS string) {
	// Shutdown cancellation should not leave a stale loading indicator behind.
	clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	b.setStatus(clearCtx, channel, threadTS, "")
}

var broadcastMention = regexp.MustCompile(`(?i)<!(channel|here|everyone|subteam\^[^>|]*)(\|[^>]*)?>`)

func neutralizeBroadcasts(text string) string {
	return broadcastMention.ReplaceAllString(text, "@$1")
}

func (b *Bot) post(ctx context.Context, channel, threadTS, text string) error {
	text = slackfmt.HideLocalPaths(text, b.config.EBIXHome)
	for _, chunk := range slackfmt.Split(text, maxSlackMessageRunes) {
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

func (b *Bot) keyedLock(locks map[string]*sync.Mutex, key string) *sync.Mutex {
	b.threadMu.Lock()
	defer b.threadMu.Unlock()
	lock := locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		locks[key] = lock
	}
	return lock
}

func formatThreadContext(messages []ThreadMessage, currentTimestamp string) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.Timestamp == currentTimestamp || (strings.TrimSpace(message.Text) == "" && len(message.Files) == 0) {
			continue
		}
		text := message.Text
		for _, file := range message.Files {
			text += "\n（添付: " + safeFileName(file.Name) + "）"
		}
		parts = append(parts, fmt.Sprintf("[%s / %s]\n%s", message.Timestamp, message.AuthorID, strings.TrimLeft(text, "\n")))
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

// The text option is not a second copy of the body: Slack shows it in
// notification previews and in clients that cannot render blocks.
func (w *webAPI) PostMessage(ctx context.Context, channel, threadTS, text string) (string, error) {
	text = neutralizeBroadcasts(text)
	fallback := slackfmt.PlainText(text)
	// Escape the plain text so model output cannot form Slack control
	// sequences such as <!channel> in it.
	_, timestamp, err := w.client.PostMessageContext(ctx, channel,
		slack.MsgOptionBlocks(slack.NewMarkdownBlock("", text)),
		slack.MsgOptionText(fallback, true),
		slack.MsgOptionTS(threadTS))
	if err == nil || !rejectedBlocks(err) {
		return timestamp, err
	}
	// A formatting fault must not swallow the answer itself.
	log.Printf("slackbot: post markdown block: %v; retrying as plain text", err)
	_, timestamp, err = w.client.PostMessageContext(ctx, channel,
		slack.MsgOptionText(fallback, true), slack.MsgOptionTS(threadTS))
	return timestamp, err
}

func rejectedBlocks(err error) bool {
	response, ok := errors.AsType[slack.SlackErrorResponse](err)
	if !ok {
		return false
	}
	switch response.Err {
	case "invalid_blocks", "invalid_blocks_format", "msg_too_long":
		return true
	}
	return false
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
			result = append(result, threadMessage(message))
		}
		if nextCursor == "" || nextCursor == cursor {
			return result, nil
		}
		cursor = nextCursor
	}
}

func (w *webAPI) GetMessage(ctx context.Context, channel, threadTS, timestamp string) (ThreadMessage, error) {
	messages, _, _, err := w.client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: threadTS,
		Oldest:    timestamp,
		Latest:    timestamp,
		Inclusive: true,
		Limit:     2,
	})
	if err != nil {
		return ThreadMessage{}, fmt.Errorf("get Slack message: %w", err)
	}
	// Replies always start with the root, so pick the message by timestamp.
	for _, message := range messages {
		if message.Timestamp == timestamp {
			return threadMessage(message), nil
		}
	}
	return ThreadMessage{}, fmt.Errorf("get Slack message: %s not found", timestamp)
}

func (w *webAPI) ChannelInfo(ctx context.Context, channel string) (ChannelInfo, error) {
	info, err := w.client.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channel})
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("get Slack channel info: %w", err)
	}
	return ChannelInfo{
		Private:  info.IsPrivate || info.IsMpIM || info.IsGroup,
		External: info.IsExtShared || info.IsPendingExtShared,
	}, nil
}

func (w *webAPI) DownloadFile(ctx context.Context, url string, out io.Writer) error {
	return w.client.GetFileContext(ctx, url, out)
}

func threadMessage(message slack.Message) ThreadMessage {
	result := ThreadMessage{AuthorID: message.User, Timestamp: message.Timestamp, Text: message.Text}
	for _, file := range message.Files {
		url := file.URLPrivateDownload
		if url == "" {
			url = file.URLPrivate
		}
		result.Files = append(result.Files, SlackFile{Name: file.Name, Mimetype: file.Mimetype, Size: file.Size, URL: url})
	}
	return result
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

func (w *webAPI) ReactionUsers(ctx context.Context, channel, timestamp, reaction string) ([]string, error) {
	// Full asks for every user; without it Slack shortens long lists.
	reactions, err := w.client.GetReactionsContext(ctx, slack.ItemRef{Channel: channel, Timestamp: timestamp}, slack.GetReactionsParameters{Full: true})
	if err != nil {
		return nil, err
	}
	for _, itemReaction := range reactions {
		if itemReaction.Name == reaction {
			return itemReaction.Users, nil
		}
	}
	return nil, nil
}

// ChannelMembers needs channels:read (groups:read for private channels).
func (w *webAPI) ChannelMembers(ctx context.Context, channel string) ([]string, error) {
	var members []string
	cursor := ""
	for {
		page, nextCursor, err := w.client.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
			ChannelID: channel,
			Cursor:    cursor,
			Limit:     200,
		})
		if err != nil {
			return nil, fmt.Errorf("list Slack channel members: %w", err)
		}
		members = append(members, page...)
		if nextCursor == "" || nextCursor == cursor {
			break
		}
		cursor = nextCursor
	}
	return members, nil
}

// UserGroupMembers needs usergroups:read.
func (w *webAPI) UserGroupMembers(ctx context.Context, group string) ([]string, error) {
	members, err := w.client.GetUserGroupMembersContext(ctx, group)
	if err != nil {
		return nil, fmt.Errorf("list Slack user group members: %w", err)
	}
	return members, nil
}

// Needs users:read. One users.list walk tells bots apart instead of a
// users.info call per user.
func (w *webAPI) People(ctx context.Context, members []string) ([]string, error) {
	users, err := w.client.GetUsersContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Slack users: %w", err)
	}
	people := make(map[string]bool, len(users))
	for _, user := range users {
		if !user.IsBot && !user.IsAppUser && !user.Deleted && user.ID != "USLACKBOT" {
			people[user.ID] = true
		}
	}
	var humans []string
	for _, member := range members {
		if people[member] {
			humans = append(humans, member)
		}
	}
	return humans, nil
}

func (w *webAPI) AddReaction(ctx context.Context, channel, timestamp, name string) error {
	return w.client.AddReactionContext(ctx, name, slack.ItemRef{Channel: channel, Timestamp: timestamp})
}

func (w *webAPI) RemoveReaction(ctx context.Context, channel, timestamp, name string) error {
	return w.client.RemoveReactionContext(ctx, name, slack.ItemRef{Channel: channel, Timestamp: timestamp})
}

// Needs the files:write scope.
func (w *webAPI) UploadFile(ctx context.Context, channel, threadTS, filename string, size int64, content io.Reader) error {
	_, err := w.client.UploadFileV2Context(ctx, slack.UploadFileV2Parameters{
		Reader:          content,
		FileSize:        int(size),
		Filename:        filename,
		Title:           filename,
		Channel:         channel,
		ThreadTimestamp: threadTS,
	})
	return err
}

func RunSocketMode(acceptCtx, turnCtx context.Context, botToken, appToken string, bot *Bot, wg *sync.WaitGroup) error {
	client := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	auth, err := client.AuthTestContext(acceptCtx)
	if err != nil {
		return fmt.Errorf("authenticate Slack bot: %w", err)
	}
	bot.api = &webAPI{client: client}
	bot.config.BotUserID = auth.UserID
	// Follow-ups post to Slack, so they start once the client is ready.
	// They stop when a restart begins; what is due is left for the next
	// process.
	loopCtx, stopLoops := bot.untilDrain(acceptCtx)
	defer stopLoops()
	wg.Go(func() { bot.runFollowUps(loopCtx, turnCtx, wg) })
	wg.Go(func() { bot.runPullWatches(loopCtx, turnCtx, wg) })
	wg.Go(func() { bot.runSchedules(loopCtx, turnCtx, wg) })
	wg.Go(func() { bot.runReminders(loopCtx) })
	wg.Go(func() { bot.runDeferred(turnCtx, wg) })
	wg.Go(func() { bot.runUsageWatch(acceptCtx) })
	wg.Go(func() { bot.runDoneWatches(acceptCtx) })
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
			if event.Type == socketmode.EventTypeInteractive {
				if callback, ok := event.Data.(slack.InteractionCallback); ok {
					wg.Go(func() {
						bot.HandleInteraction(turnCtx, &callback)
					})
				}
				continue
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
			case *slackevents.AppHomeOpenedEvent:
				wg.Go(func() {
					bot.HandleAppHomeOpened(turnCtx, innerEvent)
				})
			}
		}
	}
}
