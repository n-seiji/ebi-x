package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const followUpsFilename = "followups.json"

type FollowUp struct {
	Channel  string    `json:"channel"`
	ThreadTS string    `json:"thread_ts"`
	AuthorID string    `json:"author_id"`
	Task     string    `json:"task"`
	DueAt    time.Time `json:"due_at"`
	// Capped so an agent cannot keep itself running indefinitely.
	Chain     int       `json:"chain"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) GetFollowUp(threadKey string) (FollowUp, bool) {
	s.followUpsMu.Lock()
	defer s.followUpsMu.Unlock()
	followUp, ok := s.followUps[threadKey]
	return followUp, ok
}

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

func (s *Store) DeleteFollowUp(threadKey string) (FollowUp, bool, error) {
	s.followUpsMu.Lock()
	defer s.followUpsMu.Unlock()
	previous, existed := s.followUps[threadKey]
	if !existed {
		return FollowUp{}, false, nil
	}
	delete(s.followUps, threadKey)
	if err := s.saveFollowUps(); err != nil {
		s.followUps[threadKey] = previous
		return FollowUp{}, false, fmt.Errorf("delete follow-up %q: %w", threadKey, err)
	}
	return previous, true, nil
}

// Removed before they run, so a crash cannot run one twice; a follow-up may
// have side effects.
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

func (s *Store) FollowUps() []FollowUp {
	s.followUpsMu.Lock()
	defer s.followUpsMu.Unlock()
	followUps := make([]FollowUp, 0, len(s.followUps))
	for _, followUp := range s.followUps {
		followUps = append(followUps, followUp)
	}
	sort.Slice(followUps, func(i, j int) bool { return followUps[i].DueAt.Before(followUps[j].DueAt) })
	return followUps
}

func (s *Store) saveFollowUps() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, followUpsFilename), s.followUps); err != nil {
		return fmt.Errorf("write follow-ups: %w", err)
	}
	return nil
}
