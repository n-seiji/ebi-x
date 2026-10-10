package slackbot

import (
	"context"
	"log"
	"strings"
)

const (
	mutedMessage         = "🔇 このスレッドでは、mention なしの返信には反応しません。mention すると元に戻ります。"
	unmutedMessage       = "🔊 このスレッドでは、mention なしの返信にも反応します。"
	unmuteFailedMessage  = "このスレッドは購読の対象ではないため、mention したときだけ反応します。"
	directMessageMuteMsg = "DM では、すべてのメッセージに反応します。反応させたくないメッセージは「(aside)」で始めてください。"
)

// asidePrefixes start a message the bot ignores, so people can talk in a
// thread the bot listens to, as with Devin's (aside).
var asidePrefixes = []string{"(aside)", "（aside）", "!aside", "(独り言)", "（独り言）"}

func isAside(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	for _, prefix := range asidePrefixes {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

// Mute commands, compared like stop commands: the whole message only.
var (
	muteCommands   = map[string]struct{}{"ミュート": {}, "ミュートして": {}, "mute": {}, "黙って": {}, "静かにして": {}}
	unmuteCommands = map[string]struct{}{"ミュート解除": {}, "ミュートを解除して": {}, "unmute": {}}
)

func normalizeCommand(message string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(message), "。．.!！ "))
}

func isMuteCommand(message string) bool {
	_, ok := muteCommands[normalizeCommand(message)]
	return ok
}

func isUnmuteCommand(message string) bool {
	_, ok := unmuteCommands[normalizeCommand(message)]
	return ok
}

// muteThread stops the bot from handling plain replies in a subscribed
// thread. The next mention subscribes the thread again when it is still
// marked.
func (b *Bot) muteThread(ctx context.Context, channel, threadTS string) {
	key := threadRef(channel, threadTS)
	if err := b.store.DeleteSubscription(key); err != nil {
		log.Printf("slackbot: mute %s: %v", key, err)
		return
	}
	if err := b.post(ctx, channel, threadTS, mutedMessage); err != nil {
		log.Printf("slackbot: post mute result: %v", err)
	}
}

// unmuteThread subscribes a marked thread again without starting a turn.
func (b *Bot) unmuteThread(ctx context.Context, channel, threadTS string) {
	b.startThreadSubscription(ctx, channel, threadTS)
	message := unmuteFailedMessage
	if b.repliesWithoutMention(channel, threadTS) {
		message = unmutedMessage
	}
	if err := b.post(ctx, channel, threadTS, message); err != nil {
		log.Printf("slackbot: post unmute result: %v", err)
	}
}
