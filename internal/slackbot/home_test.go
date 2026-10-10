package slackbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

func homeText(t *testing.T, api *fakeSlack, user string) string {
	t.Helper()
	for _, call := range api.calls {
		if call.kind == "home:"+user {
			return call.text
		}
	}
	t.Fatalf("Slack calls = %v, want the home tab published for %s", api.calls, user)
	return ""
}

func TestHomeTabListsTheUsersWork(t *testing.T) {
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newTestBot(t, store, api, &fakeRunner{})
	bot.now = func() time.Time { return now }
	bot.allowedUsers = makeSet([]string{"U1", "U2"})
	store.followUps = map[string]state.FollowUp{
		"C1:100.1": {Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Task: "CIを確認する", DueAt: now.Add(time.Hour)},
		"C1:300.1": {Channel: "C1", ThreadTS: "300.1", AuthorID: "U2", Task: "他人の予定", DueAt: now.Add(time.Hour)},
	}
	_ = store.SetSchedule("C1:600.1", state.Schedule{Channel: "C1", ThreadTS: "600.1", AuthorID: "U1", Spec: "平日 09:00", Task: "朝の確認", NextAt: now.Add(24 * time.Hour)})
	_ = store.SetSchedule("C1:700.1", state.Schedule{Channel: "C1", ThreadTS: "700.1", AuthorID: "U2", Spec: "毎日 09:00", Task: "他人の定期実行", NextAt: now.Add(time.Hour)})
	_ = store.SetPullWatch(state.PullWatch{Channel: "D1", ThreadTS: "500.1", AuthorID: "U1", Owner: "o", Repo: "r", Number: 7, URL: testPullURL, Checks: "failure"})
	_, untrack := bot.trackRequest(context.Background(), "C1:200.1", "C1:200.2", "U1")
	defer untrack()
	_, untrackOther := bot.trackRequest(context.Background(), "C1:400.1", "C1:400.2", "U2")
	defer untrackOther()

	bot.HandleAppHomeOpened(context.Background(), &slackevents.AppHomeOpenedEvent{User: "U1", Tab: "home"})

	text := homeText(t, api, "U1")
	for _, want := range []string{
		"*⏳ 作業中（1）*\n• <https://slack.test/archives/C1/p200.1|<#C1> のスレッド>",
		"*⏰ 予定中のフォローアップ（1）*\n• <https://slack.test/archives/C1/p100.1|<#C1> のスレッド> — ",
		"ごろ: CIを確認する",
		"*👀 見守り中の PR（1）*\n• <https://slack.test/archives/D1/p500.1|DM のスレッド> — " + testPullURL + "（CI: 失敗）",
		"*🔁 定期実行（1）*\n• <https://slack.test/archives/C1/p600.1|<#C1> のスレッド> — 平日 09:00「朝の確認」（次回 ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("home text does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "他人の予定") || strings.Contains(text, "400.1") {
		t.Errorf("home text shows another user's work:\n%s", text)
	}
}

func TestHomeTabEmptyAndForbidden(t *testing.T) {
	api := &fakeSlack{}
	bot := newTestBot(t, &fakeStore{claim: true}, api, &fakeRunner{})

	bot.HandleAppHomeOpened(context.Background(), &slackevents.AppHomeOpenedEvent{User: "U1", Tab: "home"})
	bot.HandleAppHomeOpened(context.Background(), &slackevents.AppHomeOpenedEvent{User: "U9", Tab: "home"})

	if text := homeText(t, api, "U1"); strings.Count(text, homeEmpty) != 4 {
		t.Errorf("home text = %q, want four empty lists", text)
	}
	if text := homeText(t, api, "U9"); text != homeForbidden {
		t.Errorf("home text for an unallowed user = %q, want %q", text, homeForbidden)
	}
}
