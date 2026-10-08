package slackbot

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

const (
	approveActionID = "ebix_approval_approve"
	denyActionID    = "ebix_approval_deny"
	approvalBlockID = "ebix_approval"

	listApprovalsCommand  = "許可一覧"
	revokeApprovalCommand = "許可取消"

	approvalRequestedMessage = "403 forbidden. まだ許可されていないため、承認を依頼しました。許可されたら、もう一度 mention してください。"
	notApproverMessage       = "承認チャンネルのメンバーだけが操作できます。"
	approverCheckFailMessage = "メンバーかどうかを確認できなかったため、操作していません。もう一度お試しください。"
	approvalSaveFailMessage  = "記録の保存に失敗したため、操作していません。もう一度お試しください。"
	approvalCancelledMessage = "この依頼は取り消されています。"
)

var (
	userIDPattern     = regexp.MustCompile(`^[UW][A-Z0-9]+$`)
	channelIDPattern  = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
	workflowIDPattern = regexp.MustCompile(`^Wf[A-Z0-9]+$`)
)

// Approvals records access decisions made in the approval channel.
type Approvals interface {
	ApprovalStatus(kind state.ApprovalKind, id string) state.ApprovalStatus
	GetApproval(kind state.ApprovalKind, id string) (state.Approval, bool)
	ListApprovals() []state.Approval
	RequestApproval(kind state.ApprovalKind, id, requestedBy, requestChannel string, now time.Time) (bool, error)
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

func parseApprovalValue(value string) (approvalSubject, bool) {
	kind, id, ok := strings.Cut(value, ":")
	subject := approvalSubject{kind: state.ApprovalKind(kind), id: id}
	return subject, ok && subject.valid()
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
		created, err := b.config.Approvals.RequestApproval(subject.kind, subject.id, event.User, event.Channel, b.now())
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

func (b *Bot) postApprovalRequest(ctx context.Context, subject approvalSubject, event *slackevents.AppMentionEvent) error {
	approval := state.Approval{
		Kind:           subject.kind,
		ID:             subject.id,
		Status:         state.Pending,
		RequestedBy:    event.User,
		RequestChannel: event.Channel,
	}
	_, err := b.api.PostBlocks(ctx, b.config.ApprovalChannelID, approvalFallback(subject), approvalBlocks(approval))
	return err
}

// HandleApprovalAction applies an approve or deny button click from the
// approval channel. Only current members of that channel may decide, and the
// first decision wins.
func (b *Bot) HandleApprovalAction(ctx context.Context, callback slack.InteractionCallback) {
	if !b.approvalsEnabled() || callback.Channel.ID != b.config.ApprovalChannelID {
		return
	}
	for _, action := range callback.ActionCallback.BlockActions {
		if action == nil || (action.ActionID != approveActionID && action.ActionID != denyActionID) {
			continue
		}
		subject, ok := parseApprovalValue(action.Value)
		if !ok {
			log.Printf("slackbot: ignoring approval action with value %q", action.Value)
			continue
		}
		b.decideApproval(ctx, callback, subject, action.ActionID == approveActionID)
	}
}

func (b *Bot) decideApproval(ctx context.Context, callback slack.InteractionCallback, subject approvalSubject, approve bool) {
	channel := callback.Channel.ID
	user := callback.User.ID
	if !b.isApprover(ctx, user) {
		b.ephemeral(ctx, channel, user, notApproverMessage)
		return
	}
	approval, decided, err := b.config.Approvals.DecideApproval(subject.kind, subject.id, approve, user, b.now())
	if err != nil {
		log.Printf("slackbot: decide approval %s: %v", subject.value(), err)
		b.ephemeral(ctx, channel, user, approvalSaveFailMessage)
		return
	}
	timestamp := callback.Container.MessageTs
	if timestamp == "" {
		timestamp = callback.Message.Timestamp
	}
	if !decided {
		current, ok := b.config.Approvals.GetApproval(subject.kind, subject.id)
		if !ok {
			b.updateApprovalMessage(ctx, channel, timestamp, subject, cancelledApprovalBlocks(subject))
			b.ephemeral(ctx, channel, user, approvalCancelledMessage)
			return
		}
		b.updateApprovalMessage(ctx, channel, timestamp, subject, approvalBlocks(current))
		b.ephemeral(ctx, channel, user, "すでに"+decisionText(current)+"。")
		return
	}
	log.Printf("slackbot: %s %s by %q", approval.Status, subject.value(), user)
	b.updateApprovalMessage(ctx, channel, timestamp, subject, approvalBlocks(approval))
}

// isApprover reports whether user is a current member of the approval
// channel. Lookup failures deny.
func (b *Bot) isApprover(ctx context.Context, user string) bool {
	member, err := b.api.IsChannelMember(ctx, b.config.ApprovalChannelID, user)
	if err != nil {
		log.Printf("slackbot: check approval channel membership of %q: %v", user, err)
		return false
	}
	return member
}

func (b *Bot) updateApprovalMessage(ctx context.Context, channel, timestamp string, subject approvalSubject, blocks []slack.Block) {
	if timestamp == "" {
		return
	}
	if err := b.api.UpdateBlocks(ctx, channel, timestamp, approvalFallback(subject), blocks); err != nil {
		log.Printf("slackbot: update approval message %s: %v", subject.value(), err)
	}
}

func (b *Bot) ephemeral(ctx context.Context, channel, user, text string) {
	if err := b.api.PostEphemeral(ctx, channel, user, text); err != nil {
		log.Printf("slackbot: post ephemeral to %q: %v", user, err)
	}
}

// handleApprovalCommand runs 許可一覧 and 許可取消 in the approval channel. It
// reports false for any other text, which is then handled as a normal
// mention.
func (b *Bot) handleApprovalCommand(ctx context.Context, event *slackevents.AppMentionEvent) bool {
	fields := strings.Fields(stripBotMention(event.Text, b.config.BotUserID))
	if len(fields) == 0 {
		return false
	}
	var reply string
	switch {
	case fields[0] == listApprovalsCommand && len(fields) == 1:
		if !b.isApprover(ctx, event.User) {
			reply = notApproverMessage
			break
		}
		reply = b.approvalList()
	case fields[0] == revokeApprovalCommand && len(fields) == 2:
		if !b.isApprover(ctx, event.User) {
			reply = notApproverMessage
			break
		}
		reply = b.revokeApproval(normalizeSlackID(fields[1]))
	default:
		return false
	}
	if err := b.post(ctx, event.Channel, mentionThreadTS(event), reply); err != nil {
		log.Printf("slackbot: post approval command reply: %v", err)
	}
	return true
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
	lines = append(lines, "", "記録を消すには `"+revokeApprovalCommand+" <ID>` と mention してください。")
	return strings.Join(lines, "\n")
}

func (b *Bot) revokeApproval(id string) string {
	kind, ok := approvalKindOf(id)
	if !ok {
		return "`" + id + "` はユーザー・チャンネル・Workflow の ID として読み取れませんでした。"
	}
	subject := approvalSubject{kind: kind, id: id}
	fixed := map[state.ApprovalKind][]string{
		state.ApprovalUser:     b.config.AllowedUserIDs,
		state.ApprovalChannel:  b.config.AllowedChannelIDs,
		state.ApprovalWorkflow: b.config.AllowedWorkflowIDs,
	}
	if slices.Contains(fixed[kind], id) {
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

func approvalFallback(subject approvalSubject) string {
	return "ebi-x の利用許可の依頼: " + subject.label()
}

// approvalBlocks renders a request, with buttons while it is pending and with
// the decision afterwards.
func approvalBlocks(approval state.Approval) []slack.Block {
	subject := approvalSubject{kind: approval.Kind, id: approval.ID}
	body := "*ebi-x の利用許可の依頼*\n対象: " + subject.label()
	if approval.RequestedBy != "" && approval.RequestChannel != "" {
		body += "\n<@" + approval.RequestedBy + "> が <#" + approval.RequestChannel + "> で mention しました。"
	}
	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, body, false, false), nil, nil),
	}
	if approval.Status == state.Pending {
		approve := slack.NewButtonBlockElement(approveActionID, subject.value(),
			slack.NewTextBlockObject(slack.PlainTextType, "許可", false, false)).WithStyle(slack.StylePrimary)
		deny := slack.NewButtonBlockElement(denyActionID, subject.value(),
			slack.NewTextBlockObject(slack.PlainTextType, "拒否", false, false)).WithStyle(slack.StyleDanger)
		return append(blocks, slack.NewActionBlock(approvalBlockID, approve, deny))
	}
	return append(blocks, slack.NewContextBlock("",
		slack.NewTextBlockObject(slack.MarkdownType, statusIcon(approval.Status)+" "+decisionText(approval)+"（"+slackDate(approval.DecidedAt)+"）", false, false)))
}

func cancelledApprovalBlocks(subject approvalSubject) []slack.Block {
	body := "*ebi-x の利用許可の依頼*\n対象: " + subject.label()
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, body, false, false), nil, nil),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, approvalCancelledMessage, false, false)),
	}
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

