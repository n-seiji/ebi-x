package slackbot

import (
	"context"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack/slackevents"
)

const (
	approveCommand        = "許可"
	denyCommand           = "拒否"
	listApprovalsCommand  = "許可一覧"
	revokeApprovalCommand = "許可取消"

	approvalRequestedMessage = "403 forbidden. まだ許可されていないため、承認を依頼しました。許可されたら、もう一度 mention してください。"
	notApproverMessage       = "承認者（SLACK_APPROVER_USER_IDS）だけが操作できます。"
	approvalSaveFailMessage  = "記録の保存に失敗したため、操作していません。もう一度お試しください。"
	approvalThreadMissing    = "このスレッドの依頼が見つかりません。`" + approveCommand + " <ID>` の形で指定してください。"
)

var (
	userIDPattern     = regexp.MustCompile(`^[UW][A-Z0-9]+$`)
	channelIDPattern  = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
	workflowIDPattern = regexp.MustCompile(`^Wf[A-Z0-9]+$`)
)

// Approvals records access decisions made in the approval channel.
type Approvals interface {
	ApprovalStatus(kind state.ApprovalKind, id string) state.ApprovalStatus
	ListApprovals() []state.Approval
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

// handleApprovalCommand runs the approval commands in the approval channel:
// 許可 and 拒否 (in a request's thread or with an ID), 許可一覧, and
// 許可取消. It reports false for any other text, which is then handled as a
// normal mention. Only SLACK_APPROVER_USER_IDS may run them.
func (b *Bot) handleApprovalCommand(ctx context.Context, event *slackevents.AppMentionEvent) bool {
	fields := strings.Fields(stripBotMention(event.Text, b.config.BotUserID))
	if len(fields) == 0 || len(fields) > 2 {
		return false
	}
	command, argument := fields[0], ""
	if len(fields) == 2 {
		argument = normalizeSlackID(fields[1])
	}
	switch {
	case command == approveCommand || command == denyCommand:
	case command == listApprovalsCommand && argument == "":
	case command == revokeApprovalCommand && argument != "":
	default:
		return false
	}

	var reply string
	switch {
	case !b.isApprover(event.User):
		reply = notApproverMessage
	case command == listApprovalsCommand:
		reply = b.approvalList()
	case command == revokeApprovalCommand:
		reply = b.revokeApproval(argument)
	default:
		reply = b.decideApproval(ctx, event, command == approveCommand, argument)
	}
	if err := b.post(ctx, event.Channel, mentionThreadTS(event), reply); err != nil {
		log.Printf("slackbot: post approval command reply: %v", err)
	}
	return true
}

func (b *Bot) isApprover(user string) bool {
	_, ok := b.approvers[user]
	return ok
}

// decideApproval approves or denies the subject named by id, or the request
// whose thread the command was posted in when id is empty. The first
// decision wins; changing it takes 許可取消 first.
func (b *Bot) decideApproval(ctx context.Context, event *slackevents.AppMentionEvent, approve bool, id string) string {
	var subject approvalSubject
	if id == "" {
		request, ok := b.config.Approvals.FindApprovalByRequestMessage(event.ThreadTimeStamp)
		if !ok {
			return approvalThreadMissing
		}
		subject = approvalSubject{kind: request.Kind, id: request.ID}
	} else {
		kind, ok := approvalKindOf(id)
		if !ok {
			return "`" + id + "` はユーザー・チャンネル・Workflow の ID として読み取れませんでした。"
		}
		subject = approvalSubject{kind: kind, id: id}
	}
	if b.fixedAllowed(subject) {
		return subject.label() + " は .env の固定の許可なので、ここでは変更できません。"
	}
	approval, decided, err := b.config.Approvals.DecideApproval(subject.kind, subject.id, approve, event.User, b.now())
	if err != nil {
		log.Printf("slackbot: decide approval %s: %v", subject.value(), err)
		return approvalSaveFailMessage
	}
	if !decided {
		return subject.label() + " は、すでに" + decisionText(approval) + "。変えるには `" + revokeApprovalCommand + " " + subject.id + "` で記録を消してから、もう一度決めてください。"
	}
	log.Printf("slackbot: %s %s by %q", approval.Status, subject.value(), event.User)
	reply := statusIcon(approval.Status) + " " + subject.label() + ": " + decisionText(approval) + "。"
	// A decision made by ID elsewhere is also noted in the request's thread.
	if approval.RequestMessageTS != "" && approval.RequestMessageTS != mentionThreadTS(event) {
		if err := b.post(ctx, b.config.ApprovalChannelID, approval.RequestMessageTS, reply); err != nil {
			log.Printf("slackbot: post approval decision to request thread %s: %v", subject.value(), err)
		}
	}
	if approval.Status == state.Approved && approval.RequestChannel != "" && approval.RequestThreadTS != "" {
		notice := "✅ " + subject.label() + " が許可されました。もう一度 mention してください。"
		if err := b.post(ctx, approval.RequestChannel, approval.RequestThreadTS, notice); err != nil {
			log.Printf("slackbot: notify approval %s: %v", subject.value(), err)
		}
	}
	return reply
}

// fixedAllowed reports whether subject is allowed by .env, which the
// approval channel cannot change.
func (b *Bot) fixedAllowed(subject approvalSubject) bool {
	fixed := map[state.ApprovalKind][]string{
		state.ApprovalUser:     b.config.AllowedUserIDs,
		state.ApprovalChannel:  b.config.AllowedChannelIDs,
		state.ApprovalWorkflow: b.config.AllowedWorkflowIDs,
	}
	return slices.Contains(fixed[subject.kind], subject.id)
}

func (b *Bot) approvalList() string {
	var lines []string
	lines = append(lines, "固定の許可（.env）:")
	fixed := []struct {
		kind state.ApprovalKind
		ids  []string
	}{
		{state.ApprovalUser, b.config.AllowedUserIDs},
		{state.ApprovalChannel, b.config.AllowedChannelIDs},
		{state.ApprovalWorkflow, b.config.AllowedWorkflowIDs},
	}
	fixedCount := 0
	for _, group := range fixed {
		for _, id := range group.ids {
			lines = append(lines, "- "+approvalSubject{kind: group.kind, id: id}.label())
			fixedCount++
		}
	}
	if fixedCount == 0 {
		lines = append(lines, "- なし")
	}
	lines = append(lines, "", "Slack で決めたもの:")
	approvals := b.config.Approvals.ListApprovals()
	if len(approvals) == 0 {
		lines = append(lines, "- なし")
	}
	for _, approval := range approvals {
		subject := approvalSubject{kind: approval.Kind, id: approval.ID}
		lines = append(lines, "- "+statusIcon(approval.Status)+" "+subject.label()+" — "+decisionText(approval))
	}
	lines = append(lines, "", "決めるには `"+approveCommand+" <ID>` / `"+denyCommand+" <ID>`、記録を消すには `"+revokeApprovalCommand+" <ID>` と mention してください。")
	return strings.Join(lines, "\n")
}

func (b *Bot) revokeApproval(id string) string {
	kind, ok := approvalKindOf(id)
	if !ok {
		return "`" + id + "` はユーザー・チャンネル・Workflow の ID として読み取れませんでした。"
	}
	subject := approvalSubject{kind: kind, id: id}
	if b.fixedAllowed(subject) {
		return subject.label() + " は .env の固定の許可なので、ここでは取り消せません。"
	}
	deleted, err := b.config.Approvals.DeleteApproval(kind, id)
	if err != nil {
		log.Printf("slackbot: revoke approval %s: %v", subject.value(), err)
		return approvalSaveFailMessage
	}
	if !deleted {
		return subject.label() + " の記録はありません。"
	}
	log.Printf("slackbot: revoked approval %s", subject.value())
	return subject.label() + " の記録を削除しました。次に mention されたときに、もう一度確認します。"
}

// approvalKindOf infers the kind from the ID's prefix. Workflow IDs start
// with "Wf", which no user or channel ID does.
func approvalKindOf(id string) (state.ApprovalKind, bool) {
	for _, kind := range []state.ApprovalKind{state.ApprovalWorkflow, state.ApprovalUser, state.ApprovalChannel} {
		if (approvalSubject{kind: kind, id: id}).valid() {
			return kind, true
		}
	}
	return "", false
}

// normalizeSlackID accepts IDs pasted as Slack mentions such as <@U123> or
// <#C123|general>.
func normalizeSlackID(text string) string {
	text = strings.TrimPrefix(text, "<")
	text = strings.TrimSuffix(text, ">")
	text = strings.TrimLeft(text, "@#")
	text, _, _ = strings.Cut(text, "|")
	return text
}

func statusIcon(status state.ApprovalStatus) string {
	switch status {
	case state.Approved:
		return "✅"
	case state.Denied:
		return "🚫"
	default:
		return "⏳"
	}
}

func decisionText(approval state.Approval) string {
	switch approval.Status {
	case state.Approved:
		return "<@" + approval.DecidedBy + "> が許可しました"
	case state.Denied:
		return "<@" + approval.DecidedBy + "> が拒否しました"
	default:
		return "承認待ちです"
	}
}

func mentionThreadTS(event *slackevents.AppMentionEvent) string {
	if event.ThreadTimeStamp != "" {
		return event.ThreadTimeStamp
	}
	return event.TimeStamp
}
