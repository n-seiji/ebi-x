package slackbot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n-seiji/ebi-x/internal/github"
	"github.com/n-seiji/ebi-x/internal/state"
)

var pullWatchNow = time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)

const testPullURL = "https://github.com/o/r/pull/7"

type fakeGitHub struct {
	mu       sync.Mutex
	pull     github.Pull
	pullErr  error
	checks   github.Checks
	activity []github.Activity
	cursor   github.Cursor
	ignored  []string
}

func (g *fakeGitHub) ParsePullURL(raw string) (github.PullRef, bool) {
	if raw != testPullURL {
		return github.PullRef{}, false
	}
	return github.PullRef{Owner: "o", Repo: "r", Number: 7}, true
}

func (g *fakeGitHub) Pull(context.Context, github.PullRef) (github.Pull, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pull, g.pullErr
}

func (g *fakeGitHub) Checks(context.Context, github.PullRef, string) (github.Checks, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checks, nil
}

func (g *fakeGitHub) ActivitySince(_ context.Context, _ github.PullRef, cursor github.Cursor, ignore string) ([]github.Activity, github.Cursor, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ignored = append(g.ignored, ignore)
	if cursor == g.cursor {
		return nil, cursor, nil
	}
	return g.activity, g.cursor, nil
}

func openPull(sha string) github.Pull {
	return github.Pull{Title: "修正", URL: testPullURL, State: "open", HeadSHA: sha}
}

func newPullWatchBot(t *testing.T, store *fakeStore, api *fakeSlack, runner *fakeRunner, gh *fakeGitHub) *Bot {
	t.Helper()
	bot := newTestBot(t, store, api, runner)
	bot.now = func() time.Time { return pullWatchNow }
	bot.config.GitHub = gh
	bot.config.GitHubLogin = "ebix-bot"
	return bot
}

func watched(checks string, cursor state.PullCursor) state.PullWatch {
	return state.PullWatch{
		Channel: "C1", ThreadTS: "100.1", AuthorID: "U1", Owner: "o", Repo: "r", Number: 7,
		URL: testPullURL, HeadSHA: "abc", Checks: checks, Cursor: cursor,
		CreatedAt: pullWatchNow.Add(-time.Hour), ExpiresAt: pullWatchNow.Add(time.Hour),
	}
}

func checkOnce(t *testing.T, bot *Bot, watch state.PullWatch) {
	t.Helper()
	var wg sync.WaitGroup
	bot.checkPullWatch(context.Background(), context.Background(), &wg, watch)
	wg.Wait()
}

func TestTurnStartsPullWatchFromCurrentState(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := followUpRunner("PR を作りました。\n\n## PR の見守り\n- " + testPullURL + "\n- https://example.com/x")
	gh := &fakeGitHub{pull: openPull("abc"), checks: github.Checks{State: github.ChecksPending}, cursor: github.Cursor{Comment: 3}}
	bot := newPullWatchBot(t, store, api, runner, gh)

	bot.HandleMention(context.Background(), mention())

	watch, ok := store.GetPullWatch("o/r#7")
	if !ok {
		t.Fatal("pull request is not watched")
	}
	if watch.HeadSHA != "abc" || watch.Checks != github.ChecksPending || watch.Cursor.Comment != 3 ||
		watch.Channel != "C1" || watch.ThreadTS != "100.1" || watch.AuthorID != "U1" ||
		!watch.ExpiresAt.Equal(pullWatchNow.Add(pullWatchTTL)) {
		t.Fatalf("watch = %+v, want the current state of the pull request", watch)
	}
	post := api.postTexts[len(api.postTexts)-1]
	for _, want := range []string{"PR を作りました。", "👀 次の PR を見守ります。", "- " + testPullURL, "⚠️ 次の URL は見守れる PR として読み取れなかった", "- https://example.com/x"} {
		if !strings.Contains(post, want) {
			t.Errorf("result post %q does not contain %q", post, want)
		}
	}
	if strings.Contains(post, "## PR の見守り") {
		t.Error("result post still carries the watch section")
	}
	if !strings.Contains(runner.prompts[0], "## PR の見守り") {
		t.Error("prompt does not describe the watch contract")
	}
}

func TestPullWatchDisabledWithoutGitHub(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := followUpRunner("PR を作りました。\n\n## PR の見守り\n- " + testPullURL)
	bot := newTestBot(t, store, api, runner)

	bot.HandleMention(context.Background(), mention())

	if post := api.postTexts[len(api.postTexts)-1]; !strings.Contains(post, pullWatchDisabledNotice) {
		t.Fatalf("result post = %q, want the disabled notice", post)
	}
	if strings.Contains(runner.prompts[0], "## PR の見守り") {
		t.Error("prompt describes the watch contract while watching is off")
	}
}

