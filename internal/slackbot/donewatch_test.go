package slackbot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

func TestParseDoneWatchCommand(t *testing.T) {
	tests := []struct {
		text string
		days int
		ok   bool
	}{
		{"<@U2> <@U3> 資料を見てください <@UBOT> 監視して", 1, true},
		{"<@U2> お願いします\n<@UBOT> 監視して。", 1, true},
		{"<@U2> <@UBOT> 毎日監視して", 1, true},
		{"<@U2> <@UBOT> 1日おきに監視して", 2, true},
		{"<@U2> <@UBOT> 2日おきに監視して", 3, true},
		{"<@U2> <@UBOT> 3日ごとに監視して", 3, true},
		{"<@U2> <@UBOT> 監視お願いします！", 1, true},
		{"<@U2> <@UBOT> 7日ごとに監視して", 7, true},
		{"<@U2> <@UBOT> 8日ごとに監視して", 0, false},
		{"<@U2> <@UBOT> 30日ごとに監視して", 0, false},
		{"<@UBOT> サーバーを監視して", 0, false},
		{"<@UBOT> 監視して <@U2> 見てください", 0, false},
		{"<@U2> 監視して", 0, false},
	}
	for _, test := range tests {
		days, ok := parseDoneWatchCommand(test.text, "UBOT")
		if days != test.days || ok != test.ok {
			t.Errorf("parseDoneWatchCommand(%q) = %d, %v, want %d, %v", test.text, days, ok, test.days, test.ok)
		}
	}
}

func TestDoneWatchStopCommand(t *testing.T) {
	for _, text := range []string{"監視をとめて", "監視を止めて。", "監視停止", "監視をやめて", "監視を停止して", "監視を解除して"} {
		if !isDoneWatchStopCommand(text) {
			t.Errorf("isDoneWatchStopCommand(%q) = false, want true", text)
		}
	}
	for _, text := range []string{"監視して", "サーバーの監視をとめて", "とめて"} {
		if isDoneWatchStopCommand(text) {
			t.Errorf("isDoneWatchStopCommand(%q) = true, want false", text)
		}
	}
}

func doneWatchMention(text string) *slackevents.AppMentionEvent {
	return &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "100.1", Text: text}
}

func TestDoneWatchStartsWithoutTurn(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{reactionUsers: []string{"U3"}}
	runner := &fakeRunner{}
	bot := newFollowUpBot(t, store, api, runner)

	bot.HandleMention(context.Background(), doneWatchMention("<@U2> <@U3> <@U2> 確認お願いします <@UBOT> 監視して"))

	if runner.calls != 0 {
		t.Fatalf("runner calls = %d, want none", runner.calls)
	}
	want := state.DoneWatch{
		Channel: "C1", ThreadTS: "100.1", MessageTS: "100.1", AuthorID: "U1",
		Targets: []string{"U2", "U3"}, IntervalDays: 1,
		NextAt: nextDoneWatchReminder(followUpNow, 1), CreatedAt: followUpNow,
	}
	got, ok := store.doneWatches["C1:100.1"]
	if !ok || got.ThreadTS != want.ThreadTS || got.MessageTS != want.MessageTS || got.AuthorID != want.AuthorID ||
		strings.Join(got.Targets, ",") != "U2,U3" || got.IntervalDays != 1 || !got.NextAt.Equal(want.NextAt) {
		t.Fatalf("watch = %#v, want %#v", got, want)
	}
	if post := api.postTexts[len(api.postTexts)-1]; !strings.Contains(post, "対象は元メッセージで @メンションされた2人") || !strings.Contains(post, "平日の毎日19時") || !strings.Contains(post, "10回通知したら") {
		t.Fatalf("notice = %q", post)
	}
}

func TestDoneWatchUsesThreadRootWithoutMentions(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{threadMessages: []ThreadMessage{{AuthorID: "U1", Timestamp: "90.1", Text: "<@U4> 申請してください"}}}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})
	event := doneWatchMention("<@UBOT> 毎日監視して")
	event.ThreadTimeStamp = "90.1"

	bot.HandleMention(context.Background(), event)

	got, ok := store.doneWatches["C1:90.1"]
	if !ok || got.ThreadTS != "90.1" || strings.Join(got.Targets, ",") != "U4" || got.IntervalDays != 1 {
		t.Fatalf("watches = %#v, want the thread root watched daily", store.doneWatches)
	}
}

