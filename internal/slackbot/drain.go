package slackbot

import (
	"context"
	"errors"
	"log"
	"sync"

	"github.com/n-seiji/ebi-x/internal/state"
)

// deferredMessage tells the requester that a request which arrived during a
// restart will start once the restarted bot is up.
const deferredMessage = "🔄 ebi-x を更新しています。いま動いている作業が終わり次第再起動し、この依頼は再起動後に自動で始めます。"

// deferredStoppedMessage reports that a stop cancelled requests that were
// waiting for the restart.
const deferredStoppedMessage = "再起動後に始める予定だった依頼を取り消しました。"

// errDraining reports that a request was not started because the bot is
// finishing its running work before a restart.
var errDraining = errors.New("ebi-x is draining for a restart")

var triggerSourceNames = map[triggerSource]string{
	mentionTrigger:   "mention",
	messageTrigger:   "message",
	followUpTrigger:  "follow_up",
	pullWatchTrigger: "pull_watch",
	scheduleTrigger:  "schedule",
}

// drainer stops new requests from starting while the running ones finish, so
// the process can be replaced without cutting off anyone's work.
type drainer struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	active sync.WaitGroup
}

func newDrainer() *drainer {
	ctx, cancel := context.WithCancel(context.Background())
	return &drainer{ctx: ctx, cancel: cancel}
}

// Drain makes the bot defer every request that has not started work yet and
// stops follow-ups, pull request watches, and schedules from firing. The
// returned channel closes when the requests that had already started are
// done. Deferred requests are saved and run by the next process.
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

// beginRequest registers a request that is about to start, unless the bot is
// draining. finish must be called once the request is done.
func (b *Bot) beginRequest() (finish func(), ok bool) {
	b.drain.mu.Lock()
	defer b.drain.mu.Unlock()
	if b.draining() {
		return nil, false
	}
	b.drain.active.Add(1)
	return b.drain.active.Done, true
}

// untilDrain returns a context that is also cancelled, with errDraining as
// its cause, once the bot starts draining.
func (b *Bot) untilDrain(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(b.drain.ctx, func() { cancel(errDraining) })
	return ctx, func() {
		stop()
		cancel(nil)
	}
}

// deferRequest saves a request that arrived while draining and tells the
// thread it will start after the restart. The request's event stays claimed
// as received; the next process's startup recovery releases it, so it can
// be claimed again when the deferred request runs.
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

// runDeferred runs the requests the previous process deferred. Requests in
// one thread run in the order they arrived.
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

// stopDeferred cancels the thread's requests that wait for a restart and
// reports whether there were any.
func (b *Bot) stopDeferred(channel, threadTS string) bool {
	removed, err := b.store.DeleteDeferred(func(deferred state.Deferred) bool {
		return deferred.Channel == channel && deferred.ThreadTS == threadTS
	})
	if err != nil {
		log.Printf("slackbot: cancel deferred requests in %s: %v", threadRef(channel, threadTS), err)
	}
	return removed > 0
}
