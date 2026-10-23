package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const doneWatchesFilename = "donewatches.json"

// DoneWatch is a Slack message whose @-mentioned people the bot reminds
// until each has reacted with the done reaction.
type DoneWatch struct {
	Channel string `json:"channel"`
	// ThreadTS is the thread the watch was started in, where reminders are
	// posted and where the watch is stopped.
	ThreadTS string `json:"thread_ts"`
	// MessageTS is the message whose reactions are checked.
	MessageTS string `json:"message_ts"`
	// AuthorID is the user who started the watch; it runs only while that
	// user may use the bot.
	AuthorID string `json:"author_id"`
	// Targets are the users mentioned in the message, in mention order.
	Targets []string `json:"targets"`
	// IntervalDays is the gap between two reminders.
	IntervalDays int       `json:"interval_days"`
	NextAt       time.Time `json:"next_at"`
	// Reminders counts the reminders posted, which are capped.
	Reminders int       `json:"reminders"`
	CreatedAt time.Time `json:"created_at"`
}

// Key identifies the watch: one per message.
func (w DoneWatch) Key() string {
	return w.Channel + ":" + w.MessageTS
}

// DoneWatches returns every watch, oldest first.
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

// SetDoneWatch saves watch, replacing any earlier watch of the same message.
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

// DeleteDoneWatches removes the watches match selects and returns them.
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
