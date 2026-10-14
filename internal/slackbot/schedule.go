package slackbot

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
)

const (
	// maxSchedulesPerUser bounds the unattended work one person can set up.
	maxSchedulesPerUser  = 5
	maxScheduleTaskRunes = 500

	scheduleSetNotice     = "🔁 定期実行を設定しました: %s に「%s」（次回 %s）\n毎回このチャンネルの新しいスレッドで始めます。止める場合は、このスレッドで「停止」と mention してください。"
	scheduleStopNotice    = "🔁 定期実行（%s）を止めました。"
	scheduleInvalidNotice = "⚠️ 定期実行の指定を読み取れなかったため、設定していません。"
	scheduleSpecNotice    = "⚠️ 定期実行の「%s」を読み取れなかったため、設定していません。「毎日 09:00」「平日 09:00」「週末 10:00」「毎週月水金 09:30」の形で指定できます。"
	scheduleLimitNotice   = "⚠️ 1人が設定できる定期実行は%d件までのため、設定していません。不要な定期実行を、設定したスレッドで「停止」と mention して止めてください。"
	scheduleSourceNotice  = "⚠️ 定期実行は人の依頼への回答でだけ設定・変更できるため、指定を無視しました。"
	scheduleStartMessage  = "🔁 定期実行（%s）: %s"
	scheduleOriginLink    = "\n設定したスレッド: <%s|こちら>（止める場合はそこで「停止」と mention してください）"
	scheduleStoppedMsg    = "定期実行を止めました。"
	scheduleRunPrefix     = "（依頼者が「%s」の定期実行として設定した作業です。前回までの結果はこのスレッドにはありません）\n"
)

// applySchedule sets or stops the thread's schedule as a finished turn asked
// and returns the notice to post after its result. Only a person's request
// may change a schedule, so unattended turns cannot start more unattended
// work.
func (b *Bot) applySchedule(trigger processingTrigger, output workOutput) string {
	if !output.scheduleInvalid && output.schedule == nil {
		return ""
	}
	if trigger.source != mentionTrigger && trigger.source != messageTrigger {
		log.Printf("slackbot: ignore schedule from an unattended turn in %s:%s", trigger.channel, trigger.threadTS)
		return scheduleSourceNotice
	}
	if output.scheduleInvalid {
		return scheduleInvalidNotice
	}
	threadKey := threadRef(trigger.channel, trigger.threadTS)
	request := output.schedule
	if request.Stop {
		previous, deleted, err := b.store.DeleteSchedule(threadKey)
		if err != nil {
			log.Printf("slackbot: stop schedule %q: %v", threadKey, err)
			return scheduleInvalidNotice
		}
		if !deleted {
			return ""
		}
		return fmt.Sprintf(scheduleStopNotice, previous.Spec)
	}
	spec, err := codex.ParseScheduleSpec(request.When)
	if err != nil {
		log.Printf("slackbot: reject schedule %q in %q: %v", request.When, threadKey, err)
		return fmt.Sprintf(scheduleSpecNotice, shortDetail(request.When))
	}
	others := 0
	for _, schedule := range b.store.Schedules() {
		if schedule.AuthorID == trigger.authorID && threadRef(schedule.Channel, schedule.ThreadTS) != threadKey {
			others++
		}
	}
	if others >= maxSchedulesPerUser {
		return fmt.Sprintf(scheduleLimitNotice, maxSchedulesPerUser)
	}
	now := b.now()
	schedule := state.Schedule{
		Channel:   trigger.channel,
		ThreadTS:  trigger.threadTS,
		AuthorID:  trigger.authorID,
		Spec:      spec.String(),
		Task:      clipText(request.Task, maxScheduleTaskRunes),
		NextAt:    spec.Next(now.In(time.Local)),
		CreatedAt: now,
	}
	// Saving replaces the thread's earlier schedule.
	if err := b.store.SetSchedule(threadKey, schedule); err != nil {
		log.Printf("slackbot: save schedule %q: %v", threadKey, err)
		return scheduleInvalidNotice
	}
	return fmt.Sprintf(scheduleSetNotice, schedule.Spec, schedule.Task, formatFollowUpTime(schedule.NextAt))
}

// nextOccurrence is when schedule runs after now.
func (b *Bot) nextOccurrence(schedule state.Schedule) time.Time {
	spec, err := codex.ParseScheduleSpec(schedule.Spec)
	if err != nil {
		// Specs are saved in canonical form, so this means a hand-edited
		// file. Park it rather than run it on every poll.
		log.Printf("slackbot: unreadable schedule %q in %s:%s: %v", schedule.Spec, schedule.Channel, schedule.ThreadTS, err)
		return b.now().AddDate(100, 0, 0)
	}
	return spec.Next(b.now().In(time.Local))
}

// runSchedules starts due schedules until acceptCtx is done, like
// runFollowUps.
func (b *Bot) runSchedules(acceptCtx, turnCtx context.Context, wg *sync.WaitGroup) {
	for {
		due, err := b.store.TakeDueSchedules(b.now(), b.nextOccurrence)
		if err != nil {
			log.Printf("slackbot: take due schedules: %v", err)
		}
		for _, schedule := range due {
			wg.Go(func() { b.fireSchedule(turnCtx, schedule) })
		}
		if err := b.sleep(acceptCtx, followUpPollInterval); err != nil {
			return
		}
	}
}

// fireSchedule starts one occurrence in a new thread of the schedule's
// channel, as a request from the user who set it. Access is checked again,
// since the user or channel may have lost it since.
func (b *Bot) fireSchedule(ctx context.Context, schedule state.Schedule) {
	threadKey := threadRef(schedule.Channel, schedule.ThreadTS)
	if !b.userAllowed(schedule.AuthorID) || !b.conversationAllowed(schedule.Channel) {
		log.Printf("slackbot: skip schedule %q: user %q or channel is no longer allowed", threadKey, schedule.AuthorID)
		return
	}
	text := fmt.Sprintf(scheduleStartMessage, schedule.Spec, schedule.Task)
	if link, err := b.api.Permalink(ctx, schedule.Channel, schedule.ThreadTS); err == nil {
		text += fmt.Sprintf(scheduleOriginLink, link)
	} else {
		log.Printf("slackbot: link schedule thread %q: %v", threadKey, err)
	}
	var timestamp string
	if err := b.retrySlack(ctx, func() error {
		var err error
		timestamp, err = b.api.PostMessage(ctx, schedule.Channel, "", text)
		return err
	}); err != nil {
		log.Printf("slackbot: announce schedule %q: %v", threadKey, err)
		return
	}
	b.processTrigger(ctx, processingTrigger{
		source:    scheduleTrigger,
		authorID:  schedule.AuthorID,
		channel:   schedule.Channel,
		timestamp: timestamp,
		threadTS:  timestamp,
		message:   fmt.Sprintf(scheduleRunPrefix, schedule.Spec) + schedule.Task,
	})
}

// stopSchedule removes the schedule set in the thread, reporting whether
// there was one.
func (b *Bot) stopSchedule(channel, threadTS string) bool {
	_, deleted, err := b.store.DeleteSchedule(threadRef(channel, threadTS))
	if err != nil {
		log.Printf("slackbot: stop schedule %s:%s: %v", channel, threadTS, err)
	}
	return deleted
}

// scheduleSummary describes a schedule for the Home tab.
func scheduleSummary(schedule state.Schedule) string {
	return schedule.Spec + "「" + clipText(schedule.Task, 100) + "」（次回 " + formatFollowUpTime(schedule.NextAt) + "）"
}
