package slackbot

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/slack-go/slack"
)

const (
	// requestShortcutID is the message shortcut "ebi-x に頼む".
	requestShortcutID = "ebix_request"
	// requestModalID is the modal the shortcut opens.
	requestModalID    = "ebix_request_modal"
	requestBlockID    = "request"
	requestActionID   = "request_text"
	maxQuotedRunes    = 500
	shortcutForbidden = "このメッセージについて ebi-x に依頼する許可がありません。"
)

// requestTarget is the message a shortcut request is about, carried through
// the modal's private metadata.
type requestTarget struct {
	Channel   string `json:"channel"`
	MessageTS string `json:"message_ts"`
	ThreadTS  string `json:"thread_ts"`
}

func (t requestTarget) thread() string {
	if t.ThreadTS != "" {
		return t.ThreadTS
	}
	return t.MessageTS
}

// HandleInteraction routes the message shortcut and its modal.
func (b *Bot) HandleInteraction(ctx context.Context, callback *slack.InteractionCallback) {
	switch {
	case callback.Type == slack.InteractionTypeMessageAction && callback.CallbackID == requestShortcutID:
		b.HandleRequestShortcut(ctx, callback)
	case callback.Type == slack.InteractionTypeViewSubmission && callback.View.CallbackID == requestModalID:
		b.HandleRequestSubmission(ctx, callback)
	}
}

// HandleRequestShortcut opens the request modal for a message, like Devin's
// "Create a new session" message shortcut. Only users who may use the bot
// in that conversation get it.
func (b *Bot) HandleRequestShortcut(ctx context.Context, callback *slack.InteractionCallback) {
	target := requestTarget{Channel: callback.Channel.ID, MessageTS: callback.Message.Timestamp, ThreadTS: callback.Message.ThreadTimestamp}
	if target.Channel == "" || target.MessageTS == "" {
		return
	}
	if !b.userAllowed(callback.User.ID) || !b.channelAllowed(target.Channel) ||
		(b.approvalsEnabled() && target.Channel == b.config.ApprovalChannelID) {
		if err := b.api.PostEphemeral(ctx, target.Channel, callback.User.ID, shortcutForbidden); err != nil {
			log.Printf("slackbot: post shortcut refusal: %v", err)
		}
		return
	}
	metadata, err := json.Marshal(target)
	if err != nil {
		log.Printf("slackbot: encode request target: %v", err)
		return
	}
	quoted := clipText(callback.Message.Text, maxQuotedRunes)
	if err := b.api.OpenRequestModal(ctx, callback.TriggerID, string(metadata), quoted); err != nil {
		log.Printf("slackbot: open request modal: %v", err)
	}
}

// HandleRequestSubmission starts the request entered in the modal in the
// message's thread. Permissions are checked again, since the modal may have
// stayed open while they changed.
func (b *Bot) HandleRequestSubmission(ctx context.Context, callback *slack.InteractionCallback) {
	var target requestTarget
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &target); err != nil || target.Channel == "" || target.MessageTS == "" {
		log.Printf("slackbot: read request target: %v", err)
		return
	}
	user := callback.User.ID
	if !b.userAllowed(user) || !b.channelAllowed(target.Channel) ||
		(b.approvalsEnabled() && target.Channel == b.config.ApprovalChannelID) {
		return
	}
	text := strings.TrimSpace(callback.View.State.Values[requestBlockID][requestActionID].Value)
	if text == "" {
		return
	}
	threadTS := target.thread()
	var timestamp string
	if err := b.retrySlack(ctx, func() error {
		var err error
		timestamp, err = b.api.PostMessage(ctx, target.Channel, threadTS, formatRequestPost(user, text))
		return err
	}); err != nil {
		log.Printf("slackbot: post shortcut request: %v", err)
		return
	}
	// The thread, including the message the request is about, reaches the
	// session as context, so the request text is sent as written.
	b.processTrigger(ctx, processingTrigger{
		source:      mentionTrigger,
		authorID:    user,
		channel:     target.Channel,
		timestamp:   timestamp,
		threadTS:    threadTS,
		message:     text,
		threadReply: true,
	})
}

func formatRequestPost(user, text string) string {
	return "📝 <@" + user + "> からの依頼（このスレッドのメッセージについて）:\n> " + strings.ReplaceAll(text, "\n", "\n> ")
}

// OpenRequestModal opens the modal where the user writes a request about
// the quoted message.
func (w *webAPI) OpenRequestModal(ctx context.Context, triggerID, metadata, quoted string) error {
	input := slack.NewPlainTextInputBlockElement(slack.NewTextBlockObject(slack.PlainTextType, "例: このエラーの原因を調べて", false, false), requestActionID)
	input.Multiline = true
	blocks := []slack.Block{
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.PlainTextType, "対象のメッセージ: "+quoted, false, false)),
		slack.NewInputBlock(requestBlockID, slack.NewTextBlockObject(slack.PlainTextType, "依頼内容", false, false), nil, input),
	}
	_, err := w.client.OpenViewContext(ctx, triggerID, slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      requestModalID,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "ebi-x に頼む", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "依頼する", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "やめる", false, false),
		Blocks:          slack.Blocks{BlockSet: blocks},
		PrivateMetadata: metadata,
	})
	return err
}

func (w *webAPI) PostEphemeral(ctx context.Context, channel, user, text string) error {
	_, err := w.client.PostEphemeralContext(ctx, channel, user, slack.MsgOptionText(text, false))
	return err
}
