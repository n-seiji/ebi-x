package slackbot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
)

const (
	// followUpPollInterval is how often due follow-ups are looked for, so a
	// follow-up starts at most this long after its time.
	followUpPollInterval = 30 * time.Second
	maxFollowUpTaskRunes = 500
	followUpTimeLayout   = "1/2 15:04 MST"

	followUpStartMessage    = "⏰ 予定していたフォローアップを始めます: %s"
	followUpScheduledNotice = "⏰ %s ごろにフォローアップします: %s\n止める場合は、このスレッドで「停止」と mention してください。"
	followUpCancelledNotice = "⏰ 予定していたフォローアップ（%s）は取り消しました。"
	followUpInvalidNotice   = "⚠️ フォローアップの指定を読み取れなかったため、予定していません。"
	followUpTimeNotice      = "⚠️ フォローアップの時刻「%s」を読み取れないか範囲外（%s）のため、予定していません。"
	followUpChainNotice     = "⚠️ 人の発言なしで続けられるフォローアップの上限（%d回）に達したため、次は予定していません。続ける場合は、このスレッドで依頼してください。"
	followUpStoppedMessage  = "予定していたフォローアップを取り消しました。"
)

// pendingFollowUp describes the thread's scheduled follow-up for a prompt,
// or returns "" when there is none.
func (b *Bot) pendingFollowUp(threadKey string) string {
	followUp, ok := b.store.GetFollowUp(threadKey)
	if !ok {
		return ""
	}
	return formatFollowUpTime(followUp.DueAt) + " に「" + followUp.Task + "」"
}

// scheduleFollowUp applies the follow-up a finished turn asked for and
// returns the notice to post after its result. The answer to each request
// decides the thread's next follow-up: one that sets none cancels the one
// pending, so a follow-up never outlives the plan that made it.
func (b *Bot) scheduleFollowUp(trigger processingTrigger, output workOutput) string {
	threadKey := threadRef(trigger.channel, trigger.threadTS)
	next, notice := b.nextFollowUp(trigger, output)
	if next != nil {
		// Saving replaces any pending follow-up.
		if err := b.store.SetFollowUp(threadKey, *next); err != nil {
			log.Printf("slackbot: save follow-up %q: %v", threadKey, err)
			return followUpInvalidNotice
		}
		return fmt.Sprintf(followUpScheduledNotice, formatFollowUpTime(next.DueAt), next.Task)
	}
	// A due follow-up was taken from the store before it ran, so only a
	// person's request can still have one pending.
	if trigger.followUp == nil {
		previous, deleted, err := b.store.DeleteFollowUp(threadKey)
		if err != nil {
			log.Printf("slackbot: cancel follow-up %q: %v", threadKey, err)
		}
		if deleted {
			notice = strings.TrimSpace(fmt.Sprintf(followUpCancelledNotice, formatFollowUpTime(previous.DueAt)) + "\n" + notice)
		}
	}
	return notice
}

// nextFollowUp returns the follow-up output asks for, or nil and the notice
// that says why none is scheduled.
func (b *Bot) nextFollowUp(trigger processingTrigger, output workOutput) (*state.FollowUp, string) {
	if output.followUpInvalid {
		return nil, followUpInvalidNotice
	}
	if output.followUp == nil {
		return nil, ""
	}
	chain := 1
	if trigger.followUp != nil {
		chain = trigger.followUp.Chain + 1
	}
	if chain > codex.MaxFollowUpChain {
		return nil, fmt.Sprintf(followUpChainNotice, codex.MaxFollowUpChain)
	}
	now := b.now()
	dueAt, err := codex.FollowUpDueAt(output.followUp.When, now)
	if err != nil {
		log.Printf("slackbot: reject follow-up time %q in %s:%s: %v", output.followUp.When, trigger.channel, trigger.threadTS, err)
		return nil, fmt.Sprintf(followUpTimeNotice, shortDetail(output.followUp.When), codex.FollowUpRange())
	}
	return &state.FollowUp{
		Channel:   trigger.channel,
		ThreadTS:  trigger.threadTS,
		AuthorID:  trigger.authorID,
		Task:      clipText(output.followUp.Task, maxFollowUpTaskRunes),
		DueAt:     dueAt,
		Chain:     chain,
		CreatedAt: now,
	}, ""
}

func formatFollowUpTime(at time.Time) string {
	return at.In(time.Local).Format(followUpTimeLayout)
}

// runFollowUps starts due follow-ups until acceptCtx is done. Each runs on
// turnCtx under wg, like a turn started by a Slack event.
func (b *Bot) runFollowUps(acceptCtx, turnCtx context.Context, wg *sync.WaitGroup) {
	for {
		due, err := b.store.TakeDueFollowUps(b.now())
		if err != nil {
			log.Printf("slackbot: take due follow-ups: %v", err)
		}
		for _, followUp := range due {
			wg.Go(func() { b.fireFollowUp(turnCtx, followUp) })
		}
		if err := b.sleep(acceptCtx, followUpPollInterval); err != nil {
			return
		}
	}
}

// fireFollowUp announces a due follow-up in its thread and runs it there as
// a request from the user who scheduled it. Access is checked again, since
// the user or channel may have lost it in the meantime.
func (b *Bot) fireFollowUp(ctx context.Context, followUp state.FollowUp) {
	threadKey := threadRef(followUp.Channel, followUp.ThreadTS)
	if !b.userAllowed(followUp.AuthorID) || !b.channelAllowed(followUp.Channel) {
		log.Printf("slackbot: drop follow-up %q: user %q or channel is no longer allowed", threadKey, followUp.AuthorID)
		return
	}
	var timestamp string
	if err := b.retrySlack(ctx, func() error {
		var err error
		timestamp, err = b.api.PostMessage(ctx, followUp.Channel, followUp.ThreadTS, fmt.Sprintf(followUpStartMessage, followUp.Task))
		return err
	}); err != nil {
		log.Printf("slackbot: announce follow-up %q: %v", threadKey, err)
		return
	}
	b.processTrigger(ctx, processingTrigger{
		source:      followUpTrigger,
		authorID:    followUp.AuthorID,
		channel:     followUp.Channel,
		timestamp:   timestamp,
		threadTS:    followUp.ThreadTS,
		message:     followUp.Task,
		threadReply: true,
		followUp:    &followUp,
	})
}
