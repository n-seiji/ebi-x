package slackbot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/n-seiji/ebi-x/internal/github"
	"github.com/n-seiji/ebi-x/internal/state"
)

const (
	// pullWatchPollInterval is how often watched pull requests are checked.
	pullWatchPollInterval = 2 * time.Minute
	// pullWatchTTL is how long a watch lasts after it starts or last acts.
	pullWatchTTL = 7 * 24 * time.Hour
	// maxPullWatchWakes caps the turns one watch starts without a person
	// writing in the thread.
	maxPullWatchWakes = 10
	// maxPullWatchesPerTurn bounds the pull requests one answer may watch.
	maxPullWatchesPerTurn = 5
	maxPullActivityItems  = 10
	maxPullActivityRunes  = 300

	pullWatchStartedNotice  = "👀 次の PR を見守ります。CI の失敗やレビュー・コメントがあれば、このスレッドで対応して報告します。止める場合は、このスレッドで「停止」と mention してください。"
	pullWatchDisabledNotice = "⚠️ GitHub の見守りが設定されていないため、PR は見守っていません。"
	pullWatchInvalidNotice  = "⚠️ 次の URL は見守れる PR として読み取れなかったため、見守っていません。"
	pullWatchFailedNotice   = "⚠️ 次の PR の状態を GitHub から読めなかったため、見守っていません。"
	pullWatchClosedNotice   = "次の PR はマージ済みか閉じられているため、見守っていません。"
	pullWatchTooManyNotice  = "⚠️ 一度に見守れる PR は%d件までのため、残りは見守っていません。"
	pullWatchWakeMessage    = "🔔 PR に動きがありました（%s）: %s。対応を始めます。"
	pullMergedMessage       = "🎉 PR がマージされました: %s\nPR の見守りを終了します。"
	pullClosedMessage       = "PR がマージされずに閉じられました: %s\nPR の見守りを終了します。"
	pullChecksPassedMessage = "✅ PR の CI が通りました: %s"
	pullWatchExpiredMessage = "⌛ 7日間動きがなかったため、PR の見守りを終了しました: %s"
	pullWatchLimitMessage   = "⚠️ 人の発言なしで PR に対応できる回数の上限（%d回）に達したため、見守りを終了しました: %s\n続ける場合は、このスレッドで依頼してください。"
	pullWatchStoppedMessage = "PR の見守りを終了しました。"
)

// GitHub is the subset of the GitHub API the pull request watch uses.
type GitHub interface {
	ParsePullURL(raw string) (github.PullRef, bool)
	Pull(ctx context.Context, ref github.PullRef) (github.Pull, error)
	Checks(ctx context.Context, ref github.PullRef, sha string) (github.Checks, error)
	ActivitySince(ctx context.Context, ref github.PullRef, cursor github.Cursor, ignoreAuthor string) ([]github.Activity, github.Cursor, error)
}

func (b *Bot) pullWatchEnabled() bool {
	return b.config.GitHub != nil
}

// watchPulls starts watching the pull requests a finished turn listed and
// returns the notice to post after its result. A watch starts from the pull
// request's current state, so only later CI results and comments wake the
// session.
func (b *Bot) watchPulls(ctx context.Context, trigger processingTrigger, urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	if !b.pullWatchEnabled() {
		return pullWatchDisabledNotice
	}
	var started, invalid, failed, closed []string
	tooMany := false
	for i, raw := range urls {
		if i >= maxPullWatchesPerTurn {
			tooMany = true
			break
		}
		ref, ok := b.config.GitHub.ParsePullURL(raw)
		if !ok {
			invalid = append(invalid, shortDetail(raw))
			continue
		}
		pull, err := b.config.GitHub.Pull(ctx, ref)
		if err != nil {
			log.Printf("slackbot: read pull request %s: %v", ref, err)
			failed = append(failed, ref.String())
			continue
		}
		if pull.State != "open" {
			closed = append(closed, pull.URL)
			continue
		}
		checks, err := b.config.GitHub.Checks(ctx, ref, pull.HeadSHA)
		if err != nil {
			log.Printf("slackbot: read checks of %s: %v", ref, err)
			failed = append(failed, ref.String())
			continue
		}
		_, cursor, err := b.config.GitHub.ActivitySince(ctx, ref, github.Cursor{}, b.config.GitHubLogin)
		if err != nil {
			log.Printf("slackbot: read activity of %s: %v", ref, err)
			failed = append(failed, ref.String())
			continue
		}
		now := b.now()
		watch := state.PullWatch{
			Channel: trigger.channel, ThreadTS: trigger.threadTS, AuthorID: trigger.authorID,
			Owner: ref.Owner, Repo: ref.Repo, Number: ref.Number, URL: pull.URL,
			HeadSHA: pull.HeadSHA, Checks: checks.State, Cursor: state.PullCursor(cursor),
			CreatedAt: now, ExpiresAt: now.Add(pullWatchTTL),
		}
		// A turn the watch itself started keeps counting toward the cap;
		// a person's request in the thread starts the count again.
		if previous, ok := b.store.GetPullWatch(watch.Key()); ok && trigger.source == pullWatchTrigger &&
			previous.Channel == watch.Channel && previous.ThreadTS == watch.ThreadTS {
			watch.Wakes = previous.Wakes
			watch.CreatedAt = previous.CreatedAt
		}
		if err := b.store.SetPullWatch(watch); err != nil {
			log.Printf("slackbot: save pull request watch %s: %v", ref, err)
			failed = append(failed, ref.String())
			continue
		}
		started = append(started, pull.URL)
	}
	var notices []string
	if len(started) > 0 {
		notices = append(notices, pullWatchStartedNotice+bulletList(started))
	}
	if len(invalid) > 0 {
		notices = append(notices, pullWatchInvalidNotice+bulletList(invalid))
	}
	if len(failed) > 0 {
		notices = append(notices, pullWatchFailedNotice+bulletList(failed))
	}
	if len(closed) > 0 {
		notices = append(notices, pullWatchClosedNotice+bulletList(closed))
	}
	if tooMany {
		notices = append(notices, fmt.Sprintf(pullWatchTooManyNotice, maxPullWatchesPerTurn))
	}
	return strings.Join(notices, "\n\n")
}

