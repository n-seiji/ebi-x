package slackbot

import (
	"context"
	"log"
	"strings"

	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// appHomeMessagesTab is the App Home tab where Slack shows an agent app's
// conversations with one user.
const appHomeMessagesTab = "messages"

// suggestedPrompt is a request offered at the top of the agent's Messages tab.
// Choosing one sends message as the user's own message.
type suggestedPrompt struct {
	title   string
	message string
}

var agentSuggestedPrompts = []suggestedPrompt{
	{title: "できること", message: "ebi-x にできることと、依頼の仕方を教えて"},
	{title: "リポジトリの説明", message: "作業できるリポジトリの構成と主な機能を説明して"},
	{title: "最近の変更", message: "作業できるリポジトリの最近の変更をまとめて"},
}

// isDirectMessage reports whether channel is a one-to-one conversation with
// the bot. Slack gives every such conversation an ID starting with D.
func isDirectMessage(channel string) bool {
	return strings.HasPrefix(channel, "D")
}

// conversationAllowed reports whether the bot works in channel. A direct
// message involves only its user, so the user's own permission covers it.
func (b *Bot) conversationAllowed(channel string) bool {
	return isDirectMessage(channel) || b.channelAllowed(channel)
}

// handleDirectMessage processes a message sent to the bot in its Messages tab
// or a direct message. No mention is needed there: a top-level message starts
// a thread and replies continue it.
func (b *Bot) handleDirectMessage(ctx context.Context, event *slackevents.MessageEvent) {
	if event.User == "" ||
		event.User == b.config.BotUserID ||
		event.BotID != "" ||
		event.SubType != "" ||
		event.IsEdited() ||
		event.DeletedTimeStamp != "" ||
		!isDirectMessage(event.Channel) {
		return
	}
	message := stripBotMention(event.Text, b.config.BotUserID)
	if message == "" {
		return
	}
	threadTS := event.ThreadTimeStamp
	if threadTS == "" {
		threadTS = event.TimeStamp
	}
	if !b.userAllowed(event.User) {
		log.Printf("slackbot: rejecting direct message by %q in %q", event.User, event.Channel)
		b.rejectUnapproved(ctx, &slackevents.AppMentionEvent{
			User:            event.User,
			Channel:         event.Channel,
			TimeStamp:       event.TimeStamp,
			ThreadTimeStamp: event.ThreadTimeStamp,
		}, []approvalSubject{{kind: state.ApprovalUser, id: event.User}})
		return
	}
	if isStopCommand(message) {
		b.stopThread(ctx, event.Channel, threadTS, event.TimeStamp)
		return
	}
	b.processTrigger(ctx, processingTrigger{
		source:      messageTrigger,
		authorID:    event.User,
		channel:     event.Channel,
		timestamp:   event.TimeStamp,
		threadTS:    threadTS,
		message:     message,
		threadReply: event.ThreadTimeStamp != "",
	})
}

// HandleAppHomeOpened offers suggested prompts when an allowed user opens the
// agent's Messages tab.
func (b *Bot) HandleAppHomeOpened(ctx context.Context, event *slackevents.AppHomeOpenedEvent) {
	if event == nil || event.Tab != appHomeMessagesTab || event.Channel == "" || !b.userAllowed(event.User) {
		return
	}
	if err := b.api.SetSuggestedPrompts(ctx, event.Channel, agentSuggestedPrompts); err != nil {
		log.Printf("slackbot: set suggested prompts for %q: %v", event.Channel, err)
	}
}

// SetSuggestedPrompts replaces the prompts at the top of the agent's Messages
// tab. It needs the assistant:write scope and the Agent App setting.
func (w *webAPI) SetSuggestedPrompts(ctx context.Context, channel string, prompts []suggestedPrompt) error {
	params := slack.AssistantThreadsSetSuggestedPromptsParameters{ChannelID: channel}
	for _, prompt := range prompts {
		params.AddPrompt(prompt.title, prompt.message)
	}
	return w.client.SetAssistantThreadsSuggestedPromptsContext(ctx, params)
}
