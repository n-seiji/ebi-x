package slackbot

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/state"
)

const (
	// usageCheckInterval is how often the Codex usage is checked and
	// recorded. It also sees usage from outside ebi-x on the same account.
	usageCheckInterval = 30 * time.Minute
	usageReadTimeout   = 30 * time.Second
	usageDayLayout     = "2006-01-02"

	usageLowMessage       = "⚠️ Codex の利用枠（%s）の残りが %d%% を下回りました。%s"
	usageExhaustedMessage = "🚫 Codex の利用枠（%s）を使い切りました。%sまで、ebi-x は作業できません。"
	usageResetSuffix      = "%sにリセットされます。"
	usageSummaryPrefix    = "今の残り: "
	usageUnknownReset     = "リセットされる"
	usageReplyPrefix      = "Codex の利用枠の残り: "
	usageUnavailableReply = "⚠️ Codex の利用枠を読み取れませんでした。"
	usageDailyHeader      = "📊 %s の Codex の利用枠（%d回確認）"
	usageDailyLine        = "- %s: %d%% → %d%%（最低 %d%%）"
	// usageLimitFailureMessage replaces workFailureMessage when the turn
	// stopped on the usage limit, so the requester knows why and when to
	// ask again.
	usageLimitFailureMessage = "⚠️ Codex の利用上限に達したため、作業を完了できませんでした。%s%s、新しい mention で依頼し直してください。途中までの変更が作業領域に残っている場合があります。"
	usageLimitRetryAt        = "%s以降に"
	usageLimitRetryLater     = "利用枠がリセットされてから"
	// usageLimitAdminAlert tells the approval channel when the reset time
	// could not be read, so the limit is not announced by recordUsage.
	usageLimitAdminAlert = "🚫 Codex の利用上限に達したため、ebi-x は作業できません。利用枠がリセットされるまでお待ちください。"
	// authFailureMessage replaces workFailureMessage when Codex is not
	// signed in or its sign-in expired.
	authFailureMessage = "⚠️ ebi-x の Codex のログインが切れているため、作業できませんでした。%s復旧したら、新しい mention で依頼し直してください。"
	authAdminAlert     = "🔑 ebi-x の Codex のログインが切れているため、作業できません。ebi-x を動かしている環境で `codex login` を実行して、ログインし直してください。"
	adminNotifiedText  = "管理者に知らせたので、お待ちください。"
	adminContactText   = "管理者に連絡してください。"
	// adminAlertInterval is the shortest gap between two alerts of the same
	// kind, so every failing request does not repeat it.
	adminAlertInterval = time.Hour
)

// usageThresholds are the remaining percentages announced once per window
// period, highest first. 0 means the window is used up.
var usageThresholds = []int{10, 0}

// usageQuestionWords mark a mention in the approval channel that asks for
// the current usage.
var usageQuestionWords = []string{"usage", "残り", "利用枠", "使用量"}

// UsageReader reads the Codex account's current usage.
type UsageReader interface {
	ReadRateLimits(ctx context.Context) (*codex.RateLimits, error)
}

func (b *Bot) usageWatchEnabled() bool {
	return b.config.Usage != nil && b.config.ApprovalChannelID != ""
}

// runUsageWatch checks the Codex usage until ctx is done. Each check is
// recorded for the day's summary, which the first check of the next day
// posts in the approval channel, and a window that runs low is announced
// there.
func (b *Bot) runUsageWatch(ctx context.Context) {
	if !b.usageWatchEnabled() {
		return
	}
	failing := false
	for {
		limits, err := b.readUsage(ctx)
		switch {
		case err != nil && !failing:
			// Logged once per outage; an API-key login never has usage.
			log.Printf("slackbot: read Codex usage: %v", err)
			failing = true
		case err == nil:
			failing = false
			b.recordUsage(ctx, limits)
		}
		if err := b.sleep(ctx, usageCheckInterval); err != nil {
			return
		}
	}
}

func (b *Bot) readUsage(ctx context.Context) (*codex.RateLimits, error) {
	readCtx, cancel := context.WithTimeout(ctx, usageReadTimeout)
	defer cancel()
	return b.config.Usage.ReadRateLimits(readCtx)
}