func bulletList(items []string) string {
	return "\n- " + strings.Join(items, "\n- ")
}

// runPullWatches checks the watched pull requests until acceptCtx is done.
// The turns they start run on turnCtx under wg, like a turn started by a
// Slack event.
func (b *Bot) runPullWatches(acceptCtx, turnCtx context.Context, wg *sync.WaitGroup) {
	if !b.pullWatchEnabled() {
		return
	}
	for {
		for _, watch := range b.store.PullWatches() {
			if acceptCtx.Err() != nil {
				return
			}
			b.checkPullWatch(acceptCtx, turnCtx, wg, watch)
		}
		if err := b.sleep(acceptCtx, pullWatchPollInterval); err != nil {
			return
		}
	}
}

// checkPullWatch compares a pull request with what its watch last saw. It
// reports merges, closes, and CI turning green itself, and resumes the
// thread's session for failed CI and new reviews or comments.
func (b *Bot) checkPullWatch(ctx, turnCtx context.Context, wg *sync.WaitGroup, watch state.PullWatch) {
	key := watch.Key()
	// A turn already working in the thread may be pushing to the pull
	// request; look again once it is done.
	if b.threadBusy(threadRef(watch.Channel, watch.ThreadTS)) {
		return
	}
	if !b.userAllowed(watch.AuthorID) || !b.conversationAllowed(watch.Channel) {
		log.Printf("slackbot: drop pull request watch %s: user %q or channel is no longer allowed", key, watch.AuthorID)
		b.endPullWatch(ctx, watch, "")
		return
	}
	if !b.now().Before(watch.ExpiresAt) {
		b.endPullWatch(ctx, watch, fmt.Sprintf(pullWatchExpiredMessage, watch.URL))
		return
	}
	ref := github.PullRef{Owner: watch.Owner, Repo: watch.Repo, Number: watch.Number}
	pull, err := b.config.GitHub.Pull(ctx, ref)
	if err != nil {
		log.Printf("slackbot: read pull request %s: %v", key, err)
		return
	}
	switch {
	case pull.Merged:
		b.endPullWatch(ctx, watch, fmt.Sprintf(pullMergedMessage, watch.URL))
		return
	case pull.State != "open":
		b.endPullWatch(ctx, watch, fmt.Sprintf(pullClosedMessage, watch.URL))
		return
	}
	checks, err := b.config.GitHub.Checks(ctx, ref, pull.HeadSHA)
	if err != nil {
		log.Printf("slackbot: read checks of %s: %v", key, err)
		return
	}
	activity, cursor, err := b.config.GitHub.ActivitySince(ctx, ref, github.Cursor(watch.Cursor), b.config.GitHubLogin)
	if err != nil {
		log.Printf("slackbot: read activity of %s: %v", key, err)
		return
	}

	previousChecks := watch.Checks
	if pull.HeadSHA != watch.HeadSHA {
		// A new commit's checks start over; its earlier failure was handled.
		previousChecks = ""
	}
	var events []string
	if checks.State == github.ChecksFailure && previousChecks != github.ChecksFailure {
		events = append(events, "CI が失敗しました（失敗したチェック: "+strings.Join(checks.Failed, "、")+"）")
	}
	passed := checks.State == github.ChecksSuccess && watch.Checks == github.ChecksFailure
	if len(activity) > 0 {
		events = append(events, fmt.Sprintf("レビュー・コメントが%d件付きました", len(activity)))
	}
	changed := pull.HeadSHA != watch.HeadSHA || checks.State != watch.Checks || github.Cursor(watch.Cursor) != cursor
	watch.HeadSHA = pull.HeadSHA
	watch.Checks = checks.State
	watch.Cursor = state.PullCursor(cursor)

	if passed {
		if err := b.post(ctx, watch.Channel, watch.ThreadTS, fmt.Sprintf(pullChecksPassedMessage, watch.URL)); err != nil {
			log.Printf("slackbot: post checks passed %s: %v", key, err)
		}
	}
	if len(events) == 0 {
		if changed {
			if err := b.store.SetPullWatch(watch); err != nil {
				log.Printf("slackbot: save pull request watch %s: %v", key, err)
			}
		}
		return
	}
	if watch.Wakes >= maxPullWatchWakes {
		b.endPullWatch(ctx, watch, fmt.Sprintf(pullWatchLimitMessage, maxPullWatchWakes, watch.URL))
		return
	}
	watch.Wakes++
	watch.ExpiresAt = b.now().Add(pullWatchTTL)
	// Save before the turn starts, so a crash or the next poll cannot
	// report the same events twice.
	if err := b.store.SetPullWatch(watch); err != nil {
		log.Printf("slackbot: save pull request watch %s: %v", key, err)
		return
	}
	summary := strings.Join(events, "、")
	task := describePullEvent(pull, checks, activity, events)
	wg.Go(func() { b.firePullWatch(turnCtx, watch, summary, task) })
}

