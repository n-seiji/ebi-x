package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const remindersFilename = "reminders.json"

// Reminder is a one-time message a person asked for: at DueAt the bot posts
// Text in the thread where it was set and mentions AuthorID. Unlike a
// follow-up it starts no work, and a thread may hold several.
type Reminder struct {
	// ID is unique across reminders and keys the saved file.
	ID       string `json:"id"`
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	// AuthorID asked for the reminder. They are mentioned when it fires,
	// and their access is checked again then.
	AuthorID  string    `json:"author_id"`
	Text      string    `json:"text"`
	DueAt     time.Time `json:"due_at"`
	CreatedAt time.Time `json:"created_at"`
}

// AddReminder saves reminder, replacing one with the same ID.
func (s *Store) AddReminder(reminder Reminder) error {
	s.remindersMu.Lock()
	defer s.remindersMu.Unlock()
	previous, existed := s.reminders[reminder.ID]
	s.reminders[reminder.ID] = reminder
	if err := s.saveReminders(); err != nil {
		if existed {
			s.reminders[reminder.ID] = previous
		} else {
			delete(s.reminders, reminder.ID)
		}
		return fmt.Errorf("save reminder %q: %w", reminder.ID, err)
	}
	return nil
}

// DeleteReminders removes the reminders match selects and returns them,
// earliest first.
func (s *Store) DeleteReminders(match func(Reminder) bool) ([]Reminder, error) {
	s.remindersMu.Lock()
	defer s.remindersMu.Unlock()
	var removed []Reminder
	for id, reminder := range s.reminders {
		if match(reminder) {
			removed = append(removed, reminder)
			delete(s.reminders, id)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if err := s.saveReminders(); err != nil {
		for _, reminder := range removed {
			s.reminders[reminder.ID] = reminder
		}
		return nil, fmt.Errorf("delete reminders: %w", err)
	}
	sortReminders(removed)
	return removed, nil
}

// TakeDueReminders removes and returns the reminders due at now, earliest
// first. They are removed before they are posted, so a crash cannot post
// one twice; one due while the bot was down is posted when it starts again.
func (s *Store) TakeDueReminders(now time.Time) ([]Reminder, error) {
	return s.DeleteReminders(func(reminder Reminder) bool { return !reminder.DueAt.After(now) })
}

// Reminders returns every pending reminder, earliest first.
func (s *Store) Reminders() []Reminder {
	s.remindersMu.Lock()
	defer s.remindersMu.Unlock()
	reminders := make([]Reminder, 0, len(s.reminders))
	for _, reminder := range s.reminders {
		reminders = append(reminders, reminder)
	}
	sortReminders(reminders)
	return reminders
}

func sortReminders(reminders []Reminder) {
	sort.Slice(reminders, func(i, j int) bool {
		if !reminders[i].DueAt.Equal(reminders[j].DueAt) {
			return reminders[i].DueAt.Before(reminders[j].DueAt)
		}
		return reminders[i].ID < reminders[j].ID
	})
}

func (s *Store) saveReminders() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, remindersFilename), s.reminders); err != nil {
		return fmt.Errorf("write reminders: %w", err)
	}
	return nil
}
