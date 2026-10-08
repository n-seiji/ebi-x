package slackbot

import (
	"context"
	"log"
	"regexp"
	"time"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

const (
	approveCommand = "許可"
	denyCommand    = "拒否"

	approvalRequestedMessage = "403 forbidden. まだ許可されていないため、承認を依頼しました。許可されたら、もう一度 mention してください。"
	notApproverMessage       = "承認者（SLACK_APPROVER_USER_IDS）だけが操作できます。"
	approvalUsageMessage     = "承認チャンネルでは、依頼のスレッドで「" + approveCommand + "」か「" + denyCommand + "」と mention することだけができます。"
	approvalSaveFailMessage  = "記録の保存に失敗したため、操作していません。もう一度お試しください。"
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
	FindApprovalByRequestMessage(timestamp string) (state.Approval, bool)
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

// channelAllowed reports whether the bot works in channel.
func (b *Bot) channelAllowed(id string) bool {
	_, ok := b.allowedChannels[id]
	return ok || b.approved(state.ApprovalChannel, id)
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
		b.forbidden(ctx, event)
		return
	}
	for _, subject := range missing {
		if !subject.valid() || b.config.Approvals.ApprovalStatus(subject.kind, subject.id) == state.Denied {
			b.forbidden(ctx, event)
			return
		}
	}
	requested := false
	for _, subject := range missing {
		created, err := b.config.Approvals.RequestApproval(subject.kind, subject.id, event.User, event.Channel, mentionThreadTS(event), b.now())
		if err != nil {
			log.Printf("slackbot: record approval request %s: %v", subject.value(), err)
			continue
		}
		if !created {
			// Already waiting for a decision; do not ask twice.
			requested = true
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
	}
	if !requested {
		b.forbidden(ctx, event)
		return
	}
	if err := b.post(ctx, event.Channel, mentionThreadTS(event), approvalRequestedMessage); err != nil {
		log.Printf("slackbot: post approval requested response: %v", err)
	}
}

// postApprovalRequest asks the approval channel about subject. Approvers
// answer in the request's thread.
func (b *Bot) postApprovalRequest(ctx context.Context, subject approvalSubject, event *slackevents.AppMentionEvent) error {
	text := "🔐 ebi-x の利用許可の依頼\n対象: " + subject.label() +
		"\n<@" + event.User + "> が <#" + event.Channel + "> で mention しました。" +
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

// handleApprovalCommand answers every human mention in the approval channel.
// Approvers decide a request by mentioning 許可 or 拒否 in its thread;
// nothing else is done there.
func (b *Bot) handleApprovalCommand(ctx context.Context, event *slackevents.AppMentionEvent) {
	command := stripBotMention(event.Text, b.config.BotUserID)
	request, inRequest := b.config.Approvals.FindApprovalByRequestMessage(event.ThreadTimeStamp)
	var reply string
	switch {
	case !b.isApprover(event.User):
		reply = notApproverMessage
	case !inRequest || (command != approveCommand && command != denyCommand):
		reply = approvalUsageMessage
	default:
		reply = b.decideApproval(ctx, request, command == approveCommand, event.User)
	}
	if err := b.post(ctx, event.Channel, mentionThreadTS(event), reply); err != nil {
		log.Printf("slackbot: post approval command reply: %v", err)
	}
}

func (b *Bot) isApprover(user string) bool {
	_, ok := b.approvers[user]
	return ok
}

// decideApproval approves or denies request. The first decision wins.
func (b *Bot) decideApproval(ctx context.Context, request state.Approval, approve bool, user string) string {
	subject := approvalSubject{kind: request.Kind, id: request.ID}
	approval, decided, err := b.config.Approvals.DecideApproval(subject.kind, subject.id, approve, user, b.now())
	if err != nil {
		log.Printf("slackbot: decide approval %s: %v", subject.value(), err)
		return approvalSaveFailMessage
	}
	if !decided {
		return subject.label() + " は、すでに" + decisionText(approval) + "。"
	}
	log.Printf("slackbot: %s %s by %q", approval.Status, subject.value(), user)
	if approval.Status == state.Approved && approval.RequestChannel != "" && approval.RequestThreadTS != "" {
		notice := "✅ " + subject.label() + " が許可されました。もう一度 mention してください。"
		if err := b.post(ctx, approval.RequestChannel, approval.RequestThreadTS, notice); err != nil {
			log.Printf("slackbot: notify approval %s: %v", subject.value(), err)
		}
	}
	return statusIcon(approval.Status) + " " + subject.label() + ": " + decisionText(approval) + "。"
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
