package slackbot

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

const (
	approveCommand = "許可"
	denyCommand    = "拒否"

	approvalRequestedMessage = "🔒 %sには ebi-x を使う権限がまだないため、承認者に許可を依頼しました%s。\n許可されたら、このスレッドでお知らせします。そのあと、もう一度 mention してください。"
	approvalQueuedMessage    = "🔒 %sには ebi-x を使う権限がまだないため、承認者に許可を依頼しました%s。\nそのままお待ちください！許可されたら、この依頼から自動で始めます。"

	// approvalResumeWindow is how long a mention waits for approval before
	// it is no longer started on its own.
	approvalResumeWindow    = 24 * time.Hour
	notApproverMessage      = "承認者（SLACK_APPROVER_USER_IDS）だけが操作できます。"
	approvalUsageMessage    = "許可や拒否は、依頼のスレッドで「" + approveCommand + "」か「" + denyCommand + "」とだけ mention してください。"
	approvalSaveFailMessage = "記録の保存に失敗したため、操作していません。もう一度お試しください。"
)

var (
	userIDPattern     = regexp.MustCompile(`^[UW][A-Z0-9]+$`)
	channelIDPattern  = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
	workflowIDPattern = regexp.MustCompile(`^Wf[A-Z0-9]+$`)
)

// Approvals records access decisions made in the approval channel.
type Approvals interface {
	ApprovalStatus(kind state.ApprovalKind, id string) state.ApprovalStatus
	RequestApproval(kind state.ApprovalKind, id, requestedBy, requestChannel, requestThreadTS string, now time.Time) (bool, error)
	SetApprovalRequestMessage(kind state.ApprovalKind, id, timestamp string) error
	SetApprovalMention(kind state.ApprovalKind, id string, mention state.ApprovalMention) (bool, error)
	FindApprovalByRequestMessage(timestamp string) (state.Approval, bool)
	GetApproval(kind state.ApprovalKind, id string) (state.Approval, bool)
	DecideApproval(kind state.ApprovalKind, id string, approve bool, decidedBy string, now time.Time) (state.Approval, bool, error)
	DeleteApproval(kind state.ApprovalKind, id string) (bool, error)
}

// approvalSubject is one user, channel, or workflow that needs approval.
type approvalSubject struct {
	kind state.ApprovalKind
	id   string
}

// valid reports whether the subject has the ID shape of its kind. DMs and
// malformed IDs are never offered for approval.
func (s approvalSubject) valid() bool {
	switch s.kind {
	case state.ApprovalUser:
		return userIDPattern.MatchString(s.id)
	case state.ApprovalChannel:
		return channelIDPattern.MatchString(s.id)
	case state.ApprovalWorkflow:
		return workflowIDPattern.MatchString(s.id)
	}
	return false
}

// label renders the subject for Slack mrkdwn and markdown alike.
func (s approvalSubject) label() string {
	switch s.kind {
	case state.ApprovalUser:
		return "ユーザー <@" + s.id + ">"
	case state.ApprovalChannel:
		return "チャンネル <#" + s.id + ">"
	default:
		return "Workflow `" + s.id + "`"
	}
}

// subject names the subject from the mention author's point of view.
func (s approvalSubject) subject() string {
	switch s.kind {
	case state.ApprovalUser:
		return "あなた"
	case state.ApprovalChannel:
		return "このチャンネル"
	default:
		return "この Workflow"
	}
}

// missingSubjects joins the subjects that lack access, such as
// "あなたとこのチャンネル".
func missingSubjects(missing []approvalSubject) string {
	names := make([]string, 0, len(missing))
	for _, subject := range missing {
		// A DM is only between the author and ebi-x, so only the author's
		// access matters there.
		if subject.kind == state.ApprovalChannel && strings.HasPrefix(subject.id, "D") {
			continue
		}
		names = append(names, subject.subject())
	}
	if len(names) == 0 {
		return "あなた"
	}
	return strings.Join(names, "と")
}

func (s approvalSubject) value() string {
	return string(s.kind) + ":" + s.id
}