// firePullWatch announces a pull request event in its thread and runs a
// turn for it as a request from the user who started the watch.
func (b *Bot) firePullWatch(ctx context.Context, watch state.PullWatch, summary, task string) {
	var timestamp string
	if err := b.retrySlack(ctx, func() error {
		var err error
		timestamp, err = b.api.PostMessage(ctx, watch.Channel, watch.ThreadTS, fmt.Sprintf(pullWatchWakeMessage, watch.URL, summary))
		return err
	}); err != nil {
		log.Printf("slackbot: announce pull request event %s: %v", watch.Key(), err)
		return
	}
	b.processTrigger(ctx, processingTrigger{
		source:      pullWatchTrigger,
		authorID:    watch.AuthorID,
		channel:     watch.Channel,
		timestamp:   timestamp,
		threadTS:    watch.ThreadTS,
		message:     task,
		threadReply: true,
	})
}

// describePullEvent writes what happened on the pull request for the
// session. Review and comment bodies are clipped and listed up to a limit;
// the session can read them in full on GitHub.
func describePullEvent(pull github.Pull, checks github.Checks, activity []github.Activity, events []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "PR: %s（%s）\n", pull.URL, pull.Title)
	for _, event := range events {
		builder.WriteString("- " + event + "\n")
	}
	for i, item := range activity {
		if i >= maxPullActivityItems {
			fmt.Fprintf(&builder, "- ほか%d件\n", len(activity)-i)
			break
		}
		label := map[string]string{
			github.KindReview:        "レビュー",
			github.KindReviewComment: "コードへのコメント",
			github.KindComment:       "コメント",
		}[item.Kind]
		if item.State != "" && item.Kind == github.KindReview {
			label += "（" + item.State + "）"
		}
		if item.Path != "" {
			label += " " + item.Path
		}
		fmt.Fprintf(&builder, "- @%s の%s: %s %s\n", item.Author, label, clipText(item.Body, maxPullActivityRunes), item.URL)
	}
	return strings.TrimSpace(builder.String())
}

// endPullWatch removes a watch and, when message is set, says why in its
// thread.
func (b *Bot) endPullWatch(ctx context.Context, watch state.PullWatch, message string) {
	key := watch.Key()
	if _, err := b.store.DeletePullWatches(func(w state.PullWatch) bool { return w.Key() == key }); err != nil {
		log.Printf("slackbot: delete pull request watch %s: %v", key, err)
		return
	}
	if message == "" {
		return
	}
	if err := b.post(ctx, watch.Channel, watch.ThreadTS, message); err != nil {
		log.Printf("slackbot: post pull request watch end %s: %v", key, err)
	}
}

// stopPullWatches ends every watch the thread started, for the stop command.
func (b *Bot) stopPullWatches(channel, threadTS string) bool {
	removed, err := b.store.DeletePullWatches(func(w state.PullWatch) bool {
		return w.Channel == channel && w.ThreadTS == threadTS
	})
	if err != nil {
		log.Printf("slackbot: stop pull request watches in %s: %v", threadRef(channel, threadTS), err)
	}
	return len(removed) > 0
}

// threadBusy reports whether the thread has a request running or waiting.
func (b *Bot) threadBusy(threadKey string) bool {
	b.runningMu.Lock()
	defer b.runningMu.Unlock()
	return len(b.running[threadKey]) > 0
}
