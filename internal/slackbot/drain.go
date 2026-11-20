package slackbot

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"

	"github.com/n-seiji/ebi-x/internal/state"
)

const deferredMessage = "🔄 ebi-x を更新しています。いま動いている作業が終わり次第再起動し、この依頼は再起動後に自動で始めます。"

const deferredStoppedMessage = "再起動後に始める予定だった依頼を取り消しました。"

var errDraining = errors.New("ebi-x is draining for a restart")

var triggerSourceNames = map[triggerSource]string{
	mentionTrigger:   "mention",
	messageTrigger:   "message",
	followUpTrigger:  "follow_up",
	pullWatchTrigger: "pull_watch",
	scheduleTrigger:  "schedule",
}

type drainer struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	active sync.WaitGroup
	count  atomic.Int64
}

func newDrainer() *drainer {
	ctx, cancel := context.WithCancel(context.Background())
	return &drainer{ctx: ctx, cancel: cancel}
}

func (b *Bot) Drain() <-chan struct{} {
	b.drain.mu.Lock()
	b.drain.cancel()
	b.drain.mu.Unlock()
	done := make(chan struct{})
	go func() {
		b.drain.active.Wait()
		close(done)
	}()
	return done
}

func (b *Bot) draining() bool {
	return b.drain.ctx.Err() != nil
}

func (b *Bot) beginRequest() (finish func(), ok bool) {
	b.drain.mu.Lock()
	defer b.drain.mu.Unlock()
	if b.draining() {
		return nil, false
	}
	b.drain.active.Add(1)
	b.drain.count.Add(1)
	return func() {
		b.drain.count.Add(-1)
		b.drain.active.Done()
	}, true
}

func (b *Bot) ActiveRequests() int64 {
	return b.drain.count.Load()
}

func (b *Bot) untilDrain(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(b.drain.ctx, func() { cancel(errDraining) })
	return ctx, func() {
		stop()
		cancel(nil)
	}
}

// The event stays claimed as received; the next process's startup recovery
// releases it, so it can be claimed again when the deferred request runs.
func (b *Bot) deferRequest(ctx context.Context, trigger processingTrigger) {
	deferred := state.Deferred{
		Source:      triggerSourceNames[trigger.source],
		AuthorID:    trigger.authorID,
		Channel:     trigger.channel,
		Timestamp:   trigger.timestamp,
		ThreadTS:    trigger.threadTS,
		Message:     trigger.message,
		ThreadReply: trigger.threadReply,
		FollowUp:    trigger.followUp,
		DeferredAt:  b.now().UTC(),
	}
	if err := b.store.AddDeferred(deferred); err != nil {
		log.Printf("slackbot: defer request %q: %v", deferred.Key(), err)
		if postErr := b.post(ctx, trigger.channel, trigger.threadTS, workStartFailureMessage); postErr != nil {
			log.Printf("slackbot: post defer failure: %v", postErr)
		}
		return
	}
	log.Printf("slackbot: deferred request %q until restart", deferred.Key())
	if err := b.post(ctx, trigger.channel, trigger.threadTS, deferredMessage); err != nil {
		log.Printf("slackbot: post deferred notice %q: %v", deferred.Key(), err)
	}
}

func (b *Bot) runDeferred(ctx context.Context, wg *sync.WaitGroup) {
	deferred, err := b.store.TakeDeferred()
	if err != nil {
		log.Printf("slackbot: take deferred requests: %v", err)
		return
	}
	byThread := make(map[string][]processingTrigger)
	var order []string
	for _, request := range deferred {
		trigger, ok := deferredTrigger(request)
		if !ok {
			log.Printf("slackbot: drop deferred request %q with unknown source %q", request.Key(), request.Source)
			continue
		}
		key := threadRef(request.Channel, request.ThreadTS)
		if _, seen := byThread[key]; !seen {
			order = append(order, key)
		}
		byThread[key] = append(byThread[key], trigger)
	}
	for _, key := range order {
		triggers := byThread[key]
		log.Printf("slackbot: resuming %d deferred request(s) in %s", len(triggers), key)
		wg.Go(func() {
			for _, trigger := range triggers {
				b.processTrigger(ctx, trigger)
			}
		})
	}
}

func deferredTrigger(deferred state.Deferred) (processingTrigger, bool) {
	for source, name := range triggerSourceNames {
		if name == deferred.Source {
			return processingTrigger{
				source:      source,
				authorID:    deferred.AuthorID,
				channel:     deferred.Channel,
				timestamp:   deferred.Timestamp,
				threadTS:    deferred.ThreadTS,
				message:     deferred.Message,
				threadReply: deferred.ThreadReply,
				followUp:    deferred.FollowUp,
			}, true
		}
	}
	return processingTrigger{}, false
}

func (b *Bot) stopDeferred(channel, threadTS string) bool {
	removed, err := b.store.DeleteDeferred(func(deferred state.Deferred) bool {
		return deferred.Channel == channel && deferred.ThreadTS == threadTS
	})
	if err != nil {
		log.Printf("slackbot: cancel deferred requests in %s: %v", threadRef(channel, threadTS), err)
	}
	return removed > 0
}
