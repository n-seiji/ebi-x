package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const remindersFilename = "reminders.json"

type Reminder struct {
	ID        string    `json:"id"`
	Channel   string    `json:"channel"`
	ThreadTS  string    `json:"thread_ts"`
	AuthorID  string    `json:"author_id"`
	Text      string    `json:"text"`
	DueAt     time.Time `json:"due_at"`
	CreatedAt time.Time `json:"created_at"`
}

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

// Removed before they are posted, so a crash cannot post one twice.
func (s *Store) TakeDueReminders(now time.Time) ([]Reminder, error) {
	return s.DeleteReminders(func(reminder Reminder) bool { return !reminder.DueAt.After(now) })
}

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
