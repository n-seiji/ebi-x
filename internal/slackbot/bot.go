// Package slackbot connects Slack mentions to persistent Codex sessions.
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
	forbiddenMessage        = "403 forbidden. %s に確認してください。"
	attachmentFailureNotice = "⚠️ 次のファイルを添付できませんでした。生成済みのファイルは作業領域に残しています。再送する場合は「添付を再送して」と依頼してください。"
	attachmentOutputNotice  = "⚠️ 添付ファイルの指定を読み取れなかったため、ファイルは添付していません。再送する場合は「添付を再送して」と依頼してください。"
	workingStatus           = "が作業を進めています…"
	queuedStatus            = "が作業の順番を待っています…"
	stoppedMessage          = "⏹️ 依頼により作業を停止しました。途中までの変更が作業領域に残っている場合があります。続ける場合は、新しい mention で依頼してください。"
	nothingToStopMessage    = "停止する作業はありません。"
	statusRefreshDelay      = 80 * time.Second
	// statusMinInterval is the shortest gap between two status changes.
	statusMinInterval = 3 * time.Second
)

// errStoppedByUser is the cancellation cause of a request stopped from Slack.
var errStoppedByUser = errors.New("stopped by user")

// SlackAPI is the subset of Slack Web API used by Bot.
type SlackAPI interface {
	PostMessage(ctx context.Context, channel, threadTS, text string) (string, error)
	GetThreadMessages(ctx context.Context, channel, threadTS, latest string) ([]ThreadMessage, error)
	SetStatus(ctx context.Context, channel, threadTS, status string) error
	HasReaction(ctx context.Context, channel, timestamp, reaction string) (bool, error)
	AddReaction(ctx context.Context, channel, timestamp, name string) error
	RemoveReaction(ctx context.Context, channel, timestamp, name string) error
	UploadFile(ctx context.Context, channel, threadTS, filename string, size int64, content io.Reader) error
	SetSuggestedPrompts(ctx context.Context, channel string, prompts []suggestedPrompt) error
}

// ThreadMessage is the Slack thread data supplied to a first turn.
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
	GetFollowUp(threadKey string) (state.FollowUp, bool)
	SetFollowUp(threadKey string, followUp state.FollowUp) error
	DeleteFollowUp(threadKey string) (state.FollowUp, bool, error)
	TakeDueFollowUps(now time.Time) ([]state.FollowUp, error)
}

// Runner executes one Codex turn. deniedPaths are hidden from the turn in
// addition to the runner's own protected paths. onActivity, when set,
// receives the turn's steps while it runs.
type Runner interface {
	Run(ctx context.Context, threadID, sandbox, cwd string, writableRoots, deniedPaths []string, text string, onThreadStarted func(string) error, onActivity func(codex.Activity)) (*codex.TurnResult, error)
}

// Workspaces provides per-thread working directories and git checkouts.
type Workspaces interface {
	ThreadDir(threadID string) (string, error)
	// Acquire leases the thread's workspace. Missing checkouts are created
	// only when createCheckouts is set; otherwise they are reported in
	// Lease.PendingRepos.
	Acquire(ctx context.Context, threadID string, createCheckouts bool) (*workspace.Lease, error)
	// OtherThreadPaths lists every workspace and checkout entry except
	// threadID's own, so one thread's turn cannot read another's.
	OtherThreadPaths(threadID string) ([]string, error)
}

// Config contains paths, allowlists, and timeout settings needed by Bot.
type Config struct {
	AllowedUserIDs    []string
	AllowedChannelIDs []string
	// ApprovalChannelID is where users, channels, and workflows outside the
	// allowlists are approved. Approvals are disabled unless both it and
	// Approvals are set.
	ApprovalChannelID string
	// ApproverUserIDs are the only users who may decide approvals.
	ApproverUserIDs []string
	// Approvals records the decisions made in ApprovalChannelID.
	Approvals      Approvals
	AllowWorkflows bool
	// AllowedWorkflowIDs are the only workflows accepted when AllowWorkflows
	// is set.
	AllowedWorkflowIDs         []string
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
	// MaxParallelWork is how many work turns for different Slack threads may
	// run at once. Values below 1 mean 1.
	MaxParallelWork int
	// SharedWriteChannelIDs are the channels whose work turns may write
	// playbooks and global memory, which every channel reads.
	SharedWriteChannelIDs []string
	// Workspaces gives each Slack thread its cwd and writable roots. When
	// nil, every thread shares WorkspaceDir and WritableRoots.
	Workspaces Workspaces
}

