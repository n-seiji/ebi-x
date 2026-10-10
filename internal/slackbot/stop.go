package slackbot

import (
	"context"
	"log"
	"strings"
	"time"
)

// stopCommands are the whole messages that stop a thread's work, compared
// after trimming and lowercasing. A longer message is an ordinary request,
// so "停止の仕方を教えて" still reaches the agent.
var stopCommands = map[string]struct{}{
	"停止": {}, "停止して": {}, "止めて": {}, "とめて": {}, "中止": {}, "中止して": {},
	"キャンセル": {}, "キャンセルして": {}, "ストップ": {}, "stop": {}, "cancel": {},
}

func isStopCommand(message string) bool {
	_, ok := stopCommands[normalizeCommand(message)]
	return ok
}

// threadRef names a Slack thread in the bot's per-thread records.
func threadRef(channel, threadTS string) string {
	return channel + ":" + threadTS
}

// runningRequest is a request a thread is processing or waiting to process.
type runningRequest struct {
	cancel    context.CancelCauseFunc
	authorID  string
	startedAt time.Time
}

// trackRequest makes a request stoppable from its thread until the returned
// function is called.
func (b *Bot) trackRequest(ctx context.Context, threadKey, eventKey, authorID string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	b.runningMu.Lock()
	requests := b.running[threadKey]
	if requests == nil {
		requests = make(map[string]runningRequest)
		b.running[threadKey] = requests
	}
	requests[eventKey] = runningRequest{cancel: cancel, authorID: authorID, startedAt: b.now()}
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

// stopThread cancels the requests the thread is running or waiting to run,
// its scheduled follow-up, pull request watches, and schedule.
// Each stopped request reports itself in the thread, so the stop command
// only gets a reaction.
func (b *Bot) stopThread(ctx context.Context, channel, threadTS, timestamp string) {
	key := threadRef(channel, threadTS)
	b.runningMu.Lock()
	requests := b.running[key]
	for _, request := range requests {
		request.cancel(errStoppedByUser)
	}
	stopped := len(requests)
	b.runningMu.Unlock()
	_, cancelledFollowUp, err := b.store.DeleteFollowUp(key)
	if err != nil {
		log.Printf("slackbot: cancel follow-up %s: %v", key, err)
	}
	stoppedWatches := b.stopPullWatches(channel, threadTS)
	stoppedSchedule := b.stopSchedule(channel, threadTS)
	if stopped > 0 {
		log.Printf("slackbot: stopping %d request(s) in %s", stopped, key)
		b.addReaction(ctx, channel, timestamp, "ok_hand")
		return
	}
	var stoppedParts []string
	if cancelledFollowUp {
		stoppedParts = append(stoppedParts, followUpStoppedMessage)
	}
	if stoppedWatches {
		stoppedParts = append(stoppedParts, pullWatchStoppedMessage)
	}
	if stoppedSchedule {
		stoppedParts = append(stoppedParts, scheduleStoppedMsg)
	}
	message := nothingToStopMessage
	if len(stoppedParts) > 0 {
		message = strings.Join(stoppedParts, "\n")
	}
	if err := b.post(ctx, channel, threadTS, message); err != nil {
		log.Printf("slackbot: post stop result: %v", err)
	}
}
