package slackbot

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// repeatedFailureCount failed requests within repeatedFailureWindow mean
	// something every request needs is broken, not that one request was
	// hard, so the approval channel is told.
	repeatedFailureCount  = 3
	repeatedFailureWindow = time.Hour
	// maxFailureAlertRunes keeps the error in the alert to a line or two.
	maxFailureAlertRunes = 300
	repeatedFailureAlert = "🚨 ebi-x の作業が直近%d分で%d件失敗しています。依頼者には「依頼し直してください」と返していますが、同じ原因で失敗し続けている可能性があります。ebi-x のログを確認してください。\n最新のエラー: `%s`"
)

// noteWorkFailure records a request that failed for a reason the requester
// cannot fix, and tells the approval channel once failures repeat. A broken
// setting or environment fails every request with the same generic reply, so
// without this nobody who can fix it hears about it.
func (b *Bot) noteWorkFailure(ctx context.Context, err error) {
	if b.config.ApprovalChannelID == "" {
		return
	}
	now := b.now()
	b.failureMu.Lock()
	recent := b.recentFailures[:0]
	for _, at := range b.recentFailures {
		if now.Sub(at) < repeatedFailureWindow {
			recent = append(recent, at)
		}
	}
	b.recentFailures = append(recent, now)
	count := len(b.recentFailures)
	b.failureMu.Unlock()
	if count < repeatedFailureCount {
		return
	}
	b.alertAdmin(ctx, "work", fmt.Sprintf(repeatedFailureAlert, int(repeatedFailureWindow/time.Minute), count, failureSummary(err)))
}

// failureSummary is the error's first line, short enough for Slack.
func failureSummary(err error) string {
	if err == nil {
		return "不明"
	}
	text, _, _ := strings.Cut(err.Error(), "\n")
	text = strings.ReplaceAll(text, "`", "'")
	if runes := []rune(text); len(runes) > maxFailureAlertRunes {
		text = string(runes[:maxFailureAlertRunes]) + "…"
	}
	return text
}
