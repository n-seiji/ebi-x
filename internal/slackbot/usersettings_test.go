package slackbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/usersettings"
	"github.com/slack-go/slack/slackevents"
)

func turnResponse(text string) runnerResponse {
	return runnerResponse{result: &codex.TurnResult{Completed: true, Messages: []string{text}}}
}

func TestUserSettingsReachOnlyTheirOwnerInASharedThread(t *testing.T) {
	now := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		turnResponse("わかりました。\n## 個人設定\n- 返事は3行以内にする\n- 英語で答える"),
		turnResponse("Done."),
		turnResponse("Done."),
	}}
	bot := newTestBot(t, store, api, runner)
	configureActiveSubscription(bot, store, now)

	bot.HandleMessage(context.Background(), messageReply("U2", "200.2", "今後は返事を短く英語で"))
	bot.HandleMessage(context.Background(), messageReply("U3", "300.3", "次の依頼"))
	bot.HandleMessage(context.Background(), messageReply("U2", "400.4", "さらに次の依頼"))

	settings, err := usersettings.Read(bot.config.MemoryDir, "U2")
	if err != nil || strings.Join(settings, "|") != "返事は3行以内にする|英語で答える" {
		t.Fatalf("U2 settings = %q, %v", settings, err)
	}
	if other, _ := usersettings.Read(bot.config.MemoryDir, "U3"); len(other) != 0 {
		t.Fatalf("U3 settings = %q, want none", other)
	}
	if len(runner.prompts) != 3 {
		t.Fatalf("prompts = %d, want 3", len(runner.prompts))
	}
	if !strings.Contains(runner.prompts[0], "<user_settings>\n（設定なし）\n</user_settings>") {
		t.Errorf("first prompt lacks the empty settings:\n%s", runner.prompts[0])
	}
	if strings.Contains(runner.prompts[1], "英語で答える") || !strings.Contains(runner.prompts[1], "（設定なし）") {
		t.Errorf("another speaker's prompt carries U2's settings:\n%s", runner.prompts[1])
	}
	if !strings.Contains(runner.prompts[2], "<user_settings>\n- 返事は3行以内にする\n- 英語で答える\n</user_settings>") {
		t.Errorf("U2's later prompt lacks their settings:\n%s", runner.prompts[2])
	}

	posts := strings.Join(api.postTexts, "|")
	for _, private := range []string{"## 個人設定", "返事は3行以内にする", "英語で答える"} {
		if strings.Contains(posts, private) {
			t.Errorf("thread posts expose %q: %q", private, posts)
		}
	}
	if !strings.Contains(posts, "わかりました。") {
		t.Errorf("posts = %q, want the answer", posts)
	}
	var notices []string
	for _, call := range api.calls {
		if strings.HasPrefix(call.kind, "ephemeral:") {
			notices = append(notices, call.kind+" "+call.text)
		}
	}
	if len(notices) != 1 || !strings.HasPrefix(notices[0], "ephemeral:C1:U2 ") || !strings.Contains(notices[0], "2件") || strings.Contains(notices[0], "英語") {
		t.Errorf("ephemeral notices = %q, want one count-only notice to U2", notices)
	}
}

func TestUserSettingsCanBeClearedAndIgnoreMalformedOutput(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{responses: []runnerResponse{
		turnResponse("消しました。\n## 個人設定\n- なし"),
	}}
	bot := newTestBot(t, store, api, runner)
	if _, err := usersettings.Write(bot.config.MemoryDir, "U1", []string{"敬語を使わない"}); err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []string{
		"## 個人設定\n本文だけ",
		"## 個人設定\n- a\n## 個人設定\n- b",
	} {
		rest, _, present, invalid := codex.SplitUserSettings(malformed)
		if !present || !invalid || strings.Contains(rest, "## 個人設定") {
			t.Errorf("SplitUserSettings(%q) = %q, present %v, invalid %v", malformed, rest, present, invalid)
		}
	}

	bot.HandleMention(context.Background(), mention())

	if settings, _ := usersettings.Read(bot.config.MemoryDir, "U1"); len(settings) != 0 {
		t.Fatalf("settings = %q, want cleared", settings)
	}
	if !strings.Contains(runner.prompts[0], "- 敬語を使わない") {
		t.Errorf("prompt lacks the settings before they were cleared:\n%s", runner.prompts[0])
	}
	if posts := strings.Join(api.postTexts, "|"); strings.Contains(posts, "なし") || strings.Contains(posts, "個人設定") {
		t.Errorf("posts = %q, want the settings section removed", posts)
	}
}

func TestUserSettingsChangeOnlyFromARequestAPersonWrote(t *testing.T) {
	bot := newTestBot(t, &fakeStore{claim: true}, &fakeSlack{}, &fakeRunner{})
	for _, source := range []triggerSource{followUpTrigger, pullWatchTrigger, scheduleTrigger} {
		if notice := bot.saveUserSettings("event", processingTrigger{source: source, authorID: "U1"}, []string{"短く"}); notice != "" {
			t.Errorf("source %d saved settings with notice %q", source, notice)
		}
	}
	if settings, _ := usersettings.Read(bot.config.MemoryDir, "U1"); len(settings) != 0 {
		t.Fatalf("settings = %q, want none", settings)
	}
}

func TestHomeTabShowsTheUsersOwnSettings(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, &fakeRunner{})
	bot.allowedUsers = makeSet([]string{"U1", "U2"})
	if _, err := usersettings.Write(bot.config.MemoryDir, "U1", []string{"返事は短く"}); err != nil {
		t.Fatal(err)
	}
	if _, err := usersettings.Write(bot.config.MemoryDir, "U2", []string{"他人の設定"}); err != nil {
		t.Fatal(err)
	}

	bot.HandleAppHomeOpened(context.Background(), &slackevents.AppHomeOpenedEvent{User: "U1", Tab: "home"})

	text := homeText(t, api, "U1")
	if !strings.Contains(text, "*⚙️ あなたの個人設定（1）*\n• 返事は短く") || strings.Contains(text, "他人の設定") {
		t.Errorf("home text = %q, want only U1's settings", text)
	}
}