// Bot handles Slack mentions.
type Bot struct {
	api       SlackAPI
	store     Store
	runner    Runner
	config    Config
	playbooks []playbook.Playbook

	allowedUsers        map[string]struct{}
	allowedChannels     map[string]struct{}
	allowedWorkflows    map[string]struct{}
	approvers           map[string]struct{}
	sharedWriteChannels map[string]struct{}
	workSlots           chan struct{}
	memoryMu            sync.RWMutex
	threadMu            sync.Mutex
	threadLocks         map[string]*sync.Mutex
	workLocks           map[string]*sync.Mutex
	// running holds the cancel functions of the requests each thread is
	// processing or waiting to process, keyed by thread and then event.
	runningMu sync.Mutex
	running   map[string]map[string]context.CancelCauseFunc
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
}

type triggerSource uint8

const (
	mentionTrigger triggerSource = iota
	messageTrigger
	// followUpTrigger is a follow-up the thread's session scheduled.
	followUpTrigger
)

type processingTrigger struct {
	source      triggerSource
	authorID    string
	channel     string
	timestamp   string
	threadTS    string
	message     string
	threadReply bool
	// followUp is the due follow-up a followUpTrigger runs.
	followUp *state.FollowUp
}

// New constructs a Bot.
func New(api SlackAPI, store Store, runner Runner, config Config, playbooks []playbook.Playbook) *Bot {
	config.WritableRoots = append([]string(nil), config.WritableRoots...)
	if config.Workspaces == nil {
		config.Workspaces = sharedWorkspaces{dir: config.WorkspaceDir, roots: config.WritableRoots}
	}
	b := &Bot{
		api:                 api,
		store:               store,
		runner:              runner,
		config:              config,
		playbooks:           append([]playbook.Playbook(nil), playbooks...),
		allowedUsers:        makeSet(config.AllowedUserIDs),
		allowedChannels:     makeSet(config.AllowedChannelIDs),
		allowedWorkflows:    makeSet(config.AllowedWorkflowIDs),
		approvers:           makeSet(config.ApproverUserIDs),
		sharedWriteChannels: makeSet(config.SharedWriteChannelIDs),
		workSlots:           make(chan struct{}, max(config.MaxParallelWork, 1)),
		threadLocks:         make(map[string]*sync.Mutex),
		workLocks:           make(map[string]*sync.Mutex),
		running:             make(map[string]map[string]context.CancelCauseFunc),
		now:                 time.Now,
		sleep:               sleepContext,
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
	// The approval channel is only for deciding requests.
	if b.approvalsEnabled() && event.Channel == b.config.ApprovalChannelID {
		if event.BotID == "" {
			b.handleApprovalCommand(ctx, event)
		}
		return
	}
	var missing []approvalSubject
	if event.BotID == "" {
		if !b.userAllowed(event.User) {
			missing = append(missing, approvalSubject{kind: state.ApprovalUser, id: event.User})
		}
	} else if !b.config.AllowWorkflows || !validWorkflowID(workflowID) {
		log.Printf("slackbot: rejecting bot %q with workflow %q", event.BotID, workflowID)
		b.forbidden(ctx, event)
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
// message event: a direct message to the bot, or a public-channel message
// from an active subscribed thread.
func (b *Bot) HandleMessage(ctx context.Context, event *slackevents.MessageEvent) {
	if event != nil && event.ChannelType == "im" {
		b.handleDirectMessage(ctx, event)
		return
	}
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
	// stopCtx ends when the request is stopped from Slack. Only waiting and
	// the Codex turn use it; Slack calls keep ctx, so a stop never keeps the
	// bot from reporting.
	stopCtx, untrack := b.trackRequest(ctx, threadRef(channel, threadTS), eventKey)
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

	workspaceID, _, err := b.threadWorkspace(channel, threadTS)
	if err != nil {
		log.Printf("slackbot: prepare thread workspace %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Received, state.Failed, channel, threadTS, timestamp, workStartFailureMessage)
		return
	}

	// A Slack thread owns one Codex session. Hold the lock through execution
	// and delivery so replies see the preceding result and never resume a
	// session concurrently.
	// Do not resume older sessions carrying the retired planning contract.
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
	if !hasThread && trigger.threadReply {
		threadMessages, err := b.api.GetThreadMessages(ctx, channel, threadTS, timestamp)
		if err != nil {
			log.Printf("slackbot: read thread context %q: %v", eventKey, err)
			b.fail(ctx, eventKey, state.Received, state.Failed, channel, threadTS, timestamp, threadFailureMessage)
			return
		}
		slackThread = formatThreadContext(threadMessages, timestamp)
	}
	output, started, workErr := b.work(ctx, stopCtx, eventKey, channel, threadTS, threadKey, workspaceID, threadID, slackThread, trigger)
	if !started {
		log.Printf("slackbot: start work %q: %v", eventKey, workErr)
		b.fail(ctx, eventKey, state.Received, state.Failed, channel, threadTS, timestamp, failure(workStartFailureMessage))
		return
	}
	if workErr != nil {
		log.Printf("slackbot: work turn %q: %v", eventKey, workErr)
		b.fail(ctx, eventKey, state.Working, state.Interrupted, channel, threadTS, timestamp, failure(workFailureMessage))
		return
	}
	resultText := output.text
	if resultText == "" {
		resultText = "作業が完了しました。"
	}
	if len(output.memoryScopes) > 0 {
		resultText += "\n\n📝 " + strings.Join(output.memoryScopes, "・") + "メモリを更新しました。"
	}
	if notice := b.scheduleFollowUp(trigger, output); notice != "" {
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

// workOutput is what a completed work turn hands back for posting.
type workOutput struct {
	text         string
	memoryScopes []string
	// attachmentOutputInvalid reports an attachment section the bot could not
	// read, so no files were sent.
	attachmentOutputInvalid bool
	// failedAttachments lists files that were rejected or failed to upload.
	failedAttachments []attachment.Rejection
	// followUp is the follow-up the turn asked for, and followUpInvalid
	// reports a follow-up section the bot could not read.
	followUp        *codex.FollowUpRequest
	followUpInvalid bool
	// waiting reports that the turn stopped to ask the requester questions.
	waiting   bool
	questions []string
}

// attachmentNotice tells the user which files did not reach Slack, so the
// posted result never reads as a complete delivery when it was not.
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

// work runs one complete request in the Slack thread's Codex session. started reports whether the event
// reached the working state; when it did not, nothing was run and the event
// may be retried. stopCtx bounds the waiting and the turn; ctx the rest.
func (b *Bot) work(ctx, stopCtx context.Context, eventKey, channel, threadTS, threadKey, workspaceID, threadID, slackThread string, trigger processingTrigger) (output workOutput, started bool, err error) {
	release, err := b.acquireWork(stopCtx, threadKey, channel, threadTS)
	if err != nil {
		return workOutput{}, false, err
	}
	defer release()

	// Checkouts are cloned only when the turn asks for them, so a request
	// that only reads code or answers a question does not create any.
	lease, err := b.config.Workspaces.Acquire(stopCtx, workspaceID, false)
	if err != nil {
		return workOutput{}, false, fmt.Errorf("prepare workspace: %w", err)
	}
	defer func() { lease.Release() }()
	otherThreads, err := b.config.Workspaces.OtherThreadPaths(workspaceID)
	if err != nil {
		return workOutput{}, false, fmt.Errorf("list other thread workspaces: %w", err)
	}
	// Playbooks and global memory reach every channel, so only trusted
	// channels may change them.
	_, sharedWritable := b.sharedWriteChannels[channel]

	// A new session receives the memory, playbook catalog, and rules once;
	// a resumed session already holds them, so only the request is sent.
	var turnPrompt string
	if threadID == "" {
		currentPlaybooks := b.playbooks
		if b.config.PlaybooksDir != "" {
			currentPlaybooks, err = playbook.List(b.config.PlaybooksDir)
			if err != nil {
				return workOutput{}, false, fmt.Errorf("reload playbooks: %w", err)
			}
		}
		// memoryMu is only held around the memory access itself, so other
		// turns can read memory while this one runs. The memory directory is
		// intentionally not a writable root: the agent proposes memory
		// entries through the output contract and the bot writes them.
		b.memoryMu.RLock()
		memoryContext, memErr := memory.ReadContext(b.config.MemoryDir, channel)
		b.memoryMu.RUnlock()
		if memErr != nil {
			log.Printf("slackbot: read memory: %v", memErr)
		}
		turnPrompt = prompt.BuildTurnPrompt(memoryContext, currentPlaybooks, slackThread, trigger.authorID, trigger.message, lease.Checkouts, lease.PendingRepos, sharedWritable)
	} else if trigger.followUp != nil {
		turnPrompt = prompt.BuildFollowUpPrompt(trigger.followUp.Task, trigger.followUp.DueAt, b.now(), trigger.followUp.Chain, lease.Checkouts, lease.PendingRepos)
	} else {
		turnPrompt = prompt.BuildResumePrompt(trigger.authorID, trigger.message, lease.Checkouts, lease.PendingRepos, b.pendingFollowUp(threadRef(channel, threadTS)))
	}

	if err := b.store.Transition(eventKey, state.Received, state.Working); err != nil {
		return workOutput{}, false, err
	}
	// Keep the status visible throughout the execution turn, and show what
	// the turn is doing as it goes.
	status := b.startStatus(ctx, channel, threadTS, workingStatus)
	defer status.Stop()
	tracker := &progress{}
	onActivity := func(activity codex.Activity) {
		status.Update(tracker.observe(activity))
	}

	// sessionID is the session a checkout request continues in.
	sessionID := threadID
	persistThread := func(id string) error {
		sessionID = id
		if err := b.store.SetThread(threadKey, id); err != nil {
			return fmt.Errorf("persist thread: %w", err)
		}
		return nil
	}
	finalText, err := b.runWorkTurn(stopCtx, threadID, lease, sharedWritable, otherThreads, turnPrompt, persistThread, onActivity)
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
		next, err := b.config.Workspaces.Acquire(stopCtx, workspaceID, true)
		if err != nil {
			return workOutput{}, true, fmt.Errorf("prepare requested checkouts: %w", err)
		}
		lease = next
		finalText, err = b.runWorkTurn(stopCtx, sessionID, lease, sharedWritable, otherThreads, prompt.BuildCheckoutsReadyPrompt(lease.Checkouts), persistThread, onActivity)
		if err != nil {
			return workOutput{}, true, err
		}
		// A second request has nothing more to prepare.
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
	resultText, output.questions, output.waiting = codex.SplitQuestions(resultText)
	resultText, memoryAppends, memoryOutputValid := codex.SplitMemoryAppends(resultText)
	output.text = codex.SanitizeSlackOutput(resultText)
	if !memoryOutputValid {
		log.Printf("slackbot: ignore malformed scoped memory output %q", eventKey)
	}
	if memoryAppends.Global != "" && !sharedWritable {
		log.Printf("slackbot: ignore global memory proposal from channel without shared writes %q", eventKey)
		memoryAppends.Global = ""
	}
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
				output.memoryScopes = append(output.memoryScopes, target.label)
			}
		}
		b.memoryMu.Unlock()
	}
	// Upload while the thread's work lock and lease are held, so a later
	// turn in the same thread cannot replace the files mid-upload.
	if len(attachmentPaths) > 0 {
		output.failedAttachments = b.uploadAttachments(ctx, eventKey, channel, threadTS, lease, attachmentPaths)
	}
	return output, true, nil
}

// runWorkTurn runs one workspace-write turn in lease and returns its final
// message.
func (b *Bot) runWorkTurn(ctx context.Context, threadID string, lease *workspace.Lease, sharedWritable bool, otherThreads []string, text string, onThreadStarted func(string) error, onActivity func(codex.Activity)) (string, error) {
	roots := append([]string(nil), lease.WritableRoots...)
	if b.config.PlaybooksDir != "" && sharedWritable {
		roots = append(roots, b.config.PlaybooksDir)
	}
	result, err := b.runTurn(ctx, threadID, "workspace-write", lease.Dir, roots, otherThreads, text, onThreadStarted, onActivity)
	if err != nil {
		return "", err
	}
	if result == nil || !result.Completed || len(result.Messages) == 0 {
		if result != nil && result.Err != "" {
			return "", fmt.Errorf("work turn incomplete: %s", result.Err)
		}
		return "", errors.New("work turn incomplete")
	}
	return result.Messages[len(result.Messages)-1], nil
}

// uploadAttachments sends the files a work turn listed to the Slack thread.
// Only files in the thread's own workspace directory and checkouts qualify;
// shared writable roots and the playbooks directory are other threads' too.
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

// acquireWork waits until this Slack thread has no other work turn running
// and a parallel work slot is free. Work turns for one thread run in order;
// different threads run in parallel up to MaxParallelWork. While waiting,
// the thread shows a queued status. The returned function releases both.
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

// threadWorkspace returns the workspace identifier and cwd for a Slack
// thread.
func (b *Bot) threadWorkspace(channel, threadTS string) (string, string, error) {
	id, err := workspace.ThreadID(channel, threadTS)
	if err != nil {
		return "", "", err
	}
	dir, err := b.config.Workspaces.ThreadDir(id)
	if err != nil {
		return "", "", err
	}
	return id, dir, nil
}

// sharedWorkspaces runs every thread in one directory with the same writable
// roots. It is the default when Config.Workspaces is not set.
type sharedWorkspaces struct {
	dir   string
	roots []string
}

func (w sharedWorkspaces) ThreadDir(string) (string, error) { return w.dir, nil }

func (w sharedWorkspaces) Acquire(context.Context, string, bool) (*workspace.Lease, error) {
	lease := workspace.NewLease(w.dir, w.roots, nil, func() {})
	lease.Shared = true
	return lease, nil
}

func (w sharedWorkspaces) OtherThreadPaths(string) ([]string, error) { return nil, nil }

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

func (b *Bot) runTurn(ctx context.Context, threadID, sandbox, cwd string, roots, denied []string, text string, callback func(string) error, onActivity func(codex.Activity)) (*codex.TurnResult, error) {
	turnCtx, cancel := context.WithTimeout(ctx, b.config.CodexTimeout)
	defer cancel()
	return b.runner.Run(turnCtx, threadID, sandbox, cwd, roots, denied, text, callback, onActivity)
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

// Terminal reactions on a request: ✅ when it is done, 🙋 when the turn is
// waiting for the requester's answer, and ❌ only when it failed or was
// interrupted.
const (
	finishedReaction = "white_check_mark"
	waitingReaction  = "raising_hand"
	failedReaction   = "x"
)

// finalReaction replaces the request's 👀 with its terminal reaction.
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

// broadcastMention matches Slack's special mentions that notify a whole
// channel or user group.
var broadcastMention = regexp.MustCompile(`(?i)<!(channel|here|everyone|subteam\^[^>|]*)(\|[^>]*)?>`)

// neutralizeBroadcasts keeps model output from notifying everyone in a channel.
func neutralizeBroadcasts(text string) string {
	return broadcastMention.ReplaceAllString(text, "@$1")
}

func (b *Bot) post(ctx context.Context, channel, threadTS, text string) error {
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

// PostMessage sends the text as a Block Kit markdown block so Slack renders
// headings, tables, and links rather than printing their syntax. The text
// option is not a second copy of the body: Slack shows it in notification
// previews and in clients that cannot render blocks.
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

// rejectedBlocks reports whether Slack refused the block payload itself, which
// a retry without blocks can still deliver.
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

// UploadFile shares one file in the thread through Slack's external upload
// flow (files.getUploadURLExternal, then files.completeUploadExternal). It
// needs the files:write scope.
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
	// Follow-ups post to Slack, so they start once the client is ready.
	wg.Go(func() { bot.runFollowUps(acceptCtx, turnCtx, wg) })
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
			case *slackevents.AppHomeOpenedEvent:
				wg.Go(func() {
					bot.HandleAppHomeOpened(turnCtx, innerEvent)
				})
			}
		}
	}
}
