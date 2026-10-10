package slackbot

import "strings"

// waitingNotice tells the requester that the turn stopped for their answer
// and how to give it. It mentions them so they are notified even when they
// left the thread.
func (b *Bot) waitingNotice(authorID, channel, threadTS string, questions []string) string {
	var builder strings.Builder
	builder.WriteString("🙋 ")
	if authorID != "" && b.userAllowed(authorID) {
		builder.WriteString("<@" + authorID + "> ")
	}
	builder.WriteString("回答待ちです。")
	if b.repliesWithoutMention(channel, threadTS) {
		builder.WriteString("このスレッドに返信してください。")
	} else {
		builder.WriteString("このスレッドで ebi-x に mention して答えてください。")
	}
	for _, question := range questions {
		builder.WriteString("\n- " + question)
	}
	return builder.String()
}

// repliesWithoutMention reports whether a plain reply in the thread reaches
// the bot: always in a direct message, and in a channel while the thread is
// subscribed.
func (b *Bot) repliesWithoutMention(channel, threadTS string) bool {
	if isDirectMessage(channel) {
		return true
	}
	subscription, ok := b.store.GetSubscription(threadRef(channel, threadTS))
	return ok && subscription.ExpiresAt.After(b.now())
}
