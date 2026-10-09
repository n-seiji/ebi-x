package slackbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
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
	followUpTimeNotice      = "⚠️ フォローアップの時刻「%s」を読み取れないか範囲外（%s後から%d日後まで）のため、予定していません。"
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
	return b.formatFollowUpTime(followUp.DueAt) + " に「" + followUp.Task + "」"
}

// scheduleFollowUp applies the follow-up a finished turn asked for and
// returns the notice to post after its result. The answer to each request
// decides the thread's next follow-up: one that sets none cancels the one
// pending, so a follow-up never outlives the plan that made it.
func (b *Bot) scheduleFollowUp(trigger processingTrigger, output workOutput) string {
	threadKey := trigger.channel + ":" + trigger.threadTS
	var notices []string
	// A follow-up was taken from the store when it became due, so only a
	// person's request can replace one that is still pending.
	if trigger.followUp == nil {
		if previous, ok := b.store.GetFollowUp(threadKey); ok {
			deleted, err := b.store.DeleteFollowUp(threadKey)
			if err != nil {
				log.Printf("slackbot: cancel follow-up %q: %v", threadKey, err)
			}
			if deleted && output.followUp == nil {
				notices = append(notices, fmt.Sprintf(followUpCancelledNotice, b.formatFollowUpTime(previous.DueAt)))
			}
		}
	}
	if output.followUpInvalid {
		return strings.Join(append(notices, followUpInvalidNotice), "\n")
	}
	if output.followUp == nil {
		return strings.Join(notices, "\n")
	}

	chain := 1
	if trigger.followUp != nil {
		chain = trigger.followUp.Chain + 1
	}
	if chain > codex.MaxFollowUpChain {
		return strings.Join(append(notices, fmt.Sprintf(followUpChainNotice, codex.MaxFollowUpChain)), "\n")
	}
	now := b.now()
	dueAt, err := parseFollowUpTime(output.followUp.When, now)
	if err != nil {
		log.Printf("slackbot: reject follow-up time %q in %q: %v", output.followUp.When, threadKey, err)
		notice := fmt.Sprintf(followUpTimeNotice, shortDetail(output.followUp.When), codex.FormatDelay(codex.FollowUpMinDelay), int(codex.FollowUpMaxDelay/(24*time.Hour)))
		return strings.Join(append(notices, notice), "\n")
	}
	task := truncateRunes(strings.Join(strings.Fields(output.followUp.Task), " "), maxFollowUpTaskRunes)
	if err := b.store.SetFollowUp(threadKey, state.FollowUp{
		Channel:   trigger.channel,
		ThreadTS:  trigger.threadTS,
		AuthorID:  trigger.authorID,
		Task:      task,
		DueAt:     dueAt,
		Chain:     chain,
		CreatedAt: now,
	}); err != nil {
		log.Printf("slackbot: save follow-up %q: %v", threadKey, err)
		return strings.Join(append(notices, followUpInvalidNotice), "\n")
	}
	return strings.Join(append(notices, fmt.Sprintf(followUpScheduledNotice, b.formatFollowUpTime(dueAt), task)), "\n")
}

// parseFollowUpTime reads a delay such as "30m", "2h", or "1d", or an
// RFC 3339 time, and checks that it falls within the follow-up limits.
func parseFollowUpTime(when string, now time.Time) (time.Time, error) {
	when = strings.TrimSpace(when)
	var dueAt time.Time
	if days, ok := strings.CutSuffix(when, "d"); ok {
		count, err := strconv.Atoi(days)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse days: %w", err)
		}
		dueAt = now.Add(time.Duration(count) * 24 * time.Hour)
	} else if delay, err := time.ParseDuration(when); err == nil {
		dueAt = now.Add(delay)
	} else if at, err := time.Parse(time.RFC3339, when); err == nil {
		dueAt = at
	} else {
		return time.Time{}, errors.New("not a delay or RFC 3339 time")
	}
	if delay := dueAt.Sub(now); delay < codex.FollowUpMinDelay || delay > codex.FollowUpMaxDelay {
		return time.Time{}, fmt.Errorf("delay %v is out of range", delay)
	}
	return dueAt, nil
}

func (b *Bot) formatFollowUpTime(at time.Time) string {
	return at.In(time.Local).Format(followUpTimeLayout)
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
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
	threadKey := followUp.Channel + ":" + followUp.ThreadTS
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
