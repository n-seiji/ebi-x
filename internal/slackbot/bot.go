// Package slackbot connects Slack mentions to Codex planning and work turns.
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
	failClosedMessage       = "作業指示を確定できなかったため、作業は開始していません。指示を明確にして、もう一度 mention してください。"
	planFailureMessage      = "⚠️ 方針の検討または投稿に失敗しました。もう一度 mention してください。"
	threadFailureMessage    = "⚠️ スレッドの読み込みに失敗したため、作業は開始していません。もう一度 mention してください。"
	workStartFailureMessage = "⚠️ 作業を開始できなかったため、作業は行っていません。もう一度 mention してください。"
	workFailureMessage      = "⚠️ 作業が完了したことを確認できませんでした。状況を確認し、新しい mention で依頼し直してください。"
	forbiddenMessage        = "403 forbidden. %s に確認してください。"
	attachmentFailureNotice = "⚠️ 次のファイルを添付できませんでした。生成済みのファイルは作業領域に残しています。再送する場合は「添付を再送して」と依頼してください。"
	attachmentOutputNotice  = "⚠️ 添付ファイルの指定を読み取れなかったため、ファイルは添付していません。再送する場合は「添付を再送して」と依頼してください。"
	planningStatus          = "が方針を考えています…"
	workingStatus           = "が作業を進めています…"
	queuedStatus            = "が作業の順番を待っています…"
	statusRefreshDelay      = 80 * time.Second
)