// slackDate renders t in each viewer's own time zone.
func slackDate(t time.Time) string {
	return fmt.Sprintf("<!date^%d^{date_num} {time}|%s>", t.Unix(), t.UTC().Format("2006-01-02 15:04 UTC"))
}

func mentionThreadTS(event *slackevents.AppMentionEvent) string {
	if event.ThreadTimeStamp != "" {
		return event.ThreadTimeStamp
	}
	return event.TimeStamp
}

// PostBlocks posts blocks as a new top-level message.
func (w *webAPI) PostBlocks(ctx context.Context, channel, fallback string, blocks []slack.Block) (string, error) {
	_, timestamp, err := w.client.PostMessageContext(ctx, channel,
		slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(fallback, false))
	return timestamp, err
}

// UpdateBlocks replaces a message's blocks.
func (w *webAPI) UpdateBlocks(ctx context.Context, channel, timestamp, fallback string, blocks []slack.Block) error {
	_, _, _, err := w.client.UpdateMessageContext(ctx, channel, timestamp,
		slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(fallback, false))
	return err
}

// PostEphemeral shows text only to user.
func (w *webAPI) PostEphemeral(ctx context.Context, channel, user, text string) error {
	_, err := w.client.PostEphemeralContext(ctx, channel, user, slack.MsgOptionText(text, false))
	return err
}

// IsChannelMember pages through conversations.members. A private channel
// needs the groups:read scope, a public one channels:read.
func (w *webAPI) IsChannelMember(ctx context.Context, channel, user string) (bool, error) {
	cursor := ""
	for {
		members, nextCursor, err := w.client.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
			ChannelID: channel,
			Cursor:    cursor,
			Limit:     1000,
		})
		if err != nil {
			return false, err
		}
		if slices.Contains(members, user) {
			return true, nil
		}
		if nextCursor == "" || nextCursor == cursor {
			return false, nil
		}
		cursor = nextCursor
	}
}
