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
	// defaultDoneWatchDays is the gap between reminders, in weekdays, when
	// the command names none: every weekday.
	defaultDoneWatchDays = 1
	maxDoneWatchDays     = 7
	// doneWatchHour is the local hour reminders go out at.
	doneWatchHour = 19
	// maxDoneWatchReminders is how many reminders a watch sends before it
	// ends, unless everyone reacts or someone stops it first.
	maxDoneWatchReminders = 5
	// maxDoneWatchesPerUser bounds the watches one person keeps running.
	maxDoneWatchesPerUser = 10
	maxDoneWatchTargets   = 100

	doneWatchStartedNotice    = "👀 :done: の監視を始めます。対象は元メッセージで @メンションされた%d人です。%sに、まだ :done: を付けていない人にこのスレッドで通知します（次回 %s）。%s\n全員が付けるか、%d回通知したら終了します。止める場合は、このスレッドで「@ebi-x 監視をとめて」と mention してください。"
	doneWatchAllDoneNotice    = "✅ 対象の全員がすでに :done: を付けているため、監視は始めていません。"
	doneWatchNoTargetNotice   = "⚠️ @メンションされた人が見つからなかったため、監視は始めていません。対象の人を @メンションしたメッセージの末尾で「@ebi-x 監視して」と書いてください。"
	doneWatchLimitNotice      = "⚠️ 1人が同時に続けられる監視は%d件までのため、監視は始めていません。不要な監視を、そのスレッドで「@ebi-x 監視をとめて」と mention して止めてください。"
	doneWatchTooManyNotice    = "⚠️ 対象が%d人を超えるため、監視は始めていません。"
	doneWatchScopeNotice      = "⚠️ @channel やユーザーグループの対象を調べるには、Slack アプリに channels:read・groups:read・usergroups:read・users:read の scope が必要です。管理者に追加を頼んでください。"
	doneWatchOutsideNotice    = "ユーザーグループのうち、このチャンネルにいない%d人は通知が届かないため対象から外しました。"
	doneWatchNamedOutsideNote = "%s はまだこのチャンネルに参加していないようです。招待しないと通知が届きません。"
	doneWatchAllOutsideNotice = "⚠️ ユーザーグループのメンバーは全員このチャンネルにいないため、通知が届きません。監視は始めていません。メンバーをチャンネルに招待してから、もう一度「@ebi-x 監視して」と mention してください。"
	doneWatchFailedNotice     = "⚠️ 監視を始められませんでした。もう一度 mention してください。"
	doneWatchReminder         = "%s\n:done: のリアクションがまだのようです。対応が済んだら、%sに :done: を付けてください。（%d回目の通知）"
	doneWatchLastReminder     = "\n%d回通知したため、監視はこれで終了します。"
	doneWatchCompleted        = "✅ 対象の全員が :done: を付けたため、監視を終了しました。"
	doneWatchGoneMessage      = "元メッセージが見つからなくなったため、:done: の監視を終了しました。"
	doneWatchStoppedMessage   = ":done: の監視を終了しました。"
	doneWatchNothingToStop    = "このスレッドで続けている :done: の監視はありません。"
)

// doneWatchCommand is what follows the bot's mention at the end of a message
// to start a watch, such as "監視して", "毎日監視して", or "3日おきに監視して".
var doneWatchCommand = regexp.MustCompile(`^(?:(毎日)|(\d{1,2})日(おき|ごと|毎)に?)?\s*監視(?:して|お願い(?:します)?|よろしく)?(?:ください)?[。．.!！\s]*$`)

// doneWatchStopCommand is a whole message that stops the thread's watches.
var doneWatchStopCommand = regexp.MustCompile(`^監視を?(?:とめ|止め|やめ|停止し?|終了し?|解除し?)(?:て(?:ください)?)?[。．.!！\s]*$|^監視(?:停止|終了|解除)[。．.!！\s]*$`)

// slackUserMention matches a user mention in a Slack message's text.
var slackUserMention = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)

// slackBroadcast matches @channel, @here, and @everyone, which a watch reads
// as every member of the channel.
var slackBroadcast = regexp.MustCompile(`<!(?:channel|here|everyone)(?:\|[^>]*)?>`)