func validWorkflowID(id string) bool {
	return workflowIDPattern.MatchString(id)
}

func (b *Bot) approvalsEnabled() bool {
	return b.config.ApprovalChannelID != "" && b.config.Approvals != nil
}

func (b *Bot) approved(kind state.ApprovalKind, id string) bool {
	return b.approvalsEnabled() && b.config.Approvals.ApprovalStatus(kind, id) == state.Approved
}

// userAllowed reports whether a human may use the bot: listed in .env or
// approved in the approval channel.
func (b *Bot) userAllowed(id string) bool {
	_, ok := b.allowedUsers[id]
	return ok || b.approved(state.ApprovalUser, id)
}

// channelAllowed reports whether the bot works in channel. The approval
// channel is always allowed: its members request changes to playbooks and
// global memory there.
func (b *Bot) channelAllowed(id string) bool {
	_, ok := b.allowedChannels[id]
	return ok || b.approved(state.ApprovalChannel, id) ||
		(b.approvalsEnabled() && id == b.config.ApprovalChannelID)
}

// sharedWritable reports whether work turns in channel may change state every
// channel reads: playbooks and global memory. Only the approval channel may,
// so such changes happen where the people who approve use of the bot see them.
func (b *Bot) sharedWritable(channel string) bool {
	return b.config.ApprovalChannelID != "" && channel == b.config.ApprovalChannelID
}

// workflowAllowed reports whether a bot-authored mention comes from an
// explicitly allowed Slack workflow.
func (b *Bot) workflowAllowed(id string) bool {
	if !b.config.AllowWorkflows {
		return false
	}
	_, ok := b.allowedWorkflows[id]
	return ok || b.approved(state.ApprovalWorkflow, id)
}

// rejectUnapproved answers a mention whose author, workflow, or channel is
// not allowed. With approvals enabled it asks the approval channel about each
// subject that has no decision yet; a denied subject is refused outright.
func (b *Bot) rejectUnapproved(ctx context.Context, event *slackevents.AppMentionEvent, missing []approvalSubject) {
	if !b.approvalsEnabled() {
		b.forbidden(ctx, event, missing)
		return
	}
	for _, subject := range missing {
		if !subject.valid() || b.config.Approvals.ApprovalStatus(subject.kind, subject.id) == state.Denied {
			b.forbidden(ctx, event, missing)
			return
		}
	}
	mention, resumable := waitingMention(event, b.config.BotUserID)
	requested, queued := false, resumable
	for _, subject := range missing {
		created, err := b.config.Approvals.RequestApproval(subject.kind, subject.id, event.User, event.Channel, mentionThreadTS(event), b.now())
		if err != nil {
			log.Printf("slackbot: record approval request %s: %v", subject.value(), err)
			continue
		}
		if !created {
			// Already waiting for a decision; do not ask twice.
			requested = true
			queued = queued && b.recordWaitingMention(subject, mention)
			continue
		}
		if err := b.postApprovalRequest(ctx, subject, event); err != nil {
			log.Printf("slackbot: post approval request %s to %q (is the bot a member of SLACK_APPROVAL_CHANNEL_ID?): %v", subject.value(), b.config.ApprovalChannelID, err)
			// Forget the request so the next mention asks again.
			if _, deleteErr := b.config.Approvals.DeleteApproval(subject.kind, subject.id); deleteErr != nil {
				log.Printf("slackbot: roll back approval request %s: %v", subject.value(), deleteErr)
			}
			continue
		}
		requested = true
		queued = queued && b.recordWaitingMention(subject, mention)
	}
	if !requested {
		b.forbidden(ctx, event, missing)
		return
	}
	format := approvalRequestedMessage
	if queued {
		format = approvalQueuedMessage
	}
	reply := fmt.Sprintf(format, missingSubjects(missing), b.approvalRequestLinks(ctx, missing))

	if err := b.post(ctx, event.Channel, mentionThreadTS(event), reply); err != nil {
		log.Printf("slackbot: post approval requested response: %v", err)
	}
}

