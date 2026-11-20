package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const doneWatchesFilename = "donewatches.json"

type DoneWatch struct {
	Channel      string    `json:"channel"`
	ThreadTS     string    `json:"thread_ts"`
	MessageTS    string    `json:"message_ts"`
	AuthorID     string    `json:"author_id"`
	Targets      []string  `json:"targets"`
	IntervalDays int       `json:"interval_days"`
	NextAt       time.Time `json:"next_at"`
	Reminders    int       `json:"reminders"`
	CreatedAt    time.Time `json:"created_at"`
}

func (w DoneWatch) Key() string {
	return w.Channel + ":" + w.MessageTS
}

func (s *Store) DoneWatches() []DoneWatch {
	s.doneWatchesMu.Lock()
	defer s.doneWatchesMu.Unlock()
	watches := make([]DoneWatch, 0, len(s.doneWatches))
	for _, watch := range s.doneWatches {
		watches = append(watches, watch)
	}
	sort.Slice(watches, func(i, j int) bool { return watches[i].CreatedAt.Before(watches[j].CreatedAt) })
	return watches
}

func (s *Store) SetDoneWatch(watch DoneWatch) error {
	s.doneWatchesMu.Lock()
	defer s.doneWatchesMu.Unlock()
	key := watch.Key()
	previous, existed := s.doneWatches[key]
	s.doneWatches[key] = watch
	if err := s.saveDoneWatches(); err != nil {
		if existed {
			s.doneWatches[key] = previous
		} else {
			delete(s.doneWatches, key)
		}
		return fmt.Errorf("save done watch %q: %w", key, err)
	}
	return nil
}

// Does nothing when the watch is gone, so a check that raced a stop cannot
// bring the watch back.
func (s *Store) UpdateDoneWatch(key string, update func(*DoneWatch) bool) (DoneWatch, bool, error) {
	s.doneWatchesMu.Lock()
	defer s.doneWatchesMu.Unlock()
	previous, ok := s.doneWatches[key]
	if !ok {
		return DoneWatch{}, false, nil
	}
	watch := previous
	watch.Targets = append([]string(nil), previous.Targets...)
	if !update(&watch) {
		return DoneWatch{}, false, nil
	}
	s.doneWatches[key] = watch
	if err := s.saveDoneWatches(); err != nil {
		s.doneWatches[key] = previous
		return DoneWatch{}, false, fmt.Errorf("save done watch %q: %w", key, err)
	}
	return watch, true, nil
}

func (s *Store) DeleteDoneWatches(match func(DoneWatch) bool) ([]DoneWatch, error) {
	s.doneWatchesMu.Lock()
	defer s.doneWatchesMu.Unlock()
	var removed []DoneWatch
	for key, watch := range s.doneWatches {
		if match(watch) {
			removed = append(removed, watch)
			delete(s.doneWatches, key)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if err := s.saveDoneWatches(); err != nil {
		for _, watch := range removed {
			s.doneWatches[watch.Key()] = watch
		}
		return nil, fmt.Errorf("delete done watches: %w", err)
	}
	return removed, nil
}

func (s *Store) saveDoneWatches() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, doneWatchesFilename), s.doneWatches); err != nil {
		return fmt.Errorf("write done watches: %w", err)
	}
	return nil
}