func TestDoneWatchRefusesWithoutTargetsOrWhenAllDone(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})

	bot.HandleMention(context.Background(), doneWatchMention("<@UBOT> 監視して"))
	if len(store.doneWatches) != 0 || api.postTexts[len(api.postTexts)-1] != doneWatchNoTargetNotice {
		t.Fatalf("watches = %#v, posts = %q", store.doneWatches, api.postTexts)
	}

	api.reactionUsers = []string{"U2"}
	bot.HandleMention(context.Background(), doneWatchMention("<@U2> <@UBOT> 監視して"))
	if len(store.doneWatches) != 0 || api.postTexts[len(api.postTexts)-1] != doneWatchAllDoneNotice {
		t.Fatalf("watches = %#v, posts = %q", store.doneWatches, api.postTexts)
	}
}

func TestCheckDoneWatchRemindsPendingWhenDue(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{reactionUsers: []string{"U3"}}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})
	watch := state.DoneWatch{
		Channel: "C1", ThreadTS: "100.1", MessageTS: "100.1", AuthorID: "U1",
		Targets: []string{"U2", "U3"}, IntervalDays: 2, NextAt: followUpNow.Add(time.Hour), CreatedAt: followUpNow,
	}
	store.SetDoneWatch(watch)

	bot.checkDoneWatch(context.Background(), watch)
	if len(api.postTexts) != 0 {
		t.Fatalf("posts before due = %q, want none", api.postTexts)
	}

	watch.NextAt = followUpNow
	store.SetDoneWatch(watch)
	bot.checkDoneWatch(context.Background(), watch)
	if len(api.posts) != 1 || api.posts[0].threadTS != "100.1" ||
		!strings.HasPrefix(api.posts[0].text, "<@U2>\n") || strings.Contains(api.posts[0].text, "<@U3>") ||
		!strings.Contains(api.posts[0].text, "<https://slack.test/archives/C1/p100.1|元メッセージ>") {
		t.Fatalf("posts = %#v, want one reminder to U2 only", api.posts)
	}
	got := store.doneWatches["C1:100.1"]
	if got.Reminders != 1 || !got.NextAt.Equal(nextDoneWatchReminder(followUpNow, 2)) {
		t.Fatalf("watch after reminder = %#v", got)
	}
}

func TestCheckDoneWatchEnds(t *testing.T) {
	watch := state.DoneWatch{
		Channel: "C1", ThreadTS: "100.1", MessageTS: "100.1", AuthorID: "U1",
		Targets: []string{"U2"}, IntervalDays: 2, NextAt: followUpNow,
	}
	tests := []struct {
		name      string
		reacted   []string
		err       error
		reminders int
		want      string
	}{
		{"all done", []string{"U2"}, nil, 0, doneWatchCompleted},
		{"last reminder", nil, nil, maxDoneWatchReminders - 1, "10回通知したため、監視はこれで終了します"},
		{"message deleted", nil, slack.SlackErrorResponse{Err: "message_not_found"}, 0, doneWatchGoneMessage},
	}
	for _, test := range tests {
		store := &fakeStore{claim: true}
		api := &fakeSlack{reactionUsers: test.reacted, reactionUsersErr: test.err}
		bot := newFollowUpBot(t, store, api, &fakeRunner{})
		watch := watch
		watch.Reminders = test.reminders
		store.SetDoneWatch(watch)

		bot.checkDoneWatch(context.Background(), watch)

		if len(store.doneWatches) != 0 {
			t.Errorf("%s: watches = %#v, want none", test.name, store.doneWatches)
		}
		if len(api.postTexts) != 1 || !strings.Contains(api.postTexts[0], test.want) {
			t.Errorf("%s: posts = %q, want %q", test.name, api.postTexts, test.want)
		}
	}
}

func TestCheckDoneWatchAfterStopStaysStopped(t *testing.T) {
	// The poll took its snapshot, then the watch was stopped before the
	// check ran: the check must neither remind nor save the watch again.
	for _, reacted := range [][]string{nil, {"U2"}} {
		store := &fakeStore{claim: true}
		api := &fakeSlack{reactionUsers: reacted}
		bot := newFollowUpBot(t, store, api, &fakeRunner{})
		watch := state.DoneWatch{
			Channel: "C1", ThreadTS: "100.1", MessageTS: "100.1", AuthorID: "U1",
			Targets: []string{"U2"}, IntervalDays: 2, NextAt: followUpNow,
		}

		bot.checkDoneWatch(context.Background(), watch)

		if len(store.doneWatches) != 0 || len(api.postTexts) != 0 {
			t.Errorf("reacted %q: watches = %#v, posts = %q, want none", reacted, store.doneWatches, api.postTexts)
		}
	}
}