func TestFailedChecksResumeSession(t *testing.T) {
	store := &fakeStore{claim: true, threadIDs: map[string]string{"v5:C1:100.1": "codex-thread"}}
	api := &fakeSlack{}
	runner := followUpRunner("テストを直して push しました。")
	gh := &fakeGitHub{pull: openPull("abc"), checks: github.Checks{State: github.ChecksFailure, Failed: []string{"test"}}}
	bot := newPullWatchBot(t, store, api, runner, gh)

	checkOnce(t, bot, watched(github.ChecksPending, state.PullCursor{}))

	if runner.calls != 1 || runner.threadIDs[0] != "codex-thread" {
		t.Fatalf("runner calls = %d, threads = %v; want the session resumed", runner.calls, runner.threadIDs)
	}
	for _, want := range []string{"<pull_request_event>", "CI が失敗しました（失敗したチェック: test）", testPullURL, "この PR のブランチに push"} {
		if !strings.Contains(runner.prompts[0], want) {
			t.Errorf("prompt does not contain %q", want)
		}
	}
	if !strings.HasPrefix(api.postTexts[0], "🔔 PR に動きがありました（"+testPullURL+"）: CI が失敗しました") {
		t.Fatalf("posts = %q, want an announcement first", api.postTexts)
	}
	watch, _ := store.GetPullWatch("o/r#7")
	if watch.Wakes != 1 || watch.Checks != github.ChecksFailure {
		t.Fatalf("watch = %+v, want one wake and the failure recorded", watch)
	}

	// The same failure on the same commit is not reported again.
	checkOnce(t, bot, watch)
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want no second turn for the same failure", runner.calls)
	}
}

func TestNewActivityResumesSession(t *testing.T) {
	store := &fakeStore{claim: true, threadIDs: map[string]string{"v5:C1:100.1": "codex-thread"}}
	runner := followUpRunner("指摘を直しました。")
	gh := &fakeGitHub{
		pull:   openPull("abc"),
		checks: github.Checks{State: github.ChecksSuccess},
		activity: []github.Activity{{
			Kind: github.KindReview, ID: 5, Author: "alice", State: "CHANGES_REQUESTED", Body: "エラー処理を足して", URL: "https://github.com/o/r/pull/7#r5",
		}},
		cursor: github.Cursor{Review: 5},
	}
	bot := newPullWatchBot(t, store, &fakeSlack{}, runner, gh)

	checkOnce(t, bot, watched(github.ChecksSuccess, state.PullCursor{}))

	if runner.calls != 1 || !strings.Contains(runner.prompts[0], "@alice のレビュー（CHANGES_REQUESTED）: エラー処理を足して") {
		t.Fatalf("prompts = %q, want the review in the event", runner.prompts)
	}
	if watch, _ := store.GetPullWatch("o/r#7"); watch.Cursor.Review != 5 {
		t.Fatalf("cursor = %+v, want the review marked as seen", watch.Cursor)
	}
	if len(gh.ignored) == 0 || gh.ignored[0] != "ebix-bot" {
		t.Fatalf("ignored authors = %v, want the token user", gh.ignored)
	}
}

func TestPullWatchEndsOnMergeAndClose(t *testing.T) {
	tests := map[string]struct {
		pull github.Pull
		want string
	}{
		"merged": {pull: github.Pull{URL: testPullURL, State: "closed", Merged: true}, want: "🎉 PR がマージされました"},
		"closed": {pull: github.Pull{URL: testPullURL, State: "closed"}, want: "PR がマージされずに閉じられました"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{claim: true}
			api := &fakeSlack{}
			runner := &fakeRunner{}
			bot := newPullWatchBot(t, store, api, runner, &fakeGitHub{pull: test.pull})
			watch := watched(github.ChecksPending, state.PullCursor{})
			_ = store.SetPullWatch(watch)

			checkOnce(t, bot, watch)

			if _, ok := store.GetPullWatch("o/r#7"); ok {
				t.Fatal("watch remains after the pull request ended")
			}
			if len(api.postTexts) != 1 || !strings.HasPrefix(api.postTexts[0], test.want) || runner.calls != 0 {
				t.Fatalf("posts = %q, runner calls = %d; want only %q", api.postTexts, runner.calls, test.want)
			}
		})
	}
}

