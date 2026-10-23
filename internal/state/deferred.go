package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const deferredFilename = "deferred.json"

// Deferred is a request that arrived while the bot was finishing its running
// work before a restart. The restarted process takes it and runs it as if it
// had just arrived.
type Deferred struct {
	// Source names how the request arrived, such as "mention" or "follow_up".
	Source      string    `json:"source"`
	AuthorID    string    `json:"author_id"`
	Channel     string    `json:"channel"`
	Timestamp   string    `json:"timestamp"`
	ThreadTS    string    `json:"thread_ts"`
	Message     string    `json:"message"`
	ThreadReply bool      `json:"thread_reply,omitempty"`
	FollowUp    *FollowUp `json:"follow_up,omitempty"`
	DeferredAt  time.Time `json:"deferred_at"`
}

// Key identifies the Slack event the request came from.
func (d Deferred) Key() string {
	return d.Channel + ":" + d.Timestamp
}

// AddDeferred saves deferred until the restarted process takes it. A request
// already deferred for the same event is kept as it is.
func (s *Store) AddDeferred(deferred Deferred) error {
	s.deferredMu.Lock()
	defer s.deferredMu.Unlock()
	key := deferred.Key()
	if _, exists := s.deferred[key]; exists {
		return nil
	}
	s.deferred[key] = deferred
	if err := s.saveDeferred(); err != nil {
		delete(s.deferred, key)
		return fmt.Errorf("save deferred request %q: %w", key, err)
	}
	return nil
}

// TakeDeferred removes and returns every deferred request, oldest first.
// They are removed before they run, so a crash cannot run one twice.
func (s *Store) TakeDeferred() ([]Deferred, error) {
	s.deferredMu.Lock()
	defer s.deferredMu.Unlock()
	if len(s.deferred) == 0 {
		return nil, nil
	}
	previous := s.deferred
	taken := make([]Deferred, 0, len(previous))
	for _, deferred := range previous {
		taken = append(taken, deferred)
	}
	s.deferred = make(map[string]Deferred)
	if err := s.saveDeferred(); err != nil {
		s.deferred = previous
		return nil, fmt.Errorf("take deferred requests: %w", err)
	}
	sort.Slice(taken, func(i, j int) bool { return taken[i].DeferredAt.Before(taken[j].DeferredAt) })
	return taken, nil
}

// DeleteDeferred removes the deferred requests match selects and returns
// how many there were.
func (s *Store) DeleteDeferred(match func(Deferred) bool) (int, error) {
	s.deferredMu.Lock()
	defer s.deferredMu.Unlock()
	removed := make(map[string]Deferred)
	for key, deferred := range s.deferred {
		if match(deferred) {
			removed[key] = deferred
			delete(s.deferred, key)
		}
	}
	if len(removed) == 0 {
		return 0, nil
	}
	if err := s.saveDeferred(); err != nil {
		for key, deferred := range removed {
			s.deferred[key] = deferred
		}
		return 0, fmt.Errorf("delete deferred requests: %w", err)
	}
	return len(removed), nil
}

func (s *Store) saveDeferred() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, deferredFilename), s.deferred); err != nil {
		return fmt.Errorf("write deferred requests: %w", err)
	}
	return nil
}