// SlackAPI is the subset of Slack Web API used by Bot.
type SlackAPI interface {
	PostMessage(ctx context.Context, channel, threadTS, text string) (string, error)
	GetThreadMessages(ctx context.Context, channel, threadTS, latest string) ([]ThreadMessage, error)
	SetStatus(ctx context.Context, channel, threadTS, status string) error
	HasReaction(ctx context.Context, channel, timestamp, reaction string) (bool, error)
	AddReaction(ctx context.Context, channel, timestamp, name string) error
	RemoveReaction(ctx context.Context, channel, timestamp, name string) error
	IsPublicChannel(ctx context.Context, channel string) (bool, error)
	UploadFile(ctx context.Context, channel, threadTS, filename string, size int64, content io.Reader) error
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

// Runner executes one Codex turn. deniedPaths are hidden from the turn in
// addition to the runner's own protected paths.
type Runner interface {
	Run(ctx context.Context, threadID, sandbox, cwd string, writableRoots, deniedPaths []string, text string, onThreadStarted func(string) error) (*codex.TurnResult, error)
}

// Workspaces provides per-thread working directories and git checkouts.
type Workspaces interface {
	ThreadDir(threadID string) (string, error)
	Acquire(ctx context.Context, threadID string) (*workspace.Lease, error)
	// OtherThreadPaths lists the workspace and checkout directories of every
	// thread except threadID, so one thread's turn cannot read another's.
	OtherThreadPaths(threadID string) ([]string, error)
}

// Config contains paths, allowlists, and timeout settings needed by Bot.
type Config struct {
	AllowedUserIDs    []string
	AllowedChannelIDs []string
	// AllowAllPublicChannels accepts every public channel and ignores
	// AllowedChannelIDs. DMs and private channels are always rejected.
	AllowAllPublicChannels bool
	AllowWorkflows         bool
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
	// MaxParallelPlan is how many planning turns may run at once. Values
	// below 1 mean 1.
	MaxParallelPlan int
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
	sharedWriteChannels map[string]struct{}
	workSlots           chan struct{}
	planSlots           chan struct{}
	memoryMu            sync.RWMutex
	threadMu            sync.Mutex
	threadLocks         map[string]*sync.Mutex
	workLocks           map[string]*sync.Mutex
	now                 func() time.Time
	sleep               func(context.Context, time.Duration) error
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
		sharedWriteChannels: makeSet(config.SharedWriteChannelIDs),
		workSlots:           make(chan struct{}, max(config.MaxParallelWork, 1)),
		planSlots:           make(chan struct{}, max(config.MaxParallelPlan, 1)),
		threadLocks:         make(map[string]*sync.Mutex),
		workLocks:           make(map[string]*sync.Mutex),
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
	if event.BotID == "" {
		if _, ok := b.allowedUsers[event.User]; !ok {
			log.Printf("slackbot: rejecting user %q", event.User)
			b.forbidden(ctx, event)
			return
		}
	} else if !b.workflowAllowed(workflowID) {
		log.Printf("slackbot: rejecting bot %q with workflow %q", event.BotID, workflowID)
		b.forbidden(ctx, event)
		return
	}
	if !b.mentionChannelAllowed(ctx, event.Channel) {
		log.Printf("slackbot: rejecting channel %q", event.Channel)
		b.forbidden(ctx, event)
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
	// ChannelType "channel" already guarantees a public channel.
	if _, ok := b.allowedChannels[event.Channel]; !ok && !b.config.AllowAllPublicChannels {
		return
	}
	// A subscription lets allowed users talk without mentioning the bot; it
	// does not extend the bot to everyone who can reply in the thread.
	if _, ok := b.allowedUsers[event.User]; !ok {
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
	// v4 prevents sessions created before per-thread workspaces from being
	// resumed in the shared workspace.
	threadKey := "v4:" + channel + ":" + threadTS

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

	workspaceID, cwd, err := b.threadWorkspace(channel, threadTS)
	if err != nil {
		log.Printf("slackbot: prepare thread workspace %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Planning, state.Failed, channel, threadTS, timestamp, planFailureMessage)
		return
	}

	otherThreads, err := b.config.Workspaces.OtherThreadPaths(workspaceID)
	if err != nil {
		log.Printf("slackbot: list other thread workspaces %q: %v", eventKey, err)
		b.fail(ctx, eventKey, state.Planning, state.Failed, channel, threadTS, timestamp, planFailureMessage)
		return
	}

	lock := b.keyedLock(b.threadLocks, threadKey)
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
	// The plan slot covers only the Codex turn, so slow Slack calls above do
	// not hold back other threads' planning.
	select {
	case b.planSlots <- struct{}{}:
	case <-ctx.Done():
		lock.Unlock()
		log.Printf("slackbot: wait for plan slot %q: %v", eventKey, ctx.Err())
		b.fail(ctx, eventKey, state.Planning, state.Failed, channel, threadTS, timestamp, planFailureMessage)
		return
	}
	planResult, runErr := b.runTurn(ctx, threadID, "read-only-network", cwd, nil, otherThreads, planPrompt, func(id string) error {
		if err := b.store.SetThread(threadKey, id); err != nil {
			return fmt.Errorf("persist plan thread: %w", err)
		}
		return nil
	})
	<-b.planSlots
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
	output, started, workErr := b.work(ctx, eventKey, channel, threadTS, threadKey, workspaceID, instruction)
	if !started {
		log.Printf("slackbot: start work %q: %v", eventKey, workErr)
		b.fail(ctx, eventKey, state.PlanPosted, state.Failed, channel, threadTS, timestamp, workStartFailureMessage)
		return
	}
	if workErr != nil {
		log.Printf("slackbot: work turn %q: %v", eventKey, workErr)
		b.fail(ctx, eventKey, state.Working, state.Interrupted, channel, threadTS, timestamp, workFailureMessage)
		return
	}
	resultText := output.text
	if resultText == "" {
		resultText = "作業が完了しました。"
	}
	if len(output.memoryScopes) > 0 {
		resultText += "\n\n📝 " + strings.Join(output.memoryScopes, "・") + "メモリを更新しました。"
	}
	// The deliverable that did not reach Slack must not look successful. The
	// files stay in the work area for a resend request.
	final := state.Done
	if notice := output.attachmentNotice(); notice != "" {
		resultText += "\n\n" + notice
		final = state.Interrupted
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
	b.finalReaction(ctx, channel, timestamp, final == state.Done)
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

// work runs the work turn for instruction. started reports whether the event
// reached the working state; when it did not, nothing was run and the event
// may be retried.
func (b *Bot) work(ctx context.Context, eventKey, channel, threadTS, threadKey, workspaceID, instruction string) (output workOutput, started bool, err error) {
	release, err := b.acquireWork(ctx, threadKey, channel, threadTS)
	if err != nil {
		return workOutput{}, false, err
	}
	defer release()

	lease, err := b.config.Workspaces.Acquire(ctx, workspaceID)
	if err != nil {
		return workOutput{}, false, fmt.Errorf("prepare workspace: %w", err)
	}
	defer lease.Release()
	otherThreads, err := b.config.Workspaces.OtherThreadPaths(workspaceID)
	if err != nil {
		return workOutput{}, false, fmt.Errorf("list other thread workspaces: %w", err)
	}
	// Playbooks and global memory reach every channel, so only trusted
	// channels may change them.
	_, sharedWritable := b.sharedWriteChannels[channel]
	roots := append([]string(nil), lease.WritableRoots...)
	if b.config.PlaybooksDir != "" && sharedWritable {
		roots = append(roots, b.config.PlaybooksDir)
	}

	if err := b.store.Transition(eventKey, state.PlanPosted, state.Working); err != nil {
		return workOutput{}, false, err
	}
	// A work plan is intentionally not posted: Slack would clear the progress
	// status when processing that reply. Refresh the status until work completes.
	stopWorkingStatus := b.keepStatus(ctx, channel, threadTS, workingStatus)
	defer stopWorkingStatus()

	// memoryMu is only held around the memory access itself, so other turns
	// can read memory while this one runs. The memory directory is
	// intentionally not a writable root: the agent proposes memory entries
	// through the output contract and the bot writes them.
	b.memoryMu.RLock()
	workMemoryContext, memErr := memory.ReadContext(b.config.MemoryDir, channel)
	b.memoryMu.RUnlock()
	if memErr != nil {
		log.Printf("slackbot: refresh memory before work: %v", memErr)
	}
	workPrompt := prompt.BuildWorkPrompt(instruction, workMemoryContext, lease.Checkouts, sharedWritable)
	workResult, workErr := b.runTurn(ctx, "", "workspace-write", lease.Dir, roots, otherThreads, workPrompt, nil)
	if workErr != nil {
		return workOutput{}, true, workErr
	}
	if workResult == nil || !workResult.Completed || len(workResult.Messages) == 0 {
		if workResult != nil && workResult.Err != "" {
			return workOutput{}, true, fmt.Errorf("work turn incomplete: %s", workResult.Err)
		}
		return workOutput{}, true, errors.New("work turn incomplete")
	}

	resultText, attachmentPaths, attachmentOutputValid := codex.SplitAttachments(workResult.Messages[len(workResult.Messages)-1])
	if !attachmentOutputValid {
		log.Printf("slackbot: ignore malformed attachment output %q", eventKey)
		output.attachmentOutputInvalid = true
	}
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

	stopQueuedStatus := b.keepStatus(ctx, channel, threadTS, queuedStatus)
	defer stopQueuedStatus()
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

// threadWorkspace returns the workspace identifier and plan cwd for a Slack
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

func (w sharedWorkspaces) Acquire(context.Context, string) (*workspace.Lease, error) {
	lease := workspace.NewLease(w.dir, w.roots, nil, func() {})
	lease.Shared = true
	return lease, nil
}

func (w sharedWorkspaces) OtherThreadPaths(string) ([]string, error) { return nil, nil }

// mentionChannelAllowed reports whether a mention in channel may be
// processed. App mention events carry no channel type, so the all-public mode
// asks Slack and rejects the channel when that lookup fails.
func (b *Bot) mentionChannelAllowed(ctx context.Context, channel string) bool {
	if !b.config.AllowAllPublicChannels {
		_, ok := b.allowedChannels[channel]
		return ok
	}
	// D is a DM; G is a legacy private channel or group DM.
	if !strings.HasPrefix(channel, "C") {
		return false
	}
	public, err := b.api.IsPublicChannel(ctx, channel)
	if err != nil {
		log.Printf("slackbot: look up channel %q: %v", channel, err)
		return false
	}
	return public
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

// workflowAllowed reports whether a bot-authored mention comes from an
// explicitly allowed Slack workflow. The configuration validates the IDs.
func (b *Bot) workflowAllowed(id string) bool {
	_, ok := b.allowedWorkflows[id]
	return b.config.AllowWorkflows && ok
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

func (b *Bot) runTurn(ctx context.Context, threadID, sandbox, cwd string, roots, denied []string, text string, callback func(string) error) (*codex.TurnResult, error) {
	turnCtx, cancel := context.WithTimeout(ctx, b.config.CodexTimeout)
	defer cancel()
	return b.runner.Run(turnCtx, threadID, sandbox, cwd, roots, denied, text, callback)
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

// IsPublicChannel reports whether channel is a public channel. Private
// channels and DMs need scopes ebi-x does not request, so their lookups fail
// rather than return false; callers must treat errors as not public.
func (w *webAPI) IsPublicChannel(ctx context.Context, channel string) (bool, error) {
	info, err := w.client.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channel})
	if err != nil {
		return false, err
	}
	return info.IsChannel && !info.IsPrivate && !info.IsIM && !info.IsMpIM, nil
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