// approvalRequestLinks links the approval requests for missing so the author
// can see that someone was asked. It is empty when no link can be made.
func (b *Bot) approvalRequestLinks(ctx context.Context, missing []approvalSubject) string {
	type request struct{ subject, link string }
	var requests []request
	for _, subject := range missing {
		approval, ok := b.config.Approvals.GetApproval(subject.kind, subject.id)
		if !ok || approval.RequestMessageTS == "" {
			continue
		}
		link, err := b.api.Permalink(ctx, b.config.ApprovalChannelID, approval.RequestMessageTS)
		if err != nil || link == "" {
			log.Printf("slackbot: permalink for approval request %s: %v", subject.value(), err)
			continue
		}
		requests = append(requests, request{subject.subject(), link})
	}
	if len(requests) == 0 {
		return ""
	}
	if len(requests) == 1 {
		return "（<" + requests[0].link + "|承認依頼>）"
	}
	links := make([]string, 0, len(requests))
	for _, r := range requests {
		links = append(links, "<"+r.link+"|"+r.subject+"の承認依頼>")
	}
	return "（" + strings.Join(links, "・") + "）"
}

// waitingMention returns the mention to start once it is approved. Workflow
// mentions, empty mentions and stop commands are not kept.
func waitingMention(event *slackevents.AppMentionEvent, botUserID string) (state.ApprovalMention, bool) {
	text := stripBotMention(event.Text, botUserID)
	if event.BotID != "" || text == "" || isStopCommand(text) {
		return state.ApprovalMention{}, false
	}
	return state.ApprovalMention{
		User:     event.User,
		Channel:  event.Channel,
		ThreadTS: mentionThreadTS(event),
		TS:       event.TimeStamp,
		Text:     text,
	}, true
}

func (b *Bot) recordWaitingMention(subject approvalSubject, mention state.ApprovalMention) bool {
	saved, err := b.config.Approvals.SetApprovalMention(subject.kind, subject.id, mention)
	if err != nil {
		log.Printf("slackbot: record waiting mention for %s: %v", subject.value(), err)
	}
	return saved
}

// postApprovalRequest asks the approval channel about subject. Approvers
// answer in the request's thread.
func (b *Bot) postApprovalRequest(ctx context.Context, subject approvalSubject, event *slackevents.AppMentionEvent) error {
	where := "<#" + event.Channel + "> で mention しました。"
	if isDirectMessage(event.Channel) {
		where = "ebi-x に DM で依頼しました。"
	}
	text := "🔐 ebi-x の利用許可の依頼\n対象: " + subject.label() +
		"\n<@" + event.User + "> が " + where +
		"\n\nこのスレッドで ebi-x に mention して「" + approveCommand + "」か「" + denyCommand + "」と返信してください。"
	timestamp, err := b.api.PostMessage(ctx, b.config.ApprovalChannelID, "", text)
	if err != nil {
		return err
	}
	// Without the timestamp approvers can still answer with the ID.
	if err := b.config.Approvals.SetApprovalRequestMessage(subject.kind, subject.id, timestamp); err != nil {
		log.Printf("slackbot: record approval request message %s: %v", subject.value(), err)
	}
	return nil
}

// isApprovalCommand reports whether a mention in the approval channel is
// about approvals rather than work: any mention in an approval request's
// thread, the approval words anywhere, and the usage question.
func (b *Bot) isApprovalCommand(event *slackevents.AppMentionEvent) bool {
	command := stripBotMention(event.Text, b.config.BotUserID)
	if command == approveCommand || command == denyCommand || isUsageQuestion(command) {
		return true
	}
	if event.ThreadTimeStamp == "" {
		return false
	}
	_, inRequest := b.config.Approvals.FindApprovalByRequestMessage(event.ThreadTimeStamp)
	return inRequest
}

