package slackbot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
)

type fakeUsage struct {
	limits *codex.RateLimits
	err    error
	calls  int
}

func (u *fakeUsage) ReadRateLimits(context.Context) (*codex.RateLimits, error) {
	u.calls++
	return u.limits, u.err
}

func usageLimits(primaryUsed, secondaryUsed float64) *codex.RateLimits {
	return &codex.RateLimits{
		Primary:   &codex.RateLimitWindow{UsedPercent: primaryUsed, WindowMinutes: 300, ResetsAt: time.Date(2026, 10, 20, 6, 30, 0, 0, time.UTC).Unix()},
		Secondary: &codex.RateLimitWindow{UsedPercent: secondaryUsed, WindowMinutes: 7 * 24 * 60, ResetsAt: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC).Unix()},
	}
}

func newUsageBot(t *testing.T, store *fakeStore, api *fakeSlack, usage UsageReader) *Bot {
	t.Helper()
	bot := newTestBot(t, store, api, &fakeRunner{})
	bot.config.ApprovalChannelID = "CAPPROVAL"
	bot.config.Usage = usage
	return bot
}

func TestAnnounceUsageOncePerThreshold(t *testing.T) {
	api := &fakeSlack{}
	store := &fakeStore{}
	bot := newUsageBot(t, store, api, &fakeUsage{})
	ctx := context.Background()

	steps := []struct {
		primary, secondary float64
		want               []string
	}{
		{primary: 50, secondary: 10},
		{primary: 61, secondary: 10, want: []string{"Codex の利用枠（5時間）の残りが 40% を下回りました。"}},
		{primary: 65, secondary: 10},
		{primary: 85, secondary: 75, want: []string{"（5時間）の残りが 20% を下回りました", "（1週間）の残りが 30% を下回りました"}},
		{primary: 100, secondary: 75, want: []string{"Codex の利用枠（5時間）を使い切りました。", "まで、ebi-x は作業できません。"}},
		{primary: 100, secondary: 75},
		// The 5-hour window resets; a later fall is announced again.
		{primary: 5, secondary: 76},
		{primary: 70, secondary: 76, want: []string{"（5時間）の残りが 40% を下回りました"}},
	}
	for i, step := range steps {
		before := len(api.posts)
		bot.announceUsage(ctx, usageLimits(step.primary, step.secondary))
		posted := api.posts[before:]
		if len(step.want) == 0 {
			if len(posted) != 0 {
				t.Fatalf("step %d: posted %q, want nothing", i, posted[0].text)
			}
			continue
		}
		if len(posted) != 1 {
			t.Fatalf("step %d: posts = %d, want 1", i, len(posted))
		}
		if posted[0].channel != "CAPPROVAL" || posted[0].threadTS != "" {
			t.Fatalf("step %d: posted to %q/%q", i, posted[0].channel, posted[0].threadTS)
		}
		for _, want := range step.want {
			if !strings.Contains(posted[0].text, want) {
				t.Fatalf("step %d: post %q does not contain %q", i, posted[0].text, want)
			}
		}
		if !strings.Contains(posted[0].text, usageSummaryPrefix) {
			t.Fatalf("step %d: post %q has no summary", i, posted[0].text)
		}
	}
}

func TestAnnounceUsageSurvivesRestart(t *testing.T) {
	api := &fakeSlack{}
	store := &fakeStore{}
	newUsageBot(t, store, api, &fakeUsage{}).announceUsage(context.Background(), usageLimits(65, 0))
	newUsageBot(t, store, api, &fakeUsage{}).announceUsage(context.Background(), usageLimits(66, 0))
	if len(api.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(api.posts))
	}
}

func TestUsageLevel(t *testing.T) {
	tests := []struct {
		used    float64
		level   int
		crossed bool
	}{
		{used: 60, crossed: false},
		{used: 60.5, level: 40, crossed: true},
		{used: 91, level: 10, crossed: true},
		{used: 99.9, level: 10, crossed: true},
		{used: 100, level: 0, crossed: true},
	}
	for _, tt := range tests {
		level, crossed := usageLevel(codex.RateLimitWindow{UsedPercent: tt.used})
		if level != tt.level || crossed != tt.crossed {
			t.Fatalf("usageLevel(%v) = %d, %v; want %d, %v", tt.used, level, crossed, tt.level, tt.crossed)
		}
	}
}

func TestUsageLimitFailureNamesReset(t *testing.T) {
	api := &fakeSlack{}
	limits := usageLimits(100, 40)
	bot := newUsageBot(t, &fakeStore{}, api, &fakeUsage{limits: limits})
	message := bot.usageLimitFailure(context.Background())
	if !strings.Contains(message, formatFollowUpTime(limits.Primary.ResetTime())+"以降に") {
		t.Fatalf("message = %q", message)
	}
	if len(api.posts) != 1 || api.posts[0].channel != "CAPPROVAL" {
		t.Fatalf("posts = %+v, want one announcement", api.posts)
	}
}

func TestUsageLimitFailureWithoutUsage(t *testing.T) {
	bot := newUsageBot(t, &fakeStore{}, &fakeSlack{}, &fakeUsage{err: errors.New("api key")})
	if message := bot.usageLimitFailure(context.Background()); !strings.Contains(message, usageLimitRetryLater) {
		t.Fatalf("message = %q", message)
	}
}

func TestWorkTurnUsageLimitExplainsFailure(t *testing.T) {
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{{result: &codex.TurnResult{
		ThreadID: "thread-1",
		Err:      "You’ve hit your usage limit. Try again at 3:30 PM.",
	}}}}
	store := &fakeStore{claim: true}
	bot := newTestBot(t, store, api, runner)

	bot.HandleMention(context.Background(), mention())

	got := strings.Join(api.postTexts, "\n")
	if !strings.Contains(got, "Codex の利用上限に達したため") || strings.Contains(got, workFailureMessage) {
		t.Fatalf("posts = %q", api.postTexts)
	}
}
