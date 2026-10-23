package slackbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack"
)

const (
	// doneReaction is the reaction that marks a mentioned person as done.
	doneReaction = "done"
	// doneWatchPollInterval is how often watched messages are checked, so a
	// watch ends soon after the last person reacts.
	doneWatchPollInterval = 30 * time.Minute
	// defaultDoneWatchDays is the gap between reminders when the command
	// names none: every other day.
	defaultDoneWatchDays = 2
	maxDoneWatchDays     = 14
	// maxDoneWatchReminders caps the reminders one watch posts.
	maxDoneWatchReminders = 10
	// maxDoneWatchesPerUser bounds the watches one person keeps running.
	maxDoneWatchesPerUser = 10
	maxDoneWatchTargets   = 30

	doneWatchStartedNotice  = "👀 :done: の監視を始めます。対象は元メッセージで @メンションされた%d人です。%sに確認し、まだ :done: を付けていない人にこのスレッドで通知します（次回 %s）。\n全員が付けたら終了します。止める場合は、このスレッドで「@ebi-x 監視をとめて」と mention してください。"
	doneWatchAllDoneNotice  = "✅ 対象の全員がすでに :done: を付けているため、監視は始めていません。"
	doneWatchNoTargetNotice = "⚠️ @メンションされた人が見つからなかったため、監視は始めていません。対象の人を @メンションしたメッセージの末尾で「@ebi-x 監視して」と書いてください。"
	doneWatchLimitNotice    = "⚠️ 1人が同時に続けられる監視は%d件までのため、監視は始めていません。不要な監視を、そのスレッドで「@ebi-x 監視をとめて」と mention して止めてください。"
	doneWatchFailedNotice   = "⚠️ 監視を始められませんでした。もう一度 mention してください。"
	doneWatchReminder       = "%s\n:done: のリアクションがまだのようです。対応が済んだら、%sに :done: を付けてください。（%d回目の通知）"
	doneWatchCompleted      = "✅ 対象の全員が :done: を付けたため、監視を終了しました。"
	doneWatchLimitReached   = "⚠️ 通知が上限の%d回に達したため、監視を終了しました。まだの人: %s"
	doneWatchGoneMessage    = "元メッセージが見つからなくなったため、:done: の監視を終了しました。"
	doneWatchStoppedMessage = ":done: の監視を終了しました。"
	doneWatchNothingToStop  = "このスレッドで続けている :done: の監視はありません。"
)

// doneWatchCommand is what follows the bot's mention at the end of a message
// to start a watch, such as "監視して", "毎日監視して", or "3日おきに監視して".
var doneWatchCommand = regexp.MustCompile(`^(?:(毎日)|(\d{1,2})日(おき|ごと|毎)に?)?\s*監視(?:して|お願い(?:します)?|よろしく)?(?:ください)?[。．.!！\s]*$`)

// doneWatchStopCommand is a whole message that stops the thread's watches.
var doneWatchStopCommand = regexp.MustCompile(`^監視を?(?:とめ|止め|やめ|停止し?|終了し?|解除し?)(?:て(?:ください)?)?[。．.!！\s]*$|^監視(?:停止|終了|解除)[。．.!！\s]*$`)

// slackUserMention matches a user mention in a Slack message's text.
var slackUserMention = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)

// parseDoneWatchCommand reports whether text ends with the bot's mention and
// a watch command, and the days between reminders it asks for.
func parseDoneWatchCommand(text, botUserID string) (int, bool) {
	if botUserID == "" {
		return 0, false
	}
	mention := "<@" + botUserID + ">"
	index := strings.LastIndex(text, mention)
	if index < 0 {
		return 0, false
	}
	match := doneWatchCommand.FindStringSubmatch(strings.TrimSpace(text[index+len(mention):]))
	if match == nil {
		return 0, false
	}
	switch {
	case match[1] != "":
		return 1, true
	case match[2] != "":
		n, err := strconv.Atoi(match[2])
		if err != nil || n < 1 {
			return 0, false
		}
		// "N日おき" skips N days; "N日ごと" repeats every N days.
		if match[3] == "おき" {
			n++
		}
		if n > maxDoneWatchDays {
			return 0, false
		}
		return n, true
	}
	return defaultDoneWatchDays, true
}

func isDoneWatchStopCommand(message string) bool {
	return doneWatchStopCommand.MatchString(strings.TrimSpace(message))
}