// slackUserGroup matches a user group mention such as @dev_team, which a
// watch reads as every member of the group.
var slackUserGroup = regexp.MustCompile(`<!subteam\^([A-Z0-9]+)(?:\|[^>]*)?>`)

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
// thread's first message. @channel and the like stand for every person in
// the channel but the author.
func (b *Bot) startDoneWatch(ctx context.Context, channel, threadTS, timestamp, text, authorID string, days int) {
	messageTS := timestamp
	targets := mentionedUsers(text, b.config.BotUserID)
	everyone := slackBroadcast.MatchString(text)
	groups := mentionedGroups(text)
	if len(targets) == 0 && !everyone && len(groups) == 0 && threadTS != timestamp {
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
				everyone = slackBroadcast.MatchString(message.Text)
				groups = mentionedGroups(message.Text)
				break
			}
		}
	}
	outside := 0
	var namedOutside []string
	if everyone || len(groups) > 0 {
		channelMembers, err := b.api.ChannelMembers(ctx, channel)
		if err == nil {
			namedOutside = notIn(targets, channelMembers)
			targets, outside, err = b.expandDoneWatchTargets(ctx, channelMembers, targets, everyone, groups, authorID)
		}
		if err != nil {
			log.Printf("slackbot: list members for done watch in %s: %v", channel, err)
			notice := doneWatchFailedNotice
			if response, ok := errors.AsType[slack.SlackErrorResponse](err); ok && response.Err == "missing_scope" {
				notice = doneWatchScopeNotice
			}
			b.postDoneWatchNotice(ctx, channel, threadTS, notice)
			return
		}
	} else if len(targets) > 0 {
		// Named people stay targets even when they are not in the channel,
		// since the author meant them; the start notice says so, so the
		// author can invite them. Without channels:read the note is skipped.
		if channelMembers, err := b.api.ChannelMembers(ctx, channel); err == nil {
			namedOutside = notIn(targets, channelMembers)
		} else {
			log.Printf("slackbot: list members of %s for done watch: %v", channel, err)
		}
	}
	if len(targets) == 0 {
		notice := doneWatchNoTargetNotice
		if outside > 0 {
			notice = doneWatchAllOutsideNotice
		}
		b.postDoneWatchNotice(ctx, channel, threadTS, notice)
		return
	}
	if len(targets) > maxDoneWatchTargets {
		b.postDoneWatchNotice(ctx, channel, threadTS, fmt.Sprintf(doneWatchTooManyNotice, maxDoneWatchTargets))
		return
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
		NextAt: nextDoneWatchReminder(now, days), CreatedAt: now,
	}
	if err := b.store.SetDoneWatch(watch); err != nil {
		log.Printf("slackbot: save done watch %s: %v", key, err)
		b.postDoneWatchNotice(ctx, channel, threadTS, doneWatchFailedNotice)
		return
	}
	b.addReaction(ctx, channel, timestamp, "eyes")
	outsideNote := ""
	if outside > 0 {
		outsideNote = fmt.Sprintf(doneWatchOutsideNotice, outside)
	}
	if len(namedOutside) > 0 {
		outsideNote += fmt.Sprintf(doneWatchNamedOutsideNote, userMentions(namedOutside))
	}
	b.postDoneWatchNotice(ctx, channel, threadTS, fmt.Sprintf(doneWatchStartedNotice, len(targets), doneWatchIntervalLabel(days), formatFollowUpTime(watch.NextAt), outsideNote, maxDoneWatchReminders))
}

// nextDoneWatchReminder returns the days-th weekday 19:00 local time after
// after, so reminders skip Saturdays and Sundays.
func nextDoneWatchReminder(after time.Time, days int) time.Time {
	local := after.In(time.Local)
	at := time.Date(local.Year(), local.Month(), local.Day(), doneWatchHour, 0, 0, 0, time.Local)
	if !at.After(local) {
		at = at.AddDate(0, 0, 1)
	}
	left := max(days, 1)
	for {
		if weekday := at.Weekday(); weekday != time.Saturday && weekday != time.Sunday {
			if left--; left == 0 {
				return at
			}
		}
		at = at.AddDate(0, 0, 1)
	}
}

// expandDoneWatchTargets adds the people that @channel and user group
// mentions stand for. Only channel members count, since a mention does not
// reach anyone else; outside is how many group members were left out for
// that.
func (b *Bot) expandDoneWatchTargets(ctx context.Context, channelMembers, named []string, everyone bool, groups []string, authorID string) (targets []string, outside int, err error) {
	inChannel := makeSet(channelMembers)
	var members []string
	if everyone {
		members = append(members, channelMembers...)
	}
	var away []string
	for _, group := range groups {
		groupMembers, err := b.api.UserGroupMembers(ctx, group)
		if err != nil {
			return nil, 0, err
		}
		for _, member := range groupMembers {
			if _, ok := inChannel[member]; ok {
				members = append(members, member)
			} else {
				away = append(away, member)
			}
		}
	}
	// One People call over everyone keeps it to a single users.list walk.
	people, err := b.api.People(ctx, append(members, away...))
	if err != nil {
		return nil, 0, err
	}
	isPerson := makeSet(people)
	var humans []string
	for _, member := range members {
		if _, ok := isPerson[member]; ok {
			humans = append(humans, member)
		}
	}
	targets = mergeTargets(named, humans, authorID, b.config.BotUserID)
	chosen := makeSet(targets)
	counted := map[string]struct{}{}
	for _, member := range away {
		_, person := isPerson[member]
		_, target := chosen[member]
		_, seen := counted[member]
		if person && !target && !seen && member != authorID {
			counted[member] = struct{}{}
		}
	}
	return targets, len(counted), nil
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
		return fmt.Sprintf("平日の毎日%d時", doneWatchHour)
	case 2:
		return fmt.Sprintf("平日1日おきの%d時", doneWatchHour)
	}
	return fmt.Sprintf("平日%d日ごとの%d時", days, doneWatchHour)
}

