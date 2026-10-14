package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const pullWatchesFilename = "pullwatches.json"

// PullWatch is a pull request a thread's session asked the bot to watch.
// The bot polls GitHub and resumes the session when CI fails or someone
// reviews or comments, so the agent follows its pull request through.
type PullWatch struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	// AuthorID is the user whose request started the watch; resumed turns
	// run with that user's permissions, checked again each time.
	AuthorID string `json:"author_id"`
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	URL      string `json:"url"`
	// HeadSHA and Checks are the head commit and its check state last seen.
	HeadSHA string `json:"head_sha"`
	Checks  string `json:"checks"`
	// Cursor holds the newest review and comment IDs already handled.
	Cursor PullCursor `json:"cursor"`
	// Wakes counts the turns the watch has started, which are capped so a
	// pull request cannot keep the agent running indefinitely.
	Wakes     int       `json:"wakes"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PullCursor is the newest review and comment IDs seen on a pull request,
// per kind, since each kind has its own ID sequence.
type PullCursor struct {
	Review        int64 `json:"review"`
	ReviewComment int64 `json:"review_comment"`
	Comment       int64 `json:"comment"`
}

// Key identifies the watch: one per pull request.
func (w PullWatch) Key() string {
	return fmt.Sprintf("%s/%s#%d", w.Owner, w.Repo, w.Number)
}

// PullWatches returns every watch, oldest first.
func (s *Store) PullWatches() []PullWatch {
	s.pullWatchesMu.Lock()
	defer s.pullWatchesMu.Unlock()
	watches := make([]PullWatch, 0, len(s.pullWatches))
	for _, watch := range s.pullWatches {
		watches = append(watches, watch)
	}
	sort.Slice(watches, func(i, j int) bool { return watches[i].CreatedAt.Before(watches[j].CreatedAt) })
	return watches
}

// GetPullWatch returns the watch for key.
func (s *Store) GetPullWatch(key string) (PullWatch, bool) {
	s.pullWatchesMu.Lock()
	defer s.pullWatchesMu.Unlock()
	watch, ok := s.pullWatches[key]
	return watch, ok
}

// SetPullWatch saves watch, replacing any earlier watch of the same pull
// request.
func (s *Store) SetPullWatch(watch PullWatch) error {
	s.pullWatchesMu.Lock()
	defer s.pullWatchesMu.Unlock()
	key := watch.Key()
	previous, existed := s.pullWatches[key]
	s.pullWatches[key] = watch
	if err := s.savePullWatches(); err != nil {
		if existed {
			s.pullWatches[key] = previous
		} else {
			delete(s.pullWatches, key)
		}
		return fmt.Errorf("save pull request watch %q: %w", key, err)
	}
	return nil
}

// DeletePullWatches removes the watches match selects and returns them.
func (s *Store) DeletePullWatches(match func(PullWatch) bool) ([]PullWatch, error) {
	s.pullWatchesMu.Lock()
	defer s.pullWatchesMu.Unlock()
	var removed []PullWatch
	for key, watch := range s.pullWatches {
		if match(watch) {
			removed = append(removed, watch)
			delete(s.pullWatches, key)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if err := s.savePullWatches(); err != nil {
		for _, watch := range removed {
			s.pullWatches[watch.Key()] = watch
		}
		return nil, fmt.Errorf("delete pull request watches: %w", err)
	}
	return removed, nil
}

func (s *Store) savePullWatches() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, pullWatchesFilename), s.pullWatches); err != nil {
		return fmt.Errorf("write pull request watches: %w", err)
	}
	return nil
}