// mentionedUsers lists the users text mentions, in order and once each,
// leaving out the bot.
func mentionedUsers(text, botUserID string) []string {
	seen := make(map[string]struct{})
	var users []string
	for _, match := range slackUserMention.FindAllStringSubmatch(text, -1) {
		id := match[1]
		if id == botUserID {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		users = append(users, id)
	}
	return users
}

// startDoneWatch watches the message a "監視して" mention points at. That is
// the mention's own message when it mentions people, and otherwise the
// thread's first message.
func (b *Bot) startDoneWatch(ctx context.Context, channel, threadTS, timestamp, text, authorID string, days int) {
	messageTS := timestamp
	targets := mentionedUsers(text, b.config.BotUserID)
	if len(targets) == 0 && threadTS != timestamp {
		messages, err := b.api.GetThreadMessages(ctx, channel, threadTS, threadTS)
		if err != nil {
			log.Printf("slackbot: read thread %s for done watch: %v", threadRef(channel, threadTS), err)
			b.postDoneWatchNotice(ctx, channel, threadTS, doneWatchFailedNotice)
			return
		}
		for _, message := range messages {
			if message.Timestamp == threadTS {
				messageTS = threadTS
				targets = mentionedUsers(message.Text, b.config.BotUserID)
				break
			}
		}
	}
	if len(targets) == 0 {
		b.postDoneWatchNotice(ctx, channel, threadTS, doneWatchNoTargetNotice)
		return
	}
	if len(targets) > maxDoneWatchTargets {
		targets = targets[:maxDoneWatchTargets]
	}
	key := channel + ":" + messageTS
	others := 0
	for _, watch := range b.store.DoneWatches() {
		if watch.AuthorID == authorID && watch.Key() != key {
			others++
		}
	}
	if others >= maxDoneWatchesPerUser {
		b.postDoneWatchNotice(ctx, channel, threadTS, fmt.Sprintf(doneWatchLimitNotice, maxDoneWatchesPerUser))
		return
	}
	reacted, err := b.api.ReactionUsers(ctx, channel, messageTS, doneReaction)
	if err != nil {
		log.Printf("slackbot: read reactions of %s: %v", key, err)
		b.postDoneWatchNotice(ctx, channel, threadTS, doneWatchFailedNotice)
		return
	}
	if len(pendingUsers(targets, reacted)) == 0 {
		b.postDoneWatchNotice(ctx, channel, threadTS, doneWatchAllDoneNotice)
		return
	}
	now := b.now()
	watch := state.DoneWatch{
		Channel: channel, ThreadTS: threadTS, MessageTS: messageTS, AuthorID: authorID,
		Targets: targets, IntervalDays: days,
		NextAt: now.Add(time.Duration(days) * 24 * time.Hour), CreatedAt: now,
	}
	if err := b.store.SetDoneWatch(watch); err != nil {
		log.Printf("slackbot: save done watch %s: %v", key, err)
		b.postDoneWatchNotice(ctx, channel, threadTS, doneWatchFailedNotice)
		return
	}
	b.addReaction(ctx, channel, timestamp, "eyes")
	b.postDoneWatchNotice(ctx, channel, threadTS, fmt.Sprintf(doneWatchStartedNotice, len(targets), doneWatchIntervalLabel(days), formatFollowUpTime(watch.NextAt)))
}

func (b *Bot) postDoneWatchNotice(ctx context.Context, channel, threadTS, text string) {
	if err := b.post(ctx, channel, threadTS, text); err != nil {
		log.Printf("slackbot: post done watch notice: %v", err)
	}
}

// doneWatchIntervalLabel says how often a watch reminds.
func doneWatchIntervalLabel(days int) string {
	switch days {
	case 1:
		return "毎日"
	case 2:
		return "1日おき"
	}
	return fmt.Sprintf("%d日ごと", days)
}

// pendingUsers lists the targets missing from reacted.
func pendingUsers(targets, reacted []string) []string {
	done := makeSet(reacted)
	var pending []string
	for _, target := range targets {
		if _, ok := done[target]; !ok {
			pending = append(pending, target)
		}
	}
	return pending
}

func userMentions(users []string) string {
	mentions := make([]string, len(users))
	for i, user := range users {
		mentions[i] = "<@" + user + ">"
	}
	return strings.Join(mentions, " ")
}

// runDoneWatches checks the watched messages until ctx is done.
func (b *Bot) runDoneWatches(ctx context.Context) {
	for {
		for _, watch := range b.store.DoneWatches() {
			if ctx.Err() != nil {
				return
			}
			b.checkDoneWatch(ctx, watch)
		}
		if err := b.sleep(ctx, doneWatchPollInterval); err != nil {
			return
		}
	}
}

// checkDoneWatch ends a watch once every target has reacted, and when a
// reminder is due, mentions the targets who have not yet.
func (b *Bot) checkDoneWatch(ctx context.Context, watch state.DoneWatch) {
	key := watch.Key()
	if !b.userAllowed(watch.AuthorID) || !b.conversationAllowed(watch.Channel) {
		log.Printf("slackbot: drop done watch %s: user %q or channel is no longer allowed", key, watch.AuthorID)
		b.endDoneWatch(ctx, watch, "")
		return
	}
	reacted, err := b.api.ReactionUsers(ctx, watch.Channel, watch.MessageTS, doneReaction)
	if err != nil {
		if response, ok := errors.AsType[slack.SlackErrorResponse](err); ok && response.Err == "message_not_found" {
			b.endDoneWatch(ctx, watch, doneWatchGoneMessage)
			return
		}
		log.Printf("slackbot: read reactions of %s: %v", key, err)
		return
	}
	pending := pendingUsers(watch.Targets, reacted)
	if len(pending) == 0 {
		b.endDoneWatch(ctx, watch, doneWatchCompleted)
		return
	}
	if b.now().Before(watch.NextAt) {
		return
	}
	if watch.Reminders >= maxDoneWatchReminders {
		b.endDoneWatch(ctx, watch, fmt.Sprintf(doneWatchLimitReached, maxDoneWatchReminders, userMentions(pending)))
		return
	}
	watch.Reminders++
	watch.NextAt = b.now().Add(time.Duration(max(watch.IntervalDays, 1)) * 24 * time.Hour)
	// Save before posting, so a crash cannot remind twice.
	if err := b.store.SetDoneWatch(watch); err != nil {
		log.Printf("slackbot: save done watch %s: %v", key, err)
		return
	}
	original := "元メッセージ"
	if link, err := b.api.Permalink(ctx, watch.Channel, watch.MessageTS); err == nil {
		original = "<" + link + "|元メッセージ>"
	} else {
		log.Printf("slackbot: link done watch message %s: %v", key, err)
	}
	text := fmt.Sprintf(doneWatchReminder, userMentions(pending), original, watch.Reminders)
	if err := b.post(ctx, watch.Channel, watch.ThreadTS, text); err != nil {
		log.Printf("slackbot: post done watch reminder %s: %v", key, err)
	}
}

// endDoneWatch removes a watch and, when message is set, says why in its
// thread.
func (b *Bot) endDoneWatch(ctx context.Context, watch state.DoneWatch, message string) {
	key := watch.Key()
	if _, err := b.store.DeleteDoneWatches(func(w state.DoneWatch) bool { return w.Key() == key }); err != nil {
		log.Printf("slackbot: delete done watch %s: %v", key, err)
		return
	}
	if message == "" {
		return
	}
	if err := b.post(ctx, watch.Channel, watch.ThreadTS, message); err != nil {
		log.Printf("slackbot: post done watch end %s: %v", key, err)
	}
}

// stopDoneWatches ends every watch started in the thread, reporting whether
// there was one.
func (b *Bot) stopDoneWatches(channel, threadTS string) bool {
	removed, err := b.store.DeleteDoneWatches(func(w state.DoneWatch) bool {
		return w.Channel == channel && w.ThreadTS == threadTS
	})
	if err != nil {
		log.Printf("slackbot: stop done watches in %s: %v", threadRef(channel, threadTS), err)
	}
	return len(removed) > 0
}

// doneWatchSummary describes a watch for the Home tab.
func doneWatchSummary(watch state.DoneWatch) string {
	return fmt.Sprintf("対象%d人、%s（次回 %s）", len(watch.Targets), doneWatchIntervalLabel(watch.IntervalDays), formatFollowUpTime(watch.NextAt))
}

// stopDoneWatchCommand answers "監視をとめて": it ends the thread's :done:
// watches and leaves its other work running.
func (b *Bot) stopDoneWatchCommand(ctx context.Context, channel, threadTS string) {
	message := doneWatchNothingToStop
	if b.stopDoneWatches(channel, threadTS) {
		message = doneWatchStoppedMessage
	}
	b.postDoneWatchNotice(ctx, channel, threadTS, message)
}
