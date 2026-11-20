package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const schedulesFilename = "schedules.json"

type Schedule struct {
	Channel   string    `json:"channel"`
	ThreadTS  string    `json:"thread_ts"`
	AuthorID  string    `json:"author_id"`
	Spec      string    `json:"spec"`
	Task      string    `json:"task"`
	NextAt    time.Time `json:"next_at"`
	Runs      int       `json:"runs"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) GetSchedule(threadKey string) (Schedule, bool) {
	s.schedulesMu.Lock()
	defer s.schedulesMu.Unlock()
	schedule, ok := s.schedules[threadKey]
	return schedule, ok
}

func (s *Store) SetSchedule(threadKey string, schedule Schedule) error {
	s.schedulesMu.Lock()
	defer s.schedulesMu.Unlock()
	previous, existed := s.schedules[threadKey]
	s.schedules[threadKey] = schedule
	if err := s.saveSchedules(); err != nil {
		if existed {
			s.schedules[threadKey] = previous
		} else {
			delete(s.schedules, threadKey)
		}
		return fmt.Errorf("save schedule %q: %w", threadKey, err)
	}
	return nil
}

func (s *Store) DeleteSchedule(threadKey string) (Schedule, bool, error) {
	s.schedulesMu.Lock()
	defer s.schedulesMu.Unlock()
	previous, existed := s.schedules[threadKey]
	if !existed {
		return Schedule{}, false, nil
	}
	delete(s.schedules, threadKey)
	if err := s.saveSchedules(); err != nil {
		s.schedules[threadKey] = previous
		return Schedule{}, false, fmt.Errorf("delete schedule %q: %w", threadKey, err)
	}
	return previous, true, nil
}

// Advanced before it runs, so a crash cannot run an occurrence twice, and
// occurrences missed while the bot was down run once rather than once each.
func (s *Store) TakeDueSchedules(now time.Time, next func(Schedule) time.Time) ([]Schedule, error) {
	s.schedulesMu.Lock()
	defer s.schedulesMu.Unlock()
	previous := make(map[string]Schedule)
	var due []Schedule
	for key, schedule := range s.schedules {
		if schedule.NextAt.After(now) {
			continue
		}
		previous[key] = schedule
		due = append(due, schedule)
		schedule.Runs++
		schedule.NextAt = next(schedule)
		s.schedules[key] = schedule
	}
	if len(due) == 0 {
		return nil, nil
	}
	if err := s.saveSchedules(); err != nil {
		for key, schedule := range previous {
			s.schedules[key] = schedule
		}
		return nil, fmt.Errorf("take due schedules: %w", err)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextAt.Before(due[j].NextAt) })
	return due, nil
}

func (s *Store) Schedules() []Schedule {
	s.schedulesMu.Lock()
	defer s.schedulesMu.Unlock()
	schedules := make([]Schedule, 0, len(s.schedules))
	for _, schedule := range s.schedules {
		schedules = append(schedules, schedule)
	}
	sort.Slice(schedules, func(i, j int) bool { return schedules[i].NextAt.Before(schedules[j].NextAt) })
	return schedules
}

func (s *Store) saveSchedules() error {
	if err := atomicWriteJSON(filepath.Join(s.dir, schedulesFilename), s.schedules); err != nil {
		return fmt.Errorf("write schedules: %w", err)
	}
	return nil
}
