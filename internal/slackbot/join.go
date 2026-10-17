package slackbot

import (
	"context"
	"log"
	"regexp"
	"strings"

	"github.com/n-seiji/ebi-x/internal/state"
)

const joinCommand = "参加"

// joinCommandPattern matches "参加 #channel", which Slack sends as a channel
// mention.
var joinCommandPattern = regexp.MustCompile(`^` + joinCommand + `\s*<#([CG][A-Z0-9]+)(?:\|[^>]*)?>$`)

// joinRequest returns the channel of an approver's "参加 #channel" command.
func joinRequest(command string) (string, bool) {
	match := joinCommandPattern.FindStringSubmatch(strings.TrimSpace(command))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// joinChannel joins a public channel on an approver's request and approves
// it, so mentions there, including those from Workflows, reach the bot.
// Slack delivers no event from a channel the bot is not in, so without this
// such a mention cannot even ask for approval. Private channels still need
// an invitation from one of their members.
func (b *Bot) joinChannel(ctx context.Context, channel, user string) (string, *processingTrigger) {
	subject := approvalSubject{kind: state.ApprovalChannel, id: channel}
	status := b.config.Approvals.ApprovalStatus(subject.kind, subject.id)
	if status == state.Denied {
		return "🚫 " + subject.label() + " は拒否済みのため参加しません。", nil
	}
	if err := b.api.JoinChannel(ctx, channel); err != nil {
		log.Printf("slackbot: join %q: %v", channel, err)
		if privateChannelError(err) {
			return subject.label() + " はプライベートチャンネルのため、自分では参加できません。チャンネルのメンバーが ebi-x を招待してください。", nil
		}
		return subject.label() + " に参加できませんでした（" + err.Error() + "）。", nil
	}
	b.announceJoin(ctx, channel, user)
	if b.channelAllowed(channel) {
		return "✅ " + subject.label() + " に参加しました（すでに許可されています）。", nil
	}
	if status == state.Pending {
		if request, ok := b.config.Approvals.GetApproval(subject.kind, subject.id); ok {
			reply, resume := b.decideApproval(ctx, request, true, user)
			return "✅ " + subject.label() + " に参加しました。\n" + reply, resume
		}
	}
	if _, err := b.config.Approvals.RequestApproval(subject.kind, subject.id, user, "", "", b.now()); err != nil {
		log.Printf("slackbot: record join approval %s: %v", subject.value(), err)
		return "✅ " + subject.label() + " に参加しましたが、" + approvalSaveFailMessage, nil
	}
	approval, _, err := b.config.Approvals.DecideApproval(subject.kind, subject.id, true, user, b.now())
	if err != nil {
		log.Printf("slackbot: approve joined channel %s: %v", subject.value(), err)
		return "✅ " + subject.label() + " に参加しましたが、" + approvalSaveFailMessage, nil
	}
	return "✅ " + subject.label() + " に参加し、" + decisionText(approval) + "。", nil
}

// announceJoin tells the joined channel how the bot came and how to use it.
func (b *Bot) announceJoin(ctx context.Context, channel, user string) {
	text := "👋 ebi-x が参加しました（<@" + user + "> が許可）。このチャンネルで ebi-x に mention すると依頼できます。"
	if err := b.post(ctx, channel, "", text); err != nil {
		log.Printf("slackbot: announce join in %q: %v", channel, err)
	}
}

func privateChannelError(err error) bool {
	return strings.Contains(err.Error(), "method_not_supported_for_channel_type")
}

func (w *webAPI) JoinChannel(ctx context.Context, channel string) error {
	_, _, _, err := w.client.JoinConversationContext(ctx, channel)
	return err
}