// notIn returns the users that are not among members, in order.
func notIn(users, members []string) []string {
	in := makeSet(members)
	var out []string
	for _, user := range users {
		if _, ok := in[user]; !ok {
			out = append(out, user)
		}
	}
	return out
}

// mentionedGroups lists the user groups mentioned in text, in order and
// without repeats.
func mentionedGroups(text string) []string {
	var groups []string
	seen := map[string]struct{}{}
	for _, match := range slackUserGroup.FindAllStringSubmatch(text, -1) {
		if _, ok := seen[match[1]]; ok {
			continue
		}
		seen[match[1]] = struct{}{}
		groups = append(groups, match[1])
	}
	return groups
}

// mergeTargets adds the channel's and the groups' members after the people
// mentioned by name, leaving out the author and the bot unless they were
// named.
func mergeTargets(named, members []string, authorID, botUserID string) []string {
	targets := append([]string(nil), named...)
	seen := makeSet(named)
	for _, member := range members {
		if member == authorID || member == botUserID {
			continue
		}
		if _, ok := seen[member]; ok {
			continue
		}
		seen[member] = struct{}{}
		targets = append(targets, member)
	}
	return targets
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

// runDoneWatches checks the watched messages until ctx is done. It wakes for
// the next reminder when that comes before the next poll, so reminders go
// out on the hour.
func (b *Bot) runDoneWatches(ctx context.Context) {
	for {
		for _, watch := range b.store.DoneWatches() {
			if ctx.Err() != nil {
				return
			}
			b.checkDoneWatch(ctx, watch)
		}
		wait := doneWatchPollInterval
		for _, watch := range b.store.DoneWatches() {
			if until := watch.NextAt.Sub(b.now()); until > 0 && until < wait {
				wait = until
			}
		}
		if err := b.sleep(ctx, wait); err != nil {
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
	// A crash after the last reminder was counted leaves the watch behind.
	if watch.Reminders >= maxDoneWatchReminders {
		b.endDoneWatch(ctx, watch, "")
		return
	}
	if b.now().Before(watch.NextAt) {
		return
	}
	// Count the reminder before posting, so a crash cannot remind twice, and
	// only while the watch is still saved, so a stop during this check wins.
	now := b.now()
	watch, ok, err := b.store.UpdateDoneWatch(key, func(saved *state.DoneWatch) bool {
		if now.Before(saved.NextAt) {
			return false
		}
		saved.Reminders++
		saved.NextAt = nextDoneWatchReminder(now, saved.IntervalDays)
		return true
	})
	if err != nil {
		log.Printf("slackbot: save done watch %s: %v", key, err)
		return
	}
	if !ok {
		return
	}
	original := "元メッセージ"
	if link, err := b.api.Permalink(ctx, watch.Channel, watch.MessageTS); err == nil {
		original = "<" + link + "|元メッセージ>"
	} else {
		log.Printf("slackbot: link done watch message %s: %v", key, err)
	}
	text := fmt.Sprintf(doneWatchReminder, userMentions(pending), original, watch.Reminders)
	last := watch.Reminders >= maxDoneWatchReminders
	if last {
		text += fmt.Sprintf(doneWatchLastReminder, maxDoneWatchReminders)
	}
	if err := b.post(ctx, watch.Channel, watch.ThreadTS, text); err != nil {
		log.Printf("slackbot: post done watch reminder %s: %v", key, err)
	}
	if last {
		b.endDoneWatch(ctx, watch, "")
	}
}

// endDoneWatch removes a watch and, when message is set, says why in its
// thread.
func (b *Bot) endDoneWatch(ctx context.Context, watch state.DoneWatch, message string) {
	key := watch.Key()
	removed, err := b.store.DeleteDoneWatches(func(w state.DoneWatch) bool { return w.Key() == key })
	if err != nil {
		log.Printf("slackbot: delete done watch %s: %v", key, err)
		return
	}
	// A stop during this check already removed the watch and said so.
	if len(removed) == 0 || message == "" {
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
