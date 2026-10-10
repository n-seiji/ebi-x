// Package state persists Codex sessions, Slack event processing states, and
// access approvals.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	sessionsFilename      = "sessions.json"
	eventsFilename        = "events.json"
	subscriptionsFilename = "subscriptions.json"
	approvalsFilename     = "approvals.json"
)

// State is the processing state of a Slack mention event.
type State string

const (
	// Received means the event has been claimed but execution has not started.
	Received State = "received"
	// Planning is a legacy pre-execution state recovered from older versions.
	Planning State = "planning"
	// PlanPosted is a legacy pre-execution state recovered from older versions.
	PlanPosted State = "plan_posted"
	// Working means the work turn may have started making changes.
	Working State = "working"
	// Done means the event completed successfully or was closed fail-safe.
	Done State = "done"
	// Failed means work did not start and the event may be claimed again.
	Failed State = "failed"
	// Interrupted means work may have made changes and must not be retried.
	Interrupted State = "interrupted"
)

type event struct {
	State     State     `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Subscription is an expiring subscription for a Slack thread.
type Subscription struct {
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Store persists thread sessions, event states, subscriptions, and approvals
// in separate JSON files.
type Store struct {
	dir string

	sessionsMu sync.Mutex
	sessions   map[string]string

	eventsMu sync.Mutex
	events   map[string]event

	subscriptionsMu sync.Mutex
	subscriptions   map[string]Subscription

	approvalsMu sync.Mutex
	approvals   map[string]Approval

	followUpsMu sync.Mutex
	followUps   map[string]FollowUp

	pullWatchesMu sync.Mutex
	pullWatches   map[string]PullWatch
}

// NewStore loads or creates a state store rooted at dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory %q: %w", dir, err)
	}

	sessions := make(map[string]string)
	if err := loadJSON(filepath.Join(dir, sessionsFilename), &sessions); err != nil {
		return nil, fmt.Errorf("load sessions: %w", err)
	}
	if sessions == nil {
		sessions = make(map[string]string)
	}
	events := make(map[string]event)
	if err := loadJSON(filepath.Join(dir, eventsFilename), &events); err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	if events == nil {
		events = make(map[string]event)
	}
	for key, entry := range events {
		if !validState(entry.State) {
			return nil, fmt.Errorf("load events: %w", fmt.Errorf("event %q has invalid state %q", key, entry.State))
		}
	}
	subscriptions := make(map[string]Subscription)
	if err := loadJSON(filepath.Join(dir, subscriptionsFilename), &subscriptions); err != nil {
		return nil, fmt.Errorf("load subscriptions: %w", err)
	}
	if subscriptions == nil {
		subscriptions = make(map[string]Subscription)
	}

	followUps := make(map[string]FollowUp)
	if err := loadJSON(filepath.Join(dir, followUpsFilename), &followUps); err != nil {
		return nil, fmt.Errorf("load follow-ups: %w", err)
	}
	if followUps == nil {
		followUps = make(map[string]FollowUp)
	}

	pullWatches := make(map[string]PullWatch)
	if err := loadJSON(filepath.Join(dir, pullWatchesFilename), &pullWatches); err != nil {
		return nil, fmt.Errorf("load pull request watches: %w", err)
	}
	if pullWatches == nil {
		pullWatches = make(map[string]PullWatch)
	}

	approvals, err := loadApprovals(filepath.Join(dir, approvalsFilename))
	if err != nil {
		return nil, fmt.Errorf("load approvals: %w", err)
	}

	return &Store{
		dir:           dir,
		sessions:      sessions,
		events:        events,
		subscriptions: subscriptions,
		approvals:     approvals,
		followUps:     followUps,
		pullWatches:   pullWatches,
	}, nil
}

// GetThread returns the Codex thread ID associated with threadKey.
func (s *Store) GetThread(threadKey string) (string, bool) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	threadID, ok := s.sessions[threadKey]
	return threadID, ok
}

// SetThread associates threadKey with a Codex thread ID.
func (s *Store) SetThread(threadKey string, threadID string) error {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	previous, existed := s.sessions[threadKey]
	s.sessions[threadKey] = threadID
	if err := s.saveSessions(); err != nil {
		if existed {
			s.sessions[threadKey] = previous
		} else {
			delete(s.sessions, threadKey)
		}
		return fmt.Errorf("save thread %q: %w", threadKey, err)
	}
	return nil
}

// GetSubscription returns the expiring subscription associated with threadKey.
func (s *Store) GetSubscription(threadKey string) (Subscription, bool) {
	s.subscriptionsMu.Lock()
	defer s.subscriptionsMu.Unlock()

	subscription, ok := s.subscriptions[threadKey]
	return subscription, ok
}

// SetSubscription associates threadKey with an expiring subscription.
func (s *Store) SetSubscription(threadKey string, startedAt, expiresAt time.Time) error {
	s.subscriptionsMu.Lock()
	defer s.subscriptionsMu.Unlock()

	previous, existed := s.subscriptions[threadKey]
	s.subscriptions[threadKey] = Subscription{StartedAt: startedAt, ExpiresAt: expiresAt}
	if err := s.saveSubscriptions(); err != nil {
		if existed {
			s.subscriptions[threadKey] = previous
		} else {
			delete(s.subscriptions, threadKey)
		}
		return fmt.Errorf("save subscription %q: %w", threadKey, err)
	}
	return nil
}

// DeleteSubscription removes the subscription associated with threadKey.
func (s *Store) DeleteSubscription(threadKey string) error {
	s.subscriptionsMu.Lock()
	defer s.subscriptionsMu.Unlock()

	previous, existed := s.subscriptions[threadKey]
	if !existed {
		return nil
	}
	delete(s.subscriptions, threadKey)
	if err := s.saveSubscriptions(); err != nil {
		s.subscriptions[threadKey] = previous
		return fmt.Errorf("delete subscription %q: %w", threadKey, err)
	}
	return nil
}

// DeleteSubscriptionIfExpired atomically removes a subscription only when its
// current expiry is at or before now. A concurrent renewal that acquires the
// subscription lock first is preserved.
func (s *Store) DeleteSubscriptionIfExpired(threadKey string, now time.Time) (bool, error) {
	s.subscriptionsMu.Lock()
	defer s.subscriptionsMu.Unlock()

	subscription, existed := s.subscriptions[threadKey]
	if !existed || subscription.ExpiresAt.After(now) {
		return false, nil
	}
	delete(s.subscriptions, threadKey)
	if err := s.saveSubscriptions(); err != nil {
		s.subscriptions[threadKey] = subscription
		return false, fmt.Errorf("delete expired subscription %q: %w", threadKey, err)
	}
	return true, nil
}

// ClaimEvent atomically claims an unregistered or failed event.
func (s *Store) ClaimEvent(eventKey string) (bool, error) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()

	previous, existed := s.events[eventKey]
	if existed && previous.State != Failed {
		return false, nil
	}
	s.events[eventKey] = event{State: Received, UpdatedAt: time.Now().UTC()}
	if err := s.saveEvents(); err != nil {
		if existed {
			s.events[eventKey] = previous
		} else {
			delete(s.events, eventKey)
		}
		return false, fmt.Errorf("save claimed event %q: %w", eventKey, err)
	}
	return true, nil
}

// Transition atomically changes an event from its expected state to an allowed state.
func (s *Store) Transition(eventKey string, from, to State) error {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()

	if !allowedTransition(from, to) {
		return fmt.Errorf("transition event %q from %q to %q: %w", eventKey, from, to, errors.New("transition is not allowed"))
	}
	current, ok := s.events[eventKey]
	if !ok {
		return fmt.Errorf("transition event %q from %q to %q: %w", eventKey, from, to, errors.New("event does not exist"))
	}
	if current.State != from {
		return fmt.Errorf("transition event %q: %w", eventKey, fmt.Errorf("current state %q, expected %q", current.State, from))
	}

	s.events[eventKey] = event{State: to, UpdatedAt: time.Now().UTC()}
	if err := s.saveEvents(); err != nil {
		s.events[eventKey] = current
		return fmt.Errorf("save transition for event %q: %w", eventKey, err)
	}
	return nil
}

// RecoverStartup marks abandoned pre-work events failed and working events interrupted.
func (s *Store) RecoverStartup() error {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()

	previous := cloneEvents(s.events)
	changed := false
	now := time.Now().UTC()
	for key, entry := range s.events {
		switch entry.State {
		case Received, Planning, PlanPosted:
			entry.State = Failed
		case Working:
			entry.State = Interrupted
		default:
			continue
		}
		entry.UpdatedAt = now
		s.events[key] = entry
		changed = true
	}
	if !changed {
		return nil
	}
	if err := s.saveEvents(); err != nil {
		s.events = previous
		return fmt.Errorf("save startup recovery: %w", err)
	}
	return nil
}

// GC removes terminal events older than the supplied retention duration and expired subscriptions.
func (s *Store) GC(now time.Time, olderThan time.Duration) error {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	s.subscriptionsMu.Lock()
	defer s.subscriptionsMu.Unlock()

	previousEvents := cloneEvents(s.events)
	previousSubscriptions := cloneSubscriptions(s.subscriptions)
	cutoff := now.Add(-olderThan)
	eventsChanged := false
	for key, entry := range s.events {
		if terminal(entry.State) && entry.UpdatedAt.Before(cutoff) {
			delete(s.events, key)
			eventsChanged = true
		}
	}
	subscriptionsChanged := false
	for key, subscription := range s.subscriptions {
		if !subscription.ExpiresAt.After(now) {
			delete(s.subscriptions, key)
			subscriptionsChanged = true
		}
	}
	if !eventsChanged && !subscriptionsChanged {
		return nil
	}
	if eventsChanged {
		if err := s.saveEvents(); err != nil {
			s.events = previousEvents
			s.subscriptions = previousSubscriptions
			return fmt.Errorf("save event garbage collection: %w", err)
		}
	}
	if subscriptionsChanged {
		if err := s.saveSubscriptions(); err != nil {
			s.subscriptions = previousSubscriptions
			return fmt.Errorf("save subscription garbage collection: %w", err)
		}
	}
	return nil
}

func (s *Store) saveSessions() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, sessionsFilename), s.sessions); err != nil {
		return fmt.Errorf("write sessions: %w", err)
	}
	return nil
}

func (s *Store) saveEvents() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, eventsFilename), s.events); err != nil {
		return fmt.Errorf("write events: %w", err)
	}
	return nil
}

func (s *Store) saveSubscriptions() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, subscriptionsFilename), s.subscriptions); err != nil {
		return fmt.Errorf("write subscriptions: %w", err)
	}
	return nil
}

func loadJSON(path string, destination any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("decode %q: %w", path, err)
	}
	return nil
}

func atomicWriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %q: %w", path, err)
	}
	data = append(data, '\n')

	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %q: %w", path, err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod temporary file %q: %w", tempPath, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temporary file %q: %w", tempPath, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temporary file %q: %w", tempPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary file %q: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temporary file %q to %q: %w", tempPath, path, err)
	}
	return nil
}

func allowedTransition(from, to State) bool {
	switch from {
	case Received:
		return to == Planning || to == Working || to == Failed
	case Planning:
		return to == PlanPosted || to == Done || to == Failed
	case PlanPosted:
		return to == Working || to == Done || to == Failed
	case Working:
		return to == Done || to == Interrupted
	default:
		return false
	}
}

func validState(state State) bool {
	switch state {
	case Received, Planning, PlanPosted, Working, Done, Failed, Interrupted:
		return true
	default:
		return false
	}
}

func terminal(state State) bool {
	return state == Done || state == Failed || state == Interrupted
}

func cloneEvents(source map[string]event) map[string]event {
	cloned := make(map[string]event, len(source))
	maps.Copy(cloned, source)
	return cloned
}

func cloneSubscriptions(source map[string]Subscription) map[string]Subscription {
	cloned := make(map[string]Subscription, len(source))
	maps.Copy(cloned, source)
	return cloned
}