func TestDoneWatchStopCommandKeepsOtherWork(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})
	store.SetDoneWatch(state.DoneWatch{Channel: "C1", ThreadTS: "90.1", MessageTS: "90.1", AuthorID: "U1", Targets: []string{"U2"}})
	store.SetSchedule("C1:90.1", state.Schedule{Channel: "C1", ThreadTS: "90.1", AuthorID: "U1"})
	event := doneWatchMention("<@UBOT> 監視をとめて")
	event.ThreadTimeStamp = "90.1"

	bot.HandleMention(context.Background(), event)
	if len(store.doneWatches) != 0 || len(store.schedules) != 1 || api.postTexts[len(api.postTexts)-1] != doneWatchStoppedMessage {
		t.Fatalf("watches = %#v, schedules = %#v, posts = %q", store.doneWatches, store.schedules, api.postTexts)
	}

	bot.HandleMention(context.Background(), event)
	if api.postTexts[len(api.postTexts)-1] != doneWatchNothingToStop {
		t.Fatalf("second stop posts = %q", api.postTexts)
	}
}

func TestDoneWatchChannelTargetsMembers(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{channelMembers: []string{"U1", "U2", "U5", "U6"}}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})

	bot.HandleMention(context.Background(), doneWatchMention("<!channel> <@U5> 回答してください <@UBOT> 監視して"))

	got, ok := store.doneWatches["C1:100.1"]
	if !ok || strings.Join(got.Targets, ",") != "U5,U2,U6" {
		t.Fatalf("watches = %#v, want the named user, then the other members without the author", store.doneWatches)
	}
}

func TestDoneWatchUserGroupTargetsMembers(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{
		channelMembers: []string{"U1", "U2", "U5", "U7", "U8", "B1"},
		groupMembers: map[string][]string{
			"S1": {"U1", "U2", "U5", "B1"},
			"S2": {"U5", "U7"},
		},
	}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})

	bot.HandleMention(context.Background(), doneWatchMention("<@U9> <!subteam^S1|@dev_team> <!subteam^S2> 回答してください <@UBOT> 監視して"))

	got, ok := store.doneWatches["C1:100.1"]
	if !ok || strings.Join(got.Targets, ",") != "U9,U2,U5,U7" {
		t.Fatalf("watches = %#v, want the named user, then both groups' members once each without the author or bots", store.doneWatches)
	}
	if last := api.postTexts[len(api.postTexts)-1]; strings.Contains(last, "このチャンネルにいない") {
		t.Fatalf("start notice = %q, want no left-out note", last)
	}
}

func TestDoneWatchUserGroupKeepsChannelMembersOnly(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{
		channelMembers: []string{"U1", "U2"},
		groupMembers:   map[string][]string{"S1": {"U1", "U2", "U3", "U4", "B9"}},
	}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})

	bot.HandleMention(context.Background(), doneWatchMention("<!subteam^S1> 回答してください <@UBOT> 監視して"))

	got, ok := store.doneWatches["C1:100.1"]
	if !ok || strings.Join(got.Targets, ",") != "U2" {
		t.Fatalf("watches = %#v, want only the group member in the channel", store.doneWatches)
	}
	if last := api.postTexts[len(api.postTexts)-1]; !strings.Contains(last, fmt.Sprintf(doneWatchOutsideNotice, 2)) {
		t.Fatalf("start notice = %q, want it to say 2 people outside the channel were left out", last)
	}
}

func TestDoneWatchUserGroupAllOutsideChannel(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{
		channelMembers: []string{"U1"},
		groupMembers:   map[string][]string{"S1": {"U3", "U4"}},
	}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})

	bot.HandleMention(context.Background(), doneWatchMention("<!subteam^S1> 回答してください <@UBOT> 監視して"))

	if len(store.doneWatches) != 0 || api.postTexts[len(api.postTexts)-1] != doneWatchAllOutsideNotice {
		t.Fatalf("watches = %#v, posts = %q, want no watch", store.doneWatches, api.postTexts)
	}
}

func TestDoneWatchUserGroupFromThreadRoot(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{
		channelMembers: []string{"U1", "U2", "U3"},
		groupMembers:   map[string][]string{"S1": {"U2", "U3"}},
		threadMessages: []ThreadMessage{{AuthorID: "U1", Timestamp: "100.0", Text: "<!subteam^S1> 回答してください"}},
	}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})
	event := doneWatchMention("<@UBOT> 監視して")
	event.ThreadTimeStamp = "100.0"

	bot.HandleMention(context.Background(), event)

	got, ok := store.doneWatches["C1:100.0"]
	if !ok || strings.Join(got.Targets, ",") != "U2,U3" {
		t.Fatalf("watches = %#v, want the group's members from the thread root", store.doneWatches)
	}
}

