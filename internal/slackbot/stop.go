package slackbot

import (
	"context"
	"log"
	"strings"
)

// stopCommands are the whole messages that stop a thread's work, compared
// after trimming and lowercasing. A longer message is an ordinary request,
// so "停止の仕方を教えて" still reaches the agent.
var stopCommands = map[string]struct{}{
	"停止": {}, "停止して": {}, "止めて": {}, "とめて": {}, "中止": {}, "中止して": {},
	"キャンセル": {}, "キャンセルして": {}, "ストップ": {}, "stop": {}, "cancel": {},
}

func isStopCommand(message string) bool {
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(message), "。．.!！ "))
	_, ok := stopCommands[normalized]
	return ok
}

// threadRef names a Slack thread in the bot's per-thread records.
func threadRef(channel, threadTS string) string {
	return channel + ":" + threadTS
}

// trackRequest makes a request stoppable from its thread until the returned
// function is called.
func (b *Bot) trackRequest(ctx context.Context, threadKey, eventKey string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	b.runningMu.Lock()
	requests := b.running[threadKey]
	if requests == nil {
		requests = make(map[string]context.CancelCauseFunc)
		b.running[threadKey] = requests
	}
	requests[eventKey] = cancel
	b.runningMu.Unlock()
	return ctx, func() {
		b.runningMu.Lock()
		delete(requests, eventKey)
		// A thread's map stays registered while it is not empty, so an
		// empty one is still this request's own.
		if len(requests) == 0 {
			delete(b.running, threadKey)
		}
		b.runningMu.Unlock()
		cancel(nil)
	}
}

// stopThread cancels the requests the thread is running or waiting to run.
// Each stopped request reports itself in the thread, so the stop command
// only gets a reaction.
func (b *Bot) stopThread(ctx context.Context, channel, threadTS, timestamp string) {
	b.runningMu.Lock()
	requests := b.running[threadRef(channel, threadTS)]
	for _, cancel := range requests {
		cancel(errStoppedByUser)
	}
	stopped := len(requests)
	b.runningMu.Unlock()
	if stopped == 0 {
		if err := b.post(ctx, channel, threadTS, nothingToStopMessage); err != nil {
			log.Printf("slackbot: post nothing to stop: %v", err)
		}
		return
	}
	log.Printf("slackbot: stopping %d request(s) in %s:%s", stopped, channel, threadTS)
	b.addReaction(ctx, channel, timestamp, "ok_hand")
}