func TestChecksPassingAfterFailureIsReportedWithoutATurn(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newPullWatchBot(t, store, api, runner, &fakeGitHub{pull: openPull("def"), checks: github.Checks{State: github.ChecksSuccess}})

	checkOnce(t, bot, watched(github.ChecksFailure, state.PullCursor{}))

	if runner.calls != 0 || len(api.postTexts) != 1 || api.postTexts[0] != "✅ PR の CI が通りました: "+testPullURL {
		t.Fatalf("posts = %q, runner calls = %d; want only the passed note", api.postTexts, runner.calls)
	}
	if watch, _ := store.GetPullWatch("o/r#7"); watch.HeadSHA != "def" || watch.Checks != github.ChecksSuccess {
		t.Fatalf("watch = %+v, want the new head and state saved", watch)
	}
}

func TestPullWatchWakeLimit(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	runner := &fakeRunner{}
	bot := newPullWatchBot(t, store, api, runner, &fakeGitHub{pull: openPull("abc"), checks: github.Checks{State: github.ChecksFailure, Failed: []string{"test"}}})
	watch := watched(github.ChecksPending, state.PullCursor{})
	watch.Wakes = maxPullWatchWakes
	_ = store.SetPullWatch(watch)

	checkOnce(t, bot, watch)

	if _, ok := store.GetPullWatch("o/r#7"); ok || runner.calls != 0 {
		t.Fatalf("watch kept or turn ran at the limit (runner calls = %d)", runner.calls)
	}
	if len(api.postTexts) != 1 || !strings.Contains(api.postTexts[0], "上限（10回）") {
		t.Fatalf("posts = %q, want the limit notice", api.postTexts)
	}
}

func TestPullWatchDroppedWhenAccessRemovedOrExpired(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newPullWatchBot(t, store, api, &fakeRunner{}, &fakeGitHub{pull: openPull("abc")})
	removed := watched(github.ChecksPending, state.PullCursor{})
	removed.AuthorID = "U9"
	_ = store.SetPullWatch(removed)

	checkOnce(t, bot, removed)

	if _, ok := store.GetPullWatch("o/r#7"); ok || len(api.postTexts) != 0 {
		t.Fatalf("watch of a removed user kept or announced: %q", api.postTexts)
	}

	expired := watched(github.ChecksPending, state.PullCursor{})
	expired.ExpiresAt = pullWatchNow
	_ = store.SetPullWatch(expired)
	checkOnce(t, bot, expired)
	if _, ok := store.GetPullWatch("o/r#7"); ok || len(api.postTexts) != 1 || !strings.HasPrefix(api.postTexts[0], "⌛") {
		t.Fatalf("expired watch kept or not announced: %q", api.postTexts)
	}
}

func TestPullWatchKeepsStateWhenGitHubFails(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newPullWatchBot(t, store, api, &fakeRunner{}, &fakeGitHub{pullErr: errors.New("502")})
	watch := watched(github.ChecksPending, state.PullCursor{})
	_ = store.SetPullWatch(watch)

	checkOnce(t, bot, watch)

	if got, ok := store.GetPullWatch("o/r#7"); !ok || got != watch || len(api.postTexts) != 0 {
		t.Fatalf("watch = %+v, %v; posts = %q; want it unchanged", got, ok, api.postTexts)
	}
}

func TestStopEndsPullWatches(t *testing.T) {
	store := &fakeStore{claim: true}
	api := &fakeSlack{}
	bot := newPullWatchBot(t, store, api, &fakeRunner{}, &fakeGitHub{})
	_ = store.SetPullWatch(watched(github.ChecksPending, state.PullCursor{}))

	bot.stopThread(context.Background(), "C1", "100.1", "200.1")

	if _, ok := store.GetPullWatch("o/r#7"); ok {
		t.Fatal("watch remains after stop")
	}
	if len(api.postTexts) != 1 || api.postTexts[0] != pullWatchStoppedMessage {
		t.Fatalf("posts = %q, want %q", api.postTexts, pullWatchStoppedMessage)
	}
}

func TestPullWatchEventDoesNotCancelFollowUp(t *testing.T) {
	store := &fakeStore{claim: true, threadIDs: map[string]string{"v5:C1:100.1": "codex-thread"}}
	store.followUps = map[string]state.FollowUp{"C1:100.1": {Channel: "C1", ThreadTS: "100.1", Task: "デプロイを確認する", DueAt: pullWatchNow.Add(time.Hour)}}
	bot := newPullWatchBot(t, store, &fakeSlack{}, followUpRunner("直しました。"), &fakeGitHub{pull: openPull("abc"), checks: github.Checks{State: github.ChecksFailure, Failed: []string{"test"}}})

	checkOnce(t, bot, watched(github.ChecksPending, state.PullCursor{}))

	if _, ok := store.GetFollowUp("C1:100.1"); !ok {
		t.Fatal("a pull request event cancelled the pending follow-up")
	}
}