func TestDoneWatchUserGroupMissingScope(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{groupMembersErr: slack.SlackErrorResponse{Err: "missing_scope"}}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})

	bot.HandleMention(context.Background(), doneWatchMention("<!subteam^S1> 回答してください <@UBOT> 監視して"))

	if len(store.doneWatches) != 0 || api.postTexts[len(api.postTexts)-1] != doneWatchScopeNotice {
		t.Fatalf("watches = %#v, posts = %q", store.doneWatches, api.postTexts)
	}
}

func TestDoneWatchChannelRefusals(t *testing.T) {
	many := make([]string, maxDoneWatchTargets+5)
	for i := range many {
		many[i] = fmt.Sprintf("U%d", i+10)
	}
	tests := []struct {
		name    string
		members []string
		err     error
		want    string
	}{
		{"too many", many, nil, fmt.Sprintf(doneWatchTooManyNotice, maxDoneWatchTargets)},
		{"missing scope", nil, slack.SlackErrorResponse{Err: "missing_scope"}, doneWatchScopeNotice},
	}
	for _, test := range tests {
		store := &fakeStore{claim: true}
		api := &fakeSlack{channelMembers: test.members, channelMembersErr: test.err}
		bot := newFollowUpBot(t, store, api, &fakeRunner{})

		bot.HandleMention(context.Background(), doneWatchMention("<!here> 回答してください <@UBOT> 監視して"))

		if len(store.doneWatches) != 0 || api.postTexts[len(api.postTexts)-1] != test.want {
			t.Errorf("%s: watches = %#v, posts = %q", test.name, store.doneWatches, api.postTexts)
		}
	}
}

func TestDoneWatchNotesNamedPeopleOutsideChannel(t *testing.T) {
	tests := []struct {
		name     string
		members  []string
		err      error
		wantNote string // "" for no note
	}{
		{"one outside", []string{"U1", "U2"}, nil, "<@U3> はまだこのチャンネルに参加していないようです"},
		{"all inside", []string{"U1", "U2", "U3"}, nil, ""},
		{"no channels:read", nil, slack.SlackErrorResponse{Err: "missing_scope"}, ""},
	}
	for _, test := range tests {
		store := &fakeStore{claim: true}
		api := &fakeSlack{channelMembers: test.members, channelMembersErr: test.err}
		bot := newFollowUpBot(t, store, api, &fakeRunner{})

		bot.HandleMention(context.Background(), doneWatchMention("<@U2> <@U3> 確認お願いします <@UBOT> 監視して"))

		got, ok := store.doneWatches["C1:100.1"]
		if !ok || strings.Join(got.Targets, ",") != "U2,U3" {
			t.Errorf("%s: watch = %#v, want U2 and U3 kept as targets", test.name, got)
			continue
		}
		post := api.postTexts[len(api.postTexts)-1]
		if test.wantNote == "" && strings.Contains(post, "参加していない") {
			t.Errorf("%s: notice = %q, want no outside note", test.name, post)
		}
		if test.wantNote != "" && !strings.Contains(post, test.wantNote) {
			t.Errorf("%s: notice = %q, want %q", test.name, post, test.wantNote)
		}
	}
}

func TestNextDoneWatchReminderSkipsWeekends(t *testing.T) {
	at := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.Local) }
	tests := []struct {
		name  string
		after time.Time
		days  int
		want  time.Time
	}{
		{"weekday before 19:00", at(19, 10), 1, at(19, 19)},
		{"weekday at 19:00", at(19, 19), 1, at(20, 19)},
		{"Friday evening", at(23, 20), 1, at(26, 19)},
		{"Saturday", at(24, 10), 1, at(26, 19)},
		{"every other weekday from Friday", at(23, 10), 2, at(26, 19)},
		{"every three weekdays from Thursday night", at(22, 20), 3, at(27, 19)},
	}
	for _, test := range tests {
		if got := nextDoneWatchReminder(test.after, test.days); !got.Equal(test.want) {
			t.Errorf("%s: nextDoneWatchReminder(%v, %d) = %v, want %v", test.name, test.after, test.days, got, test.want)
		}
	}
}