// isUsageQuestion reports whether a mention's text asks for the usage.
func isUsageQuestion(text string) bool {
	lower := strings.ToLower(text)
	for _, word := range usageQuestionWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// usageReply answers a question about the current usage.
func (b *Bot) usageReply(ctx context.Context) string {
	if b.config.Usage == nil {
		return usageUnavailableReply
	}
	limits, err := b.readUsage(ctx)
	if err != nil {
		log.Printf("slackbot: read Codex usage for a question: %v", err)
		return usageUnavailableReply
	}
	windows := usageWindows(limits)
	var parts []string
	for _, key := range usageWindowKeys {
		window, ok := windows[key]
		if !ok {
			continue
		}
		part := fmt.Sprintf("%s %d%%", usageWindowLabel(key, window), usageRemaining(window))
		if at := window.ResetTime(); !at.IsZero() {
			part += "（" + formatFollowUpTime(at) + "にリセット）"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return usageUnavailableReply
	}
	return usageReplyPrefix + strings.Join(parts, "・")
}

// recordUsage adds one check to the day's record, posting the previous
// day's summary first when the day changed, and announces the windows that
// fell to a level not yet announced in their current period.
func (b *Bot) recordUsage(ctx context.Context, limits *codex.RateLimits) {
	b.usageMu.Lock()
	defer b.usageMu.Unlock()
	saved := b.store.Usage()
	next := saved
	day := b.now().In(time.Local).Format(usageDayLayout)
	if saved.Day != day {
		if summary := usageDailySummary(saved); summary != "" {
			if err := b.post(ctx, b.config.ApprovalChannelID, "", summary); err != nil {
				log.Printf("slackbot: post Codex usage summary: %v", err)
			}
		}
		next.Day = day
		next.Windows = nil
	}
	windows := usageWindows(limits)
	days := make(map[string]state.UsageDay, len(windows))
	for key, record := range next.Windows {
		days[key] = record
	}
	for key, window := range windows {
		remaining := math.Max(0, 100-window.UsedPercent)
		record, ok := days[key]
		if !ok {
			record = state.UsageDay{First: remaining, Min: remaining}
		}
		record.Label = usageWindowLabel(key, window)
		record.Last = remaining
		record.Min = math.Min(record.Min, remaining)
		record.Checks++
		days[key] = record
	}
	next.Windows = days

	alerts, lines := usageAlerts(saved.Alerts, windows)
	next.Alerts = alerts
	if len(lines) > 0 {
		text := strings.Join(lines, "\n") + "\n" + usageSummaryPrefix + usageSummary(limits)
		if err := b.post(ctx, b.config.ApprovalChannelID, "", text); err != nil {
			log.Printf("slackbot: post Codex usage alert: %v", err)
			// Announce it again at the next check.
			next.Alerts = saved.Alerts
		}
	}
	if err := b.store.SetUsage(next); err != nil {
		log.Printf("slackbot: save Codex usage: %v", err)
	}
}

// usageAlerts returns the levels announced after this check and the
// announcement lines for windows that fell to a new level.
func usageAlerts(announced map[string]int, windows map[string]codex.RateLimitWindow) (map[string]int, []string) {
	next := make(map[string]int, len(windows))
	var lines []string
	for _, key := range usageWindowKeys {
		window, ok := windows[key]
		if !ok {
			continue
		}
		level, crossed := usageLevel(window)
		if !crossed {
			// Back above every threshold: the window was reset.
			continue
		}
		next[key] = level
		if previous, ok := announced[key]; ok && previous <= level {
			// Already announced, or risen since without a full reset.
			continue
		}
		label := usageWindowLabel(key, window)
		if level == 0 {
			lines = append(lines, fmt.Sprintf(usageExhaustedMessage, label, formatUsageReset(window, usageUnknownReset)))
			continue
		}
		reset := ""
		if at := window.ResetTime(); !at.IsZero() {
			reset = fmt.Sprintf(usageResetSuffix, formatFollowUpTime(at))
		}
		lines = append(lines, fmt.Sprintf(usageLowMessage, label, level, reset))
	}
	return next, lines
}

// usageDailySummary describes a recorded day, or returns "" when nothing was
// recorded.
func usageDailySummary(usage state.Usage) string {
	day, err := time.ParseInLocation(usageDayLayout, usage.Day, time.Local)
	if err != nil || len(usage.Windows) == 0 {
		return ""
	}
	checks := 0
	var lines []string
	for _, key := range usageWindowKeys {
		record, ok := usage.Windows[key]
		if !ok {
			continue
		}
		checks = max(checks, record.Checks)
		lines = append(lines, fmt.Sprintf(usageDailyLine, record.Label, int(math.Floor(record.First)), int(math.Floor(record.Last)), int(math.Floor(record.Min))))
	}
	header := fmt.Sprintf(usageDailyHeader, day.Format("1/2"), checks)
	return header + "\n" + strings.Join(lines, "\n")
}

// usageLimitFailure returns the failure message for a turn that hit the
// usage limit, naming when the exhausted window resets when Codex says so.
// The approval channel is told about the exhausted window.
func (b *Bot) usageLimitFailure(ctx context.Context) string {
	retry := usageLimitRetryLater
	var limits *codex.RateLimits
	if b.config.Usage != nil {
		var err error
		if limits, err = b.readUsage(ctx); err != nil {
			log.Printf("slackbot: read Codex usage after the usage limit: %v", err)
			limits = nil
		}
	}
	if limits != nil {
		var latest time.Time
		for _, window := range limits.Windows() {
			if at := window.ResetTime(); window.UsedPercent >= 100 && at.After(latest) {
				latest = at
			}
		}
		if !latest.IsZero() {
			retry = fmt.Sprintf(usageLimitRetryAt, formatFollowUpTime(latest))
		}
	}
	notice := adminContactText
	if b.config.ApprovalChannelID != "" {
		notice = adminNotifiedText
		if limits != nil {
			b.recordUsage(ctx, limits)
		} else {
			b.alertAdmin(ctx, "usage", usageLimitAdminAlert)
		}
	}
	return fmt.Sprintf(usageLimitFailureMessage, notice, retry)
}

// authFailure returns the failure message for a turn that failed because
// Codex is not signed in, telling the approval channel.
func (b *Bot) authFailure(ctx context.Context) string {
	if b.config.ApprovalChannelID == "" {
		return fmt.Sprintf(authFailureMessage, adminContactText)
	}
	b.alertAdmin(ctx, "auth", authAdminAlert)
	return fmt.Sprintf(authFailureMessage, adminNotifiedText)
}

// alertAdmin posts text in the approval channel unless an alert of the same
// kind was posted within adminAlertInterval.
func (b *Bot) alertAdmin(ctx context.Context, kind, text string) {
	b.usageMu.Lock()
	defer b.usageMu.Unlock()
	now := b.now()
	if last, ok := b.adminAlerts[kind]; ok && now.Sub(last) < adminAlertInterval {
		return
	}
	if err := b.post(ctx, b.config.ApprovalChannelID, "", text); err != nil {
		log.Printf("slackbot: post %s alert: %v", kind, err)
		return
	}
	if b.adminAlerts == nil {
		b.adminAlerts = make(map[string]time.Time)
	}
	b.adminAlerts[kind] = now
}

// usageWindowKeys are the window slots, shorter first.
var usageWindowKeys = []string{"primary", "secondary"}

// usageWindows keys the reported windows by their slot, which stays the same
// across resets.
func usageWindows(limits *codex.RateLimits) map[string]codex.RateLimitWindow {
	windows := make(map[string]codex.RateLimitWindow, 2)
	if limits.Primary != nil {
		windows["primary"] = *limits.Primary
	}
	if limits.Secondary != nil {
		windows["secondary"] = *limits.Secondary
	}
	return windows
}

// usageLevel returns the lowest threshold window has fallen to, if any.
func usageLevel(window codex.RateLimitWindow) (int, bool) {
	remaining := 100 - window.UsedPercent
	level, crossed := 0, false
	for _, threshold := range usageThresholds {
		if threshold == 0 {
			if window.UsedPercent >= 100 {
				level, crossed = 0, true
			}
			continue
		}
		if remaining < float64(threshold) {
			level, crossed = threshold, true
		}
	}
	return level, crossed
}

func usageRemaining(window codex.RateLimitWindow) int {
	return int(math.Max(0, math.Floor(100-window.UsedPercent)))
}

func usageWindowLabel(key string, window codex.RateLimitWindow) string {
	minutes := window.WindowMinutes
	switch {
	case minutes == 7*24*60:
		return "1週間"
	case minutes > 0 && minutes%(24*60) == 0:
		return fmt.Sprintf("%d日", minutes/(24*60))
	case minutes > 0 && minutes%60 == 0:
		return fmt.Sprintf("%d時間", minutes/60)
	case minutes > 0:
		return fmt.Sprintf("%d分", minutes)
	case key == "primary":
		return "5時間"
	default:
		return "1週間"
	}
}

func formatUsageReset(window codex.RateLimitWindow, unknown string) string {
	if at := window.ResetTime(); !at.IsZero() {
		return formatFollowUpTime(at) + "にリセットされる"
	}
	return unknown
}

// usageSummary lists every window's remaining percentage.
func usageSummary(limits *codex.RateLimits) string {
	var parts []string
	windows := usageWindows(limits)
	for _, key := range usageWindowKeys {
		window, ok := windows[key]
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d%%", usageWindowLabel(key, window), usageRemaining(window)))
	}
	return strings.Join(parts, "・")
}
