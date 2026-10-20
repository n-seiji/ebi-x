package slackbot

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
)

const (
	// usagePollInterval is how often the Codex usage is checked. It also
	// catches usage from outside ebi-x on the same account.
	usagePollInterval = 5 * time.Minute
	usageReadTimeout  = 30 * time.Second

	usageLowMessage       = "⚠️ Codex の利用枠（%s）の残りが %d%% を下回りました。%s"
	usageExhaustedMessage = "🚫 Codex の利用枠（%s）を使い切りました。%sまで、ebi-x は作業できません。"
	usageResetSuffix      = "%sにリセットされます。"
	usageSummaryPrefix    = "今の残り: "
	usageUnknownReset     = "リセットされる"
	// usageLimitFailureMessage replaces workFailureMessage when the turn
	// stopped on the usage limit, so the requester knows why and when to
	// ask again.
	usageLimitFailureMessage = "⚠️ Codex の利用上限に達したため、作業を完了できませんでした。%s、新しい mention で依頼し直してください。途中までの変更が作業領域に残っている場合があります。"
	usageLimitRetryAt        = "%s以降に"
	usageLimitRetryLater     = "利用枠がリセットされてから"
)

// usageThresholds are the remaining percentages announced once per window,
// highest first. 0 means the window is used up.
var usageThresholds = []int{40, 30, 20, 10, 0}

// UsageReader reads the Codex account's current usage.
type UsageReader interface {
	ReadRateLimits(ctx context.Context) (*codex.RateLimits, error)
}

func (b *Bot) usageWatchEnabled() bool {
	return b.config.Usage != nil && b.config.ApprovalChannelID != ""
}

// runUsageWatch announces in the approval channel when a Codex usage window
// runs low, until ctx is done.
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
			b.announceUsage(ctx, limits)
		}
		if err := b.sleep(ctx, usagePollInterval); err != nil {
			return
		}
	}
}

func (b *Bot) readUsage(ctx context.Context) (*codex.RateLimits, error) {
	readCtx, cancel := context.WithTimeout(ctx, usageReadTimeout)
	defer cancel()
	return b.config.Usage.ReadRateLimits(readCtx)
}

// announceUsage posts one message for the windows that fell below a
// threshold not yet announced in their current window.
func (b *Bot) announceUsage(ctx context.Context, limits *codex.RateLimits) {
	b.usageMu.Lock()
	defer b.usageMu.Unlock()
	announced := b.store.UsageAlerts()
	next := make(map[string]int, len(announced))
	var lines []string
	windows := usageWindows(limits)
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
	if len(lines) > 0 {
		text := strings.Join(lines, "\n") + "\n" + usageSummaryPrefix + usageSummary(limits)
		if err := b.post(ctx, b.config.ApprovalChannelID, "", text); err != nil {
			log.Printf("slackbot: post Codex usage alert: %v", err)
			return
		}
	}
	if !sameLevels(announced, next) {
		if err := b.store.SetUsageAlerts(next); err != nil {
			log.Printf("slackbot: save Codex usage alerts: %v", err)
		}
	}
}

// usageLimitFailure returns the failure message for a turn that hit the
// usage limit, naming when the exhausted window resets when Codex says so.
// It also announces the exhausted window.
func (b *Bot) usageLimitFailure(ctx context.Context) string {
	retry := usageLimitRetryLater
	if b.config.Usage == nil {
		return fmt.Sprintf(usageLimitFailureMessage, retry)
	}
	limits, err := b.readUsage(ctx)
	if err != nil {
		log.Printf("slackbot: read Codex usage after the usage limit: %v", err)
		return fmt.Sprintf(usageLimitFailureMessage, retry)
	}
	var latest time.Time
	for _, window := range limits.Windows() {
		if at := window.ResetTime(); window.UsedPercent >= 100 && at.After(latest) {
			latest = at
		}
	}
	if !latest.IsZero() {
		retry = fmt.Sprintf(usageLimitRetryAt, formatFollowUpTime(latest))
	}
	if b.config.ApprovalChannelID != "" {
		b.announceUsage(ctx, limits)
	}
	return fmt.Sprintf(usageLimitFailureMessage, retry)
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

func sameLevels(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}
