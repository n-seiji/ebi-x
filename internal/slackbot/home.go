package slackbot

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	appHomeHomeTab = "home"
	// maxHomeItems bounds each list on the Home tab, which Slack limits in
	// size.
	maxHomeItems = 15

	homeTitle     = "*ebi-x の作業*\nあなたが依頼した作業の今の状況です。ホームを開き直すと最新になります。"
	homeForbidden = "*ebi-x の作業*\nebi-x を使う許可がないため、作業はありません。"
	homeEmpty     = "ありません。"
)

// homeThread is one thread listed on the Home tab.
type homeThread struct {
	channel  string
	threadTS string
	detail   string
	at       time.Time
}

// publishHome shows the user what ebi-x is doing for them: requests being
// worked on, scheduled follow-ups, and watched pull requests, like dots'
// Activity view and Devin's session list.
func (b *Bot) publishHome(ctx context.Context, userID string) {
	if !b.userAllowed(userID) {
		if err := b.api.PublishHome(ctx, userID, []string{homeForbidden}); err != nil {
			log.Printf("slackbot: publish home for %q: %v", userID, err)
		}
		return
	}
	var running []homeThread
	b.runningMu.Lock()
	for threadKey, requests := range b.running {
		channel, threadTS, ok := strings.Cut(threadKey, ":")
		if !ok {
			continue
		}
		for _, request := range requests {
			if request.authorID == userID {
				running = append(running, homeThread{channel: channel, threadTS: threadTS, at: request.startedAt})
				break
			}
		}
	}
	b.runningMu.Unlock()
	sort.Slice(running, func(i, j int) bool { return running[i].at.Before(running[j].at) })

	var followUps []homeThread
	for _, followUp := range b.store.FollowUps() {
		if followUp.AuthorID == userID {
			followUps = append(followUps, homeThread{channel: followUp.Channel, threadTS: followUp.ThreadTS, detail: followUp.Task, at: followUp.DueAt})
		}
	}
	var watches []homeThread
	for _, watch := range b.store.PullWatches() {
		if watch.AuthorID == userID {
			detail := watch.URL
			if label := checksLabel(watch.Checks); label != "" {
				detail += "（CI: " + label + "）"
			}
			watches = append(watches, homeThread{channel: watch.Channel, threadTS: watch.ThreadTS, detail: detail})
		}
	}

	var schedules []homeThread
	for _, schedule := range b.store.Schedules() {
		if schedule.AuthorID == userID {
			schedules = append(schedules, homeThread{channel: schedule.Channel, threadTS: schedule.ThreadTS, detail: scheduleSummary(schedule)})
		}
	}

	links := make(map[string]string)
	sections := []string{homeTitle}
	sections = append(sections, b.homeSection(ctx, links, "⏳ 作業中", running, func(item homeThread) string {
		return formatFollowUpTime(item.at) + " から"
	}))
	sections = append(sections, b.homeSection(ctx, links, "⏰ 予定中のフォローアップ", followUps, func(item homeThread) string {
		return formatFollowUpTime(item.at) + " ごろ: " + clipText(item.detail, 100)
	}))
	sections = append(sections, b.homeSection(ctx, links, "👀 見守り中の PR", watches, func(item homeThread) string {
		return item.detail
	}))
	sections = append(sections, b.homeSection(ctx, links, "🔁 定期実行", schedules, func(item homeThread) string {
		return item.detail
	}))
	sections = append(sections, "_更新: "+formatFollowUpTime(b.now())+"_")
	if err := b.api.PublishHome(ctx, userID, sections); err != nil {
		log.Printf("slackbot: publish home for %q: %v", userID, err)
	}
}

// homeSection lists items under title, each with a link to its thread.
func (b *Bot) homeSection(ctx context.Context, links map[string]string, title string, items []homeThread, describe func(homeThread) string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "*%s（%d）*", title, len(items))
	if len(items) == 0 {
		return builder.String() + "\n" + homeEmpty
	}
	for i, item := range items {
		if i >= maxHomeItems {
			fmt.Fprintf(&builder, "\nほか%d件", len(items)-i)
			break
		}
		fmt.Fprintf(&builder, "\n• %s — %s", b.threadLink(ctx, links, item.channel, item.threadTS), describe(item))
	}
	return builder.String()
}

// threadLink links to a thread, falling back to its channel when Slack does
// not give a permalink.
func (b *Bot) threadLink(ctx context.Context, links map[string]string, channel, threadTS string) string {
	key := threadRef(channel, threadTS)
	if link, ok := links[key]; ok {
		return link
	}
	place := "<#" + channel + "> のスレッド"
	if isDirectMessage(channel) {
		place = "DM のスレッド"
	}
	link := place
	if permalink, err := b.api.Permalink(ctx, channel, threadTS); err == nil && permalink != "" {
		link = "<" + permalink + "|" + place + ">"
	} else if err != nil {
		log.Printf("slackbot: permalink %s: %v", key, err)
	}
	links[key] = link
	return link
}

func checksLabel(checks string) string {
	return map[string]string{
		"pending": "実行中",
		"success": "成功",
		"failure": "失敗",
	}[checks]
}

// PublishHome shows the sections on the user's Home tab, one section block
// each, separated by dividers.
func (w *webAPI) PublishHome(ctx context.Context, userID string, sections []string) error {
	var blocks []slack.Block
	for i, section := range sections {
		if i > 0 {
			blocks = append(blocks, slack.NewDividerBlock())
		}
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, section, false, false), nil, nil))
	}
	_, err := w.client.PublishViewContext(ctx, slack.PublishViewContextRequest{
		UserID: userID,
		View:   slack.HomeTabViewRequest{Type: slack.VTHomeTab, Blocks: slack.Blocks{BlockSet: blocks}},
	})
	return err
}

func (w *webAPI) Permalink(ctx context.Context, channel, timestamp string) (string, error) {
	return w.client.GetPermalinkContext(ctx, &slack.PermalinkParameters{Channel: channel, Ts: timestamp})
}
