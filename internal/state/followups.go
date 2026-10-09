package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const followUpsFilename = "followups.json"

// FollowUp is work a turn scheduled for itself: at DueAt the bot resumes the
// thread's session with Task and posts the result, so the agent can keep a
// request moving after the conversation has stopped.
type FollowUp struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	// AuthorID is the user whose request scheduled the follow-up. The
	// follow-up runs with that user's permissions, checked again when due.
	AuthorID string    `json:"author_id"`
	Task     string    `json:"task"`
	DueAt    time.Time `json:"due_at"`
	// Chain counts follow-ups scheduled in a row without a person writing in
	// the thread, so an agent cannot keep itself running indefinitely.
	Chain     int       `json:"chain"`
	CreatedAt time.Time `json:"created_at"`
}

// GetFollowUp returns the follow-up scheduled for threadKey.
func (s *Store) GetFollowUp(threadKey string) (FollowUp, bool) {
	s.followUpsMu.Lock()
	defer s.followUpsMu.Unlock()
	followUp, ok := s.followUps[threadKey]
	return followUp, ok
}

// SetFollowUp schedules followUp for threadKey, replacing any earlier one: a
// thread has at most one pending follow-up.
func (s *Store) SetFollowUp(threadKey string, followUp FollowUp) error {
	s.followUpsMu.Lock()
	defer s.followUpsMu.Unlock()
	previous, existed := s.followUps[threadKey]
	s.followUps[threadKey] = followUp
	if err := s.saveFollowUps(); err != nil {
		if existed {
			s.followUps[threadKey] = previous
		} else {
			delete(s.followUps, threadKey)
		}
		return fmt.Errorf("save follow-up %q: %w", threadKey, err)
	}
	return nil
}

// DeleteFollowUp cancels the follow-up scheduled for threadKey and reports
// whether there was one.
func (s *Store) DeleteFollowUp(threadKey string) (bool, error) {
	s.followUpsMu.Lock()
	defer s.followUpsMu.Unlock()
	previous, existed := s.followUps[threadKey]
	if !existed {
		return false, nil
	}
	delete(s.followUps, threadKey)
	if err := s.saveFollowUps(); err != nil {
		s.followUps[threadKey] = previous
		return false, fmt.Errorf("delete follow-up %q: %w", threadKey, err)
	}
	return true, nil
}

// TakeDueFollowUps removes and returns the follow-ups due at now, earliest
// first. They are removed before they run, so a crash cannot run one twice;
// like any work turn, a follow-up may have side effects.
func (s *Store) TakeDueFollowUps(now time.Time) ([]FollowUp, error) {
	s.followUpsMu.Lock()
	defer s.followUpsMu.Unlock()
	var due []FollowUp
	var keys []string
	for key, followUp := range s.followUps {
		if !followUp.DueAt.After(now) {
			due = append(due, followUp)
			keys = append(keys, key)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	for _, key := range keys {
		delete(s.followUps, key)
	}
	if err := s.saveFollowUps(); err != nil {
		for i, key := range keys {
			s.followUps[key] = due[i]
		}
		return nil, fmt.Errorf("take due follow-ups: %w", err)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].DueAt.Before(due[j].DueAt) })
	return due, nil
}

func (s *Store) saveFollowUps() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, followUpsFilename), s.followUps); err != nil {
		return fmt.Errorf("write follow-ups: %w", err)
	}
	return nil
}
