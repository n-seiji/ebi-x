package slackbot

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/n-seiji/ebi-x/internal/workspace"
)

const (
	channelScopeTTL   = 30 * 24 * time.Hour
	closedChannelNote = "[ebi-x からの注意: このチャンネルは%sです。このチャンネルの内容をほかのチャンネルへ持ち出さないよう、Slack の検索・読み取りツールは使えません。Slack の情報が必要なら、推測せず依頼者に貼ってもらってください]"
)

type ChannelInfo struct {
	Private  bool
	External bool
}

type channelScope struct {
	area         workspace.Area
	slackLimited bool
	kind         string
	rule         channelRule
	hidden       []string
}

type cachedScope struct {
	scope   channelScope
	expires time.Time
}

func (b *Bot) channelScope(ctx context.Context, channel string) channelScope {
	// A DM keeps Slack search: only its own user reads the answers.
	if isDirectMessage(channel) {
		return channelScope{area: workspace.Closed(channel)}
	}
	if b.sharedWritable(channel) {
		return channelScope{}
	}
	now := b.now()
	b.scopeMu.Lock()
	cached, ok := b.scopes[channel]
	b.scopeMu.Unlock()
	if ok && now.Before(cached.expires) {
		return cached.scope
	}
	// Not cached on failure, so the channel opens again once Slack answers.
	info, err := b.api.ChannelInfo(ctx, channel)
	if err != nil {
		log.Printf("slackbot: look up channel %q: %v; treating it as closed", channel, err)
		// Closed rather than open: a wrong open guess would expose the channel.
		return channelScope{area: workspace.Closed(channel), slackLimited: true, kind: "種類を確認できなかったチャンネル"}
	}
	scope := channelScope{}
	switch {
	case info.External:
		scope = channelScope{area: workspace.Closed(channel), slackLimited: true, kind: "外部の組織と共有しているチャンネル"}
	case info.Private:
		scope = channelScope{area: workspace.Closed(channel), slackLimited: true, kind: "プライベートチャンネル"}
	}
	b.scopeMu.Lock()
	if b.scopes == nil {
		b.scopes = make(map[string]cachedScope)
	}
	b.scopes[channel] = cachedScope{scope: scope, expires: now.Add(channelScopeTTL)}
	b.scopeMu.Unlock()
	return scope
}

func (scope channelScope) note() string {
	if !scope.slackLimited {
		return ""
	}
	return "\n\n" + fmt.Sprintf(closedChannelNote, scope.kind)
}
