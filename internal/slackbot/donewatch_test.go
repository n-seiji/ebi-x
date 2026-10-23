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
		{"<@U2> <@U3> 資料を見てください <@UBOT> 監視して", 2, true},
		{"<@U2> お願いします\n<@UBOT> 監視して。", 2, true},
		{"<@U2> <@UBOT> 毎日監視して", 1, true},
		{"<@U2> <@UBOT> 1日おきに監視して", 2, true},
		{"<@U2> <@UBOT> 2日おきに監視して", 3, true},
		{"<@U2> <@UBOT> 3日ごとに監視して", 3, true},
		{"<@U2> <@UBOT> 監視お願いします！", 2, true},
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
		Targets: []string{"U2", "U3"}, IntervalDays: 2,
		NextAt: followUpNow.Add(48 * time.Hour), CreatedAt: followUpNow,
	}
	got, ok := store.doneWatches["C1:100.1"]
	if !ok || got.ThreadTS != want.ThreadTS || got.MessageTS != want.MessageTS || got.AuthorID != want.AuthorID ||
		strings.Join(got.Targets, ",") != "U2,U3" || got.IntervalDays != 2 || !got.NextAt.Equal(want.NextAt) {
		t.Fatalf("watch = %#v, want %#v", got, want)
	}
	if post := api.postTexts[len(api.postTexts)-1]; !strings.Contains(post, "対象は元メッセージで @メンションされた2人") || !strings.Contains(post, "1日おき") {
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
	if got.Reminders != 1 || !got.NextAt.Equal(followUpNow.Add(48*time.Hour)) {
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
		{"limit", nil, nil, maxDoneWatchReminders, "通知が上限の10回に達したため"},
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
	api := &fakeSlack{groupMembers: map[string][]string{
		"S1": {"U1", "U2", "U5"},
		"S2": {"U5", "U7"},
	}}
	bot := newFollowUpBot(t, store, api, &fakeRunner{})

	bot.HandleMention(context.Background(), doneWatchMention("<@U9> <!subteam^S1|@dev_team> <!subteam^S2> 回答してください <@UBOT> 監視して"))

	got, ok := store.doneWatches["C1:100.1"]
	if !ok || strings.Join(got.Targets, ",") != "U9,U2,U5,U7" {
		t.Fatalf("watches = %#v, want the named user, then both groups' members once each without the author", store.doneWatches)
	}
}

func TestDoneWatchUserGroupFromThreadRoot(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{
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
