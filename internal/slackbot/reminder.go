package slackbot

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
)

const (
	// maxRemindersPerUser bounds how many messages one person can leave
	// pending.
	maxRemindersPerUser  = 20
	maxReminderTextRunes = 500
	// reminderLateAfter is how late a reminder may be posted before it says
	// so, which happens when the bot was down at its time.
	reminderLateAfter = 5 * time.Minute

	reminderSetNotice     = "🔔 %s にこのスレッドでリマインドします: %s\n取り消す場合は、このスレッドで「停止」と mention してください。"
	reminderStopNotice    = "🔔 予定していたリマインダー（%d件）を取り消しました。"
	reminderInvalidNotice = "⚠️ リマインダーの指定を読み取れなかったため、設定していません。"
	reminderTimeNotice    = "⚠️ リマインダーの時刻「%s」を読み取れないか範囲外（%s）のため、設定していません。"
	reminderLimitNotice   = "⚠️ 1人が予定できるリマインダーは%d件までのため、設定していません。不要なものを、設定したスレッドで「停止」と mention して取り消してください。"
	reminderSourceNotice  = "⚠️ リマインダーは人の依頼への回答でだけ設定・取り消しできるため、指定を無視しました。"
	reminderMessage       = "<@%s> 🔔 リマインダー: %s"
	reminderLateSuffix    = "\n（予定は %s でしたが、bot が止まっていたため遅れました）"
	reminderStoppedMsg    = "予定していたリマインダーを取り消しました。"
)

// applyReminder sets or cancels a reminder as a finished turn asked and
// returns the notice to post after its result. Only a person's request may
// change reminders, like schedules.
func (b *Bot) applyReminder(trigger processingTrigger, output workOutput) string {
	if !output.reminderInvalid && output.reminder == nil {
		return ""
	}
	if trigger.source != mentionTrigger && trigger.source != messageTrigger {
		log.Printf("slackbot: ignore reminder from an unattended turn in %s:%s", trigger.channel, trigger.threadTS)
		return reminderSourceNotice
	}
	if output.reminderInvalid {
		return reminderInvalidNotice
	}
	request := output.reminder
	if request.Stop {
		removed := b.stopReminders(trigger.channel, trigger.threadTS)
		if removed == 0 {
			return ""
		}
		return fmt.Sprintf(reminderStopNotice, removed)
	}
	now := b.now()
	dueAt, err := codex.ReminderDueAt(request.When, now)
	if err != nil {
		log.Printf("slackbot: reject reminder time %q in %s:%s: %v", request.When, trigger.channel, trigger.threadTS, err)
		return fmt.Sprintf(reminderTimeNotice, shortDetail(request.When), codex.ReminderRange())
	}
	pending := 0
	for _, reminder := range b.store.Reminders() {
		if reminder.AuthorID == trigger.authorID {
			pending++
		}
	}
	if pending >= maxRemindersPerUser {
		return fmt.Sprintf(reminderLimitNotice, maxRemindersPerUser)
	}
	reminder := state.Reminder{
		// One reminder per request, so the request's message keys it.
		ID:        threadRef(trigger.channel, trigger.threadTS) + ":" + trigger.timestamp,
		Channel:   trigger.channel,
		ThreadTS:  trigger.threadTS,
		AuthorID:  trigger.authorID,
		Text:      clipText(request.Text, maxReminderTextRunes),
		DueAt:     dueAt,
		CreatedAt: now,
	}
	if err := b.store.AddReminder(reminder); err != nil {
		log.Printf("slackbot: save reminder %q: %v", reminder.ID, err)
		return reminderInvalidNotice
	}
	return fmt.Sprintf(reminderSetNotice, formatFollowUpTime(reminder.DueAt), reminder.Text)
}

// runReminders posts due reminders until ctx is done. Posting starts no
// work, so it does not wait for a turn slot.
func (b *Bot) runReminders(ctx context.Context) {
	for {
		due, err := b.store.TakeDueReminders(b.now())
		if err != nil {
			log.Printf("slackbot: take due reminders: %v", err)
		}
		for _, reminder := range due {
			b.fireReminder(ctx, reminder)
		}
		if err := b.sleep(ctx, followUpPollInterval); err != nil {
			return
		}
	}
}

// fireReminder posts a reminder in its thread, mentioning who asked for it.
// Access is checked again, since the user or channel may have lost it.
func (b *Bot) fireReminder(ctx context.Context, reminder state.Reminder) {
	if !b.userAllowed(reminder.AuthorID) || !b.conversationAllowed(reminder.Channel) {
		log.Printf("slackbot: drop reminder %q: user %q or channel is no longer allowed", reminder.ID, reminder.AuthorID)
		return
	}
	text := fmt.Sprintf(reminderMessage, reminder.AuthorID, reminder.Text)
	if b.now().Sub(reminder.DueAt) > reminderLateAfter {
		text += fmt.Sprintf(reminderLateSuffix, formatFollowUpTime(reminder.DueAt))
	}
	if err := b.post(ctx, reminder.Channel, reminder.ThreadTS, text); err != nil {
		log.Printf("slackbot: post reminder %q: %v", reminder.ID, err)
	}
}

// stopReminders cancels the thread's pending reminders and returns how many
// there were.
func (b *Bot) stopReminders(channel, threadTS string) int {
	removed, err := b.store.DeleteReminders(func(reminder state.Reminder) bool {
		return reminder.Channel == channel && reminder.ThreadTS == threadTS
	})
	if err != nil {
		log.Printf("slackbot: stop reminders %s:%s: %v", channel, threadTS, err)
	}
	return len(removed)
}
