package slackbot

import (
	"context"
	"log"
	"strings"
)

const (
	aliveReply          = "🟢 生きてます。Codex にもつながっています。"
	aliveNoCodexReply   = "🟢 生きてます。"
	aliveCodexDownReply = "⚠️ 生きてますが、Codex に接続できません。今は作業を受けられないかもしれません。"
)

// Only the whole text counts, so a request that merely contains one still
// starts a turn.
var aliveQuestions = []string{
	"生きてる", "いきてる", "生きてますか", "いきてますか", "生きてる人",
	"起きてる", "おきてる", "起きてますか", "おきてますか",
	"alive", "ping",
}

func isAliveQuestion(text string) bool {
	text = strings.ToLower(strings.Trim(text, " \t\n　?？!！。.~〜ー…"))
	for _, question := range aliveQuestions {
		if text == question {
			return true
		}
	}
	return false
}

// Usage numbers are left out because it answers anyone in any channel ebi-x is
// in.
func (b *Bot) answerAliveQuestion(ctx context.Context, channel, threadTS string) {
	reply := aliveNoCodexReply
	if b.config.Usage != nil {
		reply = aliveReply
		if _, err := b.readUsage(ctx); err != nil {
			log.Printf("slackbot: alive check: %v", err)
			reply = aliveCodexDownReply
		}
	}
	if err := b.post(ctx, channel, threadTS, reply); err != nil {
		log.Printf("slackbot: post alive reply: %v", err)
	}
}
