package slackbot

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/n-seiji/ebi-x/internal/codex"
)

const (
	// maxStatusDetailRunes keeps one detail short enough for the one-line
	// status Slack shows under the thread.
	maxStatusDetailRunes = 30
)

// progress turns the activity of a running turn into the thread status, so
// the requester can see what the agent is doing instead of a fixed
// "working" line for many minutes. Each update carries the whole state, so a
// lost update only delays the status.
//
// The runner reports activity from its output loop, one event at a time, so
// progress needs no locking.
type progress struct {
	action string
	plan   codex.Activity
}

// observe records activity and returns the status to show.
func (p *progress) observe(activity codex.Activity) string {
	switch activity.Kind {
	case codex.ActivityPlan:
		p.plan = activity
	case codex.ActivityCommand:
		if label := commandLabel(activity.Command); label != "" {
			p.action = "がコマンドを実行しています（" + label + "）…"
		} else {
			p.action = "がコマンドを実行しています…"
		}
	case codex.ActivityFileChange:
		label := filepath.Base(activity.Paths[0])
		if len(activity.Paths) > 1 {
			label += fmt.Sprintf(" ほか%d件", len(activity.Paths)-1)
		}
		p.action = "がファイルを編集しています（" + shortDetail(label) + "）…"
	case codex.ActivityWebSearch:
		p.action = "がWebを検索しています（" + shortDetail(activity.Query) + "）…"
	case codex.ActivityToolCall:
		p.action = "がツールを使っています（" + shortDetail(activity.Tool) + "）…"
	}
	return p.status()
}

func (p *progress) status() string {
	if p.plan.Total == 0 {
		if p.action == "" {
			return workingStatus
		}
		return p.action
	}
	count := fmt.Sprintf("%d/%d 完了", p.plan.Done, p.plan.Total)
	if p.action != "" {
		return p.action + " " + count
	}
	if p.plan.Current == "" {
		return "が作業をまとめています… " + count
	}
	return "が「" + shortDetail(p.plan.Current) + "」を進めています… " + count
}

// shortDetail makes model-written text fit on the status line.
func shortDetail(text string) string {
	return clipText(text, maxStatusDetailRunes)
}

// clipText puts text on one line and cuts it to limit runes, marking a cut
// with "…".
func clipText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

var (
	shellWrapper = regexp.MustCompile(`^(?:\S*/)?(?:ba|z|da)?sh\s+-l?c\s+`)
	commandSplit = regexp.MustCompile(`&&|\|\||[;|]`)
	commandWord  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)
)

// commandLabel names a command by its program and subcommand, such as
// "go test" or "git commit". Arguments are left out because a command line
// can carry tokens or other values that should not appear in the thread.
func commandLabel(command string) string {
	command = strings.TrimSpace(command)
	if wrapper := shellWrapper.FindString(command); wrapper != "" {
		command = strings.Trim(strings.TrimSpace(command[len(wrapper):]), `'"`)
	}
	var fields []string
	for _, segment := range commandSplit.Split(command, -1) {
		fields = strings.Fields(segment)
		if len(fields) > 0 && fields[0] != "cd" {
			break
		}
	}
	if len(fields) == 0 {
		return ""
	}
	fields[0] = filepath.Base(fields[0])
	var words []string
	for _, field := range fields {
		if len(words) == 2 || !commandWord.MatchString(field) {
			break
		}
		words = append(words, field)
	}
	return strings.Join(words, " ")
}

// statusKeeper shows a thread status and keeps it visible: Slack clears an
// assistant status after a while, so the current text is set again every
// statusRefreshDelay, and immediately when it changes.
type statusKeeper struct {
	mu    sync.Mutex
	text  string
	dirty bool
	wake  context.CancelFunc
	stop  func()
}

// Update changes the status. Rapid changes are coalesced, so only the
// latest text is sent at most once per statusMinInterval.
func (k *statusKeeper) Update(text string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if text == k.text {
		return
	}
	k.text = text
	k.dirty = true
	if k.wake != nil {
		k.wake()
	}
}

// Stop ends the refresh. It does not clear the status.
func (k *statusKeeper) Stop() { k.stop() }

func (b *Bot) startStatus(ctx context.Context, channel, threadTS, status string) *statusKeeper {
	statusCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	keeper := &statusKeeper{text: status}
	b.setStatus(ctx, channel, threadTS, status)
	go func() {
		defer close(done)
		sent := status
		for {
			keeper.mu.Lock()
			waitCtx, wake := context.WithCancel(statusCtx)
			keeper.wake = wake
			if keeper.dirty {
				wake()
			}
			keeper.mu.Unlock()
			err := b.sleep(waitCtx, statusRefreshDelay)
			wake()
			if statusCtx.Err() != nil {
				return
			}
			keeper.mu.Lock()
			text := keeper.text
			keeper.dirty = false
			keeper.mu.Unlock()
			// The refresh resends the status; a change that was undone before
			// it was sent needs nothing.
			woken := err != nil
			if woken && text == sent {
				continue
			}
			b.setStatus(statusCtx, channel, threadTS, text)
			sent = text
			// A wake-up by Update waits before the next change is sent, so a
			// burst of commands does not run into Slack's rate limits.
			if woken {
				if b.sleep(statusCtx, statusMinInterval) != nil {
					return
				}
			}
		}
	}()

	var stopOnce sync.Once
	keeper.stop = func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}
	return keeper
}