// handleApprovalCommand answers the approval channel's approval mentions.
// Approvers decide a request by mentioning 許可 or 拒否 in its thread.
func (b *Bot) handleApprovalCommand(ctx context.Context, event *slackevents.AppMentionEvent) {
	command := stripBotMention(event.Text, b.config.BotUserID)
	request, inRequest := b.config.Approvals.FindApprovalByRequestMessage(event.ThreadTimeStamp)
	var reply string
	var resume *processingTrigger
	switch {
	case isUsageQuestion(command):
		reply = b.usageReply(ctx)
	case !b.isApprover(event.User):
		reply = notApproverMessage
	case !inRequest || (command != approveCommand && command != denyCommand):
		reply = approvalUsageMessage
	default:
		reply, resume = b.decideApproval(ctx, request, command == approveCommand, event.User)
	}
	if err := b.post(ctx, event.Channel, mentionThreadTS(event), reply); err != nil {
		log.Printf("slackbot: post approval command reply: %v", err)
	}
	if resume != nil {
		b.processTrigger(ctx, *resume)
	}
}

func (b *Bot) isApprover(user string) bool {
	_, ok := b.approvers[user]
	return ok
}

// decideApproval approves or denies request. The first decision wins. When
// the approval lets the mention that waited for it run, it returns that
// mention to start after the reply is posted.
func (b *Bot) decideApproval(ctx context.Context, request state.Approval, approve bool, user string) (string, *processingTrigger) {
	subject := approvalSubject{kind: request.Kind, id: request.ID}
	approval, decided, err := b.config.Approvals.DecideApproval(subject.kind, subject.id, approve, user, b.now())
	if err != nil {
		log.Printf("slackbot: decide approval %s: %v", subject.value(), err)
		return approvalSaveFailMessage, nil
	}
	if !decided {
		return subject.label() + " は、すでに" + decisionText(approval) + "。", nil
	}
	log.Printf("slackbot: %s %s by %q", approval.Status, subject.value(), user)
	var resume *processingTrigger
	if approval.Status == state.Approved && approval.RequestChannel != "" && approval.RequestThreadTS != "" {
		notice := "✅ " + subject.label() + " が許可されました。"
		switch waiting := b.waitingTrigger(approval); {
		case waiting == nil:
			notice += "もう一度 mention してください。"
		case !b.userAllowed(waiting.authorID) || !b.channelAllowed(waiting.channel):
			notice += "残りの許可を待っています。"
		default:
			notice += "依頼を始めます。"
			resume = waiting
		}
		if err := b.post(ctx, approval.RequestChannel, approval.RequestThreadTS, notice); err != nil {
			log.Printf("slackbot: notify approval %s: %v", subject.value(), err)
		}
	}
	return statusIcon(approval.Status) + " " + subject.label() + ": " + decisionText(approval) + "。", resume
}

// waitingTrigger returns the mention that waited for approval, or nil when
// there is none or it waited longer than approvalResumeWindow.
func (b *Bot) waitingTrigger(approval state.Approval) *processingTrigger {
	if approval.MentionText == "" || approval.RequestedBy == "" {
		return nil
	}
	mentionedAt, err := strconv.ParseFloat(approval.MentionTS, 64)
	if err != nil || b.now().Sub(time.Unix(int64(mentionedAt), 0)) > approvalResumeWindow {
		return nil
	}
	return &processingTrigger{
		source:      mentionTrigger,
		authorID:    approval.RequestedBy,
		channel:     approval.RequestChannel,
		timestamp:   approval.MentionTS,
		threadTS:    approval.RequestThreadTS,
		message:     approval.MentionText,
		threadReply: approval.RequestThreadTS != approval.MentionTS,
	}
}

func statusIcon(status state.ApprovalStatus) string {
	if status == state.Approved {
		return "✅"
	}
	return "🚫"
}

func decisionText(approval state.Approval) string {
	if approval.Status == state.Approved {
		return "<@" + approval.DecidedBy + "> が許可しました"
	}
	return "<@" + approval.DecidedBy + "> が拒否しました"
}

func mentionThreadTS(event *slackevents.AppMentionEvent) string {
	if event.ThreadTimeStamp != "" {
		return event.ThreadTimeStamp
	}
	return event.TimeStamp
}
